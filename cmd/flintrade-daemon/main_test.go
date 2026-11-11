package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeLoop runs fn as its iteration.
type fakeLoop struct {
	name string
	fn   func(ctx context.Context) ([]string, error)
}

func (f fakeLoop) Name() string                                  { return f.name }
func (f fakeLoop) RunOnce(ctx context.Context) ([]string, error) { return f.fn(ctx) }

// TestHardLesson_SickButAliveExits: 2026-07-26 an fd leak left every loop
// failing for 5 days while the process stayed up, so launchd never restarted
// it. The watchdog must name a loop with no successful iteration for
// max(6×cadence, 30 min), and fd use above 90% of the soft limit.
func TestHardLesson_SickButAliveExits(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 14, 0, 0, 0, time.UTC)
	now := t0
	fds := [2]int{50, 256}
	sup := &supervisor{
		lastOK: map[string]time.Time{},
		now:    func() time.Time { return now },
		fds:    func() (int, int) { return fds[0], fds[1] },
	}
	loops := []scheduled{
		{Loop: fakeLoop{name: "executor"}, Cadence: 30 * time.Second},       // limit: the 30 min floor
		{Loop: fakeLoop{name: "loop_technical"}, Cadence: 30 * time.Minute}, // limit: 6×cadence = 3h
	}
	sup.mark("executor")
	sup.mark("loop_technical")

	for _, c := range []struct {
		after time.Duration
		want  string // "" = healthy, else a substring of the reason
	}{
		{29 * time.Minute, ""},
		{30 * time.Minute, ""}, // at the limit is still healthy
		{30*time.Minute + time.Second, `loop "executor"`},
	} {
		now = t0.Add(c.after)
		got := sup.watchdog(loops)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("after %s: %q, want %q", c.after, got, c.want)
		}
	}

	// The executor recovers; the slow loop gets its own 6×cadence limit.
	now = t0.Add(3 * time.Hour)
	sup.mark("executor")
	if got := sup.watchdog(loops); got != "" {
		t.Errorf("3h: %q, want healthy", got)
	}
	now = t0.Add(3*time.Hour + time.Second)
	if got := sup.watchdog(loops); !strings.Contains(got, `loop "loop_technical"`) {
		t.Errorf("3h+1s: %q, want loop_technical", got)
	}

	// A loop that has not started yet is not judged.
	sup = &supervisor{lastOK: map[string]time.Time{}, now: func() time.Time { return now }, fds: sup.fds}
	if got := sup.watchdog(loops); got != "" {
		t.Errorf("before any mark: %q", got)
	}

	// fd: exit early in a leak, before EMFILE.
	for _, c := range []struct {
		used, soft int
		sick       bool
	}{
		{230, 256, false}, // 89.8%
		{231, 256, true},  // 90.2%
		{-1, 256, false},  // /dev/fd unreadable: no verdict
		{900, 0, false},   // no limit known: no verdict
	} {
		fds = [2]int{c.used, c.soft}
		got := sup.watchdog(loops)
		if (got != "") != c.sick || (c.sick && !strings.Contains(got, "fd usage")) {
			t.Errorf("fd %d/%d: %q, want sick=%v", c.used, c.soft, got, c.sick)
		}
	}
}

// TestHardLesson_FailedIterationIsNotProgress: a loop that runs but errors
// (the fd-leak symptom) must not refresh its watchdog clock, and a panic is an
// error, not a dead process.
func TestHardLesson_FailedIterationIsNotProgress(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 14, 0, 0, 0, time.UTC)
	now := t0
	sup := &supervisor{lastOK: map[string]time.Time{}, now: func() time.Time { return now }}

	run := func(name string, fn func() error) {
		ctx, cancel := context.WithCancel(context.Background())
		l := scheduled{Cadence: time.Hour, Loop: fakeLoop{name: name, fn: func(context.Context) ([]string, error) {
			now = now.Add(time.Minute) // the iteration takes time
			defer cancel()             // one iteration, then stop
			return nil, fn()
		}}}
		sup.worker(ctx, l)
	}

	run("failing", func() error { return errors.New("too many open files") })
	if got := sup.lastOK["failing"]; !got.Equal(t0) {
		t.Errorf("failing loop progress at %v, want only the start mark %v", got, t0)
	}
	run("panicking", func() error { panic("nil map") })
	if got := sup.lastOK["panicking"]; !got.Equal(t0.Add(time.Minute)) {
		t.Errorf("panicking loop progress at %v, want only its start mark", got)
	}
	run("healthy", func() error { return nil })
	if got := sup.lastOK["healthy"]; !got.Equal(t0.Add(3 * time.Minute)) {
		t.Errorf("healthy loop progress at %v, want after its iteration", got)
	}

	if err := iterate(context.Background(), fakeLoop{name: "p", fn: func(context.Context) ([]string, error) { panic("boom") }}); err == nil || !strings.Contains(err.Error(), "panic: boom") {
		t.Errorf("iterate on panic: %v", err)
	}
}
