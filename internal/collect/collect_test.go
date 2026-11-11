package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"flintrade/internal/config"
	"flintrade/internal/lb"
)

// collectsh_golden.json is scripts/collect.sh's output over testdata/fixtures,
// served by testdata/fake/longbridge (MARKET_STATUS=Intraday, OUTSIDE_RTH=RTH_ONLY).

func collector(t *testing.T, bin string) (Collector, string) {
	t.Helper()
	abs, _ := filepath.Abs("testdata")
	log := filepath.Join(t.TempDir(), "lb.log")
	t.Setenv("FAKE_LB_DIR", filepath.Join(abs, "fixtures"))
	t.Setenv("FAKE_LB_LOG", log)
	return Collector{
		LB:     &lb.Runner{Bin: bin, Retries: 1},
		Config: config.Loader{Dir: filepath.Join(abs, "config")},
		Now:    func() time.Time { return time.Date(2026, 11, 9, 7, 0, 12, 0, time.UTC) },
	}, log
}

// canon renders JSON with key order kept and numbers normalised, so 50.0
// (Python) and 50 (Go) compare equal while a reordered key does not.
func canon(t *testing.T, raw []byte) string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var sb strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch v := tok.(type) {
		case json.Number:
			f, _ := strconv.ParseFloat(string(v), 64)
			sb.WriteString(strconv.FormatFloat(f, 'g', -1, 64))
		case string:
			sb.WriteString(strconv.Quote(v))
		default:
			b, _ := json.Marshal(v)
			sb.Write(b)
		}
		sb.WriteByte(' ')
	}
	return sb.String()
}

func topLevel(t *testing.T, raw []byte) ([]string, map[string]json.RawMessage) {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.Token()
	var keys []string
	for dec.More() {
		k, _ := dec.Token()
		keys = append(keys, k.(string))
		var skip json.RawMessage
		dec.Decode(&skip)
	}
	return keys, m
}

func TestMatchesCollectSh(t *testing.T) {
	c, log := collector(t, "testdata/fake/longbridge")
	p, err := c.Collect(context.Background(), "Intraday", "RTH_ONLY")
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.JSON()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile("testdata/collectsh_golden.json")
	gk, gm := topLevel(t, got)
	wk, wm := topLevel(t, want)
	if !slices.Equal(gk, wk) {
		t.Fatalf("top-level keys %v, collect.sh %v", gk, wk)
	}
	for _, k := range wk {
		if k == "time" || k == "quotes" {
			continue
		}
		if g, w := canon(t, gm[k]), canon(t, wm[k]); g != w {
			t.Errorf("%s differs\n go: %.400s\n sh: %.400s", k, g, w)
		}
	}

	// Quotes: collect.sh's 14 rows plus SNDK/MU, which its hardcoded list missed.
	var gq, wq []map[string]any
	json.Unmarshal(gm["quotes"], &gq)
	json.Unmarshal(wm["quotes"], &wq)
	bySym := map[string]string{}
	for _, q := range gq {
		b, _ := json.Marshal(q)
		bySym[q["symbol"].(string)] = string(b)
	}
	for _, q := range wq {
		b, _ := json.Marshal(q)
		if bySym[q["symbol"].(string)] != string(b) {
			t.Errorf("quote %v differs", q["symbol"])
		}
	}
	if len(gq) != len(wq)+2 || bySym["SNDK.US"] == "" || bySym["MU.US"] == "" {
		t.Errorf("go quotes %d, collect.sh %d; want +SNDK/MU", len(gq), len(wq))
	}

	// Each kline is fetched once, with 240 bars only for playbook symbols.
	// (collect.sh grepped the whole kline body for "429" and refetched 3×.)
	calls, _ := os.ReadFile(log)
	seen := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		f := strings.Fields(line)
		if f[0] != "kline" {
			continue
		}
		seen[f[1]]++
		want := "50"
		if f[1] == "SNDK.US" || f[1] == "MU.US" {
			want = "240"
		}
		if f[5] != want {
			t.Errorf("%s fetched with --count %s, want %s", f[1], f[5], want)
		}
	}
	for sym, n := range seen {
		if n != 1 {
			t.Errorf("%s fetched %d times", sym, n)
		}
	}
	if len(seen) != 16 {
		t.Errorf("klines for %d symbols, want the whole pool + market (16)", len(seen))
	}
}

func TestCLIFailureDegradesPerField(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "longbridge")
	os.WriteFile(bin, []byte("#!/bin/bash\necho 'service unavailable' >&2; exit 1\n"), 0o755)
	c, _ := collector(t, bin)
	p, err := c.Collect(context.Background(), "Overnight", "OVERNIGHT")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Errors) != 5 || !strings.HasPrefix(p.Errors[0], "balance: ") {
		t.Fatalf("errors: %q", p.Errors)
	}
	for name, v := range map[string]json.RawMessage{"balance": p.Account.Balance, "quotes": p.Quotes, "session": p.Session} {
		if string(v) != "[]" {
			t.Errorf("%s = %s, want []", name, v)
		}
	}
	if len(p.Indicators) != 0 || p.CurrentSession != "Overnight" {
		t.Errorf("payload: %+v", p)
	}
	b, _ := p.JSON()
	if !strings.Contains(string(b), `"collect_errors"`) {
		t.Error("errors must reach the model")
	}
}

func TestKlineSymbolsFallbackWithoutQuotes(t *testing.T) {
	got := KlineSymbols([]string{"QQQ.US", "SPY.US"}, []string{"MU.US"}, []string{"AAPL.US", "MU.US", "QQQ.US", "GLD.US"}, nil)
	if want := []string{"QQQ.US", "SPY.US", "MU.US", "AAPL.US", "GLD.US"}; !slices.Equal(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestClockBlock(t *testing.T) {
	for _, c := range []struct {
		t    time.Time
		want Time
	}{
		{time.Date(2026, 11, 9, 7, 0, 12, 0, time.UTC), Time{"2026-11-09T07:00:12Z", "2026-11-09T02:00:12 EST", "2026-11-09T15:00:12 CST", 1, "Mon"}},
		{time.Date(2026, 7, 12, 15, 30, 0, 0, time.UTC), Time{"2026-07-12T15:30:00Z", "2026-07-12T11:30:00 EDT", "2026-07-12T23:30:00 CST", 7, "Sun"}},
	} {
		if got := ClockBlock(c.t); got != c.want {
			t.Errorf("%v: %+v", c.t, got)
		}
	}
}

// TestHardLesson_PoolNeverTruncated: collect.sh kept the top 5 by dollar
// volume, so in 2026-08 USO/UGL/AGQ got indicators in 0 of 514 rounds and the
// risk gate's volume_ratio floor never saw them. Every pool symbol must get a
// kline fetch, however large the pool and however thin its trading.
func TestHardLesson_PoolNeverTruncated(t *testing.T) {
	market := []string{"QQQ.US", "SPY.US"}
	playbook := []string{"MU.US"}
	var pool []string
	var rows []map[string]any
	for i := range 30 {
		sym := "S" + strconv.Itoa(i) + ".US"
		pool = append(pool, sym)
		rows = append(rows, map[string]any{"symbol": sym, "turnover": strconv.Itoa(i * 1000)}) // S0 trades nothing
	}
	pool = append(pool, "MU.US")
	rows = append(rows, map[string]any{"symbol": "MU.US", "turnover": "1"})

	got := KlineSymbols(market, playbook, pool, rows)
	if len(got) != len(market)+len(pool) {
		t.Fatalf("%d symbols, want %d (market + whole pool)", len(got), len(market)+len(pool))
	}
	for _, s := range append(slices.Clone(market), pool...) {
		if !slices.Contains(got, s) {
			t.Errorf("%s has no kline fetch", s)
		}
	}
	// Market regime and playbook first; the rest by turnover, the idle one last.
	if !slices.Equal(got[:3], []string{"QQQ.US", "SPY.US", "MU.US"}) || got[3] != "S29.US" || got[len(got)-1] != "S0.US" {
		t.Errorf("order: %v", got)
	}
}
