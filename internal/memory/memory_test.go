package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flintrade/internal/db"
	"flintrade/internal/producers/pj"
	"flintrade/internal/session"
	"flintrade/internal/testutil"
)

func TestSimilarHistory(t *testing.T) {
	ctx := context.Background()
	d := testutil.DB(t)
	m := d.MustAs(db.RoleMigrate)
	add := func(ts, sym, action string, pnl any, attr any, reason string) {
		t.Helper()
		if _, err := m.Exec(ctx, db.PermTrades, `INSERT INTO trades(ts,symbol,action,qty,fill_price,commission,pnl,source,reason,attribution)
			VALUES(?,?,?,1,1,0,?,'technical',?,?)`, ts, sym, action, pnl, reason, attr); err != nil {
			t.Fatal(err)
		}
	}
	add("2026-11-02T15:00:00Z", "AAA.US", "BUY", nil, "strategy", "open") // an entry: no pnl
	add("2026-11-02T16:00:00Z", "AAA.US", "SELL", 10.0, "strategy", "target hit")
	add("2026-11-03T09:00:00Z", "AAA.US", "COVER", -4.0, nil, "stop "+strings.Repeat("x", 200))
	add("2026-11-03T16:00:00Z", "AAA.US", "SELL", 100.0, "manual", "manual")
	add("2026-11-03T16:00:00Z", "AAA.US", "SELL", 100.0, "test", "test")
	add("2026-11-03T16:00:00Z", "AAA.US", "SELL", 100.0, "outage-degraded", "outage")
	add("2026-11-04T15:30:00Z", "AAA.US", "SELL", 100.0, "legacy", "v1") // other system, not this agent's record
	add("2026-11-04T15:30:00Z", "BBB.US", "SELL", 5.0, "strategy", "other symbol")

	s := d.MustAs(db.RoleReader)
	if h, err := SimilarHistory(ctx, s, "ZZZ.US", session.Intraday); h != nil || err != nil {
		t.Fatalf("no history must be nil, got %+v %v", h, err)
	}
	h, err := SimilarHistory(ctx, s, "AAA.US", session.Intraday)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := pj.Dumps(h)
	want := `{"symbol": "AAA.US", "summary": {"n": 2, "wins": 1, "win_rate": 0.5, "net_pnl": 6.0, "avg_pnl": 3.0}, ` +
		`"same_session": {"session": "Intraday", "n": 1, "wins": 1, "win_rate": 1.0, "net_pnl": 10.0, "avg_pnl": 10.0}, "recent": [` +
		`{"trade_id": 3, "closed_at": "2026-11-03T09:00:00Z", "side": "short", "session": "Pre", "pnl": -4.0, "exit_reason": "stop ` + strings.Repeat("x", 155) + `"}, ` +
		`{"trade_id": 2, "closed_at": "2026-11-02T16:00:00Z", "side": "long", "session": "Intraday", "pnl": 10.0, "exit_reason": "target hit"}]}`
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	// Overnight-Pre is Overnight; Closed has no same-session block; a
	// session with no trades shows n=0 with null rates.
	h, _ = SimilarHistory(ctx, s, "AAA.US", session.Closed)
	if h.SameSession != nil {
		t.Fatal("Closed must have no same_session")
	}
	h, _ = SimilarHistory(ctx, s, "AAA.US", session.OvernightPre)
	if h.SameSession == nil || h.SameSession.Session != session.Overnight || h.SameSession.N != 0 || h.SameSession.WinRate != nil {
		t.Fatalf("overnight-pre = %+v", h.SameSession)
	}
	if b, _ := pj.Dumps(h.SameSession); b != `{"session": "Overnight", "n": 0, "wins": 0, "win_rate": null, "net_pnl": 0.0, "avg_pnl": null}` {
		t.Fatal(b)
	}
}

// Sample on a VACUUM INTO copy of the live DB (read-only):
//
//	FLINTRADE_PARITY_DIR=<scratchpad> go test ./internal/memory -run LiveCopy -v
func TestLiveCopySample(t *testing.T) {
	dir := os.Getenv("FLINTRADE_PARITY_DIR")
	if dir == "" {
		t.Skip("FLINTRADE_PARITY_DIR not set")
	}
	ro, err := db.OpenReadOnly(filepath.Join(dir, "c_live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	sym := os.Getenv("FLINTRADE_SAMPLE_SYMBOL")
	h, err := SimilarHistory(context.Background(), ro.MustAs(db.RoleReader), sym, session.Intraday)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := pj.Dumps(h)
	t.Log(out)
}
