package invariants

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flintrade/internal/db"
	"flintrade/internal/loop"
	"flintrade/internal/paths"
	"flintrade/internal/settle"
	"flintrade/internal/testutil"
)

type env struct {
	t    *testing.T
	ctx  context.Context
	d    *db.DB
	boot *db.Store
	ex   *db.Store
	c    *Checker
}

func setup(t *testing.T) *env {
	d := testutil.DB(t)
	testutil.SetEquity(t, d, 10000)
	return &env{t: t, ctx: context.Background(), d: d, boot: d.MustAs(db.RoleMigrate), ex: d.MustAs(db.RoleExecutor),
		c: New(&loop.Deps{DB: d})}
}

func (e *env) violations() []string {
	e.t.Helper()
	out, err := e.c.RunOnce(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	for i := range out {
		out[i] = strings.TrimPrefix(out[i], "VIOLATION ")
	}
	return out
}

func has(v []string, parts ...string) bool {
	for _, x := range v {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(x, p)
		}
		if ok {
			return true
		}
	}
	return false
}

// test_invariants §1–§2.
func TestCleanLedgerAndOpenRiskDrift(t *testing.T) {
	e := setup(t)
	if v := e.violations(); len(v) != 0 {
		t.Fatalf("clean ledger: %v", v)
	}
	settle.Open(e.ctx, e.ex, settle.OpenInput{Symbol: "NVDA.US", Side: "long", Qty: 10, FillPrice: 200, Stop: db.Ptr(195.0),
		Target: db.Ptr(210.0), CommissionPerShare: 0.02})
	if v := e.violations(); len(v) != 0 {
		t.Fatalf("after a real settlement: %v", v)
	}
	e.boot.UpdateRisk(e.ctx, db.Fields{"open_risk": 999.0})
	if v := e.violations(); !has(v, "open_risk_ledger", "999.0", "50.0") {
		t.Fatalf("drift not named: %v", v)
	}
	e.boot.UpdateRisk(e.ctx, db.Fields{"open_risk": 50.04})
	if v := e.violations(); len(v) != 0 {
		t.Fatalf("within tolerance: %v", v)
	}
	e.boot.UpdateRisk(e.ctx, db.Fields{"open_risk": 50.0, "equity": 0.0})
	if v := e.violations(); !has(v, "equity_sane") {
		t.Fatalf("equity: %v", v)
	}
}

// test_invariants §3: approved intents get 30 min of grace before "orphan".
func TestIntentChecks(t *testing.T) {
	e := setup(t)
	iid, _, _ := e.boot.SubmitIntent(e.ctx, db.IntentInput{Source: "technical", Symbol: "AAPL.US", Side: "long", Stop: db.Ptr(180.0)})
	e.ex.DecideIntent(e.ctx, iid, "filled", nil)
	if v := e.violations(); !has(v, "filled_has_trade", "[1]") {
		t.Fatalf("filled without trade: %v", v)
	}
	iid2, _, _ := e.boot.SubmitIntent(e.ctx, db.IntentInput{Source: "technical", Symbol: "MSFT.US", Side: "long", Stop: db.Ptr(400.0)})
	e.ex.DecideIntent(e.ctx, iid2, "approved", nil)
	if v := e.violations(); has(v, "approved_has_order") {
		t.Fatalf("approved within grace reported: %v", v)
	}
	e.boot.Exec(e.ctx, db.PermIntentsDecide, "UPDATE intents SET decided_at='2026-01-01T00:00:00Z' WHERE id=?", iid2)
	if v := e.violations(); !has(v, "approved_has_order", "[2]") {
		t.Fatalf("orphan not named: %v", v)
	}
}

// test_invariants §4, with the hard-lesson settle grace.
func TestHardLesson_SettleGraceAndLedgerShape(t *testing.T) {
	e := setup(t)
	e.ex.CreateOrder(e.ctx, db.OrderInput{ClientOrderID: "test-coid-1", Symbol: "TSLA.US", Side: "BUY", Qty: 5})
	e.ex.UpdateOrder(e.ctx, "test-coid-1", db.Fields{"status": "filled", "broker_order_id": "B999"})
	if v := e.violations(); has(v, "filled_order_trade") {
		t.Fatalf("legal transient inside 60 s reported: %v", v)
	}
	e.boot.Exec(e.ctx, db.PermOrders, "UPDATE orders SET updated_at='2026-01-01T00:00:00Z' WHERE client_order_id='test-coid-1'")
	if v := e.violations(); !has(v, "filled_order_trade", "['test-coid-1']") {
		t.Fatalf("filled order without trade: %v", v)
	}

	e = setup(t)
	e.ex.OpenPosition(e.ctx, db.PositionInput{Symbol: "GLD.US", Side: "long", Qty: 5, EntryPrice: 250, RiskAmt: db.Ptr(10.0)})
	e.ex.OpenPosition(e.ctx, db.PositionInput{Symbol: "GLD.US", Side: "short", Qty: 5, EntryPrice: 251, RiskAmt: db.Ptr(10.0)})
	e.ex.UpdateRisk(e.ctx, db.Fields{"open_risk": 20.0})
	if v := e.violations(); !has(v, "no_hedged_symbol", "GLD") {
		t.Fatalf("hedge: %v", v)
	}

	e = setup(t)
	e.ex.OpenPosition(e.ctx, db.PositionInput{Symbol: "X.US", Side: "sideways", Qty: 0, EntryPrice: 1})
	if v := e.violations(); !has(v, "position_sane", "side=sideways qty=0") {
		t.Fatalf("sanity: %v", v)
	}

	e = setup(t)
	e.ex.AppendTrade(e.ctx, db.TradeInput{Symbol: "USO.US", Action: "SELL", Qty: 3, FillPrice: 80})
	e.ex.AppendTrade(e.ctx, db.TradeInput{Symbol: "USO.US", Action: "SELL", Qty: 3, FillPrice: 80, Attribution: db.Ptr("test")})
	v := e.violations()
	n := 0
	for _, x := range v {
		if strings.Contains(x, "trade_leg_pnl") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("trade_leg_pnl (test attribution exempt): %v", v)
	}
}

// test_invariants §5: result recorded in kv; heartbeat note.
func TestResultInKV(t *testing.T) {
	e := setup(t)
	e.violations()
	raw, ok, _ := e.boot.KVGet(e.ctx, "invariants_last")
	var last struct {
		OK         bool     `json:"ok"`
		Violations []string `json:"violations"`
		TS         string   `json:"ts"`
	}
	if !ok || json.Unmarshal([]byte(raw), &last) != nil || !last.OK || last.Violations == nil || last.TS == "" {
		t.Fatalf("kv: %s", raw)
	}
	hb, _ := e.boot.Heartbeats(e.ctx)
	if len(hb) != 1 || hb[0].Process != "invariants" || *hb[0].Note != "ok" {
		t.Fatalf("heartbeat %+v", hb)
	}
	// The checker holds the reader role: it can never write the ledger.
	if _, err := e.c.store.Exec(e.ctx, db.PermPositions, "DELETE FROM positions"); err == nil {
		t.Fatal("invariants could write")
	}
}

// test_invariants §8: staged take-profit closes stay consistent.
func TestPartialCloseLedgerLoop(t *testing.T) {
	e := setup(t)
	pid, _ := settle.Open(e.ctx, e.ex, settle.OpenInput{Symbol: "NVDA.US", Side: "long", Qty: 10, FillPrice: 200,
		Stop: db.Ptr(194.0), Target: db.Ptr(210.0), Target2: db.Ptr(216.0), CommissionPerShare: 0.02})
	pos, _ := e.ex.PositionFor(e.ctx, "NVDA.US")
	if pos.ID != pid || *pos.Target2 != 216 || pos.T1Done {
		t.Fatalf("open %+v", pos)
	}
	pnl1, _ := settle.Close(e.ctx, e.ex, settle.CloseInput{Held: *pos, FillPrice: 211, Qty: 5, CommissionPerShare: 0.02})
	pos, _ = e.ex.PositionFor(e.ctx, "NVDA.US")
	if pos.Qty != 5 || !pos.T1Done || *pos.RiskAmt != 30 || pnl1 != 54.9 {
		t.Fatalf("partial: %+v pnl %v", pos, pnl1)
	}
	if v := e.violations(); len(v) != 0 {
		t.Fatalf("after partial: %v", v)
	}
	settle.Close(e.ctx, e.ex, settle.CloseInput{Held: *pos, FillPrice: 216.5, Qty: 5, CommissionPerShare: 0.02})
	if r, _ := e.ex.GetRisk(e.ctx); r.OpenRisk != 0 {
		t.Fatalf("open risk %v", r.OpenRisk)
	}
	if v := e.violations(); len(v) != 0 {
		t.Fatalf("after full close: %v", v)
	}
}

// Runs the checks on a VACUUM INTO snapshot of the live ledger (read-only
// source; the copy is a temp file). Reports, does not assert zero.
func TestAgainstLiveLedgerCopy(t *testing.T) {
	root, err := paths.Root()
	if err != nil {
		t.Skip("no repo root")
	}
	live := paths.DBPath(root)
	if _, err := os.Stat(live); err != nil {
		t.Skip("no live flintrade.db")
	}
	src, err := db.OpenReadOnly(live)
	if err != nil {
		t.Skip(err)
	}
	dst := filepath.Join(t.TempDir(), "copy.db")
	if _, err := src.MustAs(db.RoleReader).Query(context.Background(), "VACUUM INTO ?", dst); err != nil {
		src.Close()
		t.Skip(err)
	}
	src.Close()
	d, err := db.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	out, err := New(&loop.Deps{DB: d}).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("live ledger copy: %d violations", len(out))
	for _, l := range out {
		t.Log(l)
	}
}
