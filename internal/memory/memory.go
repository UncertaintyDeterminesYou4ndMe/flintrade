// Package memory builds the technical producer's `similar_history` block.
//
// It replaces agent/memory_store.py (LanceDB + sentence embeddings). That
// version embedded a numeric template ("NVDA score 7 RSI 61 vol 1.2 above
// VWAP"); sentence models do not see numeric distance, so 65% of what it
// recalled was simply the same symbol's past trades, and the model cited the
// block in 6.6% of replies (2026-11-09, 738 calls). The useful part is
// structured data, so it is now a deterministic SQL read: Flintrade's closed-trade
// record on the candidate's symbol, overall and in the current session, plus
// the latest closes. No embedding backend, no vector store.
package memory

import (
	"context"
	"strings"
	"time"

	"flintrade/internal/db"
	"flintrade/internal/producers/pj"
	"flintrade/internal/pyfloat"
	"flintrade/internal/session"
)

// Recent is the number of latest closes listed.
const Recent = 5

// filter keeps Flintrade's own closed trades: the same record self_assessment
// counts, so the two memory blocks never disagree.
var filter = "pnl IS NOT NULL AND " + db.StrategyRecord("")

// Summary is a win/loss record.
type Summary struct {
	Session string `json:"session,omitempty"`
	N       int    `json:"n"`
	Wins    int    `json:"wins"`
	WinRate *pj.F  `json:"win_rate"`
	NetPnL  pj.F   `json:"net_pnl"`
	AvgPnL  *pj.F  `json:"avg_pnl"`
}

// Close is one closed trade.
type Close struct {
	TradeID    int64  `json:"trade_id"`
	ClosedAt   string `json:"closed_at"`
	Side       string `json:"side"`
	Session    string `json:"session"`
	PnL        pj.F   `json:"pnl"`
	ExitReason string `json:"exit_reason"`
}

// Similar is the similar_history payload block.
type Similar struct {
	Symbol      string   `json:"symbol"`
	Summary     Summary  `json:"summary"`
	SameSession *Summary `json:"same_session,omitempty"`
	Recent      []Close  `json:"recent"`
}

var et = session.ET

// SessionOf buckets a UTC trade timestamp into an ET session: Pre 04:00–09:30,
// Intraday 09:30–16:00, Post 16:00–20:00, otherwise Overnight (also when the
// timestamp does not parse, as reflect.py did).
func SessionOf(ts string) string {
	t := ParseUTC(ts)
	if t == nil {
		return "Overnight"
	}
	e := t.In(et)
	m := e.Hour()*60 + e.Minute()
	switch {
	case m >= 4*60 && m < 9*60+30:
		return "Pre"
	case m >= 9*60+30 && m < 16*60:
		return "Intraday"
	case m >= 16*60 && m < 20*60:
		return "Post"
	}
	return "Overnight"
}

// ParseUTC accepts what reflect.py's _parse_utc (datetime.fromisoformat)
// did for stored timestamps: RFC 3339, or a naive timestamp read as UTC.
func ParseUTC(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05Z07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999", "2006-01-02 15:04:05.999999", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return &t
		}
	}
	return nil
}

func summarize(pnls []float64, sess string) Summary {
	s := Summary{Session: sess, N: len(pnls)}
	for _, p := range pnls {
		if p > 0 {
			s.Wins++
		}
	}
	if s.N > 0 {
		s.NetPnL = pj.F(pyfloat.Round(pyfloat.Sum(pnls), 2))
		wr := pj.F(pyfloat.Round(float64(s.Wins)/float64(s.N), 3))
		avg := pj.F(pyfloat.Round(pyfloat.Sum(pnls)/float64(s.N), 2))
		s.WinRate, s.AvgPnL = &wr, &avg
	}
	return s
}

// SimilarHistory returns symbol's closed-trade record, or nil when Flintrade has
// never closed a trade on it. currentSession is the live session label;
// Overnight-Pre counts as Overnight and Closed has no same-session figures.
func SimilarHistory(ctx context.Context, s *db.Store, symbol, currentSession string) (*Similar, error) {
	rows, err := s.Query(ctx, `SELECT id, ts, action, pnl, reason FROM trades
		WHERE symbol = ? AND `+filter+` ORDER BY id DESC`, symbol)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sess := currentSession
	if sess == session.OvernightPre {
		sess = session.Overnight
	}
	out := &Similar{Symbol: symbol, Recent: []Close{}}
	var all, same []float64
	for rows.Next() {
		var c Close
		var action string
		var reason *string
		if err := rows.Scan(&c.TradeID, &c.ClosedAt, &action, &c.PnL, &reason); err != nil {
			return nil, err
		}
		c.Session = SessionOf(c.ClosedAt)
		c.Side = "long"
		if action == "COVER" {
			c.Side = "short"
		}
		if reason != nil {
			c.ExitReason = pj.Truncate(*reason, 160)
		}
		all = append(all, float64(c.PnL))
		if c.Session == sess {
			same = append(same, float64(c.PnL))
		}
		if len(out.Recent) < Recent {
			out.Recent = append(out.Recent, c)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, nil
	}
	// Oldest-first sums, matching the order trades were booked.
	reverse(all)
	reverse(same)
	out.Summary = summarize(all, "")
	if sess != session.Closed && sess != "" {
		ss := summarize(same, sess)
		out.SameSession = &ss
	}
	return out, nil
}

func reverse(xs []float64) {
	for i, j := 0, len(xs)-1; i < j; i, j = i+1, j-1 {
		xs[i], xs[j] = xs[j], xs[i]
	}
}
