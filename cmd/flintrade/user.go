package main

// Manual control + "what's going on", ported from agent/user_cli.py.
//
// Manual trades never bypass risk: this only queues intents (source=user,
// highest priority); the executor's risk gate sizes and decides them, the
// same gate automatic intents pass. Status reads with the reader role.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"flintrade/internal/config"
	"flintrade/internal/creds"
	"flintrade/internal/db"
	"flintrade/internal/lb"
	"flintrade/internal/loop"
	"flintrade/internal/paths"
	"flintrade/internal/quotes"
	"flintrade/internal/riskmonitor"
	"flintrade/internal/session"
)

type env struct {
	root string
	cfg  config.Loader
	db   *db.DB
}

func openEnv(readOnly bool) (*env, error) {
	root, err := paths.Root()
	if err != nil {
		return nil, err
	}
	if vals, err := creds.Load(creds.DefaultPath(root)); err == nil {
		creds.Apply(vals)
	}
	var d *db.DB
	if readOnly {
		d, err = db.OpenReadOnly(paths.DBPath(root))
	} else {
		d, err = db.Open(paths.DBPath(root))
	}
	if err != nil {
		return nil, err
	}
	return &env{root: root, cfg: config.Loader{Dir: paths.ConfigDir(root)}, db: d}, nil
}

// touchSeen records this interaction and returns the previous one; reflect's
// recall reads last_user_seen to give the agent a sense of elapsed time.
func touchSeen(ctx context.Context, e *env) string {
	s := e.db.MustAs(db.RoleUser)
	prev, _, _ := s.KVGet(ctx, "last_user_seen")
	_ = s.KVSet(ctx, "last_user_seen", db.Now())
	return prev
}

func fmtGap(prev string) string {
	if prev == "" {
		return "首次互动"
	}
	t, err := db.ParseTime(prev)
	if err != nil {
		return "未知"
	}
	secs := time.Since(t).Seconds()
	switch {
	case secs < 90:
		return fmt.Sprintf("%d秒前", int(secs))
	case secs < 5400:
		return fmt.Sprintf("%d分钟前", int(secs/60))
	case secs < 172800:
		return fmt.Sprintf("%.1f小时前", secs/3600)
	}
	return fmt.Sprintf("%.1f天前", secs/86400)
}

// normalizeSymbol: NVDA / nvda / NVDA.US → NVDA.US, which must be tradeable.
func normalizeSymbol(t *config.Trading, raw string) (string, error) {
	s := strings.ToUpper(strings.TrimSpace(raw))
	if s == "" {
		return "", errors.New("空标的")
	}
	if !strings.Contains(s, ".") {
		s += ".US"
	}
	if !slices.Contains(t.Universe.Symbols, s) {
		var bare []string
		for _, x := range t.Universe.Symbols {
			bare = append(bare, strings.TrimSuffix(x, ".US"))
		}
		return "", fmt.Errorf("%q 不在可交易 universe(规范化为 %q)。\n  可交易标的: %s", raw, s, strings.Join(bare, ", "))
	}
	return s, nil
}

// optFloat is a flag that may be absent.
type optFloat struct{ v *float64 }

func (o *optFloat) String() string {
	if o.v == nil {
		return ""
	}
	return fmt.Sprint(*o.v)
}
func (o *optFloat) Set(s string) error {
	var f float64
	if _, err := fmt.Sscan(s, &f); err != nil {
		return err
	}
	o.v = &f
	return nil
}

// parseInterspersed lets flags follow the positional symbol (`buy NVDA --stop 1`).
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func cmdOpen(ctx context.Context, side string, args []string) int {
	fs := flag.NewFlagSet(side, flag.ContinueOnError)
	var stop, target, entry optFloat
	fs.Var(&stop, "stop", "止损位(必填;风险定额分母)")
	fs.Var(&target, "target", "目标位")
	fs.Var(&entry, "entry", "期望入场价(限价参考);省略则取实时报价")
	confidence := fs.Int("confidence", -1, "信心 0-100")
	reason := fs.String("reason", "", "备注 / 理由")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		fmt.Fprintf(os.Stderr, "用法: flintrade %s SYMBOL --stop P [--target T] [--entry E] [--confidence N] [--reason R]\n", side)
		return 2
	}
	verb := map[string]string{"long": "buy", "short": "short"}[side]
	if stop.v == nil {
		fmt.Fprintf(os.Stderr, "错误:%s 需要 --stop(它是 Risk Gate 风险定额的分母)。\n  例: flintrade %s %s --stop <价位> [--target T] [--entry E]\n", side, verb, pos[0])
		return 2
	}
	e, err := openEnv(false)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer e.db.Close()
	touchSeen(ctx, e)
	t, err := e.cfg.Trading()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	sym, err := normalizeSymbol(t, pos[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误:%v\n", err)
		return 2
	}
	hint := entry.v
	if hint == nil {
		// Session-aware price: outside RTH the top-level last is stale.
		runner := lb.New()
		sess := session.New(session.LBFetcher(runner)).Current(ctx)
		p, ok := quotes.Source{LB: runner}.LastPrices(ctx, []string{sym}, sess)[sym]
		if !ok {
			fmt.Fprintf(os.Stderr, "错误:取不到 %s 实时报价,无法定 entry。\n  请显式给 --entry <价位>(Risk Gate 需要 entry 做风险定额)。\n", sym)
			return 2
		}
		hint = &p
		fmt.Printf("  (--entry 省略,用实时报价 %.2f 作 entry_hint,时段 %s)\n", p, sess)
	}
	r, err := e.cfg.Risk()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	prio := r.Priority["user"]
	in := db.IntentInput{Source: "user", Priority: prio, Symbol: sym, Side: side, EntryHint: hint, Stop: stop.v, Target: target.v}
	if *confidence >= 0 {
		in.Confidence = confidence
	}
	if *reason != "" {
		in.Reason = reason
	}
	id, ok, err := e.db.MustAs(db.RoleUser).SubmitIntent(ctx, in)
	if err != nil || !ok {
		fmt.Fprintf(os.Stderr, "意图未创建: %v\n", err)
		return 1
	}
	line := fmt.Sprintf("已提交 %s 意图 #%d  %s  stop=%v", side, id, sym, *stop.v)
	if target.v != nil {
		line += fmt.Sprintf(" target=%v", *target.v)
	}
	if entry.v != nil {
		line += fmt.Sprintf(" entry=%v", *entry.v)
	}
	fmt.Printf("%s  (source=user, priority=%d = 最高)\n", line, prio)
	fmt.Println("注意:意图进入待裁决队列。Executor 的 Risk Gate 会定额并批准/拒绝 —— 手动单和自动单走**同一道风控闸门**。")
	return 0
}

func cmdClose(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("close", flag.ContinueOnError)
	reason := fs.String("reason", "", "备注 / 理由")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "用法: flintrade close SYMBOL [--reason R]")
		return 2
	}
	e, err := openEnv(false)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer e.db.Close()
	touchSeen(ctx, e)
	t, err := e.cfg.Trading()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	sym, err := normalizeSymbol(t, pos[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误:%v\n", err)
		return 2
	}
	if p, _ := e.db.MustAs(db.RoleReader).PositionFor(ctx, sym); p == nil {
		fmt.Fprintf(os.Stderr, "告警:当前无 %s 的未平持仓。仍提交 close 意图 —— 若确无持仓,Risk Gate 会拒绝。\n", sym)
	}
	r, err := e.cfg.Risk()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	in := db.IntentInput{Source: "user", Priority: r.Priority["user"], Symbol: sym, Side: "close"}
	if *reason != "" {
		in.Reason = reason
	}
	id, ok, err := e.db.MustAs(db.RoleUser).SubmitIntent(ctx, in)
	if err != nil || !ok {
		fmt.Fprintf(os.Stderr, "意图未创建: %v\n", err)
		return 1
	}
	fmt.Printf("已提交 close 意图 #%d  %s  (source=user, priority=%d)\n", id, sym, in.Priority)
	fmt.Println("注意:Executor 会执行平仓;Risk Gate 对退出永远放行。")
	return 0
}

func money(v *float64, sign bool) string {
	if v == nil {
		return "n/a"
	}
	s := commas(fmt.Sprintf("%.2f", abs(*v)))
	switch {
	case *v < 0:
		return "$-" + s
	case sign:
		return "$+" + s
	}
	return "$" + s
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func commas(s string) string {
	intPart, frac, _ := strings.Cut(s, ".")
	var b strings.Builder
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String() + "." + frac
}

// expectedProcs should beat continuously (agent/supervisor.py EXPECTED).
var expectedProcs = []string{"executor", "reconciler", "risk_monitor", "loop_technical"}

func cmdStatus(ctx context.Context) int {
	e, err := openEnv(false)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer e.db.Close()
	prev := touchSeen(ctx, e)
	r := e.db.MustAs(db.RoleReader)
	risk, _ := r.GetRisk(ctx)
	lastDream, _, _ := r.KVGet(ctx, "last_dream_date")
	incDate, _, _ := r.KVGet(ctx, "inception_date")
	incEq, _, _ := r.KVGet(ctx, "inception_equity")
	positions, _ := r.OpenPositions(ctx)
	trades, _ := r.RecentTrades(ctx, 8)
	beats, _ := r.Heartbeats(ctx)

	line := strings.Repeat("─", 64)
	fmt.Println(line)
	fmt.Println("  FLINTRADE · 现在啥情况")
	anchor := "   尚未做梦"
	if lastDream != "" {
		anchor = "   上次做梦 " + lastDream
	}
	fmt.Printf("  时空锚点  距上次互动 %s%s\n", fmtGap(prev), anchor)
	fmt.Println(line)

	// A loop is stale past heartbeat_stale_sec or two of its own cadences,
	// whichever is longer: the 30-min technical loop is not "down" at 121 s.
	stale := 120.0
	cadence := map[string]float64{}
	if t, err := e.cfg.Trading(); err == nil {
		if t.Cadence.HeartbeatStaleSec > 0 {
			stale = t.Cadence.HeartbeatStaleSec
		}
		c := t.Cadence
		cadence = map[string]float64{"executor": c.ExecutorPollSec, "reconciler": c.ReconcilerPollSec,
			"risk_monitor": c.RiskMonitorSec, "loop_technical": c.TechnicalLoopSec}
	}
	fmt.Println("  PROCESSES")
	byName := map[string]db.Heartbeat{}
	for _, b := range beats {
		byName[b.Process] = b
	}
	for _, p := range expectedProcs {
		b, ok := byName[p]
		if !ok {
			fmt.Printf("    ❌ %-14s MISSING\n", p)
			continue
		}
		ts, err := db.ParseTime(b.LastBeat)
		age := time.Since(ts)
		mark, st := "✅", "up"
		if err != nil || age.Seconds() > max(stale, 2*cadence[p]) {
			mark, st = "⚠️ ", "STALE"
		}
		note := ""
		if b.Note != nil && *b.Note != "" {
			note = "  (" + *b.Note + ")"
		}
		fmt.Printf("    %s %-14s %-6s %s ago%s\n", mark, p, st, age.Round(time.Second), note)
	}

	if risk == nil {
		fmt.Println("  RISK    (risk_state 行缺失)")
	} else {
		flagS := "✅ ok"
		if risk.Halt {
			flagS = "🛑 HALTED"
		}
		var dayPnL *float64
		if risk.Equity != nil && risk.DayStartEquity != nil {
			dayPnL = db.Ptr(*risk.Equity - *risk.DayStartEquity)
		}
		fmt.Printf("  RISK    %s   equity=%s   day_pnl=%s   open_risk=%s\n", flagS, money(risk.Equity, false), money(dayPnL, true), money(&risk.OpenRisk, false))
		if risk.Halt && risk.HaltReason != nil {
			fmt.Printf("          reason: %s\n", *risk.HaltReason)
		}
		var base float64
		if _, err := fmt.Sscan(incEq, &base); err == nil && risk.Equity != nil && base != 0 {
			ret := *risk.Equity - base
			d := incDate
			if d == "" {
				d = "?"
			}
			fmt.Printf("  收益    自 %s 起  %s (%+.2f%%)   本金 sleeve %s\n", d, money(&ret, true), ret/base*100, money(&base, false))
		}
	}

	fmt.Printf("  POSITIONS (%d)\n", len(positions))
	if len(positions) == 0 {
		fmt.Println("    (无持仓)")
	} else {
		fmt.Printf("    %-10s %-5s %5s %9s %9s %9s %9s\n", "symbol", "side", "qty", "entry", "stop", "target", "risk")
		f2 := func(v *float64, dollar bool) string {
			if v == nil {
				return "n/a"
			}
			if dollar {
				return fmt.Sprintf("$%.2f", *v)
			}
			return fmt.Sprintf("%.2f", *v)
		}
		for _, p := range positions {
			fmt.Printf("    %-10s %-5s %5d %9.2f %9s %9s %9s\n", p.Symbol, p.Side, p.Qty, p.EntryPrice, f2(p.Stop, false), f2(p.Target, false), f2(p.RiskAmt, true))
		}
	}

	rows, err := r.Query(ctx, `SELECT id, source, priority, side, symbol, stop, reason FROM intents WHERE status='pending' ORDER BY priority DESC, created_at`)
	var pending []string
	if err == nil {
		for rows.Next() {
			var id int64
			var src, side, sym string
			var prio int
			var stop *float64
			var reason *string
			if rows.Scan(&id, &src, &prio, &side, &sym, &stop, &reason) == nil {
				s, rs := "", ""
				if stop != nil {
					s = fmt.Sprintf("%.2f", *stop)
				}
				if reason != nil {
					rs = truncRunes(*reason, 28)
				}
				pending = append(pending, fmt.Sprintf("    %5d %-12s %4d %-6s %-10s %8s %s", id, src, prio, side, sym, s, rs))
			}
		}
		rows.Close()
	}
	fmt.Printf("  PENDING INTENTS (%d)\n", len(pending))
	if len(pending) == 0 {
		fmt.Println("    (无待裁决意图)")
	} else {
		fmt.Printf("    %5s %-12s %4s %-6s %-10s %8s reason\n", "#id", "source", "prio", "side", "symbol", "stop")
		for _, p := range pending {
			fmt.Println(p)
		}
	}

	fmt.Println("  RECENT TRADES")
	if len(trades) == 0 {
		fmt.Println("    (无成交)")
	} else {
		fmt.Printf("    %-21s %-6s %5s %-10s %9s %10s src\n", "ts", "action", "qty", "symbol", "fill", "pnl")
		for _, t := range trades {
			pnl, src := "", ""
			if t.PnL != nil {
				pnl = strings.TrimPrefix(money(t.PnL, true), "$")
			}
			if t.Source != nil {
				src = *t.Source
			}
			fmt.Printf("    %-21s %-6s %5d %-10s %9.2f %10s %s\n", t.TS, t.Action, t.Qty, t.Symbol, t.FillPrice, pnl, src)
		}
	}
	fmt.Println(line)
	return 0
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// cmdHaltResume: the user role may not write halt; it is delegated to the
// risk monitor, which owns that permission.
func cmdHaltResume(ctx context.Context, halt bool, args []string) int {
	reason := "manual resume (flintrade cli)"
	if halt {
		reason = "manual halt (flintrade cli)"
	}
	if len(args) > 0 {
		reason = strings.Join(args, " ")
	}
	e, err := openEnv(false)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer e.db.Close()
	touchSeen(ctx, e)
	runner := lb.New()
	m := riskmonitor.New(&loop.Deps{Root: e.root, DB: e.db, Config: e.cfg, LB: runner,
		Clock: session.New(session.LBFetcher(runner)), Quotes: quotes.Source{LB: runner}})
	var lines []string
	if halt {
		lines, err = m.Halt(ctx, reason)
	} else {
		lines, err = m.Resume(ctx, reason)
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
