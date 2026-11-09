package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"flintrade/internal/creds"
	"flintrade/internal/dash"
	"flintrade/internal/db"
	"flintrade/internal/testutil"
)

func f(v float64) *float64 { return &v }

// fixture: a checkout with config + one prompt, and a snapshot as Load
// would return it for a live daemon holding one position.
func fixture(t *testing.T) *app {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "agent", "config")
	os.MkdirAll(cfg, 0o755)
	os.MkdirAll(filepath.Join(dir, "agent", "prompts"), 0o755)
	for _, n := range []string{"risk.toml", "trading.toml", "strategies.toml"} {
		os.WriteFile(filepath.Join(cfg, n), []byte(testutil.ConfigFile(t, n)), 0o644)
	}
	os.WriteFile(filepath.Join(dir, "agent", "prompts", "technical_intent.md"), []byte("You are Flintrade.\n"), 0o644)

	now := time.Now()
	loops := make([]dash.LoopState, len(dash.Loops))
	for i, n := range dash.Loops {
		loops[i] = dash.LoopState{Name: n, Core: i < 3, LastBeat: now.Add(-5 * time.Second), Age: 5 * time.Second, Cadence: 10 * time.Second}
	}
	a := newApp()
	a.root, a.rootErr = dir, nil
	a.snap = &dash.Snapshot{
		At: now, Root: dir, Live: true, Loops: loops,
		Risk:            &db.RiskState{Equity: f(9140.92), DayStartEquity: f(9164.68), OpenRisk: 9},
		InceptionDate:   "2026-06-17",
		InceptionEquity: 10000,
		Positions:       []db.Position{{ID: 1, Symbol: "GLD.US", Side: "long", Qty: 5, EntryPrice: 375.3, Stop: f(373.5), OpenedAt: "2026-11-09T09:12:00Z"}},
		Trades: []db.Trade{{ID: 7, TS: "2026-11-09T07:38:18Z", Symbol: "UGL.US", Action: "SELL", Qty: 54, FillPrice: 43.8,
			PnL: f(-22.68), Reason: db.Ptr("stop hit at 43.80")}},
		Intents:    []dash.Intent{{ID: 800, Source: "technical", Symbol: "GLD.US", Side: "long", Status: "approved", Reason: "breakout"}},
		CredPath:   filepath.Join(dir, "credentials.env"),
		CredValues: map[string]string{creds.AccessToken: "hk_abcdefghijklmnopWXYZ"},
	}
	return a
}

func TestOverview(t *testing.T) {
	a := fixture(t)
	tt := ui.NewTester(a.view, 1120, 780)
	for _, want := range []string{"LIVE", "daemon 运行中", "$9,140.92", "(−$23.76)", "−8.59%", "GLD", "risk_monitor", "SELL 54 UGL @ 43.80"} {
		if !tt.HasText(want) {
			t.Errorf("overview lacks %q; texts %q", want, tt.Texts())
		}
	}
	if got := a.trayTitle(); got != "Flintrade −23.76" {
		t.Errorf("tray title %q", got)
	}

	// A core loop goes quiet: the page and the menu bar both say so.
	a.snap.Loops[0].Stale = true
	tt.Frame()
	if !tt.HasText("daemon 已停止") || a.trayTitle() != "Flintrade 停" {
		t.Errorf("down not shown; tray %q", a.trayTitle())
	}
}

func TestTradesSelectionShowsReason(t *testing.T) {
	a := fixture(t)
	tt := ui.NewTester(a.view, 1120, 780)
	if err := tt.Click("成交"); err != nil {
		t.Fatal(err)
	}
	if a.page != "trades" || !tt.HasText("UGL.US") || !tt.HasText("−$22.68") {
		t.Fatalf("trades page: %q", tt.Texts())
	}
	if err := tt.Click("UGL.US"); err != nil {
		t.Fatal(err)
	}
	if !tt.HasText("stop hit at 43.80") {
		t.Errorf("detail pane: %q", tt.Texts())
	}
}

func TestStrategySaveValidatesThenWrites(t *testing.T) {
	a := fixture(t)
	a.page = "strategy"
	tt := ui.NewTester(a.view, 1120, 780)
	cur := a.current()
	if cur == nil || cur.file.Name != "strategies.toml" {
		t.Fatalf("first file %+v", cur)
	}
	orig := cur.disk

	cur.ed.Buffer.Set(orig + "\nnot toml at all ===\n")
	tt.Frame()
	if err := tt.Click("检查并保存"); err != nil {
		t.Fatal(err)
	}
	if a.strat.confirm || a.strat.err == "" {
		t.Fatalf("invalid TOML must be refused before the diff; err %q", a.strat.err)
	}
	if b, _ := os.ReadFile(cur.file.Path); string(b) != orig {
		t.Fatal("refused save touched the file")
	}

	edited := orig + "\n# note from the app\n"
	cur.ed.Buffer.Set(edited)
	tt.Frame()
	if err := tt.Click("检查并保存"); err != nil {
		t.Fatal(err)
	}
	if !a.strat.confirm {
		t.Fatalf("valid edit must open the diff; err %q", a.strat.err)
	}
	tt.Frame()
	if err := tt.Click("只保存"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cur.file.Path); string(b) != edited {
		t.Fatal("save did not write the edit")
	}
	if !strings.Contains(a.strat.msg, "已保存") {
		t.Errorf("msg %q", a.strat.msg)
	}
}

func TestRiskTomlIsReadOnly(t *testing.T) {
	a := fixture(t)
	a.page = "strategy"
	tt := ui.NewTester(a.view, 1120, 780)
	a.strat.sel = "risk.toml"
	tt.Frame()
	cur := a.current()
	if cur == nil || !cur.ed.ReadOnly || !tt.HasText("风控数值改动必须先过回测(CLAUDE.md)。Go 版回测落地前这里只读。") {
		t.Fatalf("risk.toml must be read-only; texts %q", tt.Texts())
	}
}

// Regression: inputs bound to a per-frame local lost every keystroke.
func TestCredInputsKeepTyping(t *testing.T) {
	a := fixture(t)
	a.page = "creds"
	tt := ui.NewTester(a.view, 1120, 780)
	if tt.HasText("hk_abcdefghijklmnopWXYZ") || !tt.HasText("••••••••WXYZ") {
		t.Fatalf("token must be masked: %q", tt.Texts())
	}
	// "Access Token" is also the label of the masked current value; click the input itself.
	in, ok := tt.AccessByRole(ui.RoleTextField, "Access Token")
	if !ok || !in.Password {
		t.Fatalf("token input missing or not a password field: %+v", in)
	}
	tt.ClickAt(in.Bounds.X+10, in.Bounds.Y+in.Bounds.H/2)
	tt.Type("new-token")
	tt.Frame()
	if a.cred.in[2] != "new-token" {
		t.Fatalf("typed value lost: %q", a.cred.in)
	}
}

func TestFormat(t *testing.T) {
	cases := map[string]string{
		money(9140.92): "$9,140.92", money(-23.76): "−$23.76", signed(12.3): "+$12.30", signed(0): "+$0.00",
		signedPlain(-23.76): "−23.76", money(1234567.006): "$1,234,567.01",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
}
