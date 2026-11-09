package riskmonitor

import (
	"context"
	"strings"
	"testing"
	"time"

	"flintrade/internal/db"
	"flintrade/internal/loop"
	"flintrade/internal/session"
	"flintrade/internal/settle"
	"flintrade/internal/testutil"
)

type env struct {
	t      *testing.T
	ctx    context.Context
	boot   *db.Store
	ex     *db.Store
	m      *Monitor
	prices map[string]float64
	sess   string
	now    time.Time
	calls  int
}

func setup(t *testing.T, replace map[string]string) *env {
	d := testutil.DB(t)
	testutil.SetEquity(t, d, 10000)
	e := &env{t: t, ctx: context.Background(), boot: d.MustAs(db.RoleMigrate), ex: d.MustAs(db.RoleExecutor),
		sess: session.Intraday, now: time.Date(2026, 11, 9, 15, 0, 0, 0, time.UTC)}
	deps := &loop.Deps{DB: d, Config: testutil.Config(t, replace), Now: func() time.Time { return e.now }}
	e.m = New(deps)
	e.m.Session = func(context.Context) string { return e.sess }
	e.m.Prices = func(_ context.Context, syms []string, _ string) map[string]float64 {
		e.calls++
		out := map[string]float64{}
		for _, s := range syms {
			if p, ok := e.prices[s]; ok {
				out[s] = p
			}
		}
		return out
	}
	// Suppress the day roll unless a test wants it.
	e.boot.KVSet(e.ctx, "risk_day", e.now.In(session.ET).Format("2006-01-02"))
	return e
}

func (e *env) run(prices map[string]float64) []string {
	e.t.Helper()
	e.prices = prices
	log, err := e.m.RunOnce(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, l := range log {
		if strings.HasPrefix(l, "[error]") {
			e.t.Fatalf("duty error: %v", log)
		}
	}
	return log
}

func (e *env) pending(sym string) []db.Intent {
	e.t.Helper()
	all, err := e.boot.ClaimIntents(e.ctx, 100)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []db.Intent
	for _, it := range all {
		if it.Symbol == sym && it.Side == "close" {
			out = append(out, it)
		}
	}
	return out
}

func (e *env) clearPending() {
	e.boot.Exec(e.ctx, db.PermIntentsDecide, "UPDATE intents SET status='cancelled' WHERE status='pending'")
}

func (e *env) open(sym, side string, qty int, entry float64, stop *float64, target, target2 *float64) int64 {
	e.t.Helper()
	id, err := settle.Open(e.ctx, e.ex, settle.OpenInput{Symbol: sym, Side: side, Qty: qty, FillPrice: entry, Stop: stop,
		Target: target, Target2: target2, CommissionPerShare: 0.02, Source: "technical"})
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// test_stop_guard C1–C7: the stop is a pure rule, run every 10 s, no LLM.
func TestHardLesson_StopGuardIsMechanical(t *testing.T) {
	e := setup(t, nil)
	e.open("AAPL.US", "long", 12, 331.21, db.Ptr(328.0), db.Ptr(337.6), nil)
	log := e.run(map[string]float64{"AAPL.US": 308.55})
	p := e.pending("AAPL.US")
	if len(p) != 1 || p[0].HypothesisQty != nil {
		t.Fatalf("C1 long below stop: %v %v", p, log)
	}
	if !strings.Contains(strings.Join(log, "\n"), "308.55") || !strings.Contains(strings.Join(log, "\n"), "328.0") {
		t.Fatalf("C1 log names price and stop: %v", log)
	}
	if p[0].Priority != 101 || *p[0].Confidence != 100 || p[0].Source != "risk_monitor" || !strings.HasPrefix(*p[0].DedupKey, "stop-") {
		t.Fatalf("C1 intent shape: %+v", p[0])
	}
	e.run(map[string]float64{"AAPL.US": 307})
	if len(e.pending("AAPL.US")) != 1 {
		t.Fatal("C2 pending close must block a second one")
	}

	e = setup(t, nil)
	e.open("AAPL.US", "long", 12, 331.21, db.Ptr(328.0), nil, nil)
	e.run(map[string]float64{})
	if len(e.pending("AAPL.US")) != 0 {
		t.Fatal("C3 no price → no intent (never a stale price)")
	}
	e.run(map[string]float64{"AAPL.US": 330})
	if len(e.pending("AAPL.US")) != 0 {
		t.Fatal("C4 above stop")
	}

	e = setup(t, nil)
	e.open("TSLA.US", "short", 5, 300, db.Ptr(310.0), nil, nil)
	e.run(map[string]float64{"TSLA.US": 311.5})
	e.run(map[string]float64{"TSLA.US": 305})
	if len(e.pending("TSLA.US")) != 1 {
		t.Fatal("C5 short above stop triggers, direction not inverted")
	}

	// C6: an orphaned 'approved' close whose order is long filled must not lock the guard.
	e = setup(t, nil)
	e.open("NVDA.US", "long", 18, 215.6, db.Ptr(208.5), nil, nil)
	old, _, _ := e.boot.SubmitIntent(e.ctx, db.IntentInput{Source: "technical", Symbol: "NVDA.US", Side: "close"})
	e.boot.DecideIntent(e.ctx, old, "approved", nil)
	e.boot.CreateOrder(e.ctx, db.OrderInput{ClientOrderID: "orphan", Symbol: "NVDA.US", Side: "SELL", Qty: 18, Price: db.Ptr(210.0), IntentID: &old})
	e.boot.UpdateOrder(e.ctx, "orphan", db.Fields{"status": "filled", "broker_order_id": "B-ORPHAN"})
	e.run(map[string]float64{"NVDA.US": 207})
	if len(e.pending("NVDA.US")) != 1 {
		t.Fatal("C6 orphaned approved intent locked the stop guard")
	}
	// C6b: but a resting exit order is in flight.
	e = setup(t, nil)
	e.open("NVDA.US", "long", 18, 215.6, db.Ptr(208.5), nil, nil)
	e.boot.CreateOrder(e.ctx, db.OrderInput{ClientOrderID: "live", Symbol: "NVDA.US", Side: "SELL", Qty: 18, Price: db.Ptr(208.0)})
	e.boot.UpdateOrder(e.ctx, "live", db.Fields{"status": "submitted", "broker_order_id": "B-LIVE"})
	e.run(map[string]float64{"NVDA.US": 207})
	if len(e.pending("NVDA.US")) != 0 {
		t.Fatal("C6b resting exit order must count as in flight")
	}
	// C7: no stop → not watched.
	e = setup(t, nil)
	e.open("GLD.US", "long", 3, 250, nil, nil, nil)
	e.run(map[string]float64{"GLD.US": 1})
	if len(e.pending("GLD.US")) != 0 || e.calls != 0 {
		t.Fatal("C7 stop NULL must be skipped (and cost no quote call)")
	}
	// Closed session: no quote call at all.
	e = setup(t, nil)
	e.open("AAPL.US", "long", 1, 300, db.Ptr(290.0), nil, nil)
	e.sess = session.Closed
	e.run(map[string]float64{"AAPL.US": 1})
	if e.calls != 0 || len(e.pending("AAPL.US")) != 0 {
		t.Fatal("Closed session must not fetch quotes or trigger")
	}
}

// A rejected stop intent must be retried next minute: the dedup key carries
// a minute bucket because the unique index ignores intent status.
func TestStopRetriesNextMinute(t *testing.T) {
	e := setup(t, nil)
	e.open("AAPL.US", "long", 2, 300, db.Ptr(290.0), nil, nil)
	e.run(map[string]float64{"AAPL.US": 280})
	e.clearPending() // e.g. rejected by the gate
	e.run(map[string]float64{"AAPL.US": 280})
	if len(e.pending("AAPL.US")) != 0 {
		t.Fatal("same minute must dedup")
	}
	e.now = e.now.Add(time.Minute)
	e.run(map[string]float64{"AAPL.US": 280})
	if len(e.pending("AAPL.US")) != 1 {
		t.Fatal("next minute must retry")
	}
}

// test_invariants §9: staged take-profit, playbook symbols only.
func TestTakeProfitGuard(t *testing.T) {
	e := setup(t, nil)
	pid := e.open("MU.US", "long", 8, 900, db.Ptr(873.0), db.Ptr(945.0), db.Ptr(990.0))
	e.run(map[string]float64{"MU.US": 920})
	if len(e.pending("MU.US")) != 0 {
		t.Fatal("no level touched")
	}
	e.run(map[string]float64{"MU.US": 946})
	p := e.pending("MU.US")
	if len(p) != 1 || p[0].HypothesisQty == nil || *p[0].HypothesisQty != 4 {
		t.Fatalf("T1 half close: %+v", p)
	}
	e.run(map[string]float64{"MU.US": 946})
	if len(e.pending("MU.US")) != 1 {
		t.Fatal("pending close must block a repeat")
	}
	e.clearPending()
	pos, _ := e.ex.PositionFor(e.ctx, "MU.US")
	if pos.ID != pid {
		t.Fatal("position")
	}
	settle.Close(e.ctx, e.ex, settle.CloseInput{Held: *pos, FillPrice: 946, Qty: 4, CommissionPerShare: 0.02})
	e.run(map[string]float64{"MU.US": 947})
	if len(e.pending("MU.US")) != 0 {
		t.Fatal("T1 done: same level must not fire again")
	}
	e.run(map[string]float64{"MU.US": 991})
	p = e.pending("MU.US")
	if len(p) != 1 || p[0].HypothesisQty != nil {
		t.Fatalf("T2 clears the rest: %+v", p)
	}

	// Stop beats take-profit; qty 1 → T1 closes everything.
	e = setup(t, nil)
	e.open("SNDK.US", "long", 1, 1500, db.Ptr(1455.0), db.Ptr(1575.0), db.Ptr(1650.0))
	e.run(map[string]float64{"SNDK.US": 1450})
	p = e.pending("SNDK.US")
	if len(p) != 1 || !strings.Contains(*p[0].Reason, "stop breached") {
		t.Fatalf("stop first: %+v", p)
	}
	e.clearPending()
	e.run(map[string]float64{"SNDK.US": 1580})
	p = e.pending("SNDK.US")
	if len(p) != 1 || p[0].HypothesisQty != nil {
		t.Fatalf("qty 1 → full close: %+v", p)
	}

	// Non-playbook target stays advisory; its stop is still mechanical.
	e = setup(t, nil)
	e.open("AAPL.US", "long", 8, 300, db.Ptr(294.0), db.Ptr(306.0), nil)
	e.run(map[string]float64{"AAPL.US": 307})
	if len(e.pending("AAPL.US")) != 0 {
		t.Fatal("non-playbook target must not fire")
	}
	e.run(map[string]float64{"AAPL.US": 293})
	if p := e.pending("AAPL.US"); len(p) != 1 || !strings.Contains(*p[0].Reason, "stop breached") {
		t.Fatal("non-playbook stop must fire")
	}

	// Short playbook position: targets are below entry.
	e = setup(t, nil)
	e.open("MU.US", "short", 4, 900, db.Ptr(930.0), db.Ptr(860.0), db.Ptr(820.0))
	e.run(map[string]float64{"MU.US": 859})
	if p := e.pending("MU.US"); len(p) != 1 || *p[0].HypothesisQty != 2 {
		t.Fatalf("short T1: %+v", p)
	}
}

func TestHardLesson_DayRollAndBreaker(t *testing.T) {
	e := setup(t, nil)
	e.boot.KVSet(e.ctx, "risk_day", "2026-11-06")
	e.boot.UpdateRisk(e.ctx, db.Fields{"equity": 9400.0, "day_start_equity": 10000.0, "day_realized_pnl": -600.0})
	e.boot.SetHalt(e.ctx, true, db.Ptr("yesterday"))
	log := e.run(nil)
	r, _ := e.boot.GetRisk(e.ctx)
	if r.Halt || *r.DayStartEquity != 9400 || r.DayRealizedPnL != 0 || !strings.Contains(strings.Join(log, "\n"), "daily-reset") {
		t.Fatalf("day roll: %+v %v", r, log)
	}
	if v, _, _ := e.boot.KVGet(e.ctx, "risk_day"); v != "2026-11-09" {
		t.Fatalf("risk_day %s", v)
	}

	// 4.99% → no trip; 5% → halt, held (no re-trip) until the next day roll.
	e.boot.UpdateRisk(e.ctx, db.Fields{"equity": 8930.94})
	e.run(nil)
	if r, _ := e.boot.GetRisk(e.ctx); r.Halt {
		t.Fatal("tripped below the limit")
	}
	e.boot.UpdateRisk(e.ctx, db.Fields{"equity": 8930.0})
	log = e.run(nil)
	r, _ = e.boot.GetRisk(e.ctx)
	if !r.Halt || *r.HaltReason != "daily drawdown 5.00% >= 5.0%" || !strings.Contains(strings.Join(log, "\n"), "flatten_on_halt=false") {
		t.Fatalf("breaker: %+v %v", r, log)
	}
	if log := e.run(nil); strings.Contains(strings.Join(log, "\n"), "CIRCUIT BREAKER") {
		t.Fatal("re-tripped while halted")
	}
	e.now = e.now.Add(24 * time.Hour)
	e.run(nil)
	if r, _ := e.boot.GetRisk(e.ctx); r.Halt {
		t.Fatal("day roll must clear halt")
	}
}

func TestFlattenOnHalt(t *testing.T) {
	risk := strings.Replace(testutil.ConfigFile(t, "risk.toml"), "flatten_on_halt      = false", "flatten_on_halt      = true", 1)
	e := setup(t, map[string]string{"risk.toml": risk})
	e.open("AAPL.US", "long", 2, 300, nil, nil, nil)
	e.open("NVDA.US", "short", 2, 200, nil, nil, nil)
	e.boot.UpdateRisk(e.ctx, db.Fields{"equity": 9000.0})
	log := e.run(nil)
	if len(e.pending("AAPL.US")) != 1 || len(e.pending("NVDA.US")) != 1 {
		t.Fatalf("flatten: %v", log)
	}
	if p := e.pending("AAPL.US"); p[0].Priority != 101 || *p[0].Reason != "circuit breaker flatten" {
		t.Fatalf("flatten intent %+v", p[0])
	}
}

func TestManualSwitchesAndRoleGuard(t *testing.T) {
	e := setup(t, nil)
	if _, err := e.m.Halt(e.ctx, ""); err != nil {
		t.Fatal(err)
	}
	if r, _ := e.boot.GetRisk(e.ctx); !r.Halt || *r.HaltReason != "manual halt" {
		t.Fatal("halt")
	}
	e.m.Resume(e.ctx, "back")
	if r, _ := e.boot.GetRisk(e.ctx); r.Halt {
		t.Fatal("resume")
	}
	out, err := e.m.Status(e.ctx)
	if err != nil || !strings.Contains(strings.Join(out, "\n"), "risk_day(kv)") {
		t.Fatalf("status %v %v", out, err)
	}
	// The monitor may halt and queue intents — never touch the ledger.
	if err := e.m.store.UpdateRisk(e.ctx, db.Fields{"equity": 1.0}); err == nil {
		t.Fatal("risk_monitor wrote risk_state")
	}
	if _, err := e.m.store.OpenPosition(e.ctx, db.PositionInput{Symbol: "X", Side: "long", Qty: 1, EntryPrice: 1}); err == nil {
		t.Fatal("risk_monitor opened a position")
	}
}
