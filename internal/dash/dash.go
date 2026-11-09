// Package dash is what Flintrade.app shows: one read-only snapshot of the ledger,
// the loops' health, the credentials' state, plus the few writes the app is
// allowed (config files, the credentials file, restarting the daemon). It
// never writes flintrade.db and never places an order.
package dash

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"flintrade/internal/broker"
	"flintrade/internal/config"
	"flintrade/internal/creds"
	"flintrade/internal/db"
	"flintrade/internal/paths"
	"flintrade/internal/postmortem"
	"flintrade/internal/reflect"
	"flintrade/internal/session"
)

// Label is the daemon's launchd job.
const Label = "com.flintrade.daemon"

// TokenWarn is how early the app starts warning about token expiry.
const TokenWarn = 14 * 24 * time.Hour

// PlistPath is the installed launchd job.
func PlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist")
}

var workDirRE = regexp.MustCompile(`<key>WorkingDirectory</key>\s*<string>([^<]+)</string>`)

// FindRoot locates the repo the daemon runs from: $FLINTRADE_DIR, else the
// launchd job's WorkingDirectory. An app opened from Finder has neither a
// useful working directory nor FLINTRADE_DIR, and it must watch exactly the
// checkout the daemon uses.
func FindRoot() (string, error) {
	if os.Getenv("FLINTRADE_DIR") != "" {
		return paths.Root()
	}
	b, err := os.ReadFile(PlistPath())
	if err != nil {
		return "", fmt.Errorf("no FLINTRADE_DIR and no launchd job at %s: %w", PlistPath(), err)
	}
	m := workDirRE.FindSubmatch(b)
	if m == nil {
		return "", fmt.Errorf("%s has no WorkingDirectory", PlistPath())
	}
	dir := string(m[1])
	if _, err := os.Stat(filepath.Join(dir, "agent", "config", "risk.toml")); err != nil {
		return "", fmt.Errorf("WorkingDirectory %s is not a Flintrade checkout", dir)
	}
	return dir, nil
}

// Loops are the daemon's loops in display order. Core loops trade or guard;
// if one of them is stale the daemon counts as down.
var Loops = []string{"executor", "reconciler", "risk_monitor", "loop_technical", "loop_event", "news_collector", "reflect", "invariants"}

var core = map[string]bool{"executor": true, "reconciler": true, "risk_monitor": true}

// LoopState is one loop's heartbeat.
type LoopState struct {
	Name     string
	Core     bool
	Cadence  time.Duration
	LastBeat time.Time // zero when the loop never beat
	Age      time.Duration
	Stale    bool // missing, or silent past max(heartbeat_stale_sec, 2 cadences)
	PID      int
	Note     string
}

// Point is the equity after one closed strategy trade.
type Point struct {
	T      time.Time
	Equity float64
}

// Intent is an intent as the app lists it.
type Intent struct {
	ID           int64
	CreatedAt    string
	Source       string
	Symbol       string
	Side         string
	Status       string
	Confidence   *int
	Reason       string
	RejectReason string
}

// Review is a postmortem of one closed trade.
type Review struct {
	TradeID     int64
	TS          string
	Symbol      string
	PnL         *float64
	HoldMinutes *int
	ExitKind    string
	Confidence  *int
	Verdict     string
	EntryGrade  string
	ExitGrade   string
	Lesson      string
	Thesis      string
}

// SymbolRecord is Flintrade's closed-trade record on one symbol.
type SymbolRecord struct {
	Symbol string
	N      int
	Wins   int
	Net    float64
	Series []float64 // cumulative net PnL after each close, oldest first
	LastTS string
}

// Snapshot is everything the app shows at one moment.
type Snapshot struct {
	At   time.Time
	Root string
	Err  error // the ledger could not be read; the rest may be partial

	Loops []LoopState
	Live  bool // the credentials file says FLINTRADE_DRY_RUN=0 (takes effect on the next daemon start)

	Risk            *db.RiskState
	InceptionDate   string
	InceptionEquity float64
	Curve           []Point
	Positions       []db.Position
	Orders          []db.Order // working orders
	Trades          []db.Trade // newest first
	Intents         []Intent   // newest first
	Symbols         []SymbolRecord // most recently traded first
	DaySeries       []float64      // cumulative realized PnL of today's (ET) closes
	WinRateSeries   []float64      // win rate over the trailing 20 closes, after each close
	Recall          *reflect.Recall
	Reviews         []Review
	ReviewStats     *postmortem.StatsResult

	CredPath      string
	CredValues    map[string]string // never rendered raw; the app masks secrets
	Token         creds.TokenInfo
	TokenProblems []creds.Problem
}

// DaemonDown: a core loop is missing or stale.
func (s *Snapshot) DaemonDown() bool {
	for _, l := range s.Loops {
		if l.Core && l.Stale {
			return true
		}
	}
	return len(s.Loops) == 0
}

// StaleLoops names every stale loop.
func (s *Snapshot) StaleLoops() []string {
	var out []string
	for _, l := range s.Loops {
		if l.Stale {
			out = append(out, l.Name)
		}
	}
	return out
}

// Halted reports the risk monitor's halt flag.
func (s *Snapshot) Halted() bool { return s.Risk != nil && s.Risk.Halt }

// DayPnL is equity minus the day's starting equity.
func (s *Snapshot) DayPnL() *float64 {
	if s.Risk == nil || s.Risk.Equity == nil || s.Risk.DayStartEquity == nil {
		return nil
	}
	v := *s.Risk.Equity - *s.Risk.DayStartEquity
	return &v
}

// Return is equity minus the inception sleeve.
func (s *Snapshot) Return() *float64 {
	if s.Risk == nil || s.Risk.Equity == nil || s.InceptionEquity == 0 {
		return nil
	}
	v := *s.Risk.Equity - s.InceptionEquity
	return &v
}

// Pending lists intents waiting for the executor.
func (s *Snapshot) Pending() []Intent {
	var out []Intent
	for _, in := range s.Intents {
		if in.Status == "pending" || in.Status == "approved" {
			out = append(out, in)
		}
	}
	return out
}

// Load reads one snapshot. It opens flintrade.db read-only for the duration of
// the call, so the app never holds a connection between refreshes.
func Load(ctx context.Context, root string, now time.Time) *Snapshot {
	s := &Snapshot{At: now, Root: root, InceptionEquity: 10000}
	loadCreds(s, root, now)

	cad, stale := cadences(root)
	d, err := db.OpenReadOnly(paths.DBPath(root))
	if err != nil {
		s.Err = err
		return s
	}
	defer d.Close()
	r := d.MustAs(db.RoleReader)

	var errs []error
	keep := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	beats, err := r.Heartbeats(ctx)
	keep(err)
	s.Loops = loopStates(beats, cad, stale, now)

	s.Risk, err = r.GetRisk(ctx)
	keep(err)
	if v, ok, _ := r.KVGet(ctx, "inception_equity"); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			s.InceptionEquity = f
		}
	}
	s.InceptionDate, _, _ = r.KVGet(ctx, "inception_date")
	s.Curve, err = curve(ctx, r, s.InceptionDate, s.InceptionEquity)
	keep(err)
	s.Symbols, s.DaySeries, s.WinRateSeries, err = records(ctx, r, now)
	keep(err)
	s.Positions, err = r.OpenPositions(ctx)
	keep(err)
	s.Orders, err = r.OpenOrders(ctx)
	keep(err)
	s.Trades, err = r.RecentTrades(ctx, 300)
	keep(err)
	s.Intents, err = intents(ctx, r, 300)
	keep(err)
	s.Recall, err = reflect.GetRecall(ctx, r, now)
	keep(err)
	s.Reviews, err = reviews(ctx, r, 200)
	keep(err)
	s.ReviewStats, err = postmortem.Stats(ctx, r)
	keep(err)
	s.Err = errors.Join(errs...)
	return s
}

func loadCreds(s *Snapshot, root string, now time.Time) {
	s.CredPath = creds.DefaultPath(root)
	vals, err := creds.Load(s.CredPath)
	if err != nil {
		s.TokenProblems = []creds.Problem{{Key: "file", Message: err.Error(), Fatal: true}}
		return
	}
	s.CredValues = vals
	s.Live = !broker.DryRunValue(vals[creds.DryRun])
	s.Token, s.TokenProblems = creds.Check(vals, now, TokenWarn)
}

func cadences(root string) (map[string]time.Duration, time.Duration) {
	sec := func(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }
	stale := 120 * time.Second
	cad := map[string]time.Duration{}
	t, err := config.Loader{Dir: paths.ConfigDir(root)}.Trading()
	if err != nil {
		return cad, stale
	}
	c := t.Cadence
	if c.HeartbeatStaleSec > 0 {
		stale = sec(c.HeartbeatStaleSec)
	}
	cad["executor"] = sec(c.ExecutorPollSec)
	cad["reconciler"] = sec(c.ReconcilerPollSec)
	cad["risk_monitor"] = sec(c.RiskMonitorSec)
	cad["loop_technical"] = sec(c.TechnicalLoopSec)
	cad["loop_event"] = sec(c.EventLoopSec)
	cad["news_collector"] = sec(c.NewsPollSec)
	cad["reflect"] = sec(c.ReflectCheckSec)
	cad["invariants"] = sec(c.InvariantsCheckSec)
	return cad, stale
}

func loopStates(beats []db.Heartbeat, cad map[string]time.Duration, stale time.Duration, now time.Time) []LoopState {
	by := map[string]db.Heartbeat{}
	for _, b := range beats {
		by[b.Process] = b
	}
	out := make([]LoopState, 0, len(Loops))
	for _, name := range Loops {
		l := LoopState{Name: name, Core: core[name], Cadence: cad[name], Stale: true}
		if b, ok := by[name]; ok {
			if t, err := db.ParseTime(b.LastBeat); err == nil {
				l.LastBeat = t
				l.Age = now.Sub(t)
				l.Stale = l.Age > max(stale, 2*l.Cadence)
			}
			if b.PID != nil {
				l.PID = *b.PID
			}
			if b.Note != nil {
				l.Note = *b.Note
			}
		}
		out = append(out, l)
	}
	return out
}

func curve(ctx context.Context, r *db.Store, since string, start float64) ([]Point, error) {
	if since == "" {
		return nil, nil
	}
	rows, err := r.Query(ctx, `SELECT ts, pnl FROM trades WHERE pnl IS NOT NULL AND ts >= ? AND `+
		db.StrategyRecord("")+` ORDER BY ts`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	eq := start
	var out []Point
	for rows.Next() {
		var ts string
		var pnl float64
		if err := rows.Scan(&ts, &pnl); err != nil {
			return out, err
		}
		t, err := db.ParseTime(ts)
		if err != nil {
			continue
		}
		eq += pnl
		out = append(out, Point{t, eq})
	}
	return out, rows.Err()
}

// records walks every strategy close once, oldest first.
func records(ctx context.Context, r *db.Store, now time.Time) ([]SymbolRecord, []float64, []float64, error) {
	rows, err := r.Query(ctx, `SELECT ts, symbol, pnl FROM trades WHERE pnl IS NOT NULL AND `+
		db.StrategyRecord("")+` ORDER BY ts, id`)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	today := now.In(session.ET).Format(time.DateOnly)
	by := map[string]*SymbolRecord{}
	var day, rate []float64
	var window []bool
	dayNet := 0.0
	for rows.Next() {
		var ts, sym string
		var pnl float64
		if err := rows.Scan(&ts, &sym, &pnl); err != nil {
			return nil, nil, nil, err
		}
		rec := by[sym]
		if rec == nil {
			rec = &SymbolRecord{Symbol: sym}
			by[sym] = rec
		}
		rec.N++
		if pnl > 0 {
			rec.Wins++
		}
		rec.Net += pnl
		rec.Series = append(rec.Series, rec.Net)
		rec.LastTS = ts
		if t, err := db.ParseTime(ts); err == nil && t.In(session.ET).Format(time.DateOnly) == today {
			dayNet += pnl
			day = append(day, dayNet)
		}
		window = append(window, pnl > 0)
		if len(window) > 20 {
			window = window[1:]
		}
		wins := 0
		for _, w := range window {
			if w {
				wins++
			}
		}
		rate = append(rate, float64(wins)/float64(len(window)))
	}
	out := make([]SymbolRecord, 0, len(by))
	for _, rec := range by {
		out = append(out, *rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastTS > out[j].LastTS })
	return out, day, rate, rows.Err()
}

func intents(ctx context.Context, r *db.Store, limit int) ([]Intent, error) {
	rows, err := r.Query(ctx, `SELECT id, created_at, source, symbol, side, status, confidence,
		COALESCE(reason,''), COALESCE(reject_reason,'') FROM intents ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Intent
	for rows.Next() {
		var in Intent
		if err := rows.Scan(&in.ID, &in.CreatedAt, &in.Source, &in.Symbol, &in.Side, &in.Status,
			&in.Confidence, &in.Reason, &in.RejectReason); err != nil {
			return out, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

func reviews(ctx context.Context, r *db.Store, limit int) ([]Review, error) {
	rows, err := r.Query(ctx, `SELECT rv.trade_id, COALESCE(t.ts,''), COALESCE(rv.symbol,''), rv.pnl, rv.hold_minutes,
		COALESCE(rv.exit_kind,''), rv.confidence, COALESCE(rv.thesis_verdict,''), COALESCE(rv.entry_grade,''),
		COALESCE(rv.exit_grade,''), COALESCE(rv.lesson,''), COALESCE(rv.thesis,'')
		FROM trade_reviews rv LEFT JOIN trades t ON t.id = rv.trade_id ORDER BY rv.trade_id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Review
	for rows.Next() {
		var v Review
		if err := rows.Scan(&v.TradeID, &v.TS, &v.Symbol, &v.PnL, &v.HoldMinutes, &v.ExitKind, &v.Confidence,
			&v.Verdict, &v.EntryGrade, &v.ExitGrade, &v.Lesson, &v.Thesis); err != nil {
			return out, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Alert is one notification the app raises on a state change.
type Alert struct {
	Key   string // dedup key: down | up | halt | token
	Title string
	Body  string
}

// Alerts compares two snapshots and returns what changed for the worse (or
// recovered). prev is nil on the first load: anything already wrong is
// reported once at startup.
func Alerts(prev, cur *Snapshot) []Alert {
	if cur.Err != nil && len(cur.Loops) == 0 {
		return nil // unreadable ledger: shown in the window, not a daemon verdict
	}
	var out []Alert
	wasDown := prev != nil && prev.DaemonDown()
	if cur.DaemonDown() && !wasDown {
		out = append(out, Alert{"down", "Flintrade daemon 停了", "无心跳:" + strings.Join(cur.StaleLoops(), "、")})
	}
	if !cur.DaemonDown() && wasDown {
		out = append(out, Alert{"up", "Flintrade daemon 恢复", "核心 loop 心跳正常"})
	}
	if cur.Halted() && (prev == nil || !prev.Halted()) {
		reason := "原因未记录"
		if cur.Risk.HaltReason != nil {
			reason = *cur.Risk.HaltReason
		}
		out = append(out, Alert{"halt", "Flintrade 已暂停开新仓", reason})
	}
	if msg := tokenIssue(cur); msg != "" && (prev == nil || tokenIssue(prev) == "") {
		out = append(out, Alert{"token", "Longbridge 凭证需要处理", msg})
	}
	return out
}

func tokenIssue(s *Snapshot) string {
	if len(s.TokenProblems) == 0 {
		return ""
	}
	return s.TokenProblems[0].Message
}
