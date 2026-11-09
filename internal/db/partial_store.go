package db

import (
	"context"
	"database/sql"
	"errors"
)

// Booked is what the ledger already holds for one broker order: the shares
// booked, their notional, and the position the first fill opened or closed.
type Booked struct {
	Qty        int
	Notional   float64
	PositionID *int64
}

// BookedFor sums the trades carrying brokerOrderID. It is the settlement
// idempotency key for partial fills: whatever the broker reports beyond this
// is new, and nothing at or below it is ever booked twice.
func (s *Store) BookedFor(ctx context.Context, brokerOrderID string) (Booked, error) {
	var b Booked
	var qty sql.NullInt64
	var notional sql.NullFloat64
	err := s.q.QueryRowContext(ctx, `SELECT SUM(qty), SUM(qty*fill_price),
		(SELECT position_id FROM trades WHERE broker_order_id=? ORDER BY id LIMIT 1)
		FROM trades WHERE broker_order_id=?`, brokerOrderID, brokerOrderID).Scan(&qty, &notional, &b.PositionID)
	if errors.Is(err, sql.ErrNoRows) {
		return b, nil
	}
	b.Qty, b.Notional = int(qty.Int64), notional.Float64
	return b, err
}

// Position returns position id (open or closed), or nil.
func (s *Store) Position(ctx context.Context, id int64) (*Position, error) {
	return queryOne(ctx, s.q, scanPosition, "SELECT "+positionCols+" FROM positions WHERE id=?", id)
}

// AddToPosition grows a position by a later fill of the order that opened
// it: new qty, blended entry price, recomputed risk.
func (s *Store) AddToPosition(ctx context.Context, id int64, qty int, entryPrice float64, riskAmt *float64) error {
	if err := s.require(PermPositions); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, "UPDATE positions SET qty=?, entry_price=?, risk_amt=? WHERE id=?", qty, entryPrice, riskAmt, id)
	return err
}

// ShrinkPosition reduces a position by part of a full exit that is still
// filling. Unlike ReducePosition it does not mark the first take-profit done.
func (s *Store) ShrinkPosition(ctx context.Context, id int64, qty int, riskAmt *float64) error {
	if err := s.require(PermPositions); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, "UPDATE positions SET qty=?, risk_amt=? WHERE id=?", qty, riskAmt, id)
	return err
}
