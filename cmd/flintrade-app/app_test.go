package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/egoist/mygo/i18n"
	"github.com/egoist/mygo/ui"

	"flintrade/cmd/flintrade-app/locales"
	"flintrade/internal/creds"
	"flintrade/internal/dash"
	"flintrade/internal/db"
	"flintrade/internal/marks"
	"flintrade/internal/reasoncode"
	"flintrade/internal/reflect"
	"flintrade/internal/testutil"
)

// testLocale is the language the tests render in. Most assertions are in
// Chinese, the language the app was written in.
var testLocale = "zh-Hans"

func TestMain(m *testing.M) {
	i18n.SetLocaleFunc(func() string { return testLocale })
	os.Exit(m.Run())
}

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
		Marks:           map[string]marks.Mark{"GLD.US": {Price: 377.3, At: db.FormatTime(now.Add(-8 * time.Second)), Session: "Intraday"}},
		RiskDay:         "2026-11-09",
		DayStartFloat:   &marks.DayStart{Day: "2026-11-09", Float: marks.Float{Sum: 4, Complete: true}},
		Trades: []db.Trade{
			{ID: 8, TS: "2026-11-09T09:12:00Z", Symbol: "GLD.US", Action: "BUY", Qty: 5, FillPrice: 375.3, PositionID: db.Ptr(int64(1)), IntentID: db.Ptr(int64(800)),
				Source: db.Ptr("technical"), Attribution: db.Ptr("strategy")},
			{ID: 7, TS: "2026-11-09T07:38:18Z", Symbol: "UGL.US", Action: "SELL", Qty: 54, FillPrice: 43.8,
				PnL: f(-22.68), Reason: db.Ptr("stop hit at 43.80"), Source: db.Ptr("technical"), Attribution: db.Ptr("test")},
			{ID: 6, TS: "2026-11-06T15:00:00Z", Symbol: "UGL.US", Action: "BUY", Qty: 54, FillPrice: 44.22, Source: db.Ptr("technical")},
		},
		Intents: []dash.Intent{
			{ID: 800, CreatedAt: "2026-11-09T09:10:00Z", Source: "technical", Symbol: "GLD.US", Side: "long", Status: "approved", Confidence: db.Ptr(58), Reason: "breakout"},
			{ID: 799, CreatedAt: "2026-11-09T08:00:00Z", Source: "event", Symbol: "UGL.US", Side: "short", Status: "rejected",
				Reason: "fade the gap", RejectReason: "距收盘 2min < 30.0min 黑窗,不开新仓",
				RejectCode: `{"key":"closeBlackout","args":{"minutesLeft":"2","blackout":"30.0"}}`},
		},
		CredPath:   filepath.Join(dir, "credentials.env"),
		CredValues: map[string]string{creds.AccessToken: "hk_abcdefghijklmnopWXYZ"},
	}
	return a
}

func TestOverview(t *testing.T) {
	a := fixture(t)
	tt := ui.NewTester(a.view, 1120, 780)
	// Net = realized 9140.92 + floating 5 × (377.30 − 375.30) = 9150.92.
	// Today = net − (day_start 9164.68 + float carried in 4) = −17.76.
	for _, want := range []string{"LIVE", "daemon 运行中", "$9,150.92 (−$849.08)", "已实现 $9,140.92 · 浮动 +$10.00",
		"−8.49%", "GLD", "377.30", "+$10.00 +0.53%", "打到止损 −$9.00", "8 秒前", "全部打到止损 −$9.00", "多 5 股 @ 375.30",
		"已平仓 0 笔 · 含浮动", "(−$17.76)", "risk_monitor", "SELL 54 UGL @ 43.80"} {
		if !tt.HasText(want) {
			t.Errorf("overview lacks %q; texts %q", want, tt.Texts())
		}
	}
	// The menu bar says only the name unless told otherwise; the menu says
	// what the figure is.
	if got := a.trayTitle(); got != "Flintrade" {
		t.Errorf("tray title %q", got)
	}
	if got := a.dayLine(); got != "今日盈亏 −$17.76 (−0.19%)" {
		t.Errorf("day line %q", got)
	}
	a.menuBar = "pnl"
	if got := a.trayTitle(); got != "Flintrade −$17.76" {
		t.Errorf("pnl tray title %q", got)
	}
	a.menuBar = "pct"
	if got := a.trayTitle(); got != "Flintrade −0.19%" {
		t.Errorf("pct tray title %q", got)
	}
	a.menuBar = "pnl"

	// No price for the position: realized only, said so, never a guessed figure.
	a.snap.Marks = nil
	tt.Frame()
	for _, want := range []string{"无现价", "浮动 —(无现价)", "已实现 $9,140.92 · 浮动 —(无现价)", "已平仓 0 笔 · 仅已实现", "(−$23.76)"} {
		if !tt.HasText(want) {
			t.Errorf("unpriced lacks %q; texts %q", want, tt.Texts())
		}
	}
	if got := a.trayTitle(); got != "Flintrade −$23.76" {
		t.Errorf("unpriced tray title %q", got)
	}
	if got := a.dayLine(); got != "今日盈亏 −$23.76 (−0.26%) · 仅已实现" {
		t.Errorf("unpriced day line %q", got)
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
	// Day headers by ET date with the day's closes; fills named by what they
	// do; the open position's fill says it is held, with its floating P&L.
	for _, want := range []string{"美东 11月9日周一", "平仓 1 笔 · 赚 0 亏 1 · −$22.68", "美东 11月6日周五", "无平仓",
		"开多", "平多", "GLD", "持仓中", "浮动 +$10.00", "技术面", "技术面 · 测试", "本地时间"} {
		if !tt.HasText(want) {
			t.Errorf("trades page lacks %q; texts %q", want, tt.Texts())
		}
	}
	if a.page != "trades" || tt.HasText("UGL.US") {
		t.Fatalf("trades page: %q", tt.Texts())
	}
	if err := tt.Click("−$22.68"); err != nil {
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
		money(9140.92): "$9,140.92", money(-23.76): "−$23.76", signed(12.3): "+$12.30", signed(0): "$0.00", money(-0.004): "$0.00", pct(0): "0.00%", pct(-0.001): "0.00%", pct(-8.594): "−8.59%",
		money(1234567.006): "$1,234,567.01",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
}

func TestDayRollNote(t *testing.T) {
	bj := time.FixedZone("CST", 8*3600)
	if got := dayRollNote(time.Date(2026, 11, 10, 13, 4, 0, 0, bj)); !strings.Contains(got, "13:00") {
		t.Errorf("winter: %q", got)
	}
	if got := dayRollNote(time.Date(2026, 7, 10, 13, 4, 0, 0, bj)); !strings.Contains(got, "12:00") {
		t.Errorf("summer: %q", got)
	}
}

// An ET trading day on a Beijing clock: 13:00 to 13:00 in US winter.
func TestTradeDaySpan(t *testing.T) {
	old := time.Local
	time.Local = time.FixedZone("CST", 8*3600)
	defer func() { time.Local = old }()
	items := tradeItems([]db.Trade{{TS: "2026-11-10T05:46:00Z"}, {TS: "2026-11-09T04:59:00Z"}})
	if len(items) != 4 || items[0].day == nil || items[2].day == nil {
		t.Fatalf("one header per ET day: %+v", items)
	}
	if got := daySpan(items[0].day.Date); got != "本地 11-10 13:00 – 11-11 13:00" {
		t.Errorf("span %q", got)
	}
	if got := dayTitle(items[2].day.Date); got != "美东 11月8日周日" {
		t.Errorf("title %q", got)
	}
}

func TestIntentsPage(t *testing.T) {
	a := fixture(t)
	tt := ui.NewTester(a.view, 1120, 780)
	if err := tt.Click("意图"); err != nil {
		t.Fatal(err)
	}
	// A rejected intent shows why in the list; the others their reason.
	for _, want := range []string{"美东 11月9日周一", "2 条 · 已批准 1 · 被拒 1", "做多", "做空", "技术面", "事件",
		"被拒", "距收盘 2min < 30.0min 黑窗,不开新仓", "breakout", "GLD", "本地时间"} {
		if !tt.HasText(want) {
			t.Errorf("intents page lacks %q; texts %q", want, tt.Texts())
		}
	}
	if tt.HasText("fade the gap") || tt.HasText("GLD.US") {
		t.Errorf("list shows the reject reason, not the hypothesis, and no .US: %q", tt.Texts())
	}
	if err := tt.Click("breakout"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(tt.Texts(), "\n")
	for _, want := range []string{"#800  技术面 做多 GLD · 已批准 · 置信度 58", "开多 5 @ 375.30"} {
		if !strings.Contains(joined, want) {
			t.Errorf("detail lacks %q; texts %q", want, tt.Texts())
		}
	}
}

// With the app in English, no page may show a Han character. Fixture data is
// English, except the daemon's text of intent 799's reason, which the app
// says from its code instead.
func TestEnglishHasNoChinese(t *testing.T) {
	testLocale = "en"
	defer func() { testLocale = "zh-Hans" }()

	for _, page := range []string{"overview", "trades", "intents", "memory", "reviews", "strategy", "creds", "restart", "credsSaved"} {
		a := fixture(t)
		a.snap.Orders = []db.Order{{ClientOrderID: "o1", Symbol: "GLD.US", Side: "buy", Qty: 3, FilledQty: 1, Status: "submitted"}}
		a.snap.Symbols = []dash.SymbolRecord{{Symbol: "UGL.US", N: 2, Wins: 1, Net: 3}}
		a.snap.Recall = &reflect.Recall{
			TimeAnchor:     reflect.TimeAnchor{SinceLastTrade: "3.2 hours", SinceLastDream: "never"},
			Lessons:        []reflect.Lesson{{Text: "wait for the retest", Confidence: 0.6, N: 4}},
			SelfAssessment: reflect.SelfAssessment{N: 2},
		}
		a.snap.Reviews = []dash.Review{{TradeID: 7, TS: "2026-11-09T07:38:18Z", Symbol: "UGL.US", ExitKind: "stop", Verdict: "ok", EntryGrade: "B", ExitGrade: "C", Lesson: "tight stop", Thesis: "gap fade"}}
		a.page = page
		switch page {
		case "reviews":
			a.page, a.memTab = "memory", 1
			a.reviews.sel = 0
		case "restart":
			a.page, a.confirmRestart = "overview", true
		case "credsSaved":
			a.page, a.cred.askRestart = "creds", true
		}
		tt := ui.NewTester(a.view, 1320, 880)
		tt.Frame()
		check := func(when string) {
			for _, text := range tt.Texts() {
				if strings.IndexFunc(text, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0 {
					t.Errorf("%s (%s) shows Chinese: %q", page, when, text)
				}
			}
		}
		check("list")
		if page == "trades" || page == "intents" {
			a.trades.sel, a.intents.sel = 1, 1
			tt.Frame()
			check("detail")
		}
	}

	// The tray, status line and day note say English too.
	a := fixture(t)
	for _, text := range []string{a.statusLine(), a.dayLine(), dayRollNote(time.Now()), a.trayTitle(), ago(90 * time.Second), every(30 * time.Second), gapName("3.2 hours"), dayTitle(time.Now())} {
		if strings.IndexFunc(text, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0 {
			t.Errorf("shows Chinese: %q", text)
		}
	}
	a.snap.Loops[0].Stale = true
	if got := a.trayTitle(); got != "Flintrade stopped" {
		t.Errorf("tray title %q", got)
	}
}

// Every reason the daemon can code has a message in each language, with the
// code's arguments and no others.
func TestReasonCatalog(t *testing.T) {
	for code, names := range reasoncode.Keys {
		key := reasonKey(code)
		for _, lang := range []string{"en", "zh-Hans"} {
			m, got, ok := locales.Catalog.Message(lang, key)
			if !ok || got != lang {
				t.Errorf("%s: no %s message %s", code, lang, key)
				continue
			}
			var args []string
			for _, a := range m.Args() {
				args = append(args, a.Name)
			}
			want := append([]string(nil), names...)
			slices.Sort(args)
			slices.Sort(want)
			if !slices.Equal(slices.Compact(args), want) {
				t.Errorf("%s %s: args %v, want %v", key, lang, args, want)
			}
		}
	}
}

// A coded reason is said in the app's language; one without a code (decided
// before codes existed) shows the daemon's text.
func TestRejectText(t *testing.T) {
	in := &dash.Intent{RejectReason: "距收盘 2min < 30.0min 黑窗,不开新仓",
		RejectCode: `{"key":"closeBlackout","args":{"minutesLeft":"2","blackout":"30.0"}}`}
	testLocale = "en"
	defer func() { testLocale = "zh-Hans" }()
	if got := rejectText(in); got != "2 min to the close, inside the 30.0 min no-entry window" {
		t.Errorf("en: %q", got)
	}
	testLocale = "zh-Hans"
	if got := rejectText(in); got != in.RejectReason {
		t.Errorf("zh: %q", got)
	}
	testLocale = "en"
	in.RejectCode = ""
	if got := rejectText(in); got != in.RejectReason {
		t.Errorf("no code: %q", got)
	}
	in.RejectCode = `{"key":"closeBlackout","args":{"minutesLeft":"2"}}`
	if got := rejectText(in); got != in.RejectReason {
		t.Errorf("bad args: %q", got)
	}
}

// Notifications and dash errors are said in the app's language.
func TestAlertAndErrText(t *testing.T) {
	testLocale = "en"
	defer func() { testLocale = "zh-Hans" }()
	if ti, b := alertText(dash.Alert{Key: "down", Loops: []string{"executor", "reconciler"}}); ti != "Flintrade daemon stopped" || b != "No heartbeat: executor, reconciler" {
		t.Errorf("down: %q %q", ti, b)
	}
	if _, b := alertText(dash.Alert{Key: "halt"}); b != "No reason recorded" {
		t.Errorf("halt: %q", b)
	}
	if got := errText(fmt.Errorf("save: %w", dash.ErrNoChanges)); got != "Nothing changed" {
		t.Errorf("wrapped sentinel: %q", got)
	}
	testLocale = "zh-Hans"
	if _, b := alertText(dash.Alert{Key: "down", Loops: []string{"executor", "reconciler"}}); b != "无心跳:executor、reconciler" {
		t.Errorf("zh down: %q", b)
	}
	if got := errText(&dash.CredsRejectedError{Err: errors.New("exit status 1"), Output: "401"}); got != "longbridge 拒绝了这组凭证: exit status 1 401" {
		t.Errorf("zh rejected: %q", got)
	}
}
