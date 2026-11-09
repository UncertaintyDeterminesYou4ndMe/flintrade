// Package session is the shared market clock: every loop asks it which US
// session is live. Ported from agent/session.py, and pinned to it by a golden
// test over a full week of 5-minute points (testdata/python_golden.json).
//
// Two hard lessons are encoded here:
//
//   - "CLI 时刻表不含日期,周末要自己挡": `longbridge trading session` returns
//     a dateless static schedule, so on a Saturday noon the wall clock still
//     "matches" Intraday. Pre/Intraday/Post are weekday-only; only the
//     Overnight branch (Sunday >= 20:00 ET belongs to Monday) may be live on
//     a weekend.
//   - "会话边界的死区会杀掉退出单": 03:50–04:00 once matched no window, came
//     back as Closed → RTH_ONLY → the broker rejected three AAPL exits. The
//     overnight branch runs to 04:00 and labels that gap Overnight-Pre.
package session

import (
	"context"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // America/New_York must resolve even without system zoneinfo

	"flintrade/internal/lb"
)

// Session names as Longbridge and the rest of Flintrade spell them.
const (
	Pre          = "Pre"
	Intraday     = "Intraday"
	Post         = "Post"
	Overnight    = "Overnight"
	OvernightPre = "Overnight-Pre"
	Closed       = "Closed"
)

// outside_rth flags the broker accepts.
const (
	RTHOnly        = "RTH_ONLY"
	AnyTime        = "ANY_TIME"
	OvernightOrder = "OVERNIGHT"
)

// ET is the exchange time zone; session logic runs on ET wall-clock time.
var ET = mustLoad("America/New_York")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// Window is one session in the CLI's schedule.
type Window struct {
	Session string `json:"session"`
	Open    string `json:"open"`
	Close   string `json:"close"`
}

// Market is one market's schedule.
type Market struct {
	Market   string   `json:"market"`
	Sessions []Window `json:"sessions"`
}

// Fetcher returns the CLI schedule. An error is treated like Python's
// `|| echo "[]"`: no windows, only the overnight heuristic applies.
type Fetcher func(ctx context.Context) ([]Market, error)

// LBFetcher fetches `longbridge trading session --format json`.
func LBFetcher(r *lb.Runner) Fetcher {
	return func(ctx context.Context) ([]Market, error) {
		var m []Market
		if err := r.Run(ctx, 15*time.Second, "trading", "session", "--format", "json").Decode(&m); err != nil {
			return nil, err
		}
		return m, nil
	}
}

// Clock resolves sessions. The schedule is cached for TTL (it is static
// within a day); the time-of-day check runs on every call.
type Clock struct {
	Now   func() time.Time
	Fetch Fetcher
	TTL   time.Duration

	mu       sync.Mutex
	cached   []Market
	cachedAt time.Time
	hasCache bool
}

// New returns a Clock on the real time with a 30 s schedule cache.
func New(fetch Fetcher) *Clock {
	return &Clock{Now: time.Now, Fetch: fetch, TTL: 30 * time.Second}
}

func (c *Clock) schedule(ctx context.Context) []Market {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hasCache && time.Since(c.cachedAt) < c.TTL {
		return c.cached
	}
	var m []Market
	if c.Fetch != nil {
		if got, err := c.Fetch(ctx); err == nil {
			m = got
		}
	}
	// Failures are cached too, so a CLI outage is not hammered every call.
	c.cached, c.cachedAt, c.hasCache = m, time.Now(), true
	return m
}

func usWindows(m []Market) []Window {
	for _, mk := range m {
		if strings.ToUpper(mk.Market) == "US" {
			return mk.Sessions
		}
	}
	return nil
}

// normHMS turns the CLI's "4:00:00.0" into "04:00:00" so string comparison
// against an HH:MM:SS clock is well ordered (same as _norm_hms).
func normHMS(raw string) string {
	raw = strings.ReplaceAll(raw, ".0", "")
	parts := strings.Split(raw, ":")
	for i, p := range parts {
		if len(p) < 2 {
			parts[i] = strings.Repeat("0", 2-len(p)) + p
		}
	}
	return strings.Join(parts, ":")
}

// isoDOW is 1=Mon … 7=Sun, like strftime %u.
func isoDOW(t time.Time) int {
	if d := int(t.Weekday()); d != 0 {
		return d
	}
	return 7
}

// Current returns Pre, Intraday, Post, Overnight, Overnight-Pre or Closed.
func (c *Clock) Current(ctx context.Context) string {
	now := c.Now().In(ET)
	etNow := now.Format("15:04:05")
	hhmm := etNow[:5]
	dow := isoDOW(now)

	// Pre / Intraday / Post: weekdays only (see package doc).
	if dow <= 5 {
		for _, w := range usWindows(c.schedule(ctx)) {
			begin, end := normHMS(w.Open), normHMS(w.Close)
			if begin != "" && end != "" && begin <= etNow && etNow <= end {
				if w.Session == "" {
					return "Open"
				}
				return w.Session
			}
		}
	}

	// Overnight 20:00–04:00 belongs to the NEXT trading day, which must be a
	// weekday (holidays are not modelled, as in Python).
	if hhmm >= "20:00" || hhmm < "04:00" {
		next := dow
		if hhmm >= "20:00" {
			next = dow%7 + 1
		}
		if next >= 1 && next <= 5 {
			if hhmm >= "03:50" && hhmm < "04:00" {
				return OvernightPre
			}
			return Overnight
		}
	}
	return Closed
}

// OutsideRTHFor maps a session to the broker's outside_rth flag.
//
// forExit only changes the unrecognised-session fallback: an entry falls back
// to RTH_ONLY (don't reach into extended hours to open risk when unsure), an
// exit to ANY_TIME (an RTH_ONLY stop-out placed while the market is shut would
// sit idle until the next open). ANY_TIME does not cover overnight, so the
// Overnight branch stays session-accurate for exits too.
func OutsideRTHFor(session string, forExit bool) string {
	switch session {
	case Intraday:
		return RTHOnly
	case Pre, Post:
		return AnyTime
	case Overnight, OvernightPre:
		return OvernightOrder
	}
	if forExit {
		return AnyTime
	}
	return RTHOnly
}

// IsTradeable is true for every session except Closed.
func IsTradeable(session string) bool { return session != Closed }

// MinutesToClose returns whole minutes until the session closes (floored,
// never negative); ok=false when it cannot be determined. Overnight closes at
// 03:50 ET. Differences use absolute time; Python subtracted wall clocks, which
// only differs across a DST switch — and those happen at 02:00 Sunday, when no
// session is open.
func (c *Clock) MinutesToClose(ctx context.Context, session string) (int, bool) {
	now := c.Now().In(ET)
	var closeAt time.Time
	switch session {
	case Overnight, OvernightPre:
		closeAt = time.Date(now.Year(), now.Month(), now.Day(), 3, 50, 0, 0, ET)
		if now.Format("15:04") >= "20:00" {
			closeAt = closeAt.AddDate(0, 0, 1)
		}
	case Pre, Intraday, Post:
		found := false
		for _, w := range usWindows(c.schedule(ctx)) {
			if w.Session != session {
				continue
			}
			h, m, s, ok := parseHMS(normHMS(w.Close))
			if !ok {
				return 0, false
			}
			closeAt = time.Date(now.Year(), now.Month(), now.Day(), h, m, s, 0, ET)
			found = true
			break
		}
		if !found {
			return 0, false
		}
	default:
		return 0, false
	}
	mins := int(math.Floor(closeAt.Sub(now).Seconds() / 60))
	return max(0, mins), true
}

// parseHMS parses a normalised "HH:MM:SS".
func parseHMS(s string) (h, m, sec int, ok bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var v [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, 0, 0, false
		}
		v[i] = n
	}
	return v[0], v[1], v[2], true
}
