package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"flintrade/internal/lb"
)

var static = []Market{{Market: "US", Sessions: []Window{
	{Session: Pre, Open: "4:00:00.0", Close: "9:30:00.0"},
	{Session: Intraday, Open: "9:30:00.0", Close: "16:00:00.0"},
	{Session: Post, Open: "16:00:00.0", Close: "20:00:00.0"},
}}}

func clockAt(t time.Time, m []Market) *Clock {
	return &Clock{Now: func() time.Time { return t }, TTL: 0,
		Fetch: func(context.Context) ([]Market, error) { return m, nil }}
}

func et(y int, mo time.Month, d, h, mi, s int) time.Time {
	return time.Date(y, mo, d, h, mi, s, 0, ET)
}

// Every point of a full week must match what agent/session.py returned.
func TestMatchesPythonGolden(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "python_golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Static []Market `json:"static_sessions"`
		Points []struct {
			ET             string `json:"et"`
			Session        string `json:"session"`
			MinutesToClose *int   `json:"minutes_to_close"`
		} `json:"points"`
		OutsideRTH []struct {
			Session string `json:"session"`
			ForExit bool   `json:"for_exit"`
			Flag    string `json:"flag"`
		} `json:"outside_rth"`
	}
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, p := range g.Points {
		ts, err := time.ParseInLocation("2006-01-02T15:04:05", p.ET, ET)
		if err != nil {
			t.Fatal(err)
		}
		c := clockAt(ts, g.Static)
		got := c.Current(ctx)
		if got != p.Session {
			t.Errorf("%s: session %q, python %q", p.ET, got, p.Session)
			continue
		}
		mins, ok := c.MinutesToClose(ctx, got)
		switch {
		case p.MinutesToClose == nil && ok:
			t.Errorf("%s: minutes_to_close %d, python None", p.ET, mins)
		case p.MinutesToClose != nil && (!ok || mins != *p.MinutesToClose):
			t.Errorf("%s: minutes_to_close %d/%v, python %d", p.ET, mins, ok, *p.MinutesToClose)
		}
	}
	for _, r := range g.OutsideRTH {
		if got := OutsideRTHFor(r.Session, r.ForExit); got != r.Flag {
			t.Errorf("OutsideRTHFor(%q, %v) = %q, python %q", r.Session, r.ForExit, got, r.Flag)
		}
	}
	if len(g.Points) < 2000 {
		t.Fatalf("golden has only %d points", len(g.Points))
	}
}

// Hard lesson「CLI 时刻表不含日期,周末要自己挡」(flintrade-210, 2026-08-01).
func TestHardLesson_WeekendNeverMatchesDatelessSchedule(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		at   time.Time
		want string
	}{
		{et(2026, 8, 3, 12, 0, 0), Intraday},  // Monday noon: positive control
		{et(2026, 8, 1, 12, 0, 0), Closed},    // Saturday noon: the incident
		{et(2026, 8, 2, 12, 0, 0), Closed},    // Sunday noon
		{et(2026, 8, 2, 21, 0, 0), Overnight}, // Sunday night belongs to Monday
		{et(2026, 8, 1, 21, 0, 0), Closed},    // Saturday night: next day is Sunday
	}
	for _, c := range cases {
		if got := clockAt(c.at, static).Current(ctx); got != c.want {
			t.Errorf("%s: %q, want %q", c.at.Format("Mon 15:04"), got, c.want)
		}
	}
}

// Hard lesson「会话边界的死区会杀掉退出单」(flintrade-200 AAPL exit, 2026-07-31).
func TestHardLesson_NoDeadZoneAt0350(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		h, m int
		want string
	}{{3, 49, Overnight}, {3, 50, OvernightPre}, {3, 55, OvernightPre}, {3, 59, OvernightPre}, {4, 0, Pre}} {
		got := clockAt(et(2026, 7, 31, c.h, c.m, 0), static).Current(ctx)
		if got != c.want {
			t.Errorf("Fri %02d:%02d: %q, want %q", c.h, c.m, got, c.want)
		}
	}
	if OutsideRTHFor(OvernightPre, false) != OvernightOrder {
		t.Error("Overnight-Pre must map to OVERNIGHT, not RTH_ONLY")
	}
	if OutsideRTHFor(Closed, true) != AnyTime || OutsideRTHFor(Closed, false) != RTHOnly {
		t.Error("exit fallback must be ANY_TIME, entry fallback RTH_ONLY")
	}
	// Overnight heuristic alone (no schedule), as test_stop_guard.py checks it.
	// With a schedule, 20:00:00 itself is still Post: windows are inclusive.
	if got := clockAt(et(2026, 7, 31, 20, 0, 0), nil).Current(ctx); got != Closed {
		t.Errorf("Fri 20:00 = %q, want Closed (next day is Saturday)", got)
	}
	if got := clockAt(et(2026, 7, 30, 20, 0, 0), nil).Current(ctx); got != Overnight {
		t.Errorf("Thu 20:00 = %q, want Overnight", got)
	}
}

// The tradeable windows must tile the week with no gap: from Sunday 20:00 to
// Friday 20:00 ET (inclusive — Post's close is part of Post) every single
// minute is some session. A gap anywhere is the dead-zone bug class again.
func TestEveryTradeableMinuteCovered(t *testing.T) {
	ctx := context.Background()
	start := et(2026, 8, 2, 0, 0, 0) // Sunday
	for m := range 7 * 24 * 60 {
		at := start.Add(time.Duration(m) * time.Minute)
		dow, hhmm := isoDOW(at), at.Format("15:04")
		wantOpen := !(dow == 6 || (dow == 7 && hhmm < "20:00") || (dow == 5 && hhmm > "20:00"))
		got := clockAt(at, static).Current(ctx)
		if wantOpen && got == Closed {
			t.Fatalf("%s is tradeable but resolved Closed", at.Format("Mon 15:04"))
		}
		if !wantOpen && got != Closed {
			t.Fatalf("%s should be Closed, got %q", at.Format("Mon 15:04"), got)
		}
	}
}

func TestMinutesToClose(t *testing.T) {
	ctx := context.Background()
	c := clockAt(et(2026, 8, 3, 15, 30, 30), static)
	if m, ok := c.MinutesToClose(ctx, Intraday); !ok || m != 29 {
		t.Fatalf("15:30:30 → %d %v, want 29 (floored)", m, ok)
	}
	c = clockAt(et(2026, 8, 3, 21, 0, 0), static)
	if m, ok := c.MinutesToClose(ctx, Overnight); !ok || m != 6*60+50 {
		t.Fatalf("21:00 overnight → %d %v, want 410", m, ok)
	}
	if _, ok := c.MinutesToClose(ctx, Closed); ok {
		t.Fatal("Closed has no close")
	}
	if _, ok := clockAt(et(2026, 8, 3, 12, 0, 0), nil).MinutesToClose(ctx, Intraday); ok {
		t.Fatal("no schedule → not determinable")
	}
}

// A CLI failure leaves only the overnight heuristic — weekday daytime reads
// Closed (same as Python's [] fallback) — and the failure is cached.
func TestFetchFailureAndCache(t *testing.T) {
	ctx := context.Background()
	calls := 0
	c := &Clock{Now: func() time.Time { return et(2026, 8, 3, 12, 0, 0) }, TTL: time.Minute,
		Fetch: func(context.Context) ([]Market, error) { calls++; return nil, errors.New("cli down") }}
	if got := c.Current(ctx); got != Closed {
		t.Fatalf("got %q", got)
	}
	c.Current(ctx)
	c.Current(ctx)
	if calls != 1 {
		t.Fatalf("fetch called %d times within TTL", calls)
	}
	c.Now = func() time.Time { return et(2026, 8, 3, 22, 0, 0) }
	if got := c.Current(ctx); got != Overnight {
		t.Fatalf("overnight without schedule: %q", got)
	}
}

func TestLBFetcherParsesRealShape(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "longbridge")
	body := `[{"market":"US","sessions":[{"open":"4:00:00.0","close":"9:30:00.0","session":"Pre"},` +
		`{"open":"9:30:00.0","close":"16:00:00.0","session":"Intraday"},` +
		`{"open":"16:00:00.0","close":"20:00:00.0","session":"Post"}]},` +
		`{"market":"HK","sessions":[{"open":"9:30:00.0","close":"12:00:00.0","session":"Intraday"}]}]`
	os.WriteFile(bin, []byte("#!/bin/bash\necho '"+body+"'\n"), 0o755)
	f := LBFetcher(&lb.Runner{Bin: bin, Retries: 1})
	c := &Clock{Now: func() time.Time { return et(2026, 8, 3, 17, 0, 0) }, Fetch: f, TTL: time.Minute}
	if got := c.Current(context.Background()); got != Post {
		t.Fatalf("got %q, want Post", got)
	}
}

func TestNormHMS(t *testing.T) {
	for in, want := range map[string]string{"4:00:00.0": "04:00:00", "16:00:00.0": "16:00:00", "9:30:00": "09:30:00"} {
		if got := normHMS(in); got != want {
			t.Errorf("normHMS(%q) = %q, want %q", in, got, want)
		}
	}
}
