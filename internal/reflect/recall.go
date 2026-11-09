package reflect

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"flintrade/internal/db"
	"flintrade/internal/memory"
	"flintrade/internal/postmortem"
	"flintrade/internal/producers/pj"
	"flintrade/internal/pyfloat"
	"flintrade/internal/session"
)

// RecallLessonsCap bounds the lessons injected into every decision prompt.
const RecallLessonsCap = 12

// TimeAnchor orients the model in time before it reads its memory.
type TimeAnchor struct {
	NowET          string `json:"now_et"`
	TodayET        string `json:"today_et"`
	SinceLastDream string `json:"since_last_dream"`
	SinceLastUser  string `json:"since_last_user"`
	SinceLastTrade string `json:"since_last_trade"`
}

type Lesson struct {
	Text       string `json:"text"`
	Confidence pj.F   `json:"confidence"`
	N          int    `json:"n"`
}

type Plan struct {
	Text      string          `json:"text"`
	ExpiresAt *string         `json:"expires_at"`
	Tags      json.RawMessage `json:"tags"`
}

type Stat struct {
	Source  any   `json:"source"`
	N       int64 `json:"n"`
	WinRate *pj.F `json:"win_rate"`
}

// SelfAssessment is Flintrade's real record, computed in code from strategy
// trades only — never left to the model.
type SelfAssessment struct {
	N                 int                 `json:"n"`
	WinRate           *pj.F               `json:"win_rate"`
	NetPnL            any                 `json:"net_pnl"` // pj.F, or int 0 when there is no trade (Python round(sum([]), 2))
	Note              string              `json:"note,omitempty"`
	ConfidenceBuckets *postmortem.Buckets `json:"confidence_buckets,omitempty"`
}

// Recall is the memory block injected into producer payloads. Plans is a
// pointer so the event producer can drop it (it trades fresh catalysts, not
// a saved watchlist) while the technical producer always shows the list.
type Recall struct {
	TimeAnchor     TimeAnchor     `json:"time_anchor"`
	Lessons        []Lesson       `json:"lessons"`
	Plans          *[]Plan        `json:"plans,omitempty"`
	Stats          []Stat         `json:"stats"`
	SelfAssessment SelfAssessment `json:"self_assessment"`
}

// isoformat is Python's datetime.isoformat(): microseconds only when non-zero.
func isoformat(t time.Time) string {
	if t.Nanosecond()/1000 != 0 {
		return t.Format("2006-01-02T15:04:05.000000-07:00")
	}
	return t.Format("2006-01-02T15:04:05-07:00")
}

// HumanGap renders a time distance: "just now", "45 minutes", "3.2 hours",
// "4.1 days", "never".
func HumanGap(then *time.Time, ref time.Time) string {
	if then == nil {
		return "never"
	}
	secs := ref.Sub(*then).Seconds()
	switch {
	case secs < 0:
		return "in the future"
	case secs < 90:
		return "just now"
	}
	mins := secs / 60
	if mins < 90 {
		return fmt.Sprintf("%d minutes", int64(math.RoundToEven(mins)))
	}
	hours := mins / 60
	if hours < 36 {
		return strconv.FormatFloat(hours, 'f', 1, 64) + " hours"
	}
	return strconv.FormatFloat(hours/24, 'f', 1, 64) + " days"
}

// parseUTC reads a stored timestamp (naive = UTC).
func parseUTC(s string) *time.Time { return memory.ParseUTC(s) }

func round3(n, d int64) *pj.F {
	if d == 0 {
		return nil
	}
	v := pj.F(pyfloat.Round(float64(n)/float64(d), 3))
	return &v
}

// GetRecall wakes up: time anchor, active lessons, unexpired plans, per-source
// stats and the self-assessment. Any store may read. now is the wall clock.
func GetRecall(ctx context.Context, s *db.Store, now time.Time) (*Recall, error) {
	etNow := now.In(session.ET)
	today := etNow.Format(time.DateOnly)
	r := &Recall{Lessons: []Lesson{}, Stats: []Stat{}}

	kv := func(k string) string {
		v, _, _ := s.KVGet(ctx, k)
		return v
	}
	r.TimeAnchor = TimeAnchor{NowET: isoformat(etNow), TodayET: today, SinceLastDream: "never"}
	if d := kv("last_dream_date"); d != "" {
		if t, err := time.ParseInLocation(time.DateOnly, d, session.ET); err == nil {
			r.TimeAnchor.SinceLastDream = HumanGap(&t, now)
		}
	}
	r.TimeAnchor.SinceLastUser = HumanGap(parseUTC(kv("last_user_seen")), now)
	var lastTrade string
	_ = s.QueryRow(ctx, `SELECT ts FROM trades ORDER BY id DESC LIMIT 1`).Scan(&lastTrade)
	r.TimeAnchor.SinceLastTrade = HumanGap(parseUTC(lastTrade), now)

	rows, err := s.Query(ctx, `SELECT text, confidence, evidence FROM memory_lessons
		WHERE status='active' ORDER BY confidence DESC LIMIT ?`, RecallLessonsCap)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var l Lesson
		var conf float64
		var ev *string
		if err := rows.Scan(&l.Text, &conf, &ev); err != nil {
			rows.Close()
			return nil, err
		}
		l.Confidence = pj.F(pyfloat.Round(conf, 2))
		if ev != nil {
			var xs []any
			if json.Unmarshal([]byte(*ev), &xs) == nil {
				l.N = len(xs)
			}
		}
		r.Lessons = append(r.Lessons, l)
	}
	rows.Close()

	plans, err := activePlans(ctx, s, today)
	if err != nil {
		return nil, err
	}
	r.Plans = &plans

	rows, err = s.Query(ctx, `SELECT setup AS source, SUM(trips) AS trips, SUM(wins) AS wins
		FROM agg GROUP BY setup ORDER BY trips DESC`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var st Stat
		var trips, wins *int64
		if err := rows.Scan(&st.Source, &trips, &wins); err != nil {
			rows.Close()
			return nil, err
		}
		if b, ok := st.Source.([]byte); ok {
			st.Source = string(b)
		}
		if trips != nil {
			st.N = *trips
		}
		w := int64(0)
		if wins != nil {
			w = *wins
		}
		st.WinRate = round3(w, st.N)
		r.Stats = append(r.Stats, st)
	}
	rows.Close()

	sa, err := selfAssessment(ctx, s)
	if err != nil {
		return nil, err
	}
	r.SelfAssessment = *sa
	return r, nil
}

func activePlans(ctx context.Context, s *db.Store, today string) ([]Plan, error) {
	rows, err := s.Query(ctx, `SELECT text, expires_at, tags FROM memory_plans WHERE status='active' ORDER BY expires_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Plan{}
	for rows.Next() {
		var p Plan
		var exp, tags *string
		if err := rows.Scan(&p.Text, &exp, &tags); err != nil {
			return nil, err
		}
		e := ""
		if exp != nil {
			e = strings.TrimSpace(*exp)
		}
		if e != "" && prefix10(e) < today {
			continue // expired, even if decay has not run yet
		}
		if e != "" {
			p.ExpiresAt = &e
		}
		p.Tags = json.RawMessage("{}")
		if tags != nil && *tags != "" && json.Valid([]byte(*tags)) {
			p.Tags = json.RawMessage(*tags)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func prefix10(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

func selfAssessment(ctx context.Context, s *db.Store) (*SelfAssessment, error) {
	rows, err := s.Query(ctx, `SELECT pnl FROM trades
		WHERE `+db.StrategyRecord("")+` AND pnl IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	var pnls []float64
	wins := 0
	for rows.Next() {
		var p float64
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, err
		}
		pnls = append(pnls, p)
		if p > 0 {
			wins++
		}
	}
	rows.Close()
	sa := &SelfAssessment{N: len(pnls), WinRate: round3(int64(wins), int64(len(pnls)))}
	sa.NetPnL = 0 // Python: round(sum([]), 2) is the int 0
	if len(pnls) > 0 {
		sa.NetPnL = pj.F(pyfloat.Round(pyfloat.Sum(pnls), 2))
	}
	if sa.N < 30 {
		pct := 0.0
		if sa.WinRate != nil {
			pct = pyfloat.Round(float64(*sa.WinRate)*100, 1)
		}
		sa.Note = fmt.Sprintf("%d-trade sample, win %s%%, net %s — no proven edge yet; treat your own confidence as uncalibrated",
			sa.N, pj.Repr(pct), pj.Str(sa.NetPnL))
	} else if st, err := postmortem.Stats(ctx, s); err == nil {
		sa.ConfidenceBuckets = &st.ConfidenceBuckets
	}
	return sa, nil
}
