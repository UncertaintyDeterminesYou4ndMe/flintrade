package executor

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"flintrade/internal/broker"
	"flintrade/internal/db"
	"flintrade/internal/loop"
	"flintrade/internal/reasoncode"
	"flintrade/internal/session"
	"flintrade/internal/testutil"
)

// Tue 2026-11-10 10:00 ET: Intraday, 360 min to the close.
var tuesday = time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)

// stub is a controllable broker. Orders fill immediately unless submitOnly;
// OrderDetail answers detailStatus/executed; reject makes placement fail.
type stub struct {
	detailStatus string
	executed     any
	submitOnly   bool
	reject       string
	cancelled    []string
	placed       []string
}

func (b *stub) DryRun() bool { return false }
func (b *stub) place(side, symbol string, qty int, price float64) broker.Result {
	b.placed = append(b.placed, side+" "+symbol)
	if b.reject != "" {
		return broker.Result{Status: "Rejected", Raw: b.reject}
	}
	id := "B" + string(rune('0'+len(b.placed)))
	if b.submitOnly {
		return broker.Result{OK: true, BrokerOrderID: id, Status: "Submitted", Raw: "{}"}
	}
	p := price
	return broker.Result{OK: true, BrokerOrderID: id, Status: "Filled", FillPrice: &p, Raw: "{}"}
}
func (b *stub) Buy(_ context.Context, s string, q int, p float64, _, _ string) broker.Result {
	return b.place("buy", s, q, p)
}
func (b *stub) Sell(_ context.Context, s string, q int, p float64, _, _ string) broker.Result {
	return b.place("sell", s, q, p)
}
func (b *stub) Cancel(_ context.Context, id string) broker.CancelResult {
	b.cancelled = append(b.cancelled, id)
	return broker.CancelResult{OK: true}
}
func (b *stub) OrderDetail(context.Context, string) map[string]any {
	return map[string]any{"status": b.detailStatus, "executed_quantity": b.executed}
}
func (b *stub) RecentFills(context.Context, int) []map[string]any  { return nil }
func (b *stub) Assets(context.Context) any                         { return nil }
func (b *stub) Positions(context.Context) ([]map[string]any, bool) { return nil, true }

type env struct {
	t   *testing.T
	ctx context.Context
	d   *loop.Deps
	mig *db.Store
}

func newEnv(t *testing.T, b broker.Broker) *env {
	t.Helper()
	d := testutil.DB(t)
	testutil.SetEquity(t, d, 10000)
	// The session clock is pinned to a Tuesday morning; the loop clock stays
	// real because db timestamps (orders.created_at) are real.
	us := []session.Market{{Market: "US", Sessions: []session.Window{
		{Session: session.Pre, Open: "04:00:00", Close: "09:30:00"},
		{Session: session.Intraday, Open: "09:30:00", Close: "16:00:00"},
		{Session: session.Post, Open: "16:00:00", Close: "20:00:00"}}}}
	clock := &session.Clock{Now: func() time.Time { return tuesday }, TTL: time.Minute,
		Fetch: func(context.Context) ([]session.Market, error) { return us, nil }}
	deps := &loop.Deps{DB: d, Config: testutil.Config(t, nil), Broker: b, Clock: clock}
	return &env{t: t, ctx: context.Background(), d: deps, mig: d.MustAs(db.RoleMigrate)}
}

func (e *env) run() []string {
	e.t.Helper()
	log, err := New(e.d).RunOnce(e.ctx)
	if err != nil {
		e.t.Fatalf("RunOnce: %v (log %v)", err, log)
	}
	return log
}

func (e *env) submit(in db.IntentInput) int64 {
	e.t.Helper()
	if in.Source == "" {
		in.Source = "technical"
	}
	id, _, err := e.mig.SubmitIntent(e.ctx, in)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// code is the intent's reject_code; the zero Code when none is stored.
func (e *env) code(id int64) reasoncode.Code {
	e.t.Helper()
	var raw *string
	if err := e.mig.QueryRow(e.ctx, "SELECT reject_code FROM intents WHERE id=?", id).Scan(&raw); err != nil {
		e.t.Fatalf("reject_code %d: %v", id, err)
	}
	if raw == nil {
		return reasoncode.Code{}
	}
	c, _ := reasoncode.Parse(*raw)
	return c
}

func (e *env) intent(id int64) db.Intent {
	e.t.Helper()
	it, err := e.mig.Intent(e.ctx, id)
	if err != nil || it == nil {
		e.t.Fatalf("intent %d: %v", id, err)
	}
	return *it
}

func (e *env) order(coid string) db.Order {
	e.t.Helper()
	os, _ := e.mig.Query(e.ctx, "SELECT status FROM orders WHERE client_order_id=?", coid)
	defer os.Close()
	var o db.Order
	if !os.Next() {
		e.t.Fatalf("order %s missing", coid)
	}
	os.Scan(&o.Status)
	return o
}

func (e *env) risk() db.RiskState {
	r, _ := e.mig.GetRisk(e.ctx)
	return *r
}

func f(x float64) *float64 { return &x }
func i(x int) *int         { return &x }
func str(s string) *string { return &s }

func longIntent(sym string, entry, stop float64, feats any) db.IntentInput {
	return db.IntentInput{Symbol: sym, Side: "long", EntryHint: f(entry), Stop: f(stop), Target: f(entry * 1.1),
		Confidence: i(80), Reason: str("open long"), Features: feats}
}

// test_phase1 "Executor 端到端(dry-run)".
func TestEndToEnd(t *testing.T) {
	e := newEnv(t, &broker.CLI{Dry: true})
	e.submit(longIntent("NVDA.US", 100, 98, map[string]any{"volume_ratio": 1.0}))
	e.run()
	pos, _ := e.mig.PositionFor(e.ctx, "NVDA.US")
	r := e.risk()
	if pos == nil || pos.Qty != 40 || r.OpenRisk != 80 || *r.Equity != 9999.2 {
		t.Fatalf("open: pos %+v risk %+v", pos, r)
	}
	sid := e.submit(db.IntentInput{Symbol: "NVDA.US", Side: "short", EntryHint: f(100), Stop: f(102),
		Reason: str("conflict short"), Features: map[string]any{"volume_ratio": 1.0}})
	e.run()
	if it := e.intent(sid); it.Status != "rejected" || !strings.Contains(*it.RejectReason, "翻向") {
		t.Fatalf("conflict: %+v", it)
	}
	if c := e.code(sid); c.Key != "flipNeedsClose" || !c.Valid() || c.Args["side"] != "long" {
		t.Fatalf("conflict code: %+v", c)
	}
	cid := e.submit(db.IntentInput{Symbol: "NVDA.US", Side: "close", EntryHint: f(105), Reason: str("take profit")})
	e.run()
	pos, _ = e.mig.PositionFor(e.ctx, "NVDA.US")
	r = e.risk()
	trades, _ := e.mig.RecentTrades(e.ctx, 1)
	if pos != nil || *trades[0].PnL != 199.2 || *r.Equity != 10198.4 || r.OpenRisk != 0 || e.intent(cid).Status != "filled" {
		t.Fatalf("close: pos %+v pnl %v risk %+v", pos, *trades[0].PnL, r)
	}
	if *trades[0].Reason != "take profit" || trades[0].Action != "SELL" {
		t.Fatalf("close trade: %+v", trades[0])
	}
}

// test_p0 FIX 5a: a resting entry order on the symbol blocks a new entry.
func TestInflightDuplicateRejected(t *testing.T) {
	e := newEnv(t, &broker.CLI{Dry: true})
	e.mig.CreateOrder(e.ctx, db.OrderInput{ClientOrderID: "flintrade-inflight-1", Symbol: "NVDA.US", Side: "BUY", Qty: 19, Price: f(205)})
	id := e.submit(longIntent("NVDA.US", 205, 200, map[string]any{"volume_ratio": 1.0}))
	e.run()
	if it := e.intent(id); it.Status != "rejected" || !strings.Contains(*it.RejectReason, "在途") {
		t.Fatalf("%+v", it)
	}
}

func (e *env) seedEntryOrder(coid, sym string, created string, intentID int64) {
	e.t.Helper()
	e.mig.CreateOrder(e.ctx, db.OrderInput{ClientOrderID: coid, Symbol: sym, Side: "BUY", Qty: 10, Price: f(200), IntentID: &intentID})
	e.mig.UpdateOrder(e.ctx, coid, db.Fields{"broker_order_id": "BOID-" + coid})
	if created != "" {
		if _, err := e.mig.Exec(e.ctx, db.PermOrders, "UPDATE orders SET created_at=? WHERE client_order_id=?", created, coid); err != nil {
			e.t.Fatal(err)
		}
	}
}

// Entry limit orders expire after the TTL (flintrade-210 rested over a weekend
// and filled 4 cents above its own stop); exit orders never do — cancelling
// one leaves the position naked. test_p0 FIX 7 / 7b.
func TestHardLesson_EntryTTLExitsNeverExpire(t *testing.T) {
	b := &stub{detailStatus: "Cancelled", executed: 0}
	e := newEnv(t, b)
	stale := e.submit(longIntent("NVDA.US", 200.75, 197.9, nil))
	fresh := e.submit(longIntent("GLD.US", 375, 372, nil))
	e.mig.DecideIntent(e.ctx, stale, "approved", nil)
	e.mig.DecideIntent(e.ctx, fresh, "approved", nil)
	e.seedEntryOrder("flintrade-stale-1", "NVDA.US", "2026-08-01T16:01:24Z", stale)
	e.seedEntryOrder("flintrade-fresh-1", "GLD.US", db.FormatTime(time.Now().Add(-10*time.Minute)), fresh)
	e.mig.CreateOrder(e.ctx, db.OrderInput{ClientOrderID: "flintrade-exit-1", Symbol: "AAPL.US", Side: "SELL", Qty: 5, Price: f(300)})
	e.mig.Exec(e.ctx, db.PermOrders, "UPDATE orders SET created_at='2026-11-01T00:00:00Z', broker_order_id='B-EXIT' WHERE client_order_id='flintrade-exit-1'")
	log := e.run()
	if e.order("flintrade-stale-1").Status != "cancelled" {
		t.Fatalf("stale entry not cancelled: %v", log)
	}
	if it := e.intent(stale); it.Status != "expired" || !strings.Contains(*it.RejectReason, "TTL 45min") {
		t.Fatalf("intent: %+v", it)
	}
	if c := e.code(stale); c.Key != "ttlExpired" || !c.Valid() || c.Args["ttl"] != "45" {
		t.Fatalf("ttl code: %+v", c)
	}
	if e.order("flintrade-fresh-1").Status != "submitted" || e.order("flintrade-exit-1").Status != "submitted" {
		t.Fatal("fresh entry or exit order touched")
	}
	if len(b.cancelled) != 1 || b.cancelled[0] != "BOID-flintrade-stale-1" {
		t.Fatalf("cancelled %v", b.cancelled)
	}
}

// Cancel and fill race: after a cancel, order_detail decides. A filled
// order stays submitted for the reconciler — marking it cancelled would
// silently drop the fill. test_p0 FIX 7b and test_stop_guard B2.
func TestHardLesson_CancelRaceNeverMarksCancelled(t *testing.T) {
	e := newEnv(t, &stub{detailStatus: "Filled", executed: 10})
	id := e.submit(longIntent("GLD.US", 375, 372, nil))
	e.mig.DecideIntent(e.ctx, id, "approved", nil)
	e.seedEntryOrder("flintrade-old-1", "GLD.US", "2026-08-01T16:01:24Z", id)
	e.run()
	if e.order("flintrade-old-1").Status != "submitted" || e.intent(id).Status != "approved" {
		t.Fatalf("TTL race: order %s intent %s", e.order("flintrade-old-1").Status, e.intent(id).Status)
	}
	// executed_quantity > 0 counts as filled even when status lags.
	e2 := newEnv(t, &stub{detailStatus: "PartialFilled", executed: "3"})
	id2 := e2.submit(longIntent("GLD.US", 375, 372, nil))
	e2.mig.DecideIntent(e2.ctx, id2, "approved", nil)
	e2.seedEntryOrder("flintrade-old-2", "GLD.US", "2026-08-01T16:01:24Z", id2)
	e2.run()
	if e2.order("flintrade-old-2").Status != "submitted" {
		t.Fatal("partially executed order marked cancelled")
	}
}

func (e *env) seedPositionWithRestingSell() {
	e.t.Helper()
	e.mig.OpenPosition(e.ctx, db.PositionInput{Symbol: "AAPL.US", Side: "long", Qty: 12, EntryPrice: 331.21,
		Stop: f(328), Target: f(337.6), RiskAmt: f(38.52), Source: str("technical")})
	e.mig.CreateOrder(e.ctx, db.OrderInput{ClientOrderID: "flintrade-old", Symbol: "AAPL.US", Side: "SELL", Qty: 12,
		Price: f(312.30), OutsideRTH: str("OVERNIGHT")})
	e.mig.UpdateOrder(e.ctx, "flintrade-old", db.Fields{"broker_order_id": "B-OLD", "status": "submitted"})
}

// An exit first clears working orders on the symbol: the broker locks shares
// against a resting sell, so the replacement is rejected as oversell
// (flintrade-198 / 199, 2026-07-31). test_stop_guard B1 / B2.
func TestHardLesson_ExitCancelsInflightFirst(t *testing.T) {
	b := &stub{detailStatus: "Cancelled", executed: 0}
	e := newEnv(t, b)
	e.seedPositionWithRestingSell()
	id := e.submit(db.IntentInput{Source: "risk_monitor", Priority: 101, Symbol: "AAPL.US", Side: "close",
		EntryHint: f(308.55), Reason: str("stop breached")})
	e.run()
	ps, _ := e.mig.OpenPositions(e.ctx)
	if len(b.cancelled) != 1 || b.cancelled[0] != "B-OLD" || e.order("flintrade-old").Status != "cancelled" ||
		len(b.placed) != 1 || b.placed[0] != "sell AAPL.US" || len(ps) != 0 || e.intent(id).Status != "filled" {
		t.Fatalf("B1: cancelled %v placed %v positions %d intent %s", b.cancelled, b.placed, len(ps), e.intent(id).Status)
	}

	// B2: the resting sell had already filled → no second order, the old one
	// stays submitted for the reconciler, the new intent is void.
	b2 := &stub{detailStatus: "Filled", executed: 12}
	e2 := newEnv(t, b2)
	e2.seedPositionWithRestingSell()
	id2 := e2.submit(db.IntentInput{Source: "risk_monitor", Priority: 101, Symbol: "AAPL.US", Side: "close",
		EntryHint: f(308.55), Reason: str("stop breached")})
	e2.run()
	ps, _ = e2.mig.OpenPositions(e2.ctx)
	if e2.order("flintrade-old").Status != "submitted" || len(b2.placed) != 0 || e2.intent(id2).Status != "cancelled" || len(ps) != 1 {
		t.Fatalf("B2: order %s placed %v intent %s positions %d", e2.order("flintrade-old").Status, b2.placed, e2.intent(id2).Status, len(ps))
	}
}

// An order reaching a terminal state closes out its intent; only 'approved'
// is overwritten. test_stop_guard D4 / D5 (executor side).
func TestHardLesson_TerminalOrderFinishesIntent(t *testing.T) {
	seed := func(e *env) int64 {
		e.mig.OpenPosition(e.ctx, db.PositionInput{Symbol: "AAPL.US", Side: "long", Qty: 12, EntryPrice: 331.21,
			Stop: f(328), RiskAmt: f(38.52), Source: str("technical")})
		old := e.submit(db.IntentInput{Symbol: "AAPL.US", Side: "close", EntryHint: f(312.30), Stop: f(328), Reason: str("deferred")})
		e.mig.DecideIntent(e.ctx, old, "approved", nil)
		coid := "flintrade-" + itoa(old)
		e.mig.CreateOrder(e.ctx, db.OrderInput{ClientOrderID: coid, Symbol: "AAPL.US", Side: "SELL", Qty: 12, Price: f(312.30), IntentID: &old})
		e.mig.UpdateOrder(e.ctx, coid, db.Fields{"broker_order_id": "B-DEF", "status": "submitted"})
		return old
	}
	e := newEnv(t, &stub{detailStatus: "Cancelled", executed: 0})
	old := seed(e)
	nid := e.submit(db.IntentInput{Source: "risk_monitor", Priority: 101, Symbol: "AAPL.US", Side: "close", EntryHint: f(308.55), Reason: str("stop breached")})
	e.run()
	if e.intent(old).Status != "cancelled" || e.intent(nid).Status != "filled" {
		t.Fatalf("D4: old %s new %s", e.intent(old).Status, e.intent(nid).Status)
	}
	e2 := newEnv(t, &stub{detailStatus: "Filled", executed: 12})
	old2 := seed(e2)
	e2.submit(db.IntentInput{Source: "risk_monitor", Priority: 101, Symbol: "AAPL.US", Side: "close", EntryHint: f(308.55), Reason: str("stop breached")})
	e2.run()
	if e2.intent(old2).Status != "approved" {
		t.Fatalf("D5: executor must leave the old intent to the reconciler, got %s", e2.intent(old2).Status)
	}
}

// Store the broker's raw rejection, not just "Rejected" (three rejected
// orders of 2026-07-31 could not be diagnosed).
func TestHardLesson_BrokerRawErrorStored(t *testing.T) {
	e := newEnv(t, &stub{reject: "order rejected: insufficient buying power (code 603011)"})
	id := e.submit(longIntent("NVDA.US", 100, 98, map[string]any{"volume_ratio": 1.0}))
	e.run()
	it := e.intent(id)
	if it.Status != "rejected" || !strings.Contains(*it.RejectReason, "code 603011") ||
		!strings.Contains(*it.RejectReason, "orth=RTH_ONLY sess=Intraday") || e.order("flintrade-"+itoa(id)).Status != "rejected" {
		t.Fatalf("%+v", it)
	}
	if c := e.code(id); c.Key != "brokerRejected" || !c.Valid() || !strings.Contains(c.Args["raw"], "603011") {
		t.Fatalf("broker code: %+v", c)
	}
}

// The gate's criteria come from the indicator snapshot (intents.features),
// never from the model's prose; missing → fail-closed. Exits are never
// blocked by missing data.
func TestHardLesson_GateEvidenceMissingFailsClosed(t *testing.T) {
	e := newEnv(t, &broker.CLI{Dry: true})
	id := e.submit(longIntent("USO.US", 80, 78, nil)) // no features at all
	bad := e.submit(longIntent("GLD.US", 375, 372, map[string]any{"volume_ratio": "0.6"}))
	e.mig.OpenPosition(e.ctx, db.PositionInput{Symbol: "UGL.US", Side: "long", Qty: 3, EntryPrice: 50})
	exit := e.submit(db.IntentInput{Symbol: "UGL.US", Side: "close", EntryHint: f(51)})
	e.run()
	if it := e.intent(id); it.Status != "rejected" || !strings.Contains(*it.RejectReason, "fail-closed") {
		t.Fatalf("missing volume_ratio: %+v", it)
	}
	if it := e.intent(bad); it.Status != "rejected" || !strings.Contains(*it.RejectReason, "fail-closed") {
		t.Fatalf("non-numeric volume_ratio must not pass: %+v", it)
	}
	if e.intent(exit).Status != "filled" {
		t.Fatal("exit blocked by missing data")
	}
}

// A live limit order that does not fill at once stays submitted, its intent
// approved, for the reconciler to settle.
func TestDeferredFillLeftToReconciler(t *testing.T) {
	e := newEnv(t, &stub{submitOnly: true, detailStatus: "New"})
	id := e.submit(longIntent("NVDA.US", 100, 98, map[string]any{"volume_ratio": 1.0, "target2": 112.5}))
	e.run()
	ps, _ := e.mig.OpenPositions(e.ctx)
	if e.intent(id).Status != "approved" || e.order("flintrade-"+itoa(id)).Status != "submitted" || len(ps) != 0 {
		t.Fatalf("intent %s positions %d", e.intent(id).Status, len(ps))
	}
}

// target2 from features reaches the position (scale-out); trades carry the
// strategy attribution.
func TestOpenCarriesTarget2(t *testing.T) {
	e := newEnv(t, &broker.CLI{Dry: true})
	e.submit(longIntent("NVDA.US", 100, 98, map[string]any{"volume_ratio": 1.0, "target2": 112.5}))
	e.run()
	pos, _ := e.mig.PositionFor(e.ctx, "NVDA.US")
	tr, _ := e.mig.RecentTrades(e.ctx, 1)
	if pos == nil || pos.Target2 == nil || *pos.Target2 != 112.5 || *tr[0].Attribution != "strategy" {
		t.Fatalf("pos %+v trade %+v", pos, tr)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// One iteration on a snapshot of the live ledger with a dry-run broker: real
// rows (orders with legacy statuses, NULL columns) must scan and process.
func TestAgainstLiveLedgerCopy(t *testing.T) {
	src := "../../flintrade.db"
	if _, err := os.Stat(src); err != nil {
		t.Skip("no live flintrade.db")
	}
	ctx := context.Background()
	ro, err := db.OpenReadOnly(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "copy.db")
	rows, err := ro.MustAs(db.RoleReader).Query(ctx, "VACUUM INTO ?", dst)
	if err != nil {
		ro.Close()
		t.Skipf("snapshot: %v", err)
	}
	rows.Close()
	ro.Close()
	d, err := db.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Init(ctx); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, &broker.CLI{Dry: true})
	e.d.DB = d
	e.mig = d.MustAs(db.RoleMigrate)
	before, _ := e.mig.OpenOrders(ctx)
	log := e.run()
	t.Logf("live copy: %d open orders before, log %v", len(before), log)
}
