// Command flintrade is the operator CLI. Phase 1 ships one subcommand:
//
//	flintrade check   # credentials, config, ledger and market clock, read-only
//
// check never prints secret values and never writes anything.
package main

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"flintrade/internal/collect"
	"flintrade/internal/config"
	"flintrade/internal/creds"
	"flintrade/internal/db"
	"flintrade/internal/lb"
	"flintrade/internal/llm"
	"flintrade/internal/paths"
	"flintrade/internal/session"
)

const usage = `usage:
  flintrade [status]                     现在啥情况:进程 / 风险 / 持仓 / 待裁决意图 / 近期成交
  flintrade buy   SYMBOL --stop P [--target T] [--entry E] [--confidence N] [--reason R]
  flintrade short SYMBOL --stop P [...]  手动开仓意图(走同一道风控闸门)
  flintrade close SYMBOL [--reason R]    手动平仓意图
  flintrade halt [reason] / resume [reason]
  flintrade check     read-only self-check: credentials, config, models, ledger, market clock
  flintrade collect   print the market-data payload the technical producer sends the model
  flintrade ping      call every model tier once for real (budget cap $0.50 each), log to llm_calls/`

func main() {
	ctx := context.Background()
	cmd, args := "status", []string{}
	if len(os.Args) > 1 {
		cmd, args = os.Args[1], os.Args[2:]
	}
	code := 0
	switch cmd {
	case "status":
		code = cmdStatus(ctx)
	case "buy":
		code = cmdOpen(ctx, "long", args)
	case "short":
		code = cmdOpen(ctx, "short", args)
	case "close":
		code = cmdClose(ctx, args)
	case "halt":
		code = cmdHaltResume(ctx, true, args)
	case "resume":
		code = cmdHaltResume(ctx, false, args)
	case "check":
		code = boolCode(check())
	case "collect":
		code = boolCode(collectCmd())
	case "ping":
		code = boolCode(pingCmd())
	case "-h", "--help", "help":
		fmt.Println(usage)
	default:
		fmt.Fprintln(os.Stderr, usage)
		code = 2
	}
	os.Exit(code)
}

func boolCode(ok bool) int {
	if ok {
		return 0
	}
	return 1
}

// pingCmd calls each tier end to end, as `python -m agent.llm ping` did.
func pingCmd() bool {
	ctx := context.Background()
	root, err := paths.Root()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return false
	}
	l := config.Loader{Dir: paths.ConfigDir(root)}
	t, err := l.Trading()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return false
	}
	c := &llm.Client{Config: l, LogDir: llm.DefaultLogDir(root)}
	ok := true
	for _, name := range slices.Sorted(maps.Keys(t.Models)) {
		t0 := time.Now()
		out, err := c.Complete(ctx, llm.Request{User: "Reply with the single word: pong", System: "You are a connectivity probe.", Tier: name, Tag: "ping", MaxBudgetUSD: 0.5})
		if err != nil || !strings.Contains(strings.ToLower(out), "pong") {
			ok = false
			fmt.Printf("  ✗ %s: %q %v\n", name, strings.TrimSpace(out), err)
			continue
		}
		fmt.Printf("  ✓ %s: %q in %s\n", name, strings.TrimSpace(out), time.Since(t0).Round(100*time.Millisecond))
	}
	return ok
}

// collectCmd replaces `./scripts/collect.sh` for debugging what the producer
// sees. Credentials come from the environment, as for the daemon.
func collectCmd() bool {
	ctx := context.Background()
	root, err := paths.Root()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return false
	}
	if vals, err := creds.Load(creds.DefaultPath(root)); err == nil {
		creds.Apply(vals)
	}
	runner := lb.New()
	sess := session.New(session.LBFetcher(runner)).Current(ctx)
	c := collect.Collector{LB: runner, Config: config.Loader{Dir: paths.ConfigDir(root)}}
	p, err := c.Collect(ctx, sess, session.OutsideRTHFor(sess, false))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return false
	}
	b, err := p.JSON()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return false
	}
	fmt.Println(string(b))
	return true
}

func check() bool {
	ctx := context.Background()
	ok := true
	fail := func(format string, a ...any) { ok = false; fmt.Printf("  ✗ "+format+"\n", a...) }
	pass := func(format string, a ...any) { fmt.Printf("  ✓ "+format+"\n", a...) }
	warn := func(format string, a ...any) { fmt.Printf("  ! "+format+"\n", a...) }

	root, err := paths.Root()
	if err != nil {
		fmt.Println("root:", err)
		return false
	}
	fmt.Println("root:", root)

	fmt.Println("credentials:")
	cpath := creds.DefaultPath(root)
	vals, err := creds.Load(cpath)
	if err != nil {
		fail("%s: %v", cpath, err)
	} else {
		pass("%s parsed (%d keys)", cpath, len(vals))
		if err := creds.CheckPerms(cpath); err != nil {
			warn("%v", err)
		}
		info, probs := creds.Check(vals, time.Now(), 14*24*time.Hour)
		for _, p := range probs {
			if p.Fatal {
				fail("%s: %s", p.Key, p.Message)
			} else {
				warn("%s: %s", p.Key, p.Message)
			}
		}
		if !info.ExpiresAt.IsZero() {
			kind := "LIVE"
			if info.Paper() {
				kind = "paper"
			}
			pass("token: %s account, region %s, expires %s (%d days)", kind, info.Region,
				info.ExpiresAt.Format(time.DateOnly), int(info.ExpiresIn(time.Now()).Hours()/24))
		}
		pass("FLINTRADE_DRY_RUN=%s", vals[creds.DryRun])
		if err := creds.Apply(vals); err != nil {
			fail("apply env: %v", err)
		}
	}

	fmt.Println("config:")
	l := config.Loader{Dir: paths.ConfigDir(root)}
	if r, err := l.Risk(); err != nil {
		fail("%v", err)
	} else {
		pass("risk.toml: max_risk %.1f%%, open risk %.1f%%, %d clusters, equity mode %q",
			r.PerTrade.MaxRiskPct, r.Portfolio.MaxOpenRiskPct, len(r.Clusters.Groups), r.Equity.Mode)
	}
	if t, err := l.Trading(); err != nil {
		fail("%v", err)
	} else {
		pass("trading.toml: %d symbols, trader=%s/%s, flash=%s/%s", len(t.Universe.Symbols),
			t.Models["trader"].Provider, t.Models["trader"].Model,
			t.Models["flash"].Provider, t.Models["flash"].Model)
	}
	if s, err := l.Strategies(); err != nil {
		fail("%v", err)
	} else {
		pass("strategies.toml: %d playbooks", len(s.Playbooks))
	}

	fmt.Println("models:")
	if t, err := l.Trading(); err == nil {
		for _, name := range slices.Sorted(maps.Keys(t.Models)) {
			tier, err := llm.ResolveTier(t, name)
			if err != nil {
				fail("%s: %v", name, err)
				continue
			}
			if why, ok := llm.CheckTier(tier); ok {
				pass("%s: %s/%s (%s wire) — %s", name, tier.Provider, tier.Model, tier.Wire, why)
			} else {
				fail("%s: %s/%s — %s", name, tier.Provider, tier.Model, why)
			}
		}
	}

	fmt.Println("ledger:")
	if d, err := db.OpenReadOnly(paths.DBPath(root)); err != nil {
		fail("%v", err)
	} else {
		r := d.MustAs(db.RoleReader)
		if risk, err := r.GetRisk(ctx); err != nil || risk == nil {
			fail("risk_state: %v", err)
		} else {
			eq := "?"
			if risk.Equity != nil {
				eq = fmt.Sprintf("%.2f", *risk.Equity)
			}
			pass("equity %s, open risk %.2f, halt=%v", eq, risk.OpenRisk, risk.Halt)
		}
		if ps, err := r.OpenPositions(ctx); err != nil {
			fail("positions: %v", err)
		} else {
			pass("%d open positions", len(ps))
		}
		if hb, err := r.Heartbeats(ctx); err == nil {
			for _, h := range hb {
				if ts, err := db.ParseTime(h.LastBeat); err == nil && time.Since(ts) < 10*time.Minute {
					pass("heartbeat %-15s %s ago", h.Process, time.Since(ts).Round(time.Second))
				}
			}
		}
		d.Close()
	}

	fmt.Println("market clock:")
	clock := session.New(session.LBFetcher(lb.New()))
	cur := clock.Current(ctx)
	line := fmt.Sprintf("session %s, outside_rth %s", cur, session.OutsideRTHFor(cur, false))
	if m, ok := clock.MinutesToClose(ctx, cur); ok {
		line += fmt.Sprintf(", closes in %dm", m)
	}
	pass("%s", line)
	return ok
}
