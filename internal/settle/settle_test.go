package settle

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"flintrade/internal/db"
	"flintrade/internal/testutil"
)

// python_golden.json: 96 random opens / partial closes / full closes run
// through agent/settle.py on a fresh $10K ledger. Replaying them here must
// leave a bit-identical ledger.
type golden struct {
	Ops []struct {
		Op            string   `json:"op"`
		Symbol        string   `json:"symbol"`
		Side          string   `json:"side"`
		Qty           int      `json:"qty"`
		FillPrice     float64  `json:"fill_price"`
		Stop          *float64 `json:"stop"`
		Target        *float64 `json:"target"`
		Target2       *float64 `json:"target2"`
		CPS           float64  `json:"cps"`
		Source        string   `json:"source"`
		BrokerOrderID string   `json:"broker_order_id"`
		PositionID    int64    `json:"position_id"`
		PnL           float64  `json:"pnl"`
		RiskAfter     struct {
			Equity         float64 `json:"equity"`
			OpenRisk       float64 `json:"open_risk"`
			DayRealizedPnL float64 `json:"day_realized_pnl"`
		} `json:"risk_after"`
	} `json:"ops"`
	Positions []map[string]any `json:"positions"`
	Trades    []map[string]any `json:"trades"`
}

func TestMatchesPythonGolden(t *testing.T) {
	b, err := os.ReadFile("testdata/python_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	d := testutil.DB(t)
	testutil.SetEquity(t, d, 10000)
	ctx := context.Background()
	ex := d.MustAs(db.RoleExecutor)
	for i, op := range g.Ops {
		switch op.Op {
		case "open":
			id, err := Open(ctx, ex, OpenInput{Symbol: op.Symbol, Side: op.Side, Qty: op.Qty, FillPrice: op.FillPrice,
				Stop: op.Stop, Target: op.Target, Target2: op.Target2, CommissionPerShare: op.CPS, Source: op.Source,
				BrokerOrderID: db.Ptr(op.BrokerOrderID), Reason: db.Ptr("r")})
			if err != nil || id != op.PositionID {
				t.Fatalf("op %d open: id %d (python %d) %v", i, id, op.PositionID, err)
			}
		case "close":
			var held *db.Position
			ps, _ := ex.OpenPositions(ctx)
			for _, p := range ps {
				if p.ID == op.PositionID {
					held = &p
				}
			}
			if held == nil {
				t.Fatalf("op %d: position %d not open", i, op.PositionID)
			}
			pnl, err := Close(ctx, ex, CloseInput{Held: *held, FillPrice: op.FillPrice, Qty: op.Qty, CommissionPerShare: op.CPS,
				BrokerOrderID: db.Ptr(op.BrokerOrderID), Reason: db.Ptr("r")})
			if err != nil || pnl != op.PnL {
				t.Fatalf("op %d close: pnl %v (python %v) %v", i, pnl, op.PnL, err)
			}
		}
		r, _ := ex.GetRisk(ctx)
		if *r.Equity != op.RiskAfter.Equity || r.OpenRisk != op.RiskAfter.OpenRisk || r.DayRealizedPnL != op.RiskAfter.DayRealizedPnL {
			t.Fatalf("op %d risk: go %v/%v/%v python %+v", i, *r.Equity, r.OpenRisk, r.DayRealizedPnL, op.RiskAfter)
		}
	}
	rows := func(q string) []map[string]any {
		rs, err := ex.Query(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Close()
		cols, _ := rs.Columns()
		var out []map[string]any
		for rs.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			rs.Scan(ptrs...)
			m := map[string]any{}
			for i, c := range cols {
				m[c] = vals[i]
			}
			out = append(out, m)
		}
		return out
	}
	norm := func(v any) any { b, _ := json.Marshal(v); var o any; json.Unmarshal(b, &o); return o }
	if got := rows(`SELECT id,symbol,side,qty,entry_price,stop,target,target2,t1_done,risk_amt,source,status FROM positions ORDER BY id`); !reflect.DeepEqual(norm(got), norm(g.Positions)) {
		t.Errorf("positions differ")
	}
	if got := rows(`SELECT symbol,action,qty,fill_price,commission,pnl,source,intent_id,broker_order_id,position_id,attribution FROM trades ORDER BY id`); !reflect.DeepEqual(norm(got), norm(g.Trades)) {
		t.Errorf("trades differ")
	}
}

// A failure mid-settlement must leave no trace (hard-lesson: multi-step
// writes are atomic). A reader-role store fails at the first write; a
// risk_state without equity fails at the last step, after two writes.
func TestHardLesson_SettlementAtomic(t *testing.T) {
	d := testutil.DB(t) // equity NULL
	ctx := context.Background()
	ex := d.MustAs(db.RoleExecutor)
	if _, err := Open(ctx, ex, OpenInput{Symbol: "X.US", Side: "long", Qty: 1, FillPrice: 10, Stop: db.Ptr(9.0), CommissionPerShare: 0.02}); err == nil {
		t.Fatal("settled without equity")
	}
	ps, _ := ex.OpenPositions(ctx)
	ts, _ := ex.RecentTrades(ctx, 10)
	if len(ps) != 0 || len(ts) != 0 {
		t.Fatalf("torn ledger: %d positions, %d trades", len(ps), len(ts))
	}
}
