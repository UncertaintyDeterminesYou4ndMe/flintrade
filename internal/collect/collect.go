// Package collect builds the market-data payload the technical producer hands
// to the model. It replaces scripts/collect.sh: same fields, same order, same
// symbol selection, with indicators computed in-process instead of piping
// every kline through a Python subprocess.
//
// Differences from collect.sh, all deliberate:
//   - The quote pool is trading.toml [universe], not a hardcoded copy of it
//     (collect.sh missed SNDK/MU quotes; their klines came via strategies.toml).
//   - A failing CLI call degrades only its own field to [] and is listed in
//     collect_errors. collect.sh lost the whole payload on any one failure.
package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"flintrade/internal/config"
	"flintrade/internal/indicators"
	"flintrade/internal/lb"
	"flintrade/internal/quotes"
	"flintrade/internal/session"
)

// Time is the payload's clock block; ET drives the model's sense of session.
type Time struct {
	UTC     string `json:"utc"`
	ET      string `json:"et"`
	BJ      string `json:"bj"`
	DOW     int    `json:"dow"` // ISO: Mon=1 … Sun=7, in ET
	DOWName string `json:"dow_name"`
}

// Account is the broker's view, passed through verbatim.
type Account struct {
	Balance      json.RawMessage `json:"balance"`
	Positions    json.RawMessage `json:"positions"`
	RecentOrders json.RawMessage `json:"recent_orders"`
}

// Payload mirrors collect.sh's output; field order is the JSON key order.
type Payload struct {
	Time           Time              `json:"time"`
	Session        json.RawMessage   `json:"session"`
	CurrentSession string            `json:"current_session"`
	OutsideRTH     string            `json:"outside_rth"`
	Account        Account           `json:"account"`
	Quotes         json.RawMessage   `json:"quotes"`
	Indicators     []json.RawMessage `json:"indicators"`
	Errors         []string          `json:"collect_errors,omitempty"`
}

// Collector gathers one payload. Now defaults to time.Now.
type Collector struct {
	LB      *lb.Runner
	Config  config.Loader
	Now     func() time.Time
	Workers int // concurrent kline fetches; default 4 (the runner still paces starts)
}

var (
	bj      = time.FixedZone("CST", 8*3600)
	dowName = []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	empty   = json.RawMessage("[]")
)

// ClockBlock formats t the way collect.sh's three `date` calls did.
func ClockBlock(t time.Time) Time {
	et := t.In(session.ET)
	dow := int(et.Weekday())
	if dow == 0 {
		dow = 7
	}
	return Time{
		UTC:     t.UTC().Format("2006-01-02T15:04:05Z"),
		ET:      et.Format("2006-01-02T15:04:05 MST"),
		BJ:      t.In(bj).Format("2006-01-02T15:04:05") + " CST",
		DOW:     dow,
		DOWName: dowName[dow-1],
	}
}

// Collect builds the payload. currentSession / outsideRTH are resolved by the
// caller (the producer already holds the session clock).
func (c Collector) Collect(ctx context.Context, currentSession, outsideRTH string) (*Payload, error) {
	trading, err := c.Config.Trading()
	if err != nil {
		return nil, err
	}
	strategies, err := c.Config.Strategies()
	if err != nil {
		return nil, err
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	p := &Payload{Time: ClockBlock(now()), CurrentSession: currentSession, OutsideRTH: outsideRTH}

	call := func(field string, timeout time.Duration, args ...string) json.RawMessage {
		res := c.LB.Run(ctx, timeout, append(args, "--format", "json")...)
		if !res.OK || res.Data == nil {
			why := res.Err
			if why == "" {
				why = "no JSON in output"
			}
			p.Errors = append(p.Errors, field+": "+truncate(why, 200))
			return empty
		}
		return res.Data
	}

	p.Account.Balance = call("balance", 20*time.Second, "assets")
	p.Account.Positions = call("positions", 20*time.Second, "positions")
	p.Account.RecentOrders = firstN(call("recent_orders", 20*time.Second, "order", "executions"), 20)

	market := trading.Universe.Market
	pool := trading.Universe.Symbols
	p.Quotes = call("quotes", 20*time.Second, append([]string{"quote"}, dedup(append(slices.Clone(pool), market...))...)...)

	playbook, lookback := playbookSymbols(strategies)
	var rows []map[string]any
	var raw any
	if json.Unmarshal(p.Quotes, &raw) == nil {
		if xs, ok := raw.([]any); ok && len(xs) > 0 {
			rows = quotes.Rows(raw)
		}
	}
	p.Indicators = c.indicators(ctx, KlineSymbols(market, playbook, pool, rows), playbook, lookback)

	p.Session = call("session", 15*time.Second, "trading", "session")
	return p, nil
}

// playbookSymbols returns every playbook-bound symbol (file order, unique) and
// the widest h4 confirmation window, never below 12.
func playbookSymbols(s *config.Strategies) ([]string, int) {
	var syms []string
	lookback := 12
	for _, pb := range s.Playbooks {
		for _, sym := range pb.Symbols {
			if !slices.Contains(syms, sym) {
				syms = append(syms, sym)
			}
		}
		lookback = max(lookback, pb.Lookback)
	}
	return syms, lookback
}

// KlineSymbols orders the kline fetch: market regime first, then playbook
// symbols (they must always get indicators or a binding playbook can never
// fire), then the rest of the pool by dollar volume, highest first. Ranking
// is by turnover, not share count — a $1,500 stock trading $26B/day would
// otherwise lose to every $200 mega-cap. Nothing is truncated: on 2026-08 the
// old top-5 cut left USO/UGL/AGQ without indicators in all 514 rounds while
// the risk gate's volume_ratio floor depended on them.
func KlineSymbols(market, playbook, pool []string, quoteRows []map[string]any) []string {
	head := append(slices.Clone(market), playbook...)
	out := slices.Clone(head)
	if len(quoteRows) == 0 {
		for _, s := range pool {
			if !slices.Contains(head, s) {
				out = append(out, s)
			}
		}
		return out
	}
	var tradeable []map[string]any
	for _, q := range quoteRows {
		sym, _ := q["symbol"].(string)
		if !slices.Contains(head, sym) {
			tradeable = append(tradeable, q)
		}
	}
	sort.SliceStable(tradeable, func(i, j int) bool { return dollarVol(tradeable[i]) > dollarVol(tradeable[j]) })
	for _, q := range tradeable {
		if sym, _ := q["symbol"].(string); sym != "" {
			out = append(out, sym)
		}
	}
	return out
}

func pyFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

func dollarVol(q map[string]any) float64 {
	if t, ok := q["turnover"]; ok && t != nil {
		if f, ok := pyFloat(t); ok {
			return f
		}
	}
	vol, last := any(0.0), any(0.0)
	if v, ok := q["volume"]; ok {
		vol = v
	}
	if v, ok := q["last"]; ok {
		last = v
	}
	fv, ok1 := pyFloat(vol)
	fl, ok2 := pyFloat(last)
	if !ok1 || !ok2 {
		return 0
	}
	return fv * fl
}

// indicators fetches 1h klines and analyses each symbol, keeping input order.
// Playbook symbols get 240 bars and the h4 snapshot; the rest 50 bars and no
// h4, so their payload is unchanged by the playbook's existence. A symbol
// whose kline call fails or returns nothing is left out, as collect.sh did.
func (c Collector) indicators(ctx context.Context, symbols, playbook []string, lookback int) []json.RawMessage {
	workers := c.Workers
	if workers <= 0 {
		workers = 4
	}
	results := make([]json.RawMessage, len(symbols))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i, sym := range symbols {
		count, h4 := 50, 0
		if slices.Contains(playbook, sym) {
			count, h4 = 240, lookback
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			res := c.LB.Run(ctx, 30*time.Second, "kline", sym, "--period", "1h", "--count", strconv.Itoa(count), "--format", "json")
			if !res.OK || res.Data == nil {
				return
			}
			var bars []json.RawMessage
			if json.Unmarshal(res.Data, &bars) == nil && len(bars) == 0 {
				return
			}
			data := res.Data
			// Without h4 only the last 50 bars are read (as indicators.py does),
			// so a malformed older bar must not fail the symbol.
			if h4 == 0 && len(bars) > 50 {
				data, _ = json.Marshal(bars[len(bars)-50:])
			}
			k, err := indicators.ParseKlines(data)
			if err != nil {
				results[i] = calcFailed(sym)
				return
			}
			b, err := indicators.Analyze(k, sym, h4).MarshalJSON()
			if err != nil {
				results[i] = calcFailed(sym)
				return
			}
			results[i] = b
		}()
	}
	wg.Wait()
	out := make([]json.RawMessage, 0, len(results))
	for _, r := range results {
		if r != nil {
			out = append(out, r)
		}
	}
	return out
}

func calcFailed(sym string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"symbol": sym, "error": "calc failed"})
	return b
}

// firstN keeps the first n elements of a JSON array; anything else becomes [].
func firstN(raw json.RawMessage, n int) json.RawMessage {
	var xs []json.RawMessage
	if json.Unmarshal(raw, &xs) != nil {
		return empty
	}
	var sb strings.Builder
	enc := json.NewEncoder(&sb) // json.Marshal would rewrite < > & inside the raw rows
	enc.SetEscapeHTML(false)
	enc.Encode(xs[:min(n, len(xs))])
	return json.RawMessage(strings.TrimSpace(sb.String()))
}

func dedup(xs []string) []string {
	var out []string
	for _, x := range xs {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// JSON renders the payload as collect.sh printed it (indent 2, no escaping of
// non-ASCII or HTML characters).
func (p *Payload) JSON() ([]byte, error) {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(p); err != nil {
		return nil, fmt.Errorf("collect: %w", err)
	}
	return []byte(strings.TrimRight(sb.String(), "\n")), nil
}
