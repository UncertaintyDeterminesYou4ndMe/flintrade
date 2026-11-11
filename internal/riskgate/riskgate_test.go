package riskgate

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"flintrade/internal/config"
	"flintrade/internal/db"
)

// testdata/python_golden.json: agent/risk_gate.RiskGate.evaluate run over
// hand-written edge cases plus random combinations, under six risk.toml /
// strategies.toml variants, incl. cases found by search where CPython float
// floor division or compensated sum() decide the verdict. After Python is
// deleted this file is the reference.
type goldenFile struct {
	Now        string            `json:"now"`
	Risks      map[string]string `json:"risk_variants"`
	Strategies map[string]string `json:"strategy_variants"`
	Cases      []struct {
		Risk       string `json:"risk"`
		Strategies string `json:"strategies"`
		Input      struct {
			Equity    float64 `json:"equity"`
			Halted    bool    `json:"halted"`
			Positions []struct {
				Symbol     string   `json:"symbol"`
				Side       string   `json:"side"`
				Qty        int      `json:"qty"`
				EntryPrice float64  `json:"entry_price"`
				RiskAmt    *float64 `json:"risk_amt"`
			} `json:"positions"`
			RecentTrades []struct {
				Symbol string   `json:"symbol"`
				Action string   `json:"action"`
				PnL    *float64 `json:"pnl"`
				TS     *string  `json:"ts"`
			} `json:"recent_trades"`
			Session        string   `json:"session"`
			MinutesToClose *int     `json:"minutes_to_close"`
			VolumeRatio    *float64 `json:"volume_ratio"`
			OpenOrders     []struct {
				Symbol string   `json:"symbol"`
				Side   string   `json:"side"`
				Qty    *int     `json:"qty"`
				Price  *float64 `json:"price"`
			} `json:"open_orders"`
			Intent struct {
				Source        string   `json:"source"`
				Symbol        string   `json:"symbol"`
				Side          string   `json:"side"`
				EntryHint     *float64 `json:"entry_hint"`
				Stop          *float64 `json:"stop"`
				Confidence    *int     `json:"confidence"`
				HypothesisQty *int     `json:"hypothesis_qty"`
				Features      any      `json:"features"`
			} `json:"intent"`
		} `json:"input"`
		Want struct {
			Approved bool           `json:"approved"`
			Qty      int            `json:"qty"`
			Reason   string         `json:"reason"`
			Notes    map[string]any `json:"notes"`
		} `json:"want"`
	} `json:"cases"`
}

func TestMatchesPythonGolden(t *testing.T) {
	b, err := os.ReadFile("testdata/python_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g goldenFile
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	now, err := time.Parse(time.RFC3339Nano, g.Now)
	if err != nil {
		t.Fatal(err)
	}
	risks := map[string]*config.Risk{}
	for name, text := range g.Risks {
		if risks[name], err = config.ParseRisk([]byte(text)); err != nil {
			t.Fatalf("risk variant %s: %v", name, err)
		}
	}
	strats := map[string]*config.Strategies{}
	for name, text := range g.Strategies {
		if text == "" {
			strats[name] = nil
			continue
		}
		if strats[name], err = config.ParseStrategies([]byte(text)); err != nil {
			t.Fatalf("strategy variant %s: %v", name, err)
		}
	}
	failed := 0
	codeKeys := map[string]bool{}
	for i, c := range g.Cases {
		in := c.Input
		gate := &Gate{Risk: risks[c.Risk], Strategies: strats[c.Strategies], Equity: in.Equity, Halted: in.Halted,
			Session: in.Session, MinutesToClose: in.MinutesToClose, VolumeRatio: in.VolumeRatio, Now: now}
		for _, p := range in.Positions {
			gate.Positions = append(gate.Positions, db.Position{Symbol: p.Symbol, Side: p.Side, Qty: p.Qty, EntryPrice: p.EntryPrice, RiskAmt: p.RiskAmt})
		}
		for _, tr := range in.RecentTrades {
			ts := ""
			if tr.TS != nil {
				ts = *tr.TS
			}
			gate.RecentTrades = append(gate.RecentTrades, db.Trade{Symbol: tr.Symbol, Action: tr.Action, PnL: tr.PnL, TS: ts})
		}
		for _, o := range in.OpenOrders {
			q := 0
			if o.Qty != nil {
				q = *o.Qty
			}
			gate.OpenOrders = append(gate.OpenOrders, db.Order{Symbol: o.Symbol, Side: o.Side, Qty: q, Price: o.Price})
		}
		it := in.Intent
		feats, _ := it.Features.(map[string]any)
		v := gate.Evaluate(Intent{Symbol: it.Symbol, Side: it.Side, Source: it.Source, HypothesisQty: it.HypothesisQty,
			EntryHint: it.EntryHint, Stop: it.Stop, Confidence: it.Confidence, Features: feats})
		if v.Notes == nil {
			v.Notes = map[string]any{} // Python: field(default_factory=dict)
		}
		gotNotes, _ := json.Marshal(v.Notes)
		wantNotes, _ := json.Marshal(c.Want.Notes)
		var gn, wn any
		json.Unmarshal(gotNotes, &gn)
		json.Unmarshal(wantNotes, &wn)
		// Every rejection also carries its reason as a code the app can say.
		if !v.Approved && !v.Code.Valid() {
			t.Errorf("case %d: rejection %q has no valid code: %+v", i, v.Reason, v.Code)
			codeKeys = nil
		} else if !v.Approved && codeKeys != nil {
			codeKeys[v.Code.Key] = true
		}
		if v.Approved != c.Want.Approved || v.Qty != c.Want.Qty || v.Reason != c.Want.Reason || !reflect.DeepEqual(gn, wn) {
			failed++
			if failed <= 8 {
				inj, _ := json.Marshal(c.Input)
				t.Errorf("case %d [%s/%s]\n go: %v %d %q %s\n py: %v %d %q %s\n in: %.600s", i, c.Risk, c.Strategies,
					v.Approved, v.Qty, v.Reason, gotNotes, c.Want.Approved, c.Want.Qty, c.Want.Reason, wantNotes, inj)
			}
		}
	}
	if failed > 0 {
		t.Fatalf("%d of %d cases differ", failed, len(g.Cases))
	}
	t.Logf("rejection codes covered by the golden cases: %d", len(codeKeys))
	if len(g.Cases) < 5000 {
		t.Fatalf("golden has only %d cases", len(g.Cases))
	}
}

func TestFloorDivMatchesPython(t *testing.T) {
	for _, c := range []struct{ a, b, want float64 }{
		{1, 0.1, 9}, {7, 2, 3}, {-7, 2, -4}, {7, -2, -4}, {0.5, 0.25, 2}, {-0.0, 3, -0}, {200, 2.0000000001, 99},
	} {
		if got := floorDiv(c.a, c.b); got != c.want || math.Signbit(got) != math.Signbit(c.want) {
			t.Errorf("floorDiv(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestPyRepr(t *testing.T) {
	for f, want := range map[float64]string{30: "30.0", 0.3: "0.3", 0.55: "0.55", 1e16: "1e+16", 1.5e-5: "1.5e-05", 1234.5: "1234.5", -2: "-2.0"} {
		if got := PyRepr(f); got != want {
			t.Errorf("PyRepr(%v) = %q, want %q", f, got, want)
		}
	}
}

// ── hard lessons owned by the gate ─────────────────────────────────────

func liveGate(t *testing.T) *Gate {
	t.Helper()
	r, err := config.ParseRisk(mustRead(t, "../../agent/config/risk.toml"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := config.ParseStrategies(mustRead(t, "../../agent/config/strategies.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return &Gate{Risk: r, Strategies: s, Equity: 10000, Session: "Intraday", MinutesToClose: db.Ptr(300),
		VolumeRatio: db.Ptr(1.0), Now: time.Now()}
}

func mustRead(t *testing.T, p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func long(sym string, entry, stop float64) Intent {
	return Intent{Symbol: sym, Side: "long", Source: "technical", EntryHint: &entry, Stop: &stop}
}

// Resting entry orders are real exposure (5 stacked AMZN orders = $19.4K on
// a $10K sleeve while the gate thought the book was flat). test_p0 FIX 5b.
func TestHardLesson_InflightOrdersCountAsExposure(t *testing.T) {
	g := liveGate(t)
	free := g.Evaluate(long("AAPL.US", 300, 294))
	g.OpenOrders = []db.Order{{Symbol: "NVDA.US", Side: "BUY", Qty: 19, Price: db.Ptr(205.0)}}
	busy := g.Evaluate(long("AAPL.US", 300, 294))
	if !free.Approved || !busy.Approved || busy.Qty >= free.Qty {
		t.Fatalf("in-flight exposure ignored: free %+v, with in-flight %+v", free, busy)
	}
	// In-flight symbols also take concurrency slots.
	g.OpenOrders = []db.Order{
		{Symbol: "USO.US", Side: "BUY", Qty: 1, Price: db.Ptr(80.0)},
		{Symbol: "GLD.US", Side: "SHORT", Qty: 1, Price: db.Ptr(380.0)},
		{Symbol: "SLV.US", Side: "BUY", Qty: 1, Price: db.Ptr(40.0)},
	}
	if v := g.Evaluate(long("AAPL.US", 300, 294)); v.Approved || !strings.Contains(v.Reason, "并发") {
		t.Fatalf("in-flight entries must take concurrency slots: %+v", v)
	}
}

// The gate's criteria must not come from the party it constrains: a missing
// volume_ratio is fail-closed for automated entries; manual orders are
// exempt; exits are never blocked by missing data.
func TestHardLesson_GateEvidenceFailClosed(t *testing.T) {
	g := liveGate(t)
	g.VolumeRatio = nil
	if v := g.Evaluate(long("USO.US", 80, 78)); v.Approved || !strings.Contains(v.Reason, "fail-closed") {
		t.Fatalf("missing volume_ratio passed: %+v", v)
	}
	manual := long("USO.US", 80, 78)
	manual.Source = "user"
	if v := g.Evaluate(manual); !v.Approved {
		t.Fatalf("manual order blocked by missing volume_ratio: %+v", v)
	}
	g.Positions = []db.Position{{Symbol: "USO.US", Side: "long", Qty: 5, EntryPrice: 80}}
	g.Halted = true
	if v := g.Evaluate(Intent{Symbol: "USO.US", Side: "close", Source: "risk_monitor"}); !v.Approved || v.Qty != 5 {
		t.Fatalf("exit must pass without data and while halted: %+v", v)
	}
	// Binding playbook: the h4 confirmation comes from the indicator
	// snapshot; without it an automated entry is refused.
	g.Halted, g.Positions, g.VolumeRatio = false, nil, db.Ptr(1.0)
	if v := g.Evaluate(long("SNDK.US", 50, 48.5)); v.Approved || !strings.Contains(v.Reason, "h4") {
		t.Fatalf("binding playbook without confirmation: %+v", v)
	}
	sndk := long("SNDK.US", 50, 48.5)
	sndk.Features = map[string]any{"h4_confluence": true}
	if v := g.Evaluate(sndk); !v.Approved {
		t.Fatalf("confirmed playbook entry refused: %+v", v)
	}
}

// test_phase1 RiskGate unit section, against the live config.
func TestPhase1GateUnits(t *testing.T) {
	g := liveGate(t)
	check := func(name string, v Verdict, ok bool, qty int, reasonHas string) {
		t.Helper()
		if v.Approved != ok || (ok && v.Qty != qty) || !strings.Contains(v.Reason, reasonHas) {
			t.Errorf("%s: %+v", name, v)
		}
	}
	check("sizing + per_symbol cap", g.Evaluate(long("NVDA.US", 100, 98)), true, 40, "批准")
	g.VolumeRatio = db.Ptr(0.2)
	check("volume floor", g.Evaluate(long("NVDA.US", 100, 98)), false, 0, "volume")
	g.VolumeRatio, g.MinutesToClose = db.Ptr(1.0), db.Ptr(10)
	check("close blackout", g.Evaluate(long("NVDA.US", 100, 98)), false, 0, "黑窗")
	g.MinutesToClose = db.Ptr(300)
	check("stop too close", g.Evaluate(long("NVDA.US", 100, 99.99)), false, 0, "止损")
	g.RecentTrades = []db.Trade{{Symbol: "NVDA.US", Action: "SELL", PnL: db.Ptr(-5.0), TS: db.FormatTime(time.Now().Add(-30 * time.Minute))}}
	check("no-revenge", g.Evaluate(long("NVDA.US", 100, 98)), false, 0, "revenge")
	g.RecentTrades = nil
	g.Positions = []db.Position{{Symbol: "AAPL.US", Qty: 1, EntryPrice: 200, RiskAmt: db.Ptr(2.0)},
		{Symbol: "MSFT.US", Qty: 1, EntryPrice: 400, RiskAmt: db.Ptr(2.0)}, {Symbol: "GOOGL.US", Qty: 1, EntryPrice: 300, RiskAmt: db.Ptr(2.0)}}
	check("concurrency 3", g.Evaluate(long("NVDA.US", 100, 98)), false, 0, "并发")
	g.Positions = nil
	short := long("NVDA.US", 100, 102)
	short.Side = "short"
	check("short allowed", g.Evaluate(short), true, 40, "批准")
	for conf, want := range map[int]int{80: 40, 50: 20, 58: 26} {
		in := long("NVDA.US", 100, 98)
		in.Confidence = db.Ptr(conf)
		check(fmt.Sprintf("confidence %d", conf), g.Evaluate(in), true, want, "批准")
	}
	g.Positions = []db.Position{{Symbol: "NVDA.US", Side: "long", Qty: 7, EntryPrice: 100, RiskAmt: db.Ptr(14.0)}}
	check("exit not scaled", g.Evaluate(Intent{Symbol: "NVDA.US", Side: "close", Confidence: db.Ptr(40)}), true, 7, "平仓")
}

// A partially filled entry order rests only its remainder; counting the full
// order would double the filled shares, which are already a position.
func TestInflightCountsUnfilledRemainder(t *testing.T) {
	g := &Gate{OpenOrders: []db.Order{
		{Symbol: "AAPL.US", Side: "BUY", Qty: 10, FilledQty: 4, Price: db.Ptr(100.0)},
		{Symbol: "MU.US", Side: "BUY", Qty: 5, FilledQty: 5, Price: db.Ptr(100.0)},
		{Symbol: "NVDA.US", Side: "SELL", Qty: 5, Price: db.Ptr(100.0)},
	}}
	got := g.inflight()
	if len(got) != 2 || got[0].symbol != "AAPL.US" || got[0].notional != 600 || got[1].notional != 0 {
		t.Fatalf("inflight %+v", got)
	}
}
