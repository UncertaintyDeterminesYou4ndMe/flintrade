// Package lb is the single gateway to the longbridge CLI, ported from
// agent/lb.py.
//
// Several loops share one process and would otherwise hit Longbridge's rate
// limit (429002) and timeouts in bursts. The Runner spaces call starts by a
// process-wide minimum interval and retries rate limits and timeouts with
// linear backoff (2, 4, 6 … s).
//
// Credentials reach the CLI through the inherited environment (see
// creds.Apply); nothing is passed on the command line.
package lb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Result is one CLI call's outcome. Data holds the parsed JSON body when the
// output contained one; Out is the trimmed stdout; Err explains a failure.
type Result struct {
	OK   bool
	Data json.RawMessage
	Out  string
	Err  string
}

// Decode unmarshals Data into v.
func (r Result) Decode(v any) error {
	if !r.OK {
		return errors.New(r.Err)
	}
	if r.Data == nil {
		return errors.New("longbridge returned no JSON")
	}
	return json.Unmarshal(r.Data, v)
}

// Runner paces and retries longbridge invocations. Safe for concurrent use.
type Runner struct {
	Bin         string        // executable, default "longbridge"
	MinInterval time.Duration // minimum gap between call starts
	Retries     int           // attempts per call

	mu        sync.Mutex
	lastStart time.Time
	sleep     func(time.Duration) // test hook
}

// New returns a Runner configured from FLINTRADE_LB_MIN_INTERVAL (seconds,
// default 0.35) and FLINTRADE_LB_RETRIES (default 4), as in Python.
func New() *Runner {
	r := &Runner{Bin: "longbridge", MinInterval: 350 * time.Millisecond, Retries: 4}
	if v, err := strconv.ParseFloat(os.Getenv("FLINTRADE_LB_MIN_INTERVAL"), 64); err == nil && v >= 0 {
		r.MinInterval = time.Duration(v * float64(time.Second))
	}
	if v, err := strconv.Atoi(os.Getenv("FLINTRADE_LB_RETRIES")); err == nil && v > 0 {
		r.Retries = v
	}
	return r
}

func (r *Runner) doSleep(ctx context.Context, d time.Duration) {
	if r.sleep != nil {
		r.sleep(d)
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// pace makes consecutive call starts at least MinInterval apart across
// goroutines. The lock is held while waiting, so callers queue in order.
func (r *Runner) pace(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if wait := r.MinInterval - time.Since(r.lastStart); wait > 0 {
		r.doSleep(ctx, wait)
	}
	r.lastStart = time.Now()
}

func rateLimited(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "429") || strings.Contains(s, "request is limited") || strings.Contains(s, "rate limit")
}

// embeddedJSON finds a JSON object or array inside mixed output: `order buy`
// prints "Submitting Buy order: …" before the {"order_id":…} body.
var embeddedJSON = regexp.MustCompile(`(?s)(\{.*\}|\[.*\])`)

func parseOut(out string) json.RawMessage {
	if out == "" {
		return nil
	}
	if json.Valid([]byte(out)) {
		return json.RawMessage(out)
	}
	if m := embeddedJSON.FindString(out); m != "" && json.Valid([]byte(m)) {
		return json.RawMessage(m)
	}
	return nil
}

// Run executes `longbridge args…` with a per-attempt timeout.
//
// Rate limit or timeout → back off 2·(attempt+1)s and retry. A missing binary
// fails at once. Any other non-zero exit fails without retry — a rejected
// order must not be resubmitted blindly.
func (r *Runner) Run(ctx context.Context, timeout time.Duration, args ...string) Result {
	n := r.Retries
	if n <= 0 {
		n = 1
	}
	bin := r.Bin
	if bin == "" {
		bin = "longbridge"
	}
	lastErr := "retries exhausted"
	for attempt := range n {
		if err := ctx.Err(); err != nil {
			return Result{Err: err.Error()}
		}
		r.pace(ctx)
		actx, cancel := context.WithTimeout(ctx, timeout)
		cmd := exec.CommandContext(actx, bin, args...)
		// Own process group, killed whole on timeout: a grandchild holding the
		// stdout pipe must not keep the call alive past its deadline.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		cmd.WaitDelay = 2 * time.Second
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		timedOut := errors.Is(actx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
		cancel()

		backoff := time.Duration(2*(attempt+1)) * time.Second
		switch {
		case errors.Is(err, exec.ErrNotFound):
			return Result{Err: "longbridge CLI not found on PATH"}
		case timedOut:
			lastErr = fmt.Sprintf("timeout after %s", timeout)
			r.doSleep(ctx, backoff)
			continue
		case ctx.Err() != nil:
			return Result{Err: ctx.Err().Error()}
		case err != nil:
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				// Could not even start (permissions, fork failure): retry.
				lastErr = err.Error()
				r.doSleep(ctx, backoff)
				continue
			}
			lastErr = strings.TrimSpace(stderr.String())
			if lastErr == "" {
				lastErr = fmt.Sprintf("exit %d", ee.ExitCode())
			}
			if rateLimited(stdout.String()+"\n"+stderr.String()) && attempt < n-1 {
				r.doSleep(ctx, backoff)
				continue
			}
			return Result{Err: lastErr}
		}
		out := strings.TrimSpace(stdout.String())
		return Result{OK: true, Data: parseOut(out), Out: out}
	}
	return Result{Err: lastErr}
}
