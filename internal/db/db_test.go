package db

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "flintrade.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return d
}

// The embedded schema must stay byte-identical to the Python one while both
// implementations share flintrade.db.
func TestSchemaMatchesPython(t *testing.T) {
	py, err := os.ReadFile("../../agent/schema.sql")
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("agent/schema.sql gone (Python retired)")
	}
	if err != nil {
		t.Fatal(err)
	}
	if string(py) != schemaSQL {
		t.Fatal("internal/db/schema.sql drifted from agent/schema.sql")
	}
}

func TestInitIdempotent(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	if err := d.Init(ctx); err != nil {
		t.Fatalf("second Init: %v", err)
	}
	var n int
	if err := d.sql.QueryRow("SELECT count(*) FROM risk_state").Scan(&n); err != nil || n != 1 {
		t.Fatalf("risk_state rows = %d, %v", n, err)
	}
	v, ok, err := d.MustAs(RoleReader).KVGet(ctx, "schema_version")
	if err != nil || !ok || v != "1" {
		t.Fatalf("schema_version = %q %v %v", v, ok, err)
	}
}

// A database created before the attribution / target2 / t1_done / reject_code columns
// existed must be patched in place.
func TestInitMigratesOldColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	old := strings.NewReplacer(
		"    target2     REAL,                          -- 第二止盈位(触发清剩余仓)\n", "",
		"    t1_done     INTEGER NOT NULL DEFAULT 0,    -- 第一止盈已兑现(任何部分平仓都置 1)\n", "",
		",\n    attribution     TEXT                        -- strategy | manual | test | outage-degraded(见 settle.py)\n", "\n",
		"    reject_code   TEXT,                         -- JSON {key,args}:reject_reason 的多语言形式(internal/reasoncode)\n", "",
	).Replace(schemaSQL)
	if old == schemaSQL {
		t.Fatal("test fixture did not strip the migrated columns")
	}
	if _, err := raw.Exec(old); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	for _, m := range columnMigrations {
		if has, _ := d.hasColumn(ctx, m.table, m.column); has {
			t.Fatalf("fixture already has %s.%s", m.table, m.column)
		}
	}
	if err := d.Init(ctx); err != nil {
		t.Fatal(err)
	}
	for _, m := range columnMigrations {
		if has, err := d.hasColumn(ctx, m.table, m.column); err != nil || !has {
			t.Fatalf("%s.%s not migrated: %v", m.table, m.column, err)
		}
	}
}

func TestRoleGuardMatrix(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	write := map[Perm]func(*Store) error{
		PermIntentsSubmit: func(s *Store) error {
			_, _, err := s.SubmitIntent(ctx, IntentInput{Symbol: "NVDA.US", Side: "long"})
			return err
		},
		PermIntentsDecide: func(s *Store) error { return s.DecideIntent(ctx, 1, "rejected", nil) },
		PermPositions: func(s *Store) error {
			_, err := s.OpenPosition(ctx, PositionInput{Symbol: "NVDA.US", Side: "long", Qty: 1, EntryPrice: 1})
			return err
		},
		PermTrades: func(s *Store) error {
			_, err := s.AppendTrade(ctx, TradeInput{Symbol: "NVDA.US", Action: "BUY", Qty: 1, FillPrice: 1})
			return err
		},
		PermOrders: func(s *Store) error {
			_, err := s.CreateOrder(ctx, OrderInput{ClientOrderID: "c-" + string(s.role), Symbol: "NVDA.US", Side: "BUY", Qty: 1})
			return err
		},
		PermRiskState: func(s *Store) error { return s.UpdateRisk(ctx, Fields{"equity": 1.0}) },
		PermHalt:      func(s *Store) error { return s.SetHalt(ctx, false, nil) },
		PermSignals: func(s *Store) error {
			_, err := s.AddSignal(ctx, SignalInput{Kind: Ptr("test")})
			return err
		},
		PermEvents: func(s *Store) error {
			_, err := s.AddEvent(ctx, EventInput{Kind: Ptr("test")})
			return err
		},
	}
	for role, perms := range writePerms {
		s := d.MustAs(role)
		for p, fn := range write {
			err := fn(s)
			want := perms[p] || perms[permAll]
			var pe *PermissionError
			denied := errors.As(err, &pe)
			if want && err != nil {
				t.Errorf("%s/%s: want allowed, got %v", role, p, err)
			}
			if !want && !denied {
				t.Errorf("%s/%s: want PermissionError, got %v", role, p, err)
			}
		}
	}
}

func TestReadOnlyDB(t *testing.T) {
	d := newTestDB(t)
	ro, err := OpenReadOnly(d.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := ro.As(RoleExecutor); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("executor on ro db: %v", err)
	}
	r := ro.MustAs(RoleReader)
	ctx := context.Background()
	if err := r.Beat(ctx, "app", nil); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("beat on ro db: %v", err)
	}
	// Even the raw connection must refuse writes (mode=ro).
	if _, err := ro.sql.Exec("INSERT INTO kv(key,value) VALUES('x','y')"); err == nil {
		t.Fatal("raw write through mode=ro succeeded")
	}
	if risk, err := r.GetRisk(ctx); err != nil || risk == nil {
		t.Fatalf("read through ro db: %v %v", risk, err)
	}
}

func TestSubmitIntentDedup(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	s := d.MustAs(RoleEvent)
	in := IntentInput{Symbol: "NVDA.US", Side: "long", DedupKey: Ptr("news:42"),
		Features: map[string]any{"why": "财报 <beat> & raise"}}
	id, ok, err := s.SubmitIntent(ctx, in)
	if err != nil || !ok || id == 0 {
		t.Fatalf("first submit: %d %v %v", id, ok, err)
	}
	id2, ok2, err := s.SubmitIntent(ctx, in)
	if err != nil || ok2 || id2 != 0 {
		t.Fatalf("dup submit: %d %v %v", id2, ok2, err)
	}
	// Without a dedup key there is no collision.
	in.DedupKey = nil
	if _, ok, err := s.SubmitIntent(ctx, in); err != nil || !ok {
		t.Fatalf("no-key submit: %v %v", ok, err)
	}
	it, err := s.Intent(ctx, id)
	if err != nil || it == nil {
		t.Fatal(err)
	}
	if it.Source != "event" || it.Status != "pending" {
		t.Fatalf("source/status = %s/%s", it.Source, it.Status)
	}
	// Same encoding as json.dumps(ensure_ascii=False): UTF-8 and <>& unescaped.
	if got := string(it.Features); got != `{"why":"财报 <beat> & raise"}` {
		t.Fatalf("features = %s", got)
	}
}

// Only the dedup conflict is absorbed: any other failure (here, the conflict
// target's index is gone) must reach the caller instead of reading as "dup".
func TestSubmitIntentSurfacesRealErrors(t *testing.T) {
	d := newTestDB(t)
	_, err := d.sql.Exec("DROP INDEX idx_intents_dedup")
	if err != nil {
		t.Fatal(err)
	}
	s := d.MustAs(RoleTechnical)
	if _, _, err := s.SubmitIntent(context.Background(), IntentInput{Symbol: "X", Side: "long", DedupKey: Ptr("k")}); err == nil {
		t.Fatal("ON CONFLICT without its index should error, got nil")
	}
}

func TestClaimOrder(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 9, 1, 0, 0, 0, time.UTC)
	defer func() { nowFunc = time.Now }()
	submit := func(role Role, prio int, at time.Duration) int64 {
		nowFunc = func() time.Time { return base.Add(at) }
		id, _, err := d.MustAs(role).SubmitIntent(ctx, IntentInput{Symbol: "AAPL.US", Side: "long", Priority: prio})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	a := submit(RoleTechnical, 10, 2*time.Second)
	b := submit(RoleUser, 100, 3*time.Second)
	c := submit(RoleTechnical, 10, 1*time.Second)
	got, err := d.MustAs(RoleExecutor).ClaimIntents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{b, c, a}
	for i, it := range got {
		if it.ID != want[i] {
			t.Fatalf("claim order = %v, want %v", ids(got), want)
		}
	}
	if err := d.MustAs(RoleExecutor).DecideIntent(ctx, b, "rejected", Ptr("cooldown")); err != nil {
		t.Fatal(err)
	}
	got, _ = d.MustAs(RoleExecutor).ClaimIntents(ctx, 10)
	if len(got) != 2 {
		t.Fatalf("decided intent still pending: %v", ids(got))
	}
}

func ids(xs []Intent) []int64 {
	out := make([]int64, len(xs))
	for i, x := range xs {
		out[i] = x.ID
	}
	return out
}

func TestPositionLifecycle(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	ex := d.MustAs(RoleExecutor)
	id, err := ex.OpenPosition(ctx, PositionInput{Symbol: "NVDA.US", Side: "long", Qty: 20,
		EntryPrice: 200, Stop: Ptr(197.0), Target: Ptr(206.0), Target2: Ptr(210.0), RiskAmt: Ptr(60.0)})
	if err != nil {
		t.Fatal(err)
	}
	p, err := ex.PositionFor(ctx, "NVDA.US")
	if err != nil || p == nil || p.ID != id || p.T1Done {
		t.Fatalf("PositionFor = %+v %v", p, err)
	}
	if err := ex.ReducePosition(ctx, id, 10, Ptr(30.0)); err != nil {
		t.Fatal(err)
	}
	p, _ = ex.PositionFor(ctx, "NVDA.US")
	if p.Qty != 10 || *p.RiskAmt != 30 || !p.T1Done {
		t.Fatalf("after reduce = %+v", p)
	}
	if err := ex.ClosePosition(ctx, id); err != nil {
		t.Fatal(err)
	}
	if p, _ := ex.PositionFor(ctx, "NVDA.US"); p != nil {
		t.Fatalf("closed position still open: %+v", p)
	}
	if ps, _ := ex.OpenPositions(ctx); len(ps) != 0 {
		t.Fatalf("open positions = %d", len(ps))
	}
}

func TestOrdersAndUpdateWhitelist(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	ex := d.MustAs(RoleExecutor)
	if _, err := ex.CreateOrder(ctx, OrderInput{ClientOrderID: "flintrade-1", Symbol: "AAPL.US", Side: "BUY",
		Qty: 5, Price: Ptr(300.0), OutsideRTH: Ptr("RTH_ONLY")}); err != nil {
		t.Fatal(err)
	}
	if ords, _ := ex.OpenOrders(ctx); len(ords) != 1 || ords[0].Status != "submitted" {
		t.Fatalf("open orders = %+v", ords)
	}
	if err := ex.UpdateOrder(ctx, "flintrade-1", Fields{"status": "filled", "filled_qty": 5, "avg_price": 299.5,
		"broker_order_id": "B1"}); err != nil {
		t.Fatal(err)
	}
	if ords, _ := ex.OpenOrders(ctx); len(ords) != 0 {
		t.Fatalf("filled order still open: %+v", ords)
	}
	if err := ex.UpdateOrder(ctx, "flintrade-1", Fields{"status = 'x', qty": 1}); err == nil {
		t.Fatal("non-whitelisted column accepted")
	}
	if err := ex.UpdateRisk(ctx, Fields{"updated_at": "x"}); err == nil {
		t.Fatal("updated_at must not be patchable directly")
	}
}

func TestHaltStoredAsInteger(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	rm := d.MustAs(RoleRiskMonitor)
	if err := rm.SetHalt(ctx, true, Ptr("daily loss 5%")); err != nil {
		t.Fatal(err)
	}
	var raw any
	if err := d.sql.QueryRow("SELECT halt FROM risk_state WHERE id=1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != int64(1) {
		t.Fatalf("halt stored as %T %v, want int64 1 (Python reads it as 0/1)", raw, raw)
	}
	if h, _ := rm.IsHalted(ctx); !h {
		t.Fatal("IsHalted = false after SetHalt(true)")
	}
}

// Hard lesson「多步写必须原子」: a failure mid-settlement leaves nothing behind.
func TestHardLesson_MultiStepWritesAtomic(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	ex := d.MustAs(RoleExecutor)
	boom := errors.New("crash between position and trade")
	err := ex.Tx(ctx, func(tx *Store) error {
		if _, err := tx.OpenPosition(ctx, PositionInput{Symbol: "TSLA.US", Side: "long", Qty: 5, EntryPrice: 400}); err != nil {
			return err
		}
		// Nested Tx reuses the outer transaction.
		if err := tx.Tx(ctx, func(tx2 *Store) error {
			return tx2.UpdateRisk(ctx, Fields{"open_risk": 30.0})
		}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx err = %v", err)
	}
	if ps, _ := ex.OpenPositions(ctx); len(ps) != 0 {
		t.Fatal("position survived a rolled-back settlement")
	}
	if r, _ := ex.GetRisk(ctx); r.OpenRisk != 0 {
		t.Fatalf("open_risk = %v after rollback", r.OpenRisk)
	}

	// Panics roll back too.
	func() {
		defer func() { _ = recover() }()
		_ = ex.Tx(ctx, func(tx *Store) error {
			_, _ = tx.OpenPosition(ctx, PositionInput{Symbol: "TSLA.US", Side: "long", Qty: 5, EntryPrice: 400})
			panic("boom")
		})
	}()
	if ps, _ := ex.OpenPositions(ctx); len(ps) != 0 {
		t.Fatal("position survived a panicking settlement")
	}

	// And the happy path commits both steps.
	if err := ex.Tx(ctx, func(tx *Store) error {
		if _, err := tx.OpenPosition(ctx, PositionInput{Symbol: "TSLA.US", Side: "long", Qty: 5, EntryPrice: 400}); err != nil {
			return err
		}
		return tx.UpdateRisk(ctx, Fields{"open_risk": 30.0})
	}); err != nil {
		t.Fatal(err)
	}
	if ps, _ := ex.OpenPositions(ctx); len(ps) != 1 {
		t.Fatal("committed position missing")
	}
}

// Tx must take the write lock at BEGIN (BEGIN IMMEDIATE), so two settlers
// serialise instead of deadlocking on lock upgrade.
func TestTxBeginsImmediate(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	other, err := sql.Open("sqlite", "file:"+d.Path()+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	err = d.MustAs(RoleExecutor).Tx(ctx, func(tx *Store) error {
		// No write yet: only BEGIN IMMEDIATE could be holding the lock.
		_, err := other.Exec("BEGIN IMMEDIATE")
		if err == nil {
			other.Exec("ROLLBACK")
			return errors.New("second writer got the lock while Tx was open")
		}
		if !strings.Contains(err.Error(), "locked") && !strings.Contains(err.Error(), "busy") {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBeatAndKV(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	s := d.MustAs(RoleReconciler)
	if err := s.Beat(ctx, "", Ptr("ok")); err != nil {
		t.Fatal(err)
	}
	if err := s.Beat(ctx, "", nil); err != nil {
		t.Fatal(err)
	}
	hb, err := s.Heartbeats(ctx)
	if err != nil || len(hb) != 1 || hb[0].Process != "reconciler" || *hb[0].PID != os.Getpid() || hb[0].Note != nil {
		t.Fatalf("heartbeats = %+v %v", hb, err)
	}
	if _, ok, _ := s.KVGet(ctx, "missing"); ok {
		t.Fatal("missing key reported present")
	}
	if err := s.KVSet(ctx, "last_dream_date", "2026-11-08"); err != nil {
		t.Fatal(err)
	}
	if err := s.KVSet(ctx, "last_dream_date", "2026-11-09"); err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := s.KVGet(ctx, "last_dream_date"); !ok || v != "2026-11-09" {
		t.Fatalf("kv = %q %v", v, ok)
	}
}

// Init and the readers run against a copy of the live ledger: the Go layer
// must accept the database Python has been writing since June.
func TestAgainstLiveLedgerCopy(t *testing.T) {
	src := "../../flintrade.db"
	if _, err := os.Stat(src); err != nil {
		t.Skip("no live flintrade.db next to the repo")
	}
	ro, err := OpenReadOnly(src)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var trades0 int
	ro.sql.QueryRow("SELECT count(*) FROM trades").Scan(&trades0)
	// VACUUM INTO gives a consistent snapshot including the WAL.
	dst := filepath.Join(t.TempDir(), "copy.db")
	if _, err := ro.sql.Exec("VACUUM INTO ?", dst); err != nil {
		ro.Close()
		t.Skipf("snapshot: %v", err)
	}
	ro.Close()

	d, err := Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Init(ctx); err != nil {
		t.Fatalf("Init on live copy: %v", err)
	}
	r := d.MustAs(RoleReader)
	if _, err := r.OpenPositions(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.OpenOrders(ctx); err != nil {
		t.Fatal(err)
	}
	trades, err := r.RecentTrades(ctx, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) < trades0 {
		t.Fatalf("read %d trades, snapshot had at least %d", len(trades), trades0)
	}
	if risk, err := r.GetRisk(ctx); err != nil || risk == nil {
		t.Fatalf("risk: %v %v", risk, err)
	}
	if _, err := r.Heartbeats(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClaimIntents(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	var all int
	d.sql.QueryRow("SELECT count(*) FROM intents").Scan(&all)
	rows, err := d.sql.Query("SELECT " + intentCols + " FROM intents")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		if _, err := scanIntent(rows); err != nil {
			t.Fatalf("scan intent: %v", err)
		}
		n++
	}
	rows.Close()
	if n != all {
		t.Fatalf("scanned %d of %d intents", n, all)
	}
}

func TestRawExecNeedsPerm(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	rm := d.MustAs(RoleRiskMonitor)
	if _, err := rm.Exec(ctx, PermPositions, `DELETE FROM positions`); err == nil {
		t.Fatal("risk_monitor raw-wrote positions")
	}
	if err := rm.DayRoll(ctx, Ptr(1000.0)); err != nil {
		t.Fatal(err)
	}
	if err := d.MustAs(RoleTechnical).DayRoll(ctx, nil); err == nil {
		t.Fatal("technical rolled the day")
	}
	var n int
	if err := d.MustAs(RoleReader).QueryRow(ctx, `SELECT count(*) FROM risk_state`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("read: %d %v", n, err)
	}
}
