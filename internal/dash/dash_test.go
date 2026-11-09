package dash

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flintrade/internal/db"
	"flintrade/internal/testutil"
)

// root builds a throwaway checkout: config, one prompt, a ledger, and a
// credentials file. It returns the root and the writable ledger.
func root(t *testing.T, credLines string) (string, *db.DB) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "agent", "config")
	os.MkdirAll(cfg, 0o755)
	os.MkdirAll(filepath.Join(dir, "agent", "prompts"), 0o755)
	for _, n := range []string{"risk.toml", "trading.toml", "strategies.toml"} {
		os.WriteFile(filepath.Join(cfg, n), []byte(testutil.ConfigFile(t, n)), 0o644)
	}
	os.WriteFile(filepath.Join(dir, "agent", "prompts", "technical_intent.md"), []byte("prompt"), 0o644)
	cred := filepath.Join(dir, "credentials.env")
	os.WriteFile(cred, []byte(credLines), 0o600)
	t.Setenv("FLINTRADE_CREDENTIALS", cred)
	t.Setenv("FLINTRADE_DB", "")
	t.Setenv("FLINTRADE_DIR", dir)

	d, err := db.Open(filepath.Join(dir, "flintrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	testutil.SetEquity(t, d, 10000)
	return dir, d
}

func TestLoad(t *testing.T) {
	ctx := context.Background()
	dir, d := root(t, "FLINTRADE_DRY_RUN=0\n")
	m := d.MustAs(db.RoleMigrate)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := m.Exec(ctx, db.PermTrades, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	m.KVSet(ctx, "inception_date", "2026-06-01")
	m.KVSet(ctx, "inception_equity", "10000")
	exec(`INSERT INTO trades(ts,symbol,action,qty,fill_price,commission,pnl,attribution) VALUES
		('2026-07-01T15:00:00Z','A.US','SELL',1,1,0,50,'strategy'),
		('2026-07-02T15:00:00Z','A.US','SELL',1,1,0,-20,NULL),
		('2026-07-03T15:00:00Z','A.US','SELL',1,1,0,999,'manual'),
		('2026-05-01T15:00:00Z','A.US','SELL',1,1,0,7,'strategy')`)
	if _, _, err := m.SubmitIntent(ctx, db.IntentInput{Source: "technical", Symbol: "B.US", Side: "long"}); err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{"executor", "reconciler", "risk_monitor"} {
		if err := d.MustAs(db.RoleReader).Beat(ctx, l, nil); err != nil {
			t.Fatal(err)
		}
	}

	if got, err := FindRoot(); err != nil || got != dir {
		t.Fatalf("FindRoot = %q %v", got, err)
	}
	s := Load(ctx, dir, time.Now())
	if s.Err != nil {
		t.Fatal(s.Err)
	}
	if !s.Live {
		t.Error("FLINTRADE_DRY_RUN=0 must read as live")
	}
	// Strategy trades since inception only: 10000 → 10050 → 10030.
	if len(s.Curve) != 2 || s.Curve[1].Equity != 10030 {
		t.Errorf("curve %+v", s.Curve)
	}
	// Symbol records: strategy closes only (manual excluded), all time.
	if len(s.Symbols) != 1 || s.Symbols[0].N != 3 || s.Symbols[0].Net != 37 || len(s.Symbols[0].Series) != 3 {
		t.Errorf("symbols %+v", s.Symbols)
	}
	if len(s.WinRateSeries) != 3 || s.WinRateSeries[2] != 2.0/3 {
		t.Errorf("win rate series %v", s.WinRateSeries)
	}
	if len(s.DaySeries) != 0 {
		t.Errorf("no close today, day series %v", s.DaySeries)
	}
	if len(s.Pending()) != 1 || s.Pending()[0].Symbol != "B.US" {
		t.Errorf("pending %+v", s.Pending())
	}
	if s.DaemonDown() {
		t.Errorf("core loops just beat; stale: %v", s.StaleLoops())
	}
	if !contains(s.StaleLoops(), "loop_technical") {
		t.Errorf("a loop that never beat must be stale: %v", s.StaleLoops())
	}
	if s.Recall == nil || s.ReviewStats == nil {
		t.Error("recall / review stats missing")
	}
	if len(s.TokenProblems) == 0 || !s.TokenProblems[0].Fatal {
		t.Errorf("missing Longbridge keys must be a fatal problem: %+v", s.TokenProblems)
	}

	// An hour later every heartbeat is stale: the daemon is down.
	later := Load(ctx, dir, time.Now().Add(time.Hour))
	if !later.DaemonDown() {
		t.Error("an hour without beats must be down")
	}
	as := Alerts(s, later)
	if len(as) != 1 || as[0].Key != "down" {
		t.Errorf("alerts up→down: %+v", as)
	}
	if as := Alerts(later, s); len(as) != 1 || as[0].Key != "up" {
		t.Errorf("alerts down→up: %+v", as)
	}
	if as := Alerts(s, s); len(as) != 0 {
		t.Errorf("no change, no alert: %+v", as)
	}
	// First load reports what is already wrong, once.
	if as := Alerts(nil, s); len(as) != 1 || as[0].Key != "token" {
		t.Errorf("startup alerts: %+v", as)
	}

	reason := "daily loss limit"
	if err := d.MustAs(db.RoleRiskMonitor).SetHalt(ctx, true, &reason); err != nil {
		t.Fatal(err)
	}
	h := Load(ctx, dir, time.Now())
	if as := Alerts(s, h); len(as) != 1 || as[0].Key != "halt" || as[0].Body != reason {
		t.Errorf("halt alert: %+v", as)
	}
}

// Unset or unparseable FLINTRADE_DRY_RUN is dry: the app must never claim live.
func TestLoadDryByDefault(t *testing.T) {
	dir, _ := root(t, "# nothing\n")
	if s := Load(context.Background(), dir, time.Now()); s.Live {
		t.Error("no FLINTRADE_DRY_RUN must read as dry")
	}
}

func TestSave(t *testing.T) {
	dir, _ := root(t, "")
	files := ConfigFiles(dir)
	byName := map[string]File{}
	for _, f := range files {
		byName[f.Name] = f
	}
	risk, trading, prompt := byName["risk.toml"], byName["trading.toml"], byName["prompts/technical_intent.md"]
	if !risk.Gated || prompt.Path == "" {
		t.Fatalf("files %+v", files)
	}
	if err := Save(risk, testutil.ConfigFile(t, "risk.toml")); err == nil {
		t.Error("risk.toml must stay read-only until the Go backtest exists")
	}

	orig, _ := os.ReadFile(trading.Path)
	if err := Save(trading, string(orig)+"\nbogus_key = 1\n"); err == nil {
		t.Error("an unknown key must fail strict validation")
	}
	if b, _ := os.ReadFile(trading.Path); string(b) != string(orig) {
		t.Error("a rejected save must leave the file untouched")
	}
	edited := strings.Replace(string(orig), "technical_loop_sec = 1800", "technical_loop_sec = 900", 1)
	if edited == string(orig) {
		t.Fatal("fixture: cadence line not found")
	}
	os.Chmod(trading.Path, 0o640)
	if err := Save(trading, edited); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(trading.Path)
	st, _ := os.Stat(trading.Path)
	if string(b) != edited || st.Mode().Perm() != 0o640 {
		t.Errorf("saved %d bytes mode %v", len(b), st.Mode().Perm())
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(trading.Path), ".*")); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}

	if err := Save(prompt, "  \n"); err == nil {
		t.Error("an empty prompt must be rejected")
	}
}

func TestSaveCredsRejectsBadTokenOffline(t *testing.T) {
	dir, _ := root(t, "LONGBRIDGE_APP_KEY='k'\n")
	path := filepath.Join(dir, "credentials.env")
	before, _ := os.ReadFile(path)
	err := SaveCreds(context.Background(), path, map[string]string{"LONGBRIDGE_APP_KEY": "k"},
		map[string]string{"LONGBRIDGE_ACCESS_TOKEN": "not-a-jwt"}, time.Now())
	if err == nil {
		t.Fatal("a token that is not a JWT must be rejected before any network call")
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("a rejected save must not touch the file")
	}
	if err := SaveCreds(context.Background(), path, nil, map[string]string{}, time.Now()); err == nil {
		t.Error("no edits must be an error")
	}
}

func TestMask(t *testing.T) {
	if got := Mask("abcdefghijklmnop"); got != "••••••••mnop" || strings.Contains(got, "abcd") {
		t.Errorf("Mask = %q", got)
	}
	if Mask("") != "(未设置)" || Mask("short") != "••••••••" {
		t.Error("short / empty masks")
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
