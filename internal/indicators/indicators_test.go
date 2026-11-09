package indicators

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

type golden struct {
	Cases []struct {
		Name       string          `json:"name"`
		Symbol     string          `json:"symbol"`
		H4Lookback int             `json:"h4_lookback"`
		Klines     json.RawMessage `json:"klines"`
		Want       any             `json:"want"`
	} `json:"cases"`
	SeriesSymbol string                     `json:"series_symbol"`
	Series       map[string]json.RawMessage `json:"series"`
}

func loadGolden(t *testing.T) (*golden, map[string][]Kline) {
	t.Helper()
	b, err := os.ReadFile("testdata/python_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	real := map[string][]Kline{}
	files, _ := filepath.Glob("testdata/klines/*.json")
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		k, err := ParseKlines(raw)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		real[strings.TrimSuffix(filepath.Base(f), ".json")] = k
	}
	return &g, real
}

// resolve turns "ref:SYM:-N:" / "ref:SYM::END" into the slice Python used.
func resolve(t *testing.T, raw json.RawMessage, real map[string][]Kline) []Kline {
	var ref string
	if json.Unmarshal(raw, &ref) == nil {
		p := strings.Split(ref, ":")
		k := real[p[1]]
		if p[2] != "" {
			n, _ := strconv.Atoi(p[2])
			return k[max(len(k)+n, 0):]
		}
		end, _ := strconv.Atoi(p[3])
		return k[:end]
	}
	k, err := ParseKlines(raw)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func roundTrip(v any) any {
	b, _ := json.Marshal(v)
	var out any
	json.Unmarshal(b, &out)
	return out
}

// Every case must equal Python's output exactly — no tolerance.
func TestMatchesPythonGolden(t *testing.T) {
	g, real := loadGolden(t)
	failed := 0
	for _, c := range g.Cases {
		got := roundTrip(Analyze(resolve(t, c.Klines, real), c.Symbol, c.H4Lookback))
		if !reflect.DeepEqual(got, c.Want) {
			failed++
			if failed <= 5 {
				gj, _ := json.Marshal(got)
				wj, _ := json.Marshal(c.Want)
				t.Errorf("%s\n go: %s\n py: %s", c.Name, gj, wj)
			}
		}
	}
	if failed > 0 {
		t.Fatalf("%d of %d cases differ", failed, len(g.Cases))
	}
	if len(g.Cases) < 600 {
		t.Fatalf("golden has only %d cases", len(g.Cases))
	}
}

func seriesEqual(t *testing.T, name string, got []float64, raw json.RawMessage) {
	t.Helper()
	var want []*float64
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if len(got) != len(want) {
		t.Fatalf("%s: len %d, python %d", name, len(got), len(want))
	}
	for i := range got {
		switch {
		case want[i] == nil && !IsNone(got[i]):
			t.Fatalf("%s[%d] = %v, python None", name, i, got[i])
		case want[i] != nil && (IsNone(got[i]) || got[i] != *want[i]):
			t.Fatalf("%s[%d] = %v, python %v (Δ %g)", name, i, got[i], *want[i], got[i]-*want[i])
		}
	}
}

// The primitives backtest/playbook_test.py imports, compared unrounded.
func TestSeriesMatchPythonUnrounded(t *testing.T) {
	g, real := loadGolden(t)
	k := real[g.SeriesSymbol]
	n := len(k)
	o, h, l, c := make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n)
	v := make([]int64, n)
	for i, b := range k {
		o[i], h[i], l[i], c[i], v[i] = b.Open, b.High, b.Low, b.Close, b.Volume
	}
	_ = o
	m, s, hist := MACD(c)
	mtm, mtmMA := MTM(c, 12, 6)
	for name, got := range map[string][]float64{
		"sma20": SMA(c, 20), "sma_short": SMA(c[:5], 20), "ema20": EMA(c, 20), "ema_short": EMA(c[:5], 20),
		"rsi": RSI(c, 14), "macd": m, "signal": s, "hist": hist, "wr": WR(h, l, c, 14),
		"mtm": mtm, "mtm_ma": mtmMA, "atr": ATR(h, l, c, 14), "vwap": VWAP(h, l, c, v), "vol_sma20": SMAInt(v, 20),
	} {
		seriesEqual(t, name, got, g.Series[name])
	}
	if got, want := roundTrip(Aggregate4H(k)), roundTrip(g.Series["agg4h"]); !reflect.DeepEqual(got, want) {
		t.Fatal("agg4h differs")
	}
	for name, snap := range map[string]*H4{"h4_lb12": H4Snapshot(k, 12), "h4_short": H4Snapshot(k[:100], 12)} {
		if got, want := roundTrip(snap), roundTrip(g.Series[name]); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: go %v, python %v", name, got, want)
		}
	}
}

func TestPySumIsCompensated(t *testing.T) {
	// Naive left-to-right gives 0; CPython ≥ 3.12 sum() gives 2.
	xs := []float64{1, 1e100, 1, -1e100}
	if got := pySum(xs); got != 2 {
		t.Fatalf("pySum = %v, want 2", got)
	}
}

func TestPyRoundTiesToEven(t *testing.T) {
	for _, c := range []struct{ x, want float64 }{
		{0.125, 0.12}, {0.375, 0.38}, {2.675, 2.67}, {-1.5, -2}, {2.5, 2},
	} {
		n := 2
		if math.Trunc(c.want) == c.want {
			n = 0
		}
		if got := pyRound(c.x, n); got != c.want {
			t.Errorf("pyRound(%v, %d) = %v, want %v", c.x, n, got, c.want)
		}
	}
}

func TestErrorShape(t *testing.T) {
	b, _ := Analyze(nil, "X.US", 12).MarshalJSON()
	if string(b) != `{"symbol":"X.US","error":"Need >=30 bars, got 0","score":0,"signal":"WAIT"}` {
		t.Fatalf("error shape = %s", b)
	}
}

func TestParseKlinesRejectsGarbage(t *testing.T) {
	for _, raw := range []string{`[{"open":"x","high":1,"low":1,"close":1,"volume":1}]`,
		`[{"open":1,"high":1,"low":1,"close":1,"volume":"1.5"}]`, `{}`} {
		if _, err := ParseKlines([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	k, err := ParseKlines([]byte(`[{"open":1,"high":2,"low":0.5,"close":1.5,"volume":10,"time":"2026-11-06T19:30:00Z"}]`))
	if want := float64(time.Date(2026, 11, 6, 19, 30, 0, 0, time.UTC).Unix()); err != nil || k[0].Epoch != want {
		t.Fatalf("parse: %+v %v", k, err)
	}
}
