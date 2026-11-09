// Package db is Flintrade's data-access layer over SQLite (WAL), ported from
// agent/db.py.
//
// Core invariant — single writer per table, enforced by role (the role guard):
// producers (technical/event/user/news) may only submit intents and add
// signals/events; only executor/reconciler write positions/trades/orders;
// only executor/risk_monitor flip risk_state.halt.
//
// Concurrency: unlike Python's sqlite3, *sql.DB is a goroutine-safe pool, so
// the "connections must not cross threads" hard lesson is handled by the
// driver. Every pooled connection gets the same pragmas through the DSN, and
// transactions open with BEGIN IMMEDIATE (_txlock=immediate) so concurrent
// settlement by executor and reconciler serialises on the write lock.
package db

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// Role identifies who is holding a Store; it decides what the Store may write.
type Role string

const (
	RoleExecutor    Role = "executor"
	RoleReconciler  Role = "reconciler"
	RoleRiskMonitor Role = "risk_monitor"
	RoleTechnical   Role = "technical"
	RoleEvent       Role = "event"
	RoleUser        Role = "user"
	RoleNews        Role = "news"
	RoleReflect     Role = "reflect"
	RoleMigrate     Role = "migrate" // bootstrap / tests: full rights
	RoleReader      Role = "reader"  // dashboard / app: read only
)

// Perm is a protected write capability.
type Perm string

const (
	PermIntentsSubmit Perm = "intents_submit"
	// PermIntentsDecide: the reconciler finishes deferred settlements, so when
	// an order reaches a terminal state it must close out the intent too —
	// otherwise the intent sits in 'approved' forever (2026-08 review found a
	// dozen such orphans skewing every intent statistic).
	PermIntentsDecide Perm = "intents_decide"
	PermPositions     Perm = "positions"
	PermTrades        Perm = "trades"
	PermOrders        Perm = "orders"
	PermRiskState     Perm = "risk_state"
	PermHalt          Perm = "halt"
	PermSignals       Perm = "signals"
	PermEvents        Perm = "events"
	PermMemory        Perm = "memory"
	PermAgg           Perm = "agg"
	// PermDayRoll: the circuit breaker resets its own daily baseline
	// (day_start_equity, day_realized_pnl). Python did this through a raw-SQL
	// bypass of the guard; here it is an explicit, narrow permission.
	PermDayRoll Perm = "day_roll"
	permAll     Perm = "*"
)

func set(ps ...Perm) map[Perm]bool {
	m := make(map[Perm]bool, len(ps))
	for _, p := range ps {
		m[p] = true
	}
	return m
}

// writePerms mirrors _WRITE_PERMS in agent/db.py, plus PermDayRoll.
var writePerms = map[Role]map[Perm]bool{
	RoleExecutor:    set(PermIntentsDecide, PermPositions, PermTrades, PermOrders, PermRiskState, PermHalt),
	RoleReconciler:  set(PermIntentsDecide, PermPositions, PermTrades, PermOrders, PermRiskState, PermSignals),
	RoleRiskMonitor: set(PermHalt, PermIntentsSubmit, PermDayRoll),
	RoleTechnical:   set(PermIntentsSubmit, PermSignals, PermEvents),
	RoleEvent:       set(PermIntentsSubmit, PermSignals, PermEvents),
	RoleUser:        set(PermIntentsSubmit),
	RoleNews:        set(PermSignals, PermEvents),
	RoleReflect:     set(PermMemory, PermAgg),
	RoleMigrate:     set(permAll),
	RoleReader:      set(),
}

// PermissionError is returned when a role tries a write it does not own.
type PermissionError struct {
	Role Role
	Perm Perm
}

func (e *PermissionError) Error() string {
	return fmt.Sprintf("role=%q may not %q (single-writer invariant)", e.Role, e.Perm)
}

// ErrReadOnly is returned when a write is attempted through a read-only DB.
var ErrReadOnly = errors.New("db opened read-only")

// DB is a pool of connections to one flintrade.db file.
type DB struct {
	sql      *sql.DB
	path     string
	readOnly bool
}

// Open opens path for reading and writing with the same pragmas the Python
// layer set on every connection.
func Open(path string) (*DB, error) {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	return open(path, q, false)
}

// OpenReadOnly opens path with mode=ro: the desktop app's view of the ledger.
// The daemon is the only writer; the app must never take a write lock.
func OpenReadOnly(path string) (*DB, error) {
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	return open(path, q, true)
}

func open(path string, q url.Values, ro bool) (*DB, error) {
	dsn := "file:" + path + "?" + q.Encode()
	s, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := s.Ping(); err != nil {
		s.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return &DB{sql: s, path: path, readOnly: ro}, nil
}

// Close closes every pooled connection.
func (d *DB) Close() error { return d.sql.Close() }

// Path is the file this DB points at.
func (d *DB) Path() string { return d.path }

// As returns a Store acting as role. A read-only DB only hands out readers.
func (d *DB) As(role Role) (*Store, error) {
	if _, ok := writePerms[role]; !ok {
		return nil, fmt.Errorf("unknown role %q", role)
	}
	if d.readOnly && role != RoleReader {
		return nil, fmt.Errorf("%w: role %q needs a writable DB", ErrReadOnly, role)
	}
	return &Store{db: d, role: role, q: d.sql}, nil
}

// MustAs is As for roles known at compile time; it panics on misuse.
func (d *DB) MustAs(role Role) *Store {
	s, err := d.As(role)
	if err != nil {
		panic(err)
	}
	return s
}

// querier is satisfied by both *sql.DB and *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Store is a role-scoped handle. It is safe for concurrent use unless it was
// handed to a Tx callback, in which case it belongs to that callback.
type Store struct {
	db   *DB
	role Role
	q    querier
	inTx bool
}

// Role reports who this Store acts as.
func (s *Store) Role() Role { return s.role }

// Can reports whether this Store may perform p.
func (s *Store) Can(p Perm) bool {
	allowed := writePerms[s.role]
	return allowed[permAll] || allowed[p]
}

func (s *Store) require(p Perm) error {
	if s.Can(p) {
		return nil
	}
	return &PermissionError{Role: s.role, Perm: p}
}

// Tx runs fn as one atomic unit: any error or panic rolls everything back, so
// a crash mid-settlement can never leave half a trade (a position without its
// risk, a fill without its PnL). Nested calls reuse the outer transaction.
func (s *Store) Tx(ctx context.Context, fn func(*Store) error) (err error) {
	if s.inTx {
		return fn(s)
	}
	tx, err := s.db.sql.BeginTx(ctx, nil)
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

// nowFunc is the clock; tests in this package replace it.
var nowFunc = time.Now

// TimeLayout is the one timestamp format in the database: ISO8601 UTC, seconds.
const TimeLayout = "2006-01-02T15:04:05Z"

// FormatTime renders t the way every timestamp column stores it.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeLayout) }

// ParseTime parses a timestamp column value.
func ParseTime(s string) (time.Time, error) { return time.Parse(TimeLayout, s) }

// Now is the current time in database format.
func Now() string { return FormatTime(nowFunc()) }
