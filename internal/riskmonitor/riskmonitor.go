// Package riskmonitor is the independent kill-switch, ported from
// agent/risk_monitor.py. Each 10 s round:
//
//  1. day roll: a new ET date resets day_start_equity / day_realized_pnl and
//     clears halt — the only automatic un-halt; an intraday trip holds until
//     the next trading day.
//  2. position guards: a breached stop (every symbol) or a hit playbook
//     target (playbook symbols only) queues a close intent.
//  3. drawdown breaker: day drawdown ≥ daily_loss_limit_pct → halt.
//  4. on a fresh halt with flatten_on_halt, queue a close for every position.
//
// It never places orders. It only queues close intents, which the executor
// honours even while halted. Stops live here, not with the LLM: a stop that
// waits 30 minutes for a model to think is not a stop (2026-07-31 AAPL gap).
package riskmonitor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"flintrade/internal/db"
	"flintrade/internal/loop"
	"flintrade/internal/session"
)

// PriceFunc returns symbol → current-session price; absent means no price.
type PriceFunc func(ctx context.Context, symbols []string, sess string) map[string]float64

// Monitor implements loop.Loop.
type Monitor struct {
	d     *loop.Deps
	store *db.Store
	mu    sync.Mutex

	// Session and Prices default to the shared clock and quote source; tests
	// replace them.
	Session func(ctx context.Context) string
	Prices  PriceFunc
}

// New builds the monitor with the risk_monitor role.
func New(d *loop.Deps) *Monitor {
	m := &Monitor{d: d, store: d.DB.MustAs(db.RoleRiskMonitor)}
	m.Session = func(ctx context.Context) string { return d.Clock.Current(ctx) }
	m.Prices = func(ctx context.Context, syms []string, sess string) map[string]float64 {
		return d.Quotes.LastPrices(ctx, syms, sess)
	}
	return m
}

func (m *Monitor) Name() string { return "risk_monitor" }

// RunOnce runs every duty, each isolated so one failure never stops the rest.
func (m *Monitor) RunOnce(ctx context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.store.Beat(ctx, "risk_monitor", nil)
	var log []string
	if err := m.dailyReset(ctx, &log); err != nil {
		log = append(log, fmt.Sprintf("[error] daily_reset: %v", err))
	}
	if err := m.PositionGuards(ctx, &log); err != nil {
		log = append(log, fmt.Sprintf("[error] position_guards: %v", err))
	}
	halted, err := m.drawdownBreaker(ctx, &log)
	if err != nil {
		log = append(log, fmt.Sprintf("[error] drawdown_breaker: %v", err))
	}
	if halted {
		if err := m.flatten(ctx, &log); err != nil {
			log = append(log, fmt.Sprintf("[error] flatten: %v", err))
		}
	}
	return log, nil
}

func (m *Monitor) etToday() string { return m.d.Time().In(session.ET).Format("2006-01-02") }

// ── 1. day roll ──────────────────────────────────────────────────────────

func (m *Monitor) dailyReset(ctx context.Context, log *[]string) error {
	today := m.etToday()
	if v, ok, err := m.store.KVGet(ctx, "risk_day"); err != nil {
		return err
	} else if ok && v == today {
		return nil
	}
	r, err := m.store.GetRisk(ctx)
	if err != nil {
		return err
	}
	var equity *float64
	if r != nil {
		equity = r.Equity
	}
	// Python reset the baseline through a raw-SQL bypass of the role guard;
	// here it is the narrow day_roll permission.
	if err := m.store.DayRoll(ctx, equity); err != nil {
		return err
	}
	if err := m.store.SetHalt(ctx, false, db.Ptr("daily reset")); err != nil {
		return err
	}
	if err := m.store.KVSet(ctx, "risk_day", today); err != nil {
		return err
	}
	*log = append(*log, fmt.Sprintf("[daily-reset] 新交易日 %s: day_start_equity=%s, day_realized_pnl=0, halt 已清", today, pyOpt(equity)))
	return nil
}

// ── 2. position guards ───────────────────────────────────────────────────

// exitInFlight: a close is already on its way — a pending close/flatten
// intent, or a resting SELL/COVER order. An 'approved' intent does NOT count:
// orphaned approved intents (NVDA #230, AAPL #198/199/200) would otherwise
// lock the stop guard forever. Whether an order is alive is the orders
// table's call.
func (m *Monitor) exitInFlight(ctx context.Context, symbol string) (bool, error) {
	var n int
	if err := m.store.QueryRow(ctx, `SELECT count(*) FROM intents
		WHERE symbol=? AND side IN ('close','flatten') AND status='pending'`, symbol).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	if err := m.store.QueryRow(ctx, `SELECT count(*) FROM orders
		WHERE symbol=? AND side IN ('SELL','COVER') AND status IN ('submitted','partial')`, symbol).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// PositionGuards checks stops (every symbol, priority) and staged take-profit
// (playbook symbols only; elsewhere the target stays advisory and the LLM
// owns it) at one price per position.
func (m *Monitor) PositionGuards(ctx context.Context, log *[]string) error {
	positions, err := m.store.OpenPositions(ctx)
	if err != nil {
		return err
	}
	strategies, err := m.d.Config.Strategies()
	if err != nil {
		return err
	}
	var watched []db.Position
	for _, p := range positions {
		playbook := strategies.PlaybookFor(p.Symbol) != nil
		if p.Stop != nil || (playbook && (p.Target != nil || p.Target2 != nil)) {
			watched = append(watched, p)
		}
	}
	if len(watched) == 0 {
		return nil
	}
	sess := m.Session(ctx)
	if sess == session.Closed {
		return nil // nothing tradeable; save the quote budget
	}
	risk, err := m.d.Config.Risk()
	if err != nil {
		return err
	}
	syms := make([]string, len(watched))
	for i, p := range watched {
		syms[i] = p.Symbol
	}
	// Session-aware prices: the top-level last is the prior regular close
	// outside RTH — that stale price hid the 07-31 gap.
	prices := m.Prices(ctx, syms, sess)
	prio := risk.Priority["user"] + 1
	// Minute bucket in the dedup key: the unique index ignores status, so a
	// fixed key, once rejected, could never be resubmitted. One retry per
	// minute neither floods nor permanently mutes a stop.
	bucket := m.d.Time().In(session.ET).Format("200601021504")

	submit := func(p db.Position, key, reason string, qty *int, price float64) (int64, bool, error) {
		return m.store.SubmitIntent(ctx, db.IntentInput{Source: "risk_monitor", Priority: prio, Symbol: p.Symbol,
			Side: "close", HypothesisQty: qty, EntryHint: &price, Confidence: db.Ptr(100),
			DedupKey: db.Ptr(fmt.Sprintf("%s-%d-%s", key, p.ID, bucket)), Reason: &reason})
	}

	for _, p := range watched {
		price, ok := prices[p.Symbol]
		if !ok {
			continue // no price, no verdict — never trigger on a stale one
		}
		long := p.Side == "long"

		if p.Stop != nil {
			stop := *p.Stop
			if (long && price <= stop) || (!long && price >= stop) {
				busy, err := m.exitInFlight(ctx, p.Symbol)
				if err != nil {
					return err
				}
				if busy {
					continue
				}
				id, ins, err := submit(p, "stop", fmt.Sprintf("stop breached: %s %s last %s vs stop %s (session=%s)",
					p.Side, p.Symbol, pyStr(price), pyStr(stop), sess), nil, price)
				if err != nil {
					return err
				}
				if ins {
					*log = append(*log, fmt.Sprintf("[stop-guard] %s %s 现价 %s 越过止损 %s → 投 close 意图 #%d",
						p.Symbol, p.Side, pyStr(price), pyStr(stop), id))
				}
				continue // stop wins; no take-profit check this round
			}
		}

		if strategies.PlaybookFor(p.Symbol) == nil {
			continue
		}
		t1Pending := !p.T1Done
		beyond := func(level float64) bool {
			if long {
				return price >= level
			}
			return price <= level
		}
		hit1 := t1Pending && p.Target != nil && beyond(*p.Target)
		hit2 := !t1Pending && p.Target2 != nil && beyond(*p.Target2)
		if !hit1 && !hit2 {
			continue
		}
		busy, err := m.exitInFlight(ctx, p.Symbol)
		if err != nil {
			return err
		}
		if busy {
			continue
		}
		if hit1 && p.Target2 != nil && p.Qty >= 2 {
			half := p.Qty / 2
			id, ins, err := submit(p, "tp1", fmt.Sprintf("target1 hit: %s %s last %s vs target %s → 出半仓 %d/%d",
				p.Side, p.Symbol, pyStr(price), pyStr(*p.Target), half, p.Qty), &half, price)
			if err != nil {
				return err
			}
			if ins {
				*log = append(*log, fmt.Sprintf("[target-guard] %s T1 %s 命中 → 平半仓 %d/%d,意图 #%d",
					p.Symbol, pyStr(*p.Target), half, p.Qty, id))
			}
			continue
		}
		which, tgt := "T1", *p.Target
		if hit2 {
			which, tgt = "T2", *p.Target2
		}
		id, ins, err := submit(p, "tp", fmt.Sprintf("target hit (%s): %s %s last %s vs %s → 清仓 %d",
			which, p.Side, p.Symbol, pyStr(price), pyStr(tgt), p.Qty), nil, price)
		if err != nil {
			return err
		}
		if ins {
			*log = append(*log, fmt.Sprintf("[target-guard] %s %s %s 命中 → 清仓 %d,意图 #%d", p.Symbol, which, pyStr(tgt), p.Qty, id))
		}
	}
	return nil
}

// ── 3. drawdown breaker ──────────────────────────────────────────────────

// drawdownBreaker halts on a day drawdown past the limit; true = just halted.
func (m *Monitor) drawdownBreaker(ctx context.Context, log *[]string) (bool, error) {
	r, err := m.store.GetRisk(ctx)
	if err != nil || r == nil || r.Halt {
		return false, err // already halted: it holds until the day roll
	}
	if r.DayStartEquity == nil || r.Equity == nil || *r.DayStartEquity <= 0 {
		return false, nil // no baseline yet
	}
	risk, err := m.d.Config.Risk()
	if err != nil {
		return false, err
	}
	limit := risk.CircuitBreaker.DailyLossLimitPct
	ds, eq := *r.DayStartEquity, *r.Equity
	dd := (ds - eq) / ds * 100.0
	if dd < limit {
		return false, nil
	}
	reason := fmt.Sprintf("daily drawdown %.2f%% >= %s%%", dd, pyStr(limit))
	if err := m.store.SetHalt(ctx, true, &reason); err != nil {
		return false, err
	}
	*log = append(*log, fmt.Sprintf("[!! CIRCUIT BREAKER !!] HALT — %s (day_start=%s, equity=%s)", reason, pyStr(ds), pyStr(eq)))
	return true, nil
}

// ── 4. flatten on halt ───────────────────────────────────────────────────

func (m *Monitor) flatten(ctx context.Context, log *[]string) error {
	risk, err := m.d.Config.Risk()
	if err != nil {
		return err
	}
	if !risk.CircuitBreaker.FlattenOnHalt {
		*log = append(*log, "[flatten] flatten_on_halt=false:仅停新单,现有持仓交给人/Executor")
		return nil
	}
	prio := risk.Priority["user"] + 1 // above any queued entry
	positions, err := m.store.OpenPositions(ctx)
	if err != nil {
		return err
	}
	if len(positions) == 0 {
		*log = append(*log, "[flatten] 无持仓需平")
		return nil
	}
	for _, p := range positions {
		id, ins, err := m.store.SubmitIntent(ctx, db.IntentInput{Source: "risk_monitor", Priority: prio,
			Symbol: p.Symbol, Side: "close", Reason: db.Ptr("circuit breaker flatten"),
			DedupKey: db.Ptr(fmt.Sprintf("cb-flatten-%d", p.ID))})
		if err != nil {
			return err
		}
		if ins {
			*log = append(*log, fmt.Sprintf("[flatten] 投递 close 意图 #%d %s (priority=%d)", id, p.Symbol, prio))
		} else {
			*log = append(*log, fmt.Sprintf("[flatten] %s 强平意图已存在(去重),跳过", p.Symbol))
		}
	}
	return nil
}

// ── manual switches (CLI) ────────────────────────────────────────────────

// Halt stops new entries until Resume or the next day roll.
func (m *Monitor) Halt(ctx context.Context, reason string) ([]string, error) {
	if reason == "" {
		reason = "manual halt"
	}
	if err := m.store.SetHalt(ctx, true, &reason); err != nil {
		return nil, err
	}
	return []string{"[manual] HALT — " + reason}, nil
}

// Resume clears a halt.
func (m *Monitor) Resume(ctx context.Context, reason string) ([]string, error) {
	if reason == "" {
		reason = "manual resume"
	}
	if err := m.store.SetHalt(ctx, false, &reason); err != nil {
		return nil, err
	}
	return []string{"[manual] RESUME — " + reason}, nil
}

// Status renders risk_state for humans.
func (m *Monitor) Status(ctx context.Context) ([]string, error) {
	r, err := m.store.GetRisk(ctx)
	if err != nil {
		return nil, err
	}
	positions, err := m.store.OpenPositions(ctx)
	if err != nil {
		return nil, err
	}
	out := []string{"── risk_state ──"}
	if r == nil {
		out = append(out, "(risk_state 行缺失;请先初始化数据库)")
	} else {
		row := func(k, v string) { out = append(out, fmt.Sprintf("  %-18s = %s", k, v)) }
		row("equity", pyOpt(r.Equity))
		row("day_start_equity", pyOpt(r.DayStartEquity))
		row("day_realized_pnl", pyStr(r.DayRealizedPnL))
		row("open_risk", pyStr(r.OpenRisk))
		row("halt", strconv.FormatBool(r.Halt))
		row("halt_reason", strOpt(r.HaltReason))
		row("updated_at", strOpt(r.UpdatedAt))
	}
	day, ok, _ := m.store.KVGet(ctx, "risk_day")
	if !ok {
		day = "None"
	}
	out = append(out, fmt.Sprintf("  %-18s = %s", "risk_day(kv)", day))
	out = append(out, fmt.Sprintf("  %-18s = %d", "open_positions", len(positions)))
	out = append(out, fmt.Sprintf("  %-18s = %s", "session(now)", m.Session(ctx)))
	return out, nil
}

// pyStr renders a float the way Python's str() does (3.0, not 3).
func pyStr(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".eEnN") {
		s += ".0"
	}
	return s
}

func pyOpt(f *float64) string {
	if f == nil {
		return "None"
	}
	return pyStr(*f)
}

func strOpt(s *string) string {
	if s == nil {
		return "None"
	}
	return *s
}
