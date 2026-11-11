// flintrade-daemon runs every trading loop in one process under launchd.
//
// Loops never call each other; they talk only through flintrade.db, and the role
// guard keeps a single writer per table. Each loop runs in its own goroutine
// on its own cadence; one loop failing or panicking never stops the others.
//
//	flintrade-daemon               run until SIGTERM
//	flintrade-daemon --once        one iteration of every loop, then exit (self-check)
//	flintrade-daemon --seconds N   run N seconds, then shut down gracefully (smoke test)
//	flintrade-daemon --dry-run     never place orders, whatever the credentials file says
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"flintrade/internal/broker"
	"flintrade/internal/config"
	"flintrade/internal/creds"
	"flintrade/internal/db"
	"flintrade/internal/lb"
	"flintrade/internal/llm"
	"flintrade/internal/loop"
	"flintrade/internal/paths"
	"flintrade/internal/quotes"
	"flintrade/internal/session"
)

func main() {
	once := flag.Bool("once", false, "run every loop once and exit")
	seconds := flag.Float64("seconds", 0, "run this long, then shut down (smoke test)")
	dry := flag.Bool("dry-run", false, "force FLINTRADE_DRY_RUN=1 even if the credentials file says 0")
	flag.Parse()
	os.Exit(run(*once, *seconds, *dry))
}

func logf(format string, a ...any) { fmt.Printf(format+"\n", a...) }
func errf(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }

func run(once bool, seconds float64, forceDry bool) int {
	root, err := paths.Root()
	if err != nil {
		errf("[daemon] %v", err)
		return 1
	}
	// Credentials: the one file (plan D5). launchd passes only PATH.
	cpath := creds.DefaultPath(root)
	vals, err := creds.Load(cpath)
	if err != nil {
		errf("[daemon] credentials %s: %v", cpath, err)
		return 1
	}
	if err := creds.CheckPerms(cpath); err != nil {
		errf("[daemon] warning: %v", err)
	}
	if err := creds.Apply(vals); err != nil {
		errf("[daemon] %v", err)
		return 1
	}
	// The credentials file carries FLINTRADE_DRY_RUN and overrides the shell, so a
	// test run must force dry mode after loading it, not before.
	if forceDry {
		os.Setenv(creds.DryRun, "1")
	}

	if !once {
		unlock, err := singleton(filepath.Join(root, ".daemon.lock"))
		if err != nil {
			errf("[daemon] another daemon holds .daemon.lock — exiting (%v)", err)
			return 1
		}
		defer unlock()
	}

	d, err := db.Open(paths.DBPath(root))
	if err != nil {
		errf("[daemon] %v", err)
		return 1
	}
	defer d.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := d.Init(ctx); err != nil {
		errf("[daemon] db init: %v", err)
		return 1
	}

	deps, err := newDeps(root, d)
	if err != nil {
		errf("[daemon] %v", err)
		return 1
	}
	mode := "LIVE paper orders"
	if deps.Broker.DryRun() {
		mode = "DRY RUN (no orders)"
	}
	loops, err := buildLoops(deps)
	if err != nil {
		errf("[daemon] %v", err)
		return 1
	}

	if once {
		logf("[daemon] --once · %s · %d loops", mode, len(loops))
		failed := 0
		for _, l := range loops {
			if err := iterate(ctx, l.Loop); err != nil {
				failed++
				errf("[daemon] ✗ %s: %v", l.Name(), err)
			} else {
				logf("[daemon] ✓ %s ran once", l.Name())
			}
		}
		if failed > 0 {
			return 1
		}
		return 0
	}

	_ = d.MustAs(db.RoleReader).Beat(ctx, "daemon", nil)
	logf("[daemon] Flintrade daemon (Go) · %s · %d loops", mode, len(loops))
	sup := &supervisor{lastOK: map[string]time.Time{}}
	var wg sync.WaitGroup
	for _, l := range loops {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sup.worker(ctx, l)
		}()
	}

	if seconds > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(time.Duration(seconds * float64(time.Second))):
		}
		stop()
	} else {
		tick := time.NewTicker(30 * time.Second)
	watch:
		for {
			select {
			case <-ctx.Done():
				break watch
			case <-tick.C:
				if reason := sup.watchdog(loops); reason != "" {
					// Sick-but-alive is worse than dead: hand the restart to launchd.
					errf("[daemon] WATCHDOG: %s — exiting for launchd to restart", reason)
					note := truncate("restart: "+reason, 200)
					_ = d.MustAs(db.RoleReader).Beat(context.Background(), "watchdog", &note)
					os.Exit(70) // no graceful join: a stuck goroutine could block it forever
				}
			}
		}
		tick.Stop()
	}
	logf("[daemon] shutting down…")
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		logf("[daemon] all loops stopped")
	case <-time.After(10 * time.Second):
		errf("[daemon] some loops did not stop within 10s; exiting anyway")
	}
	return 0
}

func newDeps(root string, d *db.DB) (*loop.Deps, error) {
	runner := lb.New()
	b, err := broker.New(runner, broker.DryRunFromEnv())
	if err != nil {
		return nil, err
	}
	cfg := config.Loader{Dir: paths.ConfigDir(root)}
	return &loop.Deps{
		Root:   root,
		DB:     d,
		Config: cfg,
		LB:     runner,
		Broker: b,
		Clock:  session.New(session.LBFetcher(runner)),
		LLM:    &llm.Client{Config: cfg, LogDir: llm.DefaultLogDir(root)},
		Quotes: quotes.Source{LB: runner},
	}, nil
}

// scheduled is a loop with its cadence (read once at start, as Python did).
type scheduled struct {
	loop.Loop
	Cadence time.Duration
}

// iterate runs one RunOnce, printing its lines; a panic becomes an error so
// one loop's bug never takes the process down.
func iterate(ctx context.Context, l loop.Loop) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v\n%s", p, debug.Stack())
		}
	}()
	lines, err := l.RunOnce(ctx)
	for _, line := range lines {
		if line != "" {
			logf("[%s] %s", l.Name(), line)
		}
	}
	return err
}

type supervisor struct {
	mu     sync.Mutex
	lastOK map[string]time.Time
	now    func() time.Time  // nil → time.Now; tests pin it
	fds    func() (int, int) // nil → fdUsage; tests fake it
}

func (s *supervisor) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *supervisor) mark(name string) {
	s.mu.Lock()
	s.lastOK[name] = s.clock()
	s.mu.Unlock()
}

func (s *supervisor) worker(ctx context.Context, l scheduled) {
	logf("[daemon] ▶ %s every %s", l.Name(), l.Cadence)
	s.mark(l.Name())
	for {
		if err := iterate(ctx, l.Loop); err != nil {
			if ctx.Err() != nil && errors.Is(err, context.Canceled) {
				break
			}
			errf("[%s] error: %v", l.Name(), err)
		} else {
			s.mark(l.Name()) // only successful iterations count as progress
		}
		select {
		case <-ctx.Done():
			logf("[daemon] ■ %s stopped", l.Name())
			return
		case <-time.After(l.Cadence):
		}
	}
	logf("[daemon] ■ %s stopped", l.Name())
}

// watchdog returns a fatal reason, or "" when healthy: a loop without a
// successful iteration for max(6×cadence, 30 min), or fd use above 90% of the
// soft limit (2026-07-26: an fd leak left every loop failing for 5 days while
// the process stayed up, so launchd's KeepAlive never kicked in).
func (s *supervisor) watchdog(loops []scheduled) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range loops {
		last, ok := s.lastOK[l.Name()]
		if !ok {
			continue
		}
		limit := max(6*l.Cadence, 30*time.Minute)
		if age := s.clock().Sub(last); age > limit {
			return fmt.Sprintf("loop %q has had no successful iteration for %s (limit %s)", l.Name(), age.Round(time.Second), limit)
		}
	}
	fds := fdUsage
	if s.fds != nil {
		fds = s.fds
	}
	if used, soft := fds(); used >= 0 && soft > 0 && float64(used) > 0.9*float64(soft) {
		return fmt.Sprintf("fd usage %d/%d above 90%% (leak?)", used, soft)
	}
	return ""
}

func fdUsage() (int, int) {
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err != nil {
		return -1, 0
	}
	ents, err := os.ReadDir("/dev/fd")
	if err != nil {
		return -1, int(rl.Cur)
	}
	return len(ents), int(rl.Cur)
}

// singleton takes the same .daemon.lock the Python daemon used, so two
// daemons (two executors double-consuming intents) can never coexist —
// including a Go daemon started while the Python one is still alive.
func singleton(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	f.Truncate(0)
	fmt.Fprintf(f, "%d", os.Getpid())
	return func() { f.Close() }, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
