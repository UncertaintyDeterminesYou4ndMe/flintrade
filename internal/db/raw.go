package db

import (
	"context"
	"database/sql"
)

// Query runs an ad-hoc read. Any role may read.
func (s *Store) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.q.QueryContext(ctx, query, args...)
}

// QueryRow runs an ad-hoc single-row read.
func (s *Store) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return s.q.QueryRowContext(ctx, query, args...)
}

// Exec runs an ad-hoc write that needs perm. Python modules wrote through the
// raw connection unchecked; here every raw write names the permission it
// exercises, so the single-writer invariant holds for raw SQL too.
func (s *Store) Exec(ctx context.Context, perm Perm, query string, args ...any) (sql.Result, error) {
	if s.db.readOnly {
		return nil, ErrReadOnly
	}
	if err := s.require(perm); err != nil {
		return nil, err
	}
	return s.q.ExecContext(ctx, query, args...)
}

// DayRoll resets the circuit breaker's daily baseline.
func (s *Store) DayRoll(ctx context.Context, dayStartEquity *float64) error {
	_, err := s.Exec(ctx, PermDayRoll,
		`UPDATE risk_state SET day_start_equity=?, day_realized_pnl=0, updated_at=? WHERE id=1`,
		dayStartEquity, Now())
	return err
}
