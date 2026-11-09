package lb

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCLI writes an executable shell script standing in for longbridge. It
// appends one line per invocation to calls.log so tests can count attempts.
func fakeCLI(t *testing.T, body string) (*Runner, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "longbridge")
	script := "#!/bin/bash\necho \"$*\" >> \"" + dir + "/calls.log\"\n" +
		"n=$(wc -l < \"" + dir + "/calls.log\" | tr -d ' ')\n" + body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var slept []time.Duration
	r := &Runner{Bin: bin, MinInterval: 0, Retries: 4}
	r.sleep = func(d time.Duration) { mu.Lock(); slept = append(slept, d); mu.Unlock() }
	t.Cleanup(func() { _ = slept })
	return r, dir
}

func calls(_ *testing.T, dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, "calls.log"))
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "\n")
}

func TestJSONBody(t *testing.T) {
	r, _ := fakeCLI(t, `echo '[{"market":"US","sessions":[]}]'`)
	res := r.Run(context.Background(), 5*time.Second, "trading", "session", "--format", "json")
	var v []map[string]any
	if !res.OK || res.Decode(&v) != nil || v[0]["market"] != "US" {
		t.Fatalf("res = %+v", res)
	}
}

// `order buy` prints a status line before the JSON body.
func TestEmbeddedJSON(t *testing.T) {
	r, _ := fakeCLI(t, `echo "Submitting Buy order: NVDA.US x3 @200"; echo '{"order_id":"123"}'`)
	res := r.Run(context.Background(), 5*time.Second, "order", "buy")
	var v struct {
		OrderID string `json:"order_id"`
	}
	if !res.OK || res.Decode(&v) != nil || v.OrderID != "123" {
		t.Fatalf("res = %+v", res)
	}
	if !strings.HasPrefix(res.Out, "Submitting") {
		t.Fatalf("raw output lost: %q", res.Out)
	}
}

func TestNonJSONAndEmpty(t *testing.T) {
	r, _ := fakeCLI(t, `[ "$1" = text ] && echo "hello"; exit 0`)
	if res := r.Run(context.Background(), 5*time.Second, "text"); !res.OK || res.Data != nil || res.Out != "hello" {
		t.Fatalf("text: %+v", res)
	}
	if res := r.Run(context.Background(), 5*time.Second, "empty"); !res.OK || res.Data != nil || res.Out != "" {
		t.Fatalf("empty: %+v", res)
	}
}

func TestRateLimitRetriesThenSucceeds(t *testing.T) {
	r, dir := fakeCLI(t, `if [ "$n" -lt 3 ]; then echo "error 429002: request is limited" >&2; exit 1; fi; echo '{"ok":true}'`)
	var slept []time.Duration
	r.sleep = func(d time.Duration) { slept = append(slept, d) }
	res := r.Run(context.Background(), 5*time.Second, "quote", "NVDA.US")
	if !res.OK || calls(t, dir) != 3 {
		t.Fatalf("res=%+v calls=%d", res, calls(t, dir))
	}
	if len(slept) != 2 || slept[0] != 2*time.Second || slept[1] != 4*time.Second {
		t.Fatalf("backoff = %v, want [2s 4s]", slept)
	}
}

func TestRateLimitExhausted(t *testing.T) {
	r, dir := fakeCLI(t, `echo "429" >&2; exit 1`)
	res := r.Run(context.Background(), 5*time.Second, "quote")
	if res.OK || calls(t, dir) != 4 || res.Err != "429" {
		t.Fatalf("res=%+v calls=%d", res, calls(t, dir))
	}
}

// A broker rejection is not retried: resubmitting an order blindly is how
// duplicate fills happen.
func TestOtherFailureNotRetried(t *testing.T) {
	r, dir := fakeCLI(t, `echo "insufficient buying power" >&2; exit 2`)
	res := r.Run(context.Background(), 5*time.Second, "order", "buy")
	if res.OK || calls(t, dir) != 1 || res.Err != "insufficient buying power" {
		t.Fatalf("res=%+v calls=%d", res, calls(t, dir))
	}
	r2, _ := fakeCLI(t, `exit 3`)
	if res := r2.Run(context.Background(), 5*time.Second, "x"); res.Err != "exit 3" {
		t.Fatalf("silent failure err = %q", res.Err)
	}
}

func TestTimeoutRetries(t *testing.T) {
	r, dir := fakeCLI(t, `if [ "$n" -lt 2 ]; then sleep 5; fi; echo '{"ok":1}'`)
	start := time.Now()
	res := r.Run(context.Background(), 300*time.Millisecond, "quote")
	if !res.OK || calls(t, dir) != 2 {
		t.Fatalf("res=%+v calls=%d", res, calls(t, dir))
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("timed-out child was not killed promptly")
	}
}

func TestNotFound(t *testing.T) {
	r := &Runner{Bin: "definitely-not-longbridge-xyz", Retries: 4}
	if res := r.Run(context.Background(), time.Second, "x"); res.OK || !strings.Contains(res.Err, "not found") {
		t.Fatalf("res = %+v", res)
	}
}

func TestCancelledContextStops(t *testing.T) {
	r, dir := fakeCLI(t, `echo 429 >&2; exit 1`)
	ctx, cancel := context.WithCancel(context.Background())
	r.sleep = func(time.Duration) { cancel() }
	res := r.Run(ctx, time.Second, "x")
	if res.OK || calls(t, dir) != 1 {
		t.Fatalf("kept retrying after cancel: res=%+v calls=%d", res, calls(t, dir))
	}
}

// Concurrent callers are spaced by MinInterval. Measured on the Go side:
// timestamps taken inside the child are useless on macOS, where the first
// exec of a fresh script blocks every caller on the same security scan and
// releases them together.
func TestPacingAcrossGoroutines(t *testing.T) {
	r := &Runner{MinInterval: 120 * time.Millisecond}
	var mu sync.Mutex
	var starts []time.Time
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			r.pace(context.Background())
			mu.Lock()
			starts = append(starts, time.Now())
			mu.Unlock()
		})
	}
	wg.Wait()
	sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })
	for i := 1; i < len(starts); i++ {
		if gap := starts[i].Sub(starts[i-1]); gap < 110*time.Millisecond {
			t.Fatalf("starts %d and %d only %s apart", i-1, i, gap)
		}
	}
}

// Credentials reach the child through the inherited environment.
func TestEnvInherited(t *testing.T) {
	t.Setenv("LONGBRIDGE_APP_KEY", "test-key")
	r, _ := fakeCLI(t, `echo "{\"k\":\"$LONGBRIDGE_APP_KEY\"}"`)
	var v struct{ K string }
	if res := r.Run(context.Background(), 5*time.Second, "x"); res.Decode(&v) != nil || v.K != "test-key" {
		t.Fatalf("env not inherited: %+v", v)
	}
}

func TestNewFromEnv(t *testing.T) {
	t.Setenv("FLINTRADE_LB_MIN_INTERVAL", "1.5")
	t.Setenv("FLINTRADE_LB_RETRIES", "7")
	r := New()
	if r.MinInterval != 1500*time.Millisecond || r.Retries != 7 {
		t.Fatalf("r = %+v", r)
	}
}
