// Package news is the news collector, ported from
// agent/producers/news_collector.py: poll each universe symbol's news via the
// longbridge CLI, record new items as signals, and file a news_catalyst event
// for items whose title hits a catalyst keyword. No model, no orders.
//
// Dedup: a rolling "seen" list per symbol in kv (news_seen_<SYMBOL>).
package news

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"flintrade/internal/db"
	"flintrade/internal/loop"
	"flintrade/internal/producers/pj"
)

const (
	newsCount    = 20
	fetchTimeout = 30 * time.Second
	seenCap      = 500
)

// CatalystKeywords are matched as lowercase substrings of the title.
var CatalystKeywords = []string{
	// earnings / guidance
	"earnings", "guidance", "revenue", "profit warning", "outlook",
	"财报", "业绩", "指引", "预期", "盈利预警",
	// regulatory / legal
	"fda", "sec", "doj", "antitrust", "lawsuit", "subpoena", "investigation",
	"recall", "probe",
	"监管", "诉讼", "调查", "反垄断", "召回", "处罚", "罚款",
	// ratings
	"upgrade", "downgrade", "initiates", "price target", "rating",
	"上调", "下调", "评级", "目标价", "增持", "减持", "买入", "卖出",
	// M&A / capital actions
	"merger", "acquisition", "acquire", "buyout", "takeover", "stake",
	"spinoff", "ipo", "buyback", "dividend",
	"并购", "收购", "重组", "回购", "分拆", "分红", "入股",
	// shocks
	"bankruptcy", "default", "resign", "ceo", "halt", "delist",
	"破产", "违约", "辞职", "停牌", "退市",
}

// MatchedKeywords returns the catalyst keywords text contains.
func MatchedKeywords(text string) []string {
	low := strings.ToLower(text)
	var out []string
	for _, kw := range CatalystKeywords {
		if strings.Contains(low, kw) {
			out = append(out, kw)
		}
	}
	return out
}

// Item is one news item: the CLI's fields verbatim, in order, plus
// normalised _id / _title / _published_at / _url.
type Item struct {
	Obj         pj.Obj
	ID          *string
	Title       string
	PublishedAt any // raw scalar or nil
}

func scalar(raw json.RawMessage) any {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil
	}
	return v
}

// first is the first candidate key whose value is neither null nor "".
func first(o pj.Obj, keys ...string) any {
	for _, k := range keys {
		if v, ok := o.Get(k); ok {
			s := scalar(v.(json.RawMessage))
			if s != nil && s != "" {
				return s
			}
		}
	}
	return nil
}

// Normalize adds the canonical fields, keeping every original field.
func Normalize(o pj.Obj) Item {
	nid := first(o, "id", "news_id", "article_id", "uuid")
	title := first(o, "title", "headline", "name")
	if title == nil {
		title = ""
	}
	it := Item{Title: pj.Str(title), PublishedAt: first(o, "published_at", "publish_time", "time", "date", "ts")}
	var idVal any
	if nid != nil {
		s := pj.Str(nid)
		it.ID, idVal = &s, s
	}
	o.Set("_id", idVal)
	o.Set("_title", title)
	o.Set("_published_at", it.PublishedAt)
	o.Set("_url", first(o, "url", "link", "href"))
	it.Obj = o
	return it
}

// falsy is Python truthiness for a decoded scalar.
func falsy(v any) bool {
	if f, ok := pj.Float(v); ok {
		return f == 0
	}
	return v == nil || v == false || v == ""
}

// Key is the dedup key: the id, else "title|published", else none.
func (it Item) Key() (string, bool) {
	if it.ID != nil && *it.ID != "" {
		return *it.ID, true
	}
	pub := ""
	if !falsy(it.PublishedAt) {
		pub = pj.Str(it.PublishedAt)
	}
	if it.Title != "" || pub != "" {
		return it.Title + "|" + pub, true
	}
	return "", false
}

// Fetch returns symbol's latest news; any failure is an empty list.
func Fetch(ctx context.Context, d *loop.Deps, symbol string) []Item {
	res := d.LB.Run(ctx, fetchTimeout, "news", symbol, "--count", strconv.Itoa(newsCount), "--format", "json")
	if !res.OK {
		fmt.Printf("[news_collector] fetch %s 失败: %s\n", symbol, pj.Truncate(res.Err, 200))
		return nil
	}
	if res.Data == nil {
		return nil
	}
	data := res.Data
	if o, ok := pj.DecodeObj(data); ok {
		data = nil
		for _, k := range []string{"items", "news", "data", "list"} {
			if v, ok := o.Get(k); ok && strings.HasPrefix(strings.TrimSpace(string(v.(json.RawMessage))), "[") {
				data = v.(json.RawMessage)
				break
			}
		}
		if data == nil {
			wrapped, _ := json.Marshal([]json.RawMessage{res.Data})
			data = wrapped
		}
	}
	var list []json.RawMessage
	if json.Unmarshal(data, &list) != nil {
		return nil
	}
	var out []Item
	for _, raw := range list {
		if o, ok := pj.DecodeObj(raw); ok {
			out = append(out, Normalize(o))
		}
	}
	return out
}

func loadSeen(ctx context.Context, s *db.Store, symbol string) []string {
	v, _, _ := s.KVGet(ctx, "news_seen_"+symbol)
	var seen []string
	if v == "" || json.Unmarshal([]byte(v), &seen) != nil {
		return nil
	}
	return seen
}

// Loop is "news_collector".
type Loop struct {
	D     *loop.Deps
	Sleep func(ctx context.Context, d time.Duration) // between symbols; tests stub it
}

// New builds the loop on the shared deps.
func New(d *loop.Deps) *Loop { return &Loop{D: d} }

func (l *Loop) Name() string { return "news_collector" }

func (l *Loop) sleep(ctx context.Context, d time.Duration) {
	if l.Sleep != nil {
		l.Sleep(ctx, d)
		return
	}
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

// CollectSymbol records symbol's new items; returns (signals, catalysts).
func (l *Loop) CollectSymbol(ctx context.Context, s *db.Store, symbol string) (int, int, error) {
	items := Fetch(ctx, l.D, symbol)
	if len(items) == 0 {
		return 0, 0, nil
	}
	seen := loadSeen(ctx, s, symbol)
	nNew, nCat := 0, 0
	sym := symbol
	for _, it := range items {
		key, ok := it.Key()
		if !ok || slices.Contains(seen, key) {
			continue
		}
		if _, err := s.AddSignal(ctx, db.SignalInput{Source: "news_collector", Symbol: &sym, Kind: db.Ptr("news"), Payload: it.Obj}); err != nil {
			return nNew, nCat, err
		}
		nNew++
		seen = append(seen, key)
		if kws := MatchedKeywords(it.Title); len(kws) > 0 {
			payload := append(pj.Obj{}, it.Obj...)
			payload.Set("catalyst_keywords", kws)
			firesAt := db.Now() // Python: _published_at or now() — 0 / False count as missing
			if !falsy(it.PublishedAt) {
				firesAt = pj.Str(it.PublishedAt)
			}
			if _, err := s.AddEvent(ctx, db.EventInput{Symbol: &sym, Kind: db.Ptr("news_catalyst"), Title: db.Ptr(it.Title),
				FiresAt: &firesAt, Payload: payload}); err != nil {
				return nNew, nCat, err
			}
			nCat++
		}
	}
	if nNew > 0 {
		if len(seen) > seenCap {
			seen = seen[len(seen)-seenCap:]
		}
		b, _ := pj.Dumps(seen) // json.dumps(trimmed, ensure_ascii=False)
		if err := s.KVSet(ctx, "news_seen_"+symbol, b); err != nil {
			return nNew, nCat, err
		}
	}
	return nNew, nCat, nil
}

// RunOnce polls every universe symbol, throttled against 429s. One symbol's
// failure is isolated from the rest.
func (l *Loop) RunOnce(ctx context.Context) ([]string, error) {
	s := l.D.DB.MustAs(db.RoleNews)
	if err := s.Beat(ctx, "news_collector", nil); err != nil {
		return nil, err
	}
	t, err := l.D.Config.Trading()
	if err != nil {
		return nil, err
	}
	throttle := time.Duration(t.Cadence.NewsSymbolThrottleSec * float64(time.Second))
	var log []string
	for i, sym := range t.Universe.Symbols {
		n, c, err := l.CollectSymbol(ctx, s, sym)
		switch {
		case err != nil:
			log = append(log, fmt.Sprintf("%s: error %v", sym, err))
		case n > 0:
			line := fmt.Sprintf("%s: +%d news", sym, n)
			if c > 0 {
				line += fmt.Sprintf(" (%d catalyst)", c)
			}
			log = append(log, line)
		}
		if i < len(t.Universe.Symbols)-1 {
			l.sleep(ctx, throttle)
		}
	}
	if len(log) == 0 {
		log = []string{"no new news"}
	}
	return log, nil
}
