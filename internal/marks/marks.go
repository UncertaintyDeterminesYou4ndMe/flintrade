// Package marks keeps the last price seen for each open position, so the
// app can show floating P&L without asking for quotes of its own.
//
// The risk monitor already fetches one batched quote every round for the stop
// guard. It stores those prices here (kv rows, one per symbol), and at the day
// roll it stores the floating P&L of the positions it carries into the new day.
// The app reads both. No extra quote request is made for display, and the
// price the app shows is the price the stop guard judged.
//
// Display only: the drawdown breaker and sizing still use realized equity.
package marks

import (
	"context"
	"encoding/json"
	"time"

	"flintrade/internal/db"
)

const (
	markPrefix  = "mark:"
	dayFloatKey = "day_start_float"
)

// Mark is the last price the risk monitor saw for one symbol.
type Mark struct {
	Price   float64 `json:"price"`
	At      string  `json:"at"` // UTC, db.TimeLayout
	Session string  `json:"session"`
}

// Time parses At; the zero time when it does not parse.
func (m Mark) Time() time.Time {
	t, _ := db.ParseTime(m.At)
	return t
}

// Put stores the prices of one round, all stamped with the same time.
func Put(ctx context.Context, s *db.Store, prices map[string]float64, sess string, at time.Time) error {
	ts := db.FormatTime(at)
	for sym, p := range prices {
		b, err := json.Marshal(Mark{Price: p, At: ts, Session: sess})
		if err != nil {
			return err
		}
		if err := s.KVSet(ctx, markPrefix+sym, string(b)); err != nil {
			return err
		}
	}
	return nil
}

// Get returns the stored marks of the given symbols; a symbol never priced is
// absent.
func Get(ctx context.Context, s *db.Store, symbols []string) (map[string]Mark, error) {
	out := map[string]Mark{}
	for _, sym := range symbols {
		v, ok, err := s.KVGet(ctx, markPrefix+sym)
		if err != nil {
			return nil, err
		}
		var m Mark
		if !ok || json.Unmarshal([]byte(v), &m) != nil || m.Price <= 0 {
			continue
		}
		out[sym] = m
	}
	return out, nil
}

// PnL is the floating gross P&L of one position at a price. Exit commission
// is not included.
func PnL(p db.Position, price float64) float64 {
	if p.Side == "short" {
		return (p.EntryPrice - price) * float64(p.Qty)
	}
	return (price - p.EntryPrice) * float64(p.Qty)
}

// Float is the floating P&L of a set of positions. Complete is false when
// at least one position has no mark, in which case Sum covers only the
// priced ones.
type Float struct {
	Sum      float64 `json:"sum"`
	Complete bool    `json:"complete"`
}

// Total sums the floating P&L of positions over their marks.
func Total(positions []db.Position, ms map[string]Mark) Float {
	f := Float{Complete: true}
	for _, p := range positions {
		m, ok := ms[p.Symbol]
		if !ok {
			f.Complete = false
			continue
		}
		f.Sum += PnL(p, m.Price)
	}
	return f
}

// DayStart is the floating P&L carried into an ET trading day.
type DayStart struct {
	Day string `json:"day"` // ET date, as risk_day
	Float
}

// PutDayStart records the floating P&L of the open positions at the day roll.
func PutDayStart(ctx context.Context, s *db.Store, day string) error {
	pos, err := s.OpenPositions(ctx)
	if err != nil {
		return err
	}
	syms := make([]string, len(pos))
	for i, p := range pos {
		syms[i] = p.Symbol
	}
	ms, err := Get(ctx, s, syms)
	if err != nil {
		return err
	}
	b, err := json.Marshal(DayStart{Day: day, Float: Total(pos, ms)})
	if err != nil {
		return err
	}
	return s.KVSet(ctx, dayFloatKey, string(b))
}

// GetDayStart reads the day-start floating P&L; ok=false when none is stored
// or it is unreadable.
func GetDayStart(ctx context.Context, s *db.Store) (DayStart, bool, error) {
	v, ok, err := s.KVGet(ctx, dayFloatKey)
	if err != nil || !ok {
		return DayStart{}, false, err
	}
	var d DayStart
	if json.Unmarshal([]byte(v), &d) != nil || d.Day == "" {
		return DayStart{}, false, nil
	}
	return d, true, nil
}
