// Package event is the news/catalyst producer, ported from
// agent/producers/loop_event.py. It consumes new pending catalyst events,
// asks the model for direction and strength, and queues at most one
// high-priority intent. It never orders.
//
// Idempotency: the kv cursor "event_cursor" means each event is gathered
// once, and dedup_key "event:SYM:<event id>" binds an intent to its catalyst.
package event

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"flintrade/internal/db"
	"flintrade/internal/indicators"
	"flintrade/internal/llm"
	"flintrade/internal/loop"
	"flintrade/internal/memory"
	"flintrade/internal/producers/pj"
	"flintrade/internal/producers/technical"
	"flintrade/internal/reflect"
	"flintrade/internal/session"
)

const (
	cursorKey     = "event_cursor"
	maxCatalysts  = 8 // per round, so a backlog does not burst
	catalystKinds = `'news_catalyst','macro','earnings','news'`
)

// Loop is "loop_event".
type Loop struct {
	D        *loop.Deps
	Complete technical.Complete
	Prompt   string
}

// New builds the loop on the shared deps.
func New(d *loop.Deps) *Loop {
	return &Loop{
		D:        d,
		Complete: func(ctx context.Context, req llm.Request) (string, error) { return d.LLM.Complete(ctx, req) },
		Prompt:   filepath.Join(d.Root, "agent", "prompts", "event_intent.md"),
	}
}

func (l *Loop) Name() string { return "loop_event" }

// Catalyst is one event row with the symbol's latest news signals.
type Catalyst struct {
	Event   pj.Obj   `json:"event"`
	Signals []pj.Obj `json:"signals"`
}

func idOf(o pj.Obj) int64 {
	v, _ := o.Get("id")
	id, _ := v.(int64)
	return id
}

func symOf(o pj.Obj) any {
	v, _ := o.Get("symbol")
	return v
}

// Gather reads the oldest (at most eight) pending catalysts after the cursor,
// attaches each symbol's five latest news signals and advances the cursor to
// the last one taken — even if the model later says WAIT, the same news never
// refires. Taking the oldest first drains a backlog over successive rounds;
// Python took the newest eight and jumped the cursor past everything older,
// silently dropping the rest. The batch is still shown newest first.
func Gather(ctx context.Context, s *db.Store) ([]Catalyst, error) {
	cursor := int64(0)
	if v, ok, _ := s.KVGet(ctx, cursorKey); ok && v != "" {
		cursor, _ = strconv.ParseInt(v, 10, 64)
	}
	rows, err := s.Query(ctx, `SELECT * FROM events WHERE status='pending' AND id > ? AND kind IN (`+catalystKinds+`)
		ORDER BY id ASC LIMIT ?`, cursor, maxCatalysts)
	if err != nil {
		return nil, err
	}
	evs, err := pj.Rows(rows)
	if err != nil || len(evs) == 0 {
		return nil, err
	}
	slices.Reverse(evs)
	out := make([]Catalyst, 0, len(evs))
	maxID := cursor
	for _, ev := range evs {
		maxID = max(maxID, idOf(ev))
		srows, err := s.Query(ctx, `SELECT * FROM signals WHERE kind='news' AND symbol IS ? ORDER BY id DESC LIMIT 5`, symOf(ev))
		if err != nil {
			return nil, err
		}
		sigs, err := pj.Rows(srows)
		if err != nil {
			return nil, err
		}
		out = append(out, Catalyst{Event: ev, Signals: sigs})
	}
	if maxID > cursor {
		if err := s.KVSet(ctx, cursorKey, strconv.FormatInt(maxID, 10)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// quote is the CLI's raw quote object for symbol, or nil.
func (l *Loop) quote(ctx context.Context, symbol string) any {
	res := l.D.LB.Run(ctx, 20*time.Second, "quote", symbol, "--format", "json")
	if !res.OK || res.Data == nil {
		return nil
	}
	var list []json.RawMessage
	if json.Unmarshal(res.Data, &list) == nil {
		if len(list) == 0 {
			return nil
		}
		return list[0]
	}
	return res.Data
}

// VolumeRatio computes the 1h volume_ratio for one symbol on demand: the
// event payload has no indicator block and its symbols come from the news,
// not the collector's pool, yet the gate's hard floor must come from real
// data (2026-08 META / USO / UGL passed on model-supplied values). nil →
// the gate fails closed.
func (l *Loop) VolumeRatio(ctx context.Context, symbol string) *float64 {
	res := l.D.LB.Run(ctx, 25*time.Second, "kline", symbol, "--period", "1h", "--count", "50", "--format", "json")
	if !res.OK || res.Data == nil {
		return nil
	}
	k, err := indicators.ParseKlines(res.Data)
	if err != nil || len(k) == 0 {
		return nil
	}
	r := indicators.Analyze(k, symbol, 0)
	if r.Error != "" {
		return nil
	}
	return &r.VolumeRatio
}

// Payload is catalysts + quotes for their symbols + recall without plans
// (plans are the technical producer's watchlist; lessons/stats are shared
// memory and must reach every deciding producer — 2026-08-21 UGL).
func (l *Loop) Payload(ctx context.Context, cats []Catalyst) pj.Obj {
	quotes := pj.Obj{}
	for _, c := range cats {
		sym, ok := symOf(c.Event).(string)
		if !ok || sym == "" {
			continue
		}
		if _, seen := quotes.Get(sym); !seen {
			quotes.Set(sym, l.quote(ctx, sym))
		}
	}
	data := pj.O("now_utc", l.D.Time().UTC().Format("2006-01-02T15:04:05Z"), "catalysts", cats, "quotes", quotes)
	if rec, err := reflect.GetRecall(ctx, l.D.DB.MustAs(db.RoleReader), l.D.Time()); err != nil {
		data.Set("recall", pj.O("error", "recall unavailable: "+err.Error()))
	} else {
		rec.Plans = nil
		data.Set("recall", rec)
	}
	// Each catalyst symbol's closed-trade record, as the technical producer
	// gets for its best candidate (hard lesson: memory reaches every deciding
	// producer). Keyed by symbol; symbols Flintrade never traded are absent.
	sess := l.D.Clock.Current(ctx)
	hist := pj.Obj{}
	for _, c := range cats {
		sym, ok := symOf(c.Event).(string)
		if !ok || sym == "" {
			continue
		}
		if _, seen := hist.Get(sym); seen {
			continue
		}
		if h, err := memory.SimilarHistory(ctx, l.D.DB.MustAs(db.RoleReader), sym, sess); err == nil && h != nil {
			hist.Set(sym, h)
		}
	}
	if len(hist) > 0 {
		data.Set("similar_history", hist)
	}
	return data
}

// RunOnce is one producer round.
func (l *Loop) RunOnce(ctx context.Context) ([]string, error) {
	s := l.D.DB.MustAs(db.RoleEvent)
	if err := s.Beat(ctx, "loop_event", nil); err != nil {
		return nil, err
	}
	if halted, err := s.IsHalted(ctx); err != nil {
		return nil, err
	} else if halted {
		return []string{"halted — 跳过舆情信号产出"}, nil
	}
	// Closed: leave the cursor alone; the events wait for the open, where the
	// model judges their freshness.
	sess := l.D.Clock.Current(ctx)
	if !session.IsTradeable(sess) {
		return []string{fmt.Sprintf("session=%s — 非交易时段,跳过", sess)}, nil
	}
	cats, err := Gather(ctx, s)
	if err != nil {
		return nil, err
	}
	if len(cats) == 0 {
		return []string{"no new catalysts"}, nil
	}
	user, err := pj.Dumps(l.Payload(ctx, cats))
	if err != nil {
		return nil, err
	}
	system, err := os.ReadFile(l.Prompt)
	if err != nil {
		return nil, err
	}
	text, cerr := l.Complete(ctx, llm.Request{User: user, System: string(system), Tier: "trader", Tag: "producer:event"})
	decision := technical.ParseIntent(text)
	if decision == nil {
		line := fmt.Sprintf("无法解析 LLM 输出(前80字: %q)", pj.Truncate(text, 80))
		if cerr != nil {
			line += " — " + cerr.Error()
		}
		return []string{line}, nil
	}
	in := technical.ToIntent(decision)
	if in == nil {
		r, _ := decision["reasoning"].(string)
		return []string{"WAIT — " + pj.Truncate(r, 80)}, nil
	}
	if vr := l.VolumeRatio(ctx, in.Symbol); vr != nil {
		in.Features.Set("volume_ratio", pj.F(*vr))
	}
	// Bind to the catalyst on the same symbol, else the first one.
	eventID := idOf(cats[0].Event)
	for _, c := range cats {
		if sym, _ := symOf(c.Event).(string); sym == in.Symbol {
			eventID = idOf(c.Event)
			break
		}
	}
	risk, err := l.D.Config.Risk()
	if err != nil {
		return nil, err
	}
	dedup := fmt.Sprintf("event:%s:%d", in.Symbol, eventID)
	id, inserted, err := technical.Submit(ctx, s, "event", risk.Priority["event"], dedup, in)
	if err != nil {
		return nil, err
	}
	if !inserted {
		return []string{fmt.Sprintf("重复 catalyst 已去重(%s)", dedup)}, nil
	}
	return []string{fmt.Sprintf("%s (event #%d)", technical.Describe(id, in), eventID)}, nil
}
