// Package quotes is the daemon's one way to read live prices, ported from
// agent/quotes.py.
//
// The top-level "last" is stale outside regular hours: on 2026-07-31 overnight
// AAPL traded at 311.01 while the top-level last still showed the prior
// regular close 333.43, so the stop guard was blind exactly when it mattered.
// Prices are therefore read from the current session's sub-quote first
// (pre_market / post_market / overnight) and only then from the top level.
package quotes

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"flintrade/internal/lb"
	"flintrade/internal/session"
)

// Quote is one symbol's raw quote object; its shape drifts across CLI
// versions, so fields are dug out defensively.
type Quote map[string]any

// lastKeys are candidate names for the latest price, in order.
var lastKeys = []string{"last", "last_done", "last_price", "price", "close"}

// subQuoteKeys maps a session to its sub-quote's candidate keys; the first is
// the name observed live (2026-08-05 overnight NVDA), the rest guard drift.
var subQuoteKeys = map[string][]string{
	session.Pre:          {"pre_market", "pre_market_quote", "premarket_quote"},
	session.Post:         {"post_market", "post_market_quote", "postmarket_quote"},
	session.Overnight:    {"overnight", "overnight_quote"},
	session.OvernightPre: {"overnight", "overnight_quote"},
}

// toFloat parses a price; zero, negative and non-finite mean "no quote".
func toFloat(v any) (float64, bool) {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case string:
		var err error
		if f, err = strconv.ParseFloat(strings.TrimSpace(x), 64); err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	if f <= 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

func digLast(obj any) (float64, bool) {
	m, ok := obj.(map[string]any)
	if !ok {
		return 0, false
	}
	for _, k := range lastKeys {
		if f, ok := toFloat(m[k]); ok {
			return f, true
		}
	}
	return 0, false
}

// LastPrice returns the current session's latest price from q: the session
// sub-quote first, the top level only as fallback. The order must not flip.
func LastPrice(q Quote, sess string) (float64, bool) {
	for _, k := range subQuoteKeys[sess] {
		if f, ok := digLast(q[k]); ok {
			return f, true
		}
	}
	return digLast(map[string]any(q))
}

// Rows normalises a quote response (list, dict, or {data:[...]}) to objects.
func Rows(payload any) []map[string]any {
	pick := func(xs []any) []map[string]any {
		var out []map[string]any
		for _, x := range xs {
			if m, ok := x.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	switch p := payload.(type) {
	case []any:
		return pick(p)
	case map[string]any:
		for _, k := range []string{"quotes", "data", "items", "list"} {
			if xs, ok := p[k].([]any); ok {
				return pick(xs)
			}
		}
		return []map[string]any{p}
	}
	return nil
}

// Source fetches quotes through the longbridge CLI.
type Source struct {
	LB      *lb.Runner
	Timeout time.Duration // default 20 s
}

// Quotes returns symbol → quote for one batched CLI call. Any failure yields
// an empty map, never an error: a loop that cannot see prices this round must
// keep running, not die.
func (s Source) Quotes(ctx context.Context, symbols []string) map[string]Quote {
	seen := map[string]bool{}
	var syms []string
	for _, x := range symbols {
		if x != "" && !seen[x] {
			seen[x] = true
			syms = append(syms, x)
		}
	}
	out := map[string]Quote{}
	if len(syms) == 0 {
		return out
	}
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	res := s.LB.Run(ctx, timeout, append(append([]string{"quote"}, syms...), "--format", "json")...)
	if !res.OK || res.Data == nil {
		return out
	}
	var payload any
	if json.Unmarshal(res.Data, &payload) != nil {
		return out
	}
	rs := Rows(payload)
	for _, r := range rs {
		sym, _ := r["symbol"].(string)
		if sym == "" {
			sym, _ = r["code"].(string)
		}
		if sym != "" {
			out[sym] = Quote(r)
		}
	}
	// Some versions omit "symbol" for a single-symbol query: claim the row.
	if len(out) == 0 && len(syms) == 1 && len(rs) > 0 {
		out[syms[0]] = Quote(rs[0])
	}
	return out
}

// LastPrices returns symbol → current-session price; symbols without a usable
// price are simply absent.
func (s Source) LastPrices(ctx context.Context, symbols []string, sess string) map[string]float64 {
	out := map[string]float64{}
	for sym, q := range s.Quotes(ctx, symbols) {
		if p, ok := LastPrice(q, sess); ok {
			out[sym] = p
		}
	}
	return out
}
