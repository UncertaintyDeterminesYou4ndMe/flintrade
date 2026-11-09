package postmortem

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"

	"flintrade/internal/db"
	"flintrade/internal/llm"
	"flintrade/internal/pyfloat"
	"flintrade/internal/testutil"
)

// canned returns a Complete that always answers text and counts calls.
func canned(text string, calls *int) Complete {
	return func(_ context.Context, req llm.Request) (string, error) {
		*calls++
		if req.Tier != "flash" || req.Tag != "postmortem" {
			panic("postmortem must call tier flash, tag postmortem")
		}
		return text, nil
	}
}

func seed(t *testing.T, s *db.Store, perm db.Perm, q string, args ...any) int64 {
	t.Helper()
	r, err := s.Exec(context.Background(), perm, q, args...)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := r.LastInsertId()
	return id
}

// pyCtx turns a golden ctx (decoded JSON) into a Context with SQLite-like
// values: integral JSON numbers that Python held as int become int64.
func pyCtx(m map[string]any) Context {
	v := func(k string) any {
		switch x := m[k].(type) {
		case json.Number:
			if i, err := x.Int64(); err == nil {
				return i
			}
			f, _ := x.Float64()
			return f
		default:
			return x
		}
	}
	s := func(k string) string { x, _ := m[k].(string); return x }
	return Context{Symbol: s("symbol"), Side: s("side"), EntryPrice: v("entry_price"), Stop: v("stop"),
		Target: v("target"), ExitPrice: v("exit_price"), PnL: v("pnl"), HoldMinutes: v("hold_minutes"),
		Thesis: v("thesis"), Confidence: v("confidence"), ExitReason: v("exit_reason")}
}

// The review prompt is model-visible: byte-identical to Python's f-string.
// Golden: scratchpad gen_c_goldens.py over agent/postmortem.py.
func TestGolden_MetricsPromptParse(t *testing.T) {
	raw, err := os.ReadFile("testdata/pure_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Cases []struct {
			Ctx     map[string]any `json:"ctx"`
			Metrics struct {
				ExitKind string   `json:"exit_kind"`
				Slip     *float64 `json:"slippage_vs_stop"`
			} `json:"metrics"`
			Prompt string `json:"prompt"`
		} `json:"cases"`
		Parse []struct {
			Text string         `json:"text"`
			Out  map[string]any `json:"out"`
		} `json:"parse"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&g); err != nil {
		t.Fatal(err)
	}
	for i, c := range g.Cases {
		ctx := pyCtx(c.Ctx)
		m := CodeMetrics(ctx)
		if m.ExitKind != c.Metrics.ExitKind || (m.SlippageVsStop == nil) != (c.Metrics.Slip == nil) ||
			(m.SlippageVsStop != nil && *m.SlippageVsStop != *c.Metrics.Slip) {
			t.Errorf("case %d metrics = %+v, want %+v", i, m, c.Metrics)
		}
		if got := buildPrompt(ctx, m); got != c.Prompt {
			t.Errorf("case %d prompt differs:\n--- go\n%s\n--- py\n%s", i, got, c.Prompt)
		}
	}
	for _, p := range g.Parse {
		got := ParseJSONObject(p.Text)
		if (got == nil) != (p.Out == nil) {
			t.Errorf("parse %q = %v, want %v", p.Text, got, p.Out)
		}
	}
}

// test_p2 §3/§4/§5: pairing, code metrics, review_new with a canned model,
// strategy-only filter, idempotency.
func TestReviewNew_PairMetricsIdempotent(t *testing.T) {
	ctx := context.Background()
	d := testutil.DB(t)
	m := d.MustAs(db.RoleMigrate)
	iid := seed(t, m, db.PermIntentsSubmit, `INSERT INTO intents(source,priority,symbol,side,confidence,reason,status,created_at)
		VALUES('technical',0,'ABC.US','long',70,'THESIS-ABC','approved','2026-07-10T14:00:00Z')`)
	pid := seed(t, m, db.PermPositions, `INSERT INTO positions(symbol,side,qty,entry_price,stop,target,risk_amt,source,intent_id,opened_at,status)
		VALUES('ABC.US','long',35,208.9,205.7,220.0,112.0,'technical',?,'2026-07-10T14:00:00Z','closed')`, iid)
	abc := seed(t, m, db.PermTrades, `INSERT INTO trades(ts,symbol,action,qty,fill_price,commission,pnl,source,intent_id,position_id,reason,attribution)
		VALUES('2026-07-10T17:00:00Z','ABC.US','SELL',35,203.0,0.7,-112.48,'technical',?,?,'Stop breach - thesis invalidated','strategy')`, iid, pid)
	ztest := seed(t, m, db.PermTrades, `INSERT INTO trades(ts,symbol,action,qty,fill_price,commission,pnl,source,reason,attribution)
		VALUES('2026-07-10T17:00:00Z','ZTEST.US','SELL',1,10.0,0.02,1.0,'technical','test harness fill','test')`)
	manual := seed(t, m, db.PermTrades, `INSERT INTO trades(ts,symbol,action,qty,fill_price,commission,pnl,source,reason,attribution)
		VALUES('2026-07-10T17:00:00Z','MAN.US','SELL',1,10.0,0.02,1.0,'user','manual close','manual')`)
	// Migrated fill without a position → unpairable stub, not a stuck queue.
	orphan := seed(t, m, db.PermTrades, `INSERT INTO trades(ts,symbol,action,qty,fill_price,commission,pnl,source,reason)
		VALUES('2026-07-10T18:00:00Z','OLD.US','SELL',1,10.0,0.02,-1.0,'technical','old')`)

	s := d.MustAs(db.RoleReflect)
	c, err := pair(ctx, s, closing{ID: abc, TS: "2026-07-10T17:00:00Z", Symbol: "ABC.US", FillPrice: 203.0, PositionID: &pid,
		Reason: "Stop breach - thesis invalidated"})
	if err != nil || c == nil {
		t.Fatalf("pair: %v %v", c, err)
	}
	if c.HoldMinutes != int64(180) || c.Thesis != "THESIS-ABC" || c.Confidence != int64(70) {
		t.Fatalf("pair = %+v", c)
	}
	if mt := CodeMetrics(*c); mt.ExitKind != "stop" || mt.SlippageVsStop == nil || *mt.SlippageVsStop != 2.7 {
		t.Fatalf("metrics = %+v", mt)
	}
	missing := int64(999999)
	if c, _ := pair(ctx, s, closing{ID: abc, PositionID: &missing}); c != nil {
		t.Fatal("missing position must pair to nil")
	}

	calls := 0
	reply := "```json\n{\"thesis_verdict\":\"wrong\",\"entry_grade\":\"ok\",\"exit_grade\":\"right\",\"confidence_justified\":false,\"lesson\":\"L1\"}\n```"
	lines, err := ReviewNew(ctx, s, canned(reply, &calls), 10)
	if err != nil || len(lines) != 2 || calls != 1 {
		t.Fatalf("review_new = %v, %v (calls %d); want ABC + orphan stub, one model call", lines, err, calls)
	}
	var kind, verdict, lesson, thesis *string
	var slip *float64
	var hold, cj *int64
	if err := s.QueryRow(ctx, `SELECT exit_kind, slippage_vs_stop, hold_minutes, thesis, lesson, thesis_verdict, confidence_justified
		FROM trade_reviews WHERE trade_id=?`, abc).Scan(&kind, &slip, &hold, &thesis, &lesson, &verdict, &cj); err != nil {
		t.Fatal(err)
	}
	if *kind != "stop" || *slip != 2.7 || *hold != 180 || *thesis != "THESIS-ABC" || *lesson != "L1" || *verdict != "wrong" || *cj != 0 {
		t.Fatalf("review row: kind=%v slip=%v hold=%v thesis=%v lesson=%v verdict=%v cj=%v", *kind, *slip, *hold, *thesis, *lesson, *verdict, *cj)
	}
	if err := s.QueryRow(ctx, `SELECT exit_kind FROM trade_reviews WHERE trade_id=?`, orphan).Scan(&kind); err != nil || *kind != "unpairable" {
		t.Fatalf("orphan stub: %v %v", kind, err)
	}
	for _, id := range []int64{ztest, manual} {
		var n int
		s.QueryRow(ctx, `SELECT COUNT(*) FROM trade_reviews WHERE trade_id=?`, id).Scan(&n)
		if n != 0 {
			t.Fatalf("non-strategy trade %d was reviewed", id)
		}
	}

	// §4 idempotency
	lines, err = ReviewNew(ctx, s, canned(reply, &calls), 10)
	if err != nil || len(lines) != 0 || calls != 1 {
		t.Fatalf("second review_new = %v %v (calls %d)", lines, err, calls)
	}
}

// test_p2 §6: a garbage reply still writes the row with code metrics only.
func TestReviewNew_GarbageReply(t *testing.T) {
	ctx := context.Background()
	d := testutil.DB(t)
	m := d.MustAs(db.RoleMigrate)
	pid := seed(t, m, db.PermPositions, `INSERT INTO positions(symbol,side,qty,entry_price,stop,target,risk_amt,source,opened_at,status)
		VALUES('GARBAGE.US','long',10,50.0,48.0,55.0,20.0,'technical','2026-07-10T14:00:00Z','closed')`)
	tid := seed(t, m, db.PermTrades, `INSERT INTO trades(ts,symbol,action,qty,fill_price,commission,pnl,source,position_id,reason,attribution)
		VALUES('2026-07-10T15:00:00Z','GARBAGE.US','SELL',10,53.0,0.2,29.6,'technical',?,'target reached','strategy')`, pid)
	calls := 0
	s := d.MustAs(db.RoleReflect)
	lines, err := ReviewNew(ctx, s, canned("no json here, sorry", &calls), 10)
	if err != nil || len(lines) != 1 {
		t.Fatalf("lines=%v err=%v", lines, err)
	}
	var kind string
	var verdict, lesson *string
	if err := s.QueryRow(ctx, `SELECT exit_kind, thesis_verdict, lesson FROM trade_reviews WHERE trade_id=?`, tid).Scan(&kind, &verdict, &lesson); err != nil {
		t.Fatal(err)
	}
	if kind != "target" || verdict != nil || lesson != nil {
		t.Fatalf("garbage row: %s %v %v", kind, verdict, lesson)
	}
	// A model error is the same as garbage: never fails the review.
	failing := func(context.Context, llm.Request) (string, error) { return "", os.ErrDeadlineExceeded }
	seed(t, m, db.PermTrades, `INSERT INTO trades(ts,symbol,action,qty,fill_price,commission,pnl,source,position_id,reason)
		VALUES('2026-07-10T16:00:00Z','GARBAGE.US','SELL',1,53.0,0.2,1.0,'technical',?,'manual exit')`, pid)
	if lines, err := ReviewNew(ctx, s, failing, 10); err != nil || len(lines) != 1 {
		t.Fatalf("model error: %v %v", lines, err)
	}
}

// Stats buckets confidence <60 / 60-70 / >70 over reviewed trades.
func TestStatsBuckets(t *testing.T) {
	ctx := context.Background()
	d := testutil.DB(t)
	m := d.MustAs(db.RoleMigrate)
	for i, row := range []struct {
		conf any
		pnl  float64
		kind string
		slip any
	}{{55, 1, "target", nil}, {60, -1, "stop", 0.25}, {70, 2, "target", nil}, {71, -3, "stop", 0.5}, {nil, 1, "", nil}} {
		tid := seed(t, m, db.PermTrades, `INSERT INTO trades(ts,symbol,action,qty,fill_price,commission,pnl,source) VALUES('2026-07-10T15:00:00Z','X.US','SELL',1,1,0,?,'technical')`, row.pnl)
		seed(t, m, db.PermMemory, `INSERT INTO trade_reviews(trade_id,symbol,exit_kind,slippage_vs_stop,confidence,reviewed_at) VALUES(?,?,?,?,?,?)`,
			tid, "X.US", nullStr(row.kind), row.slip, row.conf, i)
	}
	st, err := Stats(ctx, d.MustAs(db.RoleReader))
	if err != nil {
		t.Fatal(err)
	}
	b := st.ConfidenceBuckets
	if st.N != 5 || b.Low.N != 1 || b.Mid.N != 2 || b.High.N != 1 || *b.Mid.WinRate != 0.5 || *b.High.WinRate != 0 ||
		st.ExitKindCounts["unknown"] != 1 || *st.AvgSlippageVsStop != pyfloat.Round(0.375, 4) || st.NetPnL != 0 {
		js, _ := json.Marshal(st)
		t.Fatalf("stats = %s", js)
	}
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
