package marks

import (
	"context"
	"testing"
	"time"

	"flintrade/internal/db"
	"flintrade/internal/testutil"
)

func TestPnLAndTotal(t *testing.T) {
	long := db.Position{Symbol: "A.US", Side: "long", Qty: 3, EntryPrice: 642}
	short := db.Position{Symbol: "B.US", Side: "short", Qty: 2, EntryPrice: 50}
	if v := PnL(long, 640); v != -6 {
		t.Fatalf("long %v", v)
	}
	if v := PnL(short, 45); v != 10 {
		t.Fatalf("short %v", v)
	}
	ms := map[string]Mark{"A.US": {Price: 650}}
	if f := Total([]db.Position{long, short}, ms); f.Sum != 24 || f.Complete {
		t.Fatalf("partial %+v", f)
	}
	ms["B.US"] = Mark{Price: 51}
	if f := Total([]db.Position{long, short}, ms); f.Sum != 22 || !f.Complete {
		t.Fatalf("complete %+v", f)
	}
	if f := Total(nil, nil); f.Sum != 0 || !f.Complete {
		t.Fatalf("no positions %+v", f)
	}
}

func TestPutGet(t *testing.T) {
	ctx := context.Background()
	s := testutil.DB(t).MustAs(db.RoleRiskMonitor)
	at := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	if err := Put(ctx, s, map[string]float64{"A.US": 12.5}, "Intraday", at); err != nil {
		t.Fatal(err)
	}
	s.KVSet(ctx, markPrefix+"BAD.US", "not json")
	ms, err := Get(ctx, s, []string{"A.US", "BAD.US", "NONE.US"})
	if err != nil || len(ms) != 1 || ms["A.US"].Price != 12.5 || !ms["A.US"].Time().Equal(at) {
		t.Fatalf("%+v %v", ms, err)
	}
	if _, ok, err := GetDayStart(ctx, s); ok || err != nil {
		t.Fatal("no day start yet")
	}
}
