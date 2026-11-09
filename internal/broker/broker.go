// Package broker places and queries orders through the longbridge CLI, ported
// from agent/broker.py.
//
// Dry run simulates fills without touching the CLI. It is the DEFAULT when
// FLINTRADE_DRY_RUN is unset: real paper orders only when explicitly opted in.
package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"flintrade/internal/creds"
	"flintrade/internal/lb"
)

// Result is a normalised order placement outcome.
type Result struct {
	OK            bool
	BrokerOrderID string   // "" when the broker gave none
	Status        string   // Filled (dry run) | Submitted | Rejected | Unknown
	FillPrice     *float64 // dry run only: simulated immediate fill
	Raw           string   // broker's raw JSON or error text — stored verbatim for diagnosis
}

// CancelResult: OK=false does not mean the order is still resting (it may
// have filled or expired). Callers must re-check with OrderDetail.
type CancelResult struct {
	OK  bool
	Raw string
}

// Broker is what the executor, reconciler and risk monitor need. *CLI is the
// real implementation; tests may supply fakes.
type Broker interface {
	DryRun() bool
	Buy(ctx context.Context, symbol string, qty int, price float64, outsideRTH, clientOrderID string) Result
	Sell(ctx context.Context, symbol string, qty int, price float64, outsideRTH, clientOrderID string) Result
	Cancel(ctx context.Context, brokerOrderID string) CancelResult
	// OrderDetail always has a "status" key ("Unknown" on failure).
	OrderDetail(ctx context.Context, brokerOrderID string) map[string]any
	// RecentFills: today's fills, newest first; empty on failure.
	RecentFills(ctx context.Context, limit int) []map[string]any
	// Assets: account balances; nil on failure.
	Assets(ctx context.Context) any
	// Positions: ok=false means the call failed — callers MUST NOT read that
	// as flat (31,606 false drift alerts came from exactly that).
	Positions(ctx context.Context) (rows []map[string]any, ok bool)
}

const cliTimeout = 15 * time.Second

// CLI is the longbridge-backed broker.
type CLI struct {
	LB  *lb.Runner
	Dry bool
}

var dryCounter atomic.Int64

// DryRunFromEnv resolves FLINTRADE_DRY_RUN. Only an explicit 0 / false / no
// places real orders; unset, empty or anything unrecognised stays dry. (Python
// treated "" and typos as live.)
func DryRunFromEnv() bool { return DryRunValue(os.Getenv(creds.DryRun)) }

// DryRunValue applies that rule to a raw FLINTRADE_DRY_RUN value, e.g. one read
// from the credentials file by the app.
func DryRunValue(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "no":
		return false
	}
	return true
}

// New builds a broker; live mode without credentials fails loudly here
// rather than having every order silently rejected.
func New(r *lb.Runner, dry bool) (*CLI, error) {
	if !dry {
		var missing []string
		for _, k := range []string{creds.AppKey, creds.AppSecret, creds.AccessToken} {
			if os.Getenv(k) == "" {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("live broker needs %v in the environment (load the credentials file first)", missing)
		}
	}
	return &CLI{LB: r, Dry: dry}, nil
}

func (b *CLI) DryRun() bool { return b.Dry }

// run returns the parsed JSON body, or an error text.
func (b *CLI) run(ctx context.Context, args ...string) (any, string, error) {
	res := b.LB.Run(ctx, cliTimeout, args...)
	if !res.OK {
		msg := res.Err
		if msg == "" {
			msg = "longbridge call failed"
		}
		return nil, msg, errors.New(msg)
	}
	if res.Data == nil {
		msg := "could not parse CLI JSON: " + truncate(res.Out, 500)
		return nil, msg, errors.New(msg)
	}
	var v any
	if err := json.Unmarshal(res.Data, &v); err != nil {
		return nil, string(res.Data), err
	}
	return v, string(res.Data), nil
}

// ExtractOrderID pulls an order id from whatever shape the CLI returned.
func ExtractOrderID(payload any) string {
	m, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	for _, k := range []string{"order_id", "orderId", "id"} {
		switch v := m[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case float64:
			if v != 0 {
				return strconv.FormatFloat(v, 'f', -1, 64)
			}
		}
	}
	if d, ok := m["data"].(map[string]any); ok {
		return ExtractOrderID(d)
	}
	return ""
}

func (b *CLI) place(ctx context.Context, side, symbol string, qty int, price float64, outsideRTH, clientOrderID string) Result {
	if b.Dry {
		suffix := clientOrderID
		if suffix == "" {
			suffix = fmt.Sprintf("%d-%d", os.Getpid(), dryCounter.Add(1))
		}
		oid := "DRY-" + suffix
		fmt.Fprintf(os.Stderr, "[DRY-RUN] %s %d %s @ %v (outside_rth=%s) -> %s\n", strings.ToUpper(side), qty, symbol, price, outsideRTH, oid)
		p := price
		return Result{OK: true, BrokerOrderID: oid, Status: "Filled", FillPrice: &p, Raw: `{"dry_run":true}`}
	}
	payload, raw, err := b.run(ctx, "order", side, symbol, strconv.Itoa(qty), "--price", strconv.FormatFloat(price, 'f', -1, 64),
		"--outside-rth", outsideRTH, "-y", "--format", "json")
	if err != nil {
		return Result{Status: "Rejected", Raw: raw}
	}
	oid := ExtractOrderID(payload)
	if oid == "" {
		return Result{Status: "Unknown", Raw: raw}
	}
	return Result{OK: true, BrokerOrderID: oid, Status: "Submitted", Raw: raw}
}

func (b *CLI) Buy(ctx context.Context, symbol string, qty int, price float64, outsideRTH, clientOrderID string) Result {
	return b.place(ctx, "buy", symbol, qty, price, outsideRTH, clientOrderID)
}

func (b *CLI) Sell(ctx context.Context, symbol string, qty int, price float64, outsideRTH, clientOrderID string) Result {
	return b.place(ctx, "sell", symbol, qty, price, outsideRTH, clientOrderID)
}

// Cancel cancels a resting order. Needed before a replacement exit: the
// broker reserves shares against a resting sell, so a second sell is
// rejected as oversell (2026-07-31 flintrade-198/199).
func (b *CLI) Cancel(ctx context.Context, id string) CancelResult {
	if b.Dry {
		fmt.Fprintf(os.Stderr, "[DRY-RUN] CANCEL %s\n", id)
		return CancelResult{OK: true, Raw: `{"dry_run":true}`}
	}
	_, raw, err := b.run(ctx, "order", "cancel", id, "-y", "--format", "json")
	return CancelResult{OK: err == nil, Raw: raw}
}

func (b *CLI) OrderDetail(ctx context.Context, id string) map[string]any {
	if b.Dry {
		return map[string]any{"status": "Filled"}
	}
	payload, raw, err := b.run(ctx, "order", "detail", id, "--format", "json")
	if err != nil {
		return map[string]any{"status": "Unknown", "error": raw}
	}
	if m, ok := payload.(map[string]any); ok {
		if _, has := m["status"]; !has {
			m["status"] = "Unknown"
		}
		return m
	}
	return map[string]any{"status": "Unknown", "raw": raw}
}

func listOf(payload any, keys ...string) ([]any, bool) {
	switch p := payload.(type) {
	case []any:
		return p, true
	case map[string]any:
		for _, k := range keys {
			if xs, ok := p[k].([]any); ok {
				return xs, true
			}
		}
	}
	return nil, false
}

func objects(xs []any) []map[string]any {
	out := []map[string]any{}
	for _, x := range xs {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func (b *CLI) RecentFills(ctx context.Context, limit int) []map[string]any {
	if b.Dry {
		return []map[string]any{}
	}
	payload, _, err := b.run(ctx, "order", "executions", "--format", "json")
	if err != nil {
		return []map[string]any{}
	}
	xs, _ := listOf(payload, "executions", "data", "items")
	return objects(xs[:min(max(limit, 0), len(xs))])
}

func (b *CLI) Assets(ctx context.Context) any {
	if b.Dry {
		return []any{}
	}
	payload, _, err := b.run(ctx, "assets", "--format", "json")
	if err != nil {
		return nil
	}
	return payload
}

func (b *CLI) Positions(ctx context.Context) ([]map[string]any, bool) {
	if b.Dry {
		return []map[string]any{}, true
	}
	payload, _, err := b.run(ctx, "positions", "--format", "json")
	if err != nil {
		return nil, false
	}
	xs, _ := listOf(payload, "positions", "data", "items")
	return objects(xs), true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
