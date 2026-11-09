package db

import (
	"context"
	"database/sql"
	"errors"
)

// ReadTx runs fn against one consistent snapshot. Unlike Tx it begins a
// DEFERRED (read-only) transaction, so it takes no write lock: a watchdog
// reading many tables must not stall the writers it is auditing, yet must not
// see a writer half-way through (that would be a false "torn ledger").
// Nested calls reuse the outer transaction.
func (s *Store) ReadTx(ctx context.Context, fn func(*Store) error) (err error) {
	if s.inTx {
		return fn(s)
	}
	tx, err := s.db.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	inner := &Store{db: s.db, role: s.role, q: tx, inTx: true}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(inner); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return errors.Join(err, rbErr)
		}
		return err
	}
	return tx.Commit()
}
