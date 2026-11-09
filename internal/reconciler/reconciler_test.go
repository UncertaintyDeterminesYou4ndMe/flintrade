package reconciler

import (
	"context"
	"strings"
	"testing"

	"flintrade/internal/broker"
	"flintrade/internal/db"
	"flintrade/internal/loop"
	"flintrade/internal/testutil"
)

// fakeBroker answers order_detail / positions / assets on demand.
type fakeBroker struct {
	dry       bool
	detail    map[string]any
	positions []map[string]any
	posOK     bool
	assets    any
}

func (f *fakeBroker) DryRun() bool { return f.dry }
func (f *fakeBroker) Buy(context.Context, string, int, float64, string, string) broker.Result {
	panic("reconciler must never place orders")
}
func (f *fakeBroker) Sell(context.Context, string, int, float64, string, string) broker.Result {
	panic("reconciler must never place orders")
}
func (f *fakeBroker) Cancel(context.Context, string) broker.CancelResult {
	panic("reconciler must never cancel")
}
func (f *fakeBroker) OrderDetail(context.Context, string) map[string]any {
	if f.detail == nil {
		return map[string]any{"status": "Unknown"}
	}
	out := map[string]any{}
	for k, v := range f.detail {
		out[k] = v
	}
	return out
}
func (f *fakeBroker) RecentFills(context.Context, int) []map[string]any { return nil }
func (f *fakeBroker) Assets(context.Context) any                        { return f.assets }
func (f *fakeBroker) Positions(context.Context) ([]map[string]any, bool) {
	return f.positions, f.posOK
}

type env struct {
	t    *testing.T
	ctx  context.Context
	d    *loop.Deps
	boot *db.Store
	r    *Reconciler
}

func setup(t *testing.T, b *fakeBroker, replace map[string]string) *env {
	d := testutil.DB(t)
	testutil.SetEquity(t, d, 10000)
	deps := &loop.Deps{DB: d, Config: testutil.Config(t, replace), Broker: b}
	return &env{t: t, ctx: context.Background(), d: deps, boot: d.MustAs(db.RoleMigrate), r: New(deps)}
}

func (e *env) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.boot.QueryRow(e.ctx, q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) run() []string {
	e.t.Helper()
	log, err := e.r.RunOnce(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, l := range log {
		if strings.Contains(l, "ERROR") {
			e.t.Fatalf("duty error: %v", log)
		}
	}
	return log
}

func (e *env) openPos(sym, side string, qty int, entry float64, stop, riskAmt *float64) int64 {
	e.t.Helper()
	id, err := e.boot.OpenPosition(e.ctx, db.PositionInput{Symbol: sym, Side: side, Qty: qty, EntryPrice: entry,
		Stop: stop, RiskAmt: riskAmt, Source: db.Ptr("technical")})
	if err != nil {
		e.t.Fatal(err)
	}
	if riskAmt != nil {
		r, _ := e.boot.GetRisk(e.ctx)
		e.boot.UpdateRisk(e.ctx, db.Fields{"open_risk": r.OpenRisk + *riskAmt})
	}
	return id
}

// seedDeferred: a submitted order plus its intent parked in 'approved' — the
// scene after the executor placed a limit order that did not fill at once.
func (e *env) seedDeferred(side string, intentSide string, stop *float64, feats any) int64 {
	e.t.Helper()
	iid, _, err := e.boot.SubmitIntent(e.ctx, db.IntentInput{Source: "technical", Symbol: "AAPL.US", Side: intentSide,
		EntryHint: db.Ptr(312.30), Stop: stop, Target: db.Ptr(340.0), Reason: db.Ptr("deferred"), Features: feats})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.boot.DecideIntent(e.ctx, iid, "approved", nil); err != nil {
		e.t.Fatal(err)
	}
	coid := "flintrade-def"
	if _, err := e.boot.CreateOrder(e.ctx, db.OrderInput{ClientOrderID: coid, Symbol: "AAPL.US", Side: side, Qty: 12,
		Price: db.Ptr(312.30), IntentID: &iid}); err != nil {
		e.t.Fatal(err)
	}
	if err := e.boot.UpdateOrder(e.ctx, coid, db.Fields{"broker_order_id": "B-DEF", "status": "submitted"}); err != nil {
		e.t.Fatal(err)
	}
	return iid
}

func (e *env) intentStatus(id int64) string {
	e.t.Helper()
	it, err := e.boot.Intent(e.ctx, id)
	if err != nil || it == nil {
		e.t.Fatalf("intent %d: %v", id, err)
	}
	return it.Status
}

// test_p0 FIX 1+2a.
func TestHardLesson_APIFailureIsNotFlat(t *testing.T) {
	e := setup(t, &fakeBroker{posOK: false}, nil)
	e.openPos("NVDA.US", "long", 10, 100, db.Ptr(98.0), nil)
	log := e.run()
	if n := e.count("SELECT count(*) FROM signals"); n != 0 {
		t.Fatalf("positions() failure produced %d drift signals", n)
	}
	if !strings.Contains(strings.Join(log, "\n"), "不可用") {
		t.Fatalf("log must say the round was skipped: %v", log)
	}
}

// test_p0 FIX 2b, plus change and clear.
func TestHardLesson_DriftReportedOnlyOnChange(t *testing.T) {
	b := &fakeBroker{posOK: true, positions: []map[string]any{}}
	e := setup(t, b, nil)
	id := e.openPos("NVDA.US", "long", 10, 100, db.Ptr(98.0), nil)
	drifts := func() int { return e.count("SELECT count(*) FROM signals WHERE kind='drift'") }
	e.run()
	if drifts() != 1 {
		t.Fatalf("first round: %d drift signals, want 1", drifts())
	}
	e.run()
	if drifts() != 1 {
		t.Fatal("unchanged delta reported again")
	}
	b.positions = []map[string]any{{"symbol": "NVDA.US", "quantity": "4"}}
	e.run()
	if drifts() != 2 {
		t.Fatal("changed delta not reported")
	}
	b.positions = []map[string]any{{"symbol": "NVDA.US", "quantity": "10"}}
	if log := e.run(); !strings.Contains(strings.Join(log, "\n"), "drift-clear") || drifts() != 2 {
		t.Fatalf("recovery: %v", log)
	}
	// No silent mutation of the ledger in report-only mode.
	if p, _ := e.boot.PositionFor(e.ctx, "NVDA.US"); p == nil || p.ID != id || p.Qty != 10 {
		t.Fatal("drift detection touched the position")
	}
}

// test_p0 FIX 4: the executor's immediate path already booked this fill.
func TestHardLesson_SettleIdempotentByBrokerOrderID(t *testing.T) {
	e := setup(t, &fakeBroker{dry: true, detail: map[string]any{"status": "Filled"}}, nil)
	pid := e.openPos("TSLA.US", "long", 5, 200, db.Ptr(195.0), nil)
	if _, err := e.boot.AppendTrade(e.ctx, db.TradeInput{Symbol: "TSLA.US", Action: "BUY", Qty: 5, FillPrice: 200,
		Commission: 0.1, Source: db.Ptr("technical"), BrokerOrderID: db.Ptr("DUPBOID-1"), PositionID: &pid}); err != nil {
		t.Fatal(err)
	}
	e.boot.CreateOrder(e.ctx, db.OrderInput{ClientOrderID: "flintrade-idem-1", Symbol: "TSLA.US", Side: "BUY", Qty: 5, Price: db.Ptr(200.0)})
	e.boot.UpdateOrder(e.ctx, "flintrade-idem-1", db.Fields{"broker_order_id": "DUPBOID-1"})
	trades, positions := e.count("SELECT count(*) FROM trades"), e.count("SELECT count(*) FROM positions")
	log := e.run()
	var st string
	e.boot.QueryRow(e.ctx, "SELECT status FROM orders WHERE client_order_id='flintrade-idem-1'").Scan(&st)
	if st != "filled" || e.count("SELECT count(*) FROM trades") != trades || e.count("SELECT count(*) FROM positions") != positions {
		t.Fatalf("double-booked: status %s log %v", st, log)
	}
	if !strings.Contains(strings.Join(log, "\n"), "settle-skip") {
		t.Fatalf("log: %v", log)
	}
}

// test_p0 FIX 3.
func TestReconciledCloseCarriesIntentReason(t *testing.T) {
	e := setup(t, &fakeBroker{dry: true, detail: map[string]any{"status": "Filled"}}, nil)
	e.openPos("AAPL.US", "long", 10, 100, db.Ptr(95.0), db.Ptr(50.0))
	iid, _, _ := e.boot.SubmitIntent(e.ctx, db.IntentInput{Source: "technical", Symbol: "AAPL.US", Side: "close",
		EntryHint: db.Ptr(105.0), Reason: db.Ptr("THESIS-XYZ")})
	e.boot.CreateOrder(e.ctx, db.OrderInput{ClientOrderID: "flintrade-close-1", Symbol: "AAPL.US", Side: "SELL", Qty: 10,
		Price: db.Ptr(105.0), IntentID: &iid})
	e.boot.UpdateOrder(e.ctx, "flintrade-close-1", db.Fields{"broker_order_id": "CLOSEBOID-1"})
	e.run()
	ts, _ := e.boot.RecentTrades(e.ctx, 1)
	if len(ts) != 1 || ts[0].Reason == nil || *ts[0].Reason != "THESIS-XYZ" || *ts[0].PnL != 49.8 {
		t.Fatalf("trade: %+v", ts)
	}
	if r, _ := e.boot.GetRisk(e.ctx); r.OpenRisk != 0 || *r.Equity != 10049.8 {
		t.Fatalf("risk after close: %+v", r)
	}
}

// test_stop_guard D1–D3, D6 and the reconciler half of D5.
func TestHardLesson_TerminalOrderFinishesIntent(t *testing.T) {
	for _, c := range []struct {
		name, status string
		executed     any
		want         string
		pre          string // intent status forced before the run
	}{
		{"filled close", "Filled", 12.0, "filled", ""},
		{"rejected", "Rejected", 0.0, "rejected", ""},
		{"expired", "Expired", "0", "expired", ""},
		{"cancelled", "Cancelled", nil, "cancelled", ""},
		{"only approved is rewritten", "Filled", 12.0, "rejected", "rejected"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t, &fakeBroker{detail: map[string]any{"status": c.status, "executed_quantity": c.executed}}, nil)
			e.openPos("AAPL.US", "long", 12, 331.21, db.Ptr(328.0), db.Ptr(38.52))
			iid := e.seedDeferred("SELL", "close", db.Ptr(328.0), nil)
			if c.pre != "" {
				e.boot.DecideIntent(e.ctx, iid, c.pre, nil)
			}
			if _, err := e.r.SettlePending(e.ctx); err != nil {
				t.Fatal(err)
			}
			if got := e.intentStatus(iid); got != c.want {
				t.Fatalf("intent %s, want %s", got, c.want)
			}
			if c.status != "Filled" && e.count("SELECT count(*) FROM orders WHERE status IN ('submitted','partial')") != 0 {
				t.Fatal("terminal order left open: it would be re-queried every round")
			}
		})
	}
}

// A deferred entry fill books the intent's stop/target/target2 and source.
func TestDeferredOpenSettlesFromIntent(t *testing.T) {
	e := setup(t, &fakeBroker{detail: map[string]any{"status": "Filled", "executed_quantity": "12", "executed_price": "311.5"}}, nil)
	iid := e.seedDeferred("BUY", "long", db.Ptr(305.0), map[string]any{"target2": 350.0, "volume_ratio": 1.2})
	log := e.run()
	p, _ := e.boot.PositionFor(e.ctx, "AAPL.US")
	if p == nil || p.Qty != 12 || p.EntryPrice != 311.5 || *p.Stop != 305 || *p.Target != 340 || *p.Target2 != 350 || *p.Source != "technical" {
		t.Fatalf("position %+v log %v", p, log)
	}
	if e.intentStatus(iid) != "filled" {
		t.Fatal("intent not finished")
	}
	if r, _ := e.boot.GetRisk(e.ctx); r.OpenRisk != 78 || *r.Equity != 9999.76 {
		t.Fatalf("risk %+v", r)
	}
}

// Partial fill that then died: settle what executed, not the order qty.
func TestPartialThenTerminalSettlesExecutedQty(t *testing.T) {
	e := setup(t, &fakeBroker{detail: map[string]any{"status": "Cancelled", "executed_quantity": 5.0, "avg_price": "320"}}, nil)
	e.openPos("AAPL.US", "long", 12, 331.21, db.Ptr(328.0), db.Ptr(38.52))
	iid := e.seedDeferred("SELL", "close", nil, nil)
	e.run()
	p, _ := e.boot.PositionFor(e.ctx, "AAPL.US")
	// A whole-position exit that only partly filled is not a first
	// take-profit: the remainder stays guarded with t1_done unset.
	if p == nil || p.Qty != 7 || p.T1Done {
		t.Fatalf("partial close: %+v", p)
	}
	if e.intentStatus(iid) != "filled" {
		t.Fatal("intent")
	}
}

// Improvement over Python: order update + settlement are one transaction, so
// a fill that cannot be booked leaves the order open, never "filled, no trade".
func TestFillWithoutPriceLeavesNoTornLedger(t *testing.T) {
	e := setup(t, &fakeBroker{detail: map[string]any{"status": "Filled"}}, nil)
	iid, _, _ := e.boot.SubmitIntent(e.ctx, db.IntentInput{Source: "technical", Symbol: "AAPL.US", Side: "long", Stop: db.Ptr(300.0)})
	e.boot.DecideIntent(e.ctx, iid, "approved", nil)
	e.boot.CreateOrder(e.ctx, db.OrderInput{ClientOrderID: "np", Symbol: "AAPL.US", Side: "BUY", Qty: 1, IntentID: &iid})
	e.boot.UpdateOrder(e.ctx, "np", db.Fields{"broker_order_id": "B-NP"})
	log, _ := e.r.RunOnce(e.ctx)
	if !strings.Contains(strings.Join(log, "\n"), "settle-ERROR") {
		t.Fatalf("log %v", log)
	}
	if e.count("SELECT count(*) FROM orders WHERE status='submitted'") != 1 || e.count("SELECT count(*) FROM trades") != 0 {
		t.Fatal("torn: order marked filled without a trade")
	}
}

func TestHardLesson_FixedSleeveEquityNotSynced(t *testing.T) {
	assets := []any{map[string]any{"currency": "USD", "net_assets": "126000.5"}}
	e := setup(t, &fakeBroker{posOK: true, positions: []map[string]any{}, assets: assets}, nil) // live risk.toml: fixed
	log := e.run()
	if r, _ := e.boot.GetRisk(e.ctx); *r.Equity != 10000 || !strings.Contains(strings.Join(log, "\n"), "mode=fixed") {
		t.Fatalf("fixed sleeve overwritten: %v %v", *r.Equity, log)
	}
	risk := strings.Replace(testutil.ConfigFile(t, "risk.toml"), `mode       = "fixed"`, `mode       = "broker"`, 1)
	e = setup(t, &fakeBroker{posOK: true, positions: []map[string]any{}, assets: assets}, map[string]string{"risk.toml": risk})
	e.run()
	if r, _ := e.boot.GetRisk(e.ctx); *r.Equity != 126000.5 {
		t.Fatalf("broker mode not synced: %v", *r.Equity)
	}
	// Dry run never syncs or checks drift.
	e = setup(t, &fakeBroker{dry: true, assets: assets}, map[string]string{"risk.toml": risk})
	e.run()
	if r, _ := e.boot.GetRisk(e.ctx); *r.Equity != 10000 {
		t.Fatal("dry run synced equity")
	}
}

func TestAdoptUntrackedWhenEnabled(t *testing.T) {
	risk := strings.Replace(testutil.ConfigFile(t, "risk.toml"), "adopt_untracked_drift      = false", "adopt_untracked_drift      = true", 1)
	if !strings.Contains(risk, "adopt_untracked_drift      = true") {
		t.Fatal("fixture edit failed")
	}
	b := &fakeBroker{posOK: true, positions: []map[string]any{
		{"symbol": "MU.US", "quantity": "4", "cost_price": "-", "avg_price": "100"},
		{"symbol": "GLD.US", "quantity": "3"}, // no cost → report only
	}}
	e := setup(t, b, map[string]string{"risk.toml": risk})
	log := e.run()
	p, _ := e.boot.PositionFor(e.ctx, "MU.US")
	if p == nil || p.Qty != 4 || *p.Stop != 97 || *p.Source != "reconciled-drift" {
		t.Fatalf("adopt: %+v %v", p, log)
	}
	if e.count("SELECT count(*) FROM signals WHERE kind='drift_adopt'") != 1 || e.count("SELECT count(*) FROM signals WHERE kind='drift' AND symbol='GLD.US'") != 1 {
		t.Fatalf("signals: %v", log)
	}
	e.run() // adopted now matches broker: nothing new
	if e.count("SELECT count(*) FROM positions WHERE symbol='MU.US'") != 1 {
		t.Fatal("adopted twice")
	}
}

func TestShapeParsers(t *testing.T) {
	for _, c := range []struct {
		in   any
		want float64
	}{
		{map[string]any{"data": map[string]any{"net_assets": 5.0}}, 5},
		{map[string]any{"list": []any{map[string]any{"total_cash": "7"}}}, 7},
		{[]any{map[string]any{"net_assets": "x", "equity": "9"}}, 9},
	} {
		if got, ok := FindEquity(c.in); !ok || got != c.want {
			t.Errorf("FindEquity(%v) = %v %v", c.in, got, ok)
		}
	}
	if _, ok := FindEquity(nil); ok {
		t.Error("nil assets")
	}
	m := BrokerPositions([]map[string]any{{"code": "A.US", "qty": 2.0}, {"symbol": "A.US", "holding_qty": "3.0"}, {"quantity": 1.0}})
	if m["A.US"] != 5 || len(m) != 1 {
		t.Errorf("positions %v", m)
	}
}

// Partial fills are booked as they arrive: a working PartialFilled order
// stays tracked, each round books only the new shares at the price the
// cumulative average implies, and the intent finishes at the terminal state.
func TestPartialFillsBookedIncrementally(t *testing.T) {
	fb := &fakeBroker{detail: map[string]any{"status": "PartialFilled", "executed_quantity": 4.0, "executed_price": "312"}}
	e := setup(t, fb, nil)
	iid := e.seedDeferred("BUY", "long", db.Ptr(300.0), nil)
	e.run()
	p, _ := e.boot.PositionFor(e.ctx, "AAPL.US")
	if p == nil || p.Qty != 4 || p.EntryPrice != 312 || *p.RiskAmt != 48 {
		t.Fatalf("first partial: %+v", p)
	}
	if e.count("SELECT count(*) FROM orders WHERE status='partial' AND filled_qty=4") != 1 || e.intentStatus(iid) != "approved" {
		t.Fatal("a working partial must stay tracked, intent approved")
	}
	e.run() // nothing new: nothing booked
	if e.count("SELECT count(*) FROM trades") != 1 {
		t.Fatal("re-run booked twice")
	}
	// 10 executed at avg 312.6 → the 6 new shares cost (3126 − 1248)/6 = 313.
	fb.detail = map[string]any{"status": "PartialFilled", "executed_quantity": 10.0, "executed_price": "312.6"}
	e.run()
	p, _ = e.boot.PositionFor(e.ctx, "AAPL.US")
	if p.Qty != 10 || p.EntryPrice != 312.6 || *p.RiskAmt != 126 {
		t.Fatalf("second partial: %+v", p)
	}
	if e.count("SELECT count(*) FROM trades WHERE qty=6 AND fill_price=313") != 1 {
		t.Fatal("delta not booked at its implied price")
	}
	fb.detail = map[string]any{"status": "Filled", "executed_quantity": 12.0, "executed_price": "312.5"}
	e.run()
	p, _ = e.boot.PositionFor(e.ctx, "AAPL.US")
	if p.Qty != 12 || e.count("SELECT count(*) FROM positions") != 1 || e.intentStatus(iid) != "filled" ||
		e.count("SELECT count(*) FROM orders WHERE status='filled' AND filled_qty=12") != 1 {
		t.Fatalf("terminal: %+v", p)
	}
	r, _ := e.boot.GetRisk(e.ctx)
	if r.OpenRisk != *p.RiskAmt {
		t.Fatalf("open_risk %v != position risk %v", r.OpenRisk, *p.RiskAmt)
	}
	if e.count("SELECT SUM(qty) FROM trades WHERE broker_order_id='B-DEF'") != 12 {
		t.Fatal("booked shares != executed")
	}
}

// A whole-position exit filling in pieces shrinks the row without marking
// t1_done and closes it on the last piece; a smaller (take-profit) order
// still marks t1_done.
func TestPartialExitFills(t *testing.T) {
	fb := &fakeBroker{detail: map[string]any{"status": "PartialFilled", "executed_quantity": 5.0, "executed_price": "320"}}
	e := setup(t, fb, nil)
	e.openPos("AAPL.US", "long", 12, 331.21, db.Ptr(328.0), db.Ptr(38.52))
	iid := e.seedDeferred("SELL", "close", nil, nil)
	e.run()
	p, _ := e.boot.PositionFor(e.ctx, "AAPL.US")
	if p == nil || p.Qty != 7 || p.T1Done || e.intentStatus(iid) != "approved" {
		t.Fatalf("first piece: %+v", p)
	}
	fb.detail = map[string]any{"status": "Filled", "executed_quantity": 12.0, "executed_price": "320"}
	e.run()
	if p, _ := e.boot.PositionFor(e.ctx, "AAPL.US"); p != nil || e.intentStatus(iid) != "filled" {
		t.Fatalf("last piece must close the position: %+v", p)
	}
	if r, _ := e.boot.GetRisk(e.ctx); r.OpenRisk != 0 {
		t.Fatalf("open_risk left: %v", r.OpenRisk)
	}

	// Take-profit order for 6 of 12 shares filling 3 at a time.
	fb = &fakeBroker{detail: map[string]any{"status": "PartialFilled", "executed_quantity": 3.0, "executed_price": "340"}}
	e = setup(t, fb, nil)
	e.openPos("AAPL.US", "long", 12, 331.21, db.Ptr(328.0), db.Ptr(38.52))
	e.seedDeferred("SELL", "close", nil, nil)
	e.boot.Exec(e.ctx, db.PermOrders, "UPDATE orders SET qty=6")
	e.run()
	if p, _ := e.boot.PositionFor(e.ctx, "AAPL.US"); p.Qty != 9 || !p.T1Done {
		t.Fatalf("take-profit piece: %+v", p)
	}
}
