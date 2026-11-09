package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func liveLoader(t *testing.T) Loader {
	t.Helper()
	dir := "../../agent/config"
	if _, err := os.Stat(filepath.Join(dir, RiskFile)); err != nil {
		t.Skip("agent/config not present")
	}
	return Loader{Dir: dir}
}

// The live files must load strictly — this is the check that would have
// flagged the misplaced adopt_* keys.
func TestLiveConfigLoads(t *testing.T) {
	l := liveLoader(t)
	r, err := l.Risk()
	if err != nil {
		t.Fatal(err)
	}
	if r.PerTrade.MaxRiskPct != 2 || r.Portfolio.MaxConcurrentPositions != 3 {
		t.Fatalf("risk values: %+v", r.PerTrade)
	}
	if r.PerTrade.AdoptProtectiveStopPct != 3 || r.PerTrade.AdoptUntrackedDrift {
		t.Fatalf("adopt_* not read from [per_trade]: %+v", r.PerTrade)
	}
	if got := r.ClusterOf()["MU.US"]; got != "memory" {
		t.Fatalf("ClusterOf(MU.US) = %q", got)
	}
	if r.Clusters.MaxPerClusterPct != 60 {
		t.Fatalf("max_per_cluster_pct = %v", r.Clusters.MaxPerClusterPct)
	}
	if !r.Equity.Fixed() || r.Equity.SleeveUSD != 10000 {
		t.Fatalf("equity = %+v", r.Equity)
	}
	if r.Priority["user"] != 100 || r.Priority["technical"] != 10 {
		t.Fatalf("priority = %v", r.Priority)
	}

	tr, err := l.Trading()
	if err != nil {
		t.Fatal(err)
	}
	if tr.Models["trader"].Model == "" || tr.Cadence.ExecutorPollSec != 10 {
		t.Fatalf("trading = %+v", tr)
	}
	if len(tr.Universe.Symbols) < 10 || tr.Execution.EntryOrderTTLMin != 45 {
		t.Fatalf("universe/execution = %+v %+v", tr.Universe, tr.Execution)
	}

	st, err := l.Strategies()
	if err != nil {
		t.Fatal(err)
	}
	pb := st.PlaybookFor("SNDK.US")
	if pb == nil || pb.Name != "h4_confluence" || !pb.Binding || pb.Lookback != 12 {
		t.Fatalf("playbook = %+v", pb)
	}
	if st.PlaybookFor("AAPL.US") != nil {
		t.Fatal("AAPL.US has no playbook")
	}
}

// Regression for the 2026-11-09 finding: keys under the wrong header fail.
func TestMisplacedKeyRejected(t *testing.T) {
	src := `
[portfolio]
max_concurrent_positions = 3
max_gross_exposure_pct = 100
max_open_risk_pct = 4
max_per_symbol_pct = 40
[per_trade]
max_risk_pct = 2
[per_trade.confidence_scale]
full_at = 75
adopt_untracked_drift = true
[clusters]
a = ["X.US"]
max_per_cluster_pct = 60
[priority]
user = 100
event = 50
technical = 10
`
	_, err := ParseRisk([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "per_trade.confidence_scale.adopt_untracked_drift") {
		t.Fatalf("want unknown-key error naming the misplaced key, got %v", err)
	}
}

func TestEmptyModelRejected(t *testing.T) {
	l := liveLoader(t)
	b, _ := os.ReadFile(filepath.Join(l.Dir, TradingFile))
	bad := strings.Replace(string(b), `trader = { provider = "claude-cli", model = "claude-sonnet-5" }`,
		`trader = { provider = "claude-cli", model = "" }`, 1)
	if bad == string(b) {
		t.Fatal("fixture: trader model line not found")
	}
	_, err := ParseTrading([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "models.trader.model is empty") {
		t.Fatalf("want empty-model error, got %v", err)
	}
}

func TestRiskValidation(t *testing.T) {
	l := liveLoader(t)
	b, _ := os.ReadFile(filepath.Join(l.Dir, RiskFile))
	cases := map[string][2]string{
		"risk pct typo": {"max_risk_pct               = 2", "max_risk_pct               = 200"},
		"dup cluster":   {`oil       = ["USO.US"]`, `oil       = ["USO.US","GLD.US"]`},
		"floor > full":  {"floor_at    = 50", "floor_at    = 90"},
	}
	for name, c := range cases {
		bad := strings.Replace(string(b), c[0], c[1], 1)
		if bad == string(b) {
			t.Fatalf("%s: fixture line %q not found", name, c[0])
		}
		if _, err := ParseRisk([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestStrategiesOrderAndDuplicates(t *testing.T) {
	src := `
[playbooks.b]
symbols = ["X.US"]
lookback = 1
stop_pct = 1
t1_pct = 1
t2_pct = 2
[playbooks.a]
symbols = ["Y.US"]
lookback = 1
stop_pct = 1
t1_pct = 1
t2_pct = 2
`
	s, err := ParseStrategies([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if s.Playbooks[0].Name != "b" || s.Playbooks[1].Name != "a" {
		t.Fatalf("file order lost: %s, %s", s.Playbooks[0].Name, s.Playbooks[1].Name)
	}
	dup := strings.Replace(src, `["Y.US"]`, `["X.US"]`, 1)
	if _, err := ParseStrategies([]byte(dup)); err == nil {
		t.Fatal("symbol bound to two playbooks accepted")
	}
}

func TestMissingStrategiesIsEmpty(t *testing.T) {
	s, err := Loader{Dir: t.TempDir()}.Strategies()
	if err != nil || len(s.Playbooks) != 0 {
		t.Fatalf("missing strategies.toml: %+v %v", s, err)
	}
}

func TestDefaultsApplyWhenKeysAbsent(t *testing.T) {
	l := liveLoader(t)
	b, _ := os.ReadFile(filepath.Join(l.Dir, TradingFile))
	without := strings.Replace(string(b), "entry_order_ttl_min = 45", "", 1)
	tr, err := ParseTrading([]byte(without))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Execution.EntryOrderTTLMin != 45 {
		t.Fatalf("default ttl = %v", tr.Execution.EntryOrderTTLMin)
	}
}

func TestParseDispatch(t *testing.T) {
	if err := Parse("nope.toml", nil); err == nil {
		t.Fatal("unknown file accepted")
	}
	if err := Parse(StrategiesFile, []byte("[playbooks.x]\nsymbols=[\"A\"]\nbogus=1\n")); err == nil {
		t.Fatal("unknown playbook key accepted")
	}
}
