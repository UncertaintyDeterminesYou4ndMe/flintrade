package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Ptr returns a pointer to v; handy for the optional fields below.
func Ptr[T any](v T) *T { return &v }

// marshalJSON encodes a features/payload snapshot like Python's
// json.dumps(ensure_ascii=False): UTF-8 kept as is, no HTML escaping.
// nil stays SQL NULL.
func marshalJSON(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		if raw == nil {
			return nil, nil
		}
		return string(raw), nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// ── intents (the bus) ────────────────────────────────────────────────────

// IntentInput is what a producer submits. Quantity is only a hypothesis; the
// risk gate decides the real size.
type IntentInput struct {
	Source        string // defaults to the Store's role
	Priority      int
	Symbol        string
	Side          string // long | short | flatten | close
	HypothesisQty *int
	EntryHint     *float64
	Stop          *float64
	Target        *float64
	Confidence    *int
	DedupKey      *string
	Reason        *string
	Features      any
	ExpiresAt     *string
}

// SubmitIntent queues a pending intent. A dedup_key collision is not an error:
// it returns inserted=false. Unlike the Python layer, which swallowed every
// IntegrityError, only the dedup conflict is absorbed — a NOT NULL violation
// is a bug and surfaces.
func (s *Store) SubmitIntent(ctx context.Context, in IntentInput) (id int64, inserted bool, err error) {
	if err := s.require(PermIntentsSubmit); err != nil {
		return 0, false, err
	}
	src := in.Source
	if src == "" {
		src = string(s.role)
	}
	feats, err := marshalJSON(in.Features)
	if err != nil {
		return 0, false, fmt.Errorf("features: %w", err)
	}
	res, err := s.q.ExecContext(ctx,
		`INSERT INTO intents(source,priority,symbol,side,hypothesis_qty,entry_hint,
		     stop,target,confidence,dedup_key,reason,features,status,created_at,expires_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,'pending',?,?)
		 ON CONFLICT(dedup_key) WHERE dedup_key IS NOT NULL DO NOTHING`,
		src, in.Priority, in.Symbol, in.Side, in.HypothesisQty, in.EntryHint, in.Stop,
		in.Target, in.Confidence, in.DedupKey, in.Reason, feats, Now(), in.ExpiresAt)
	if err != nil {
		return 0, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	if n == 0 {
		return 0, false, nil
	}
	id, err = res.LastInsertId()
	return id, err == nil, err
}

// Intent is one row of the intents table.
type Intent struct {
	ID            int64
	Source        string
	Priority      int
	Symbol        string
	Side          string
	HypothesisQty *int
	EntryHint     *float64
	Stop          *float64
	Target        *float64
	Confidence    *int
	DedupKey      *string
	Reason        *string
	Features      json.RawMessage
	Status        string
	RejectReason  *string
	CreatedAt     string
	DecidedAt     *string
	ExpiresAt     *string
}

const intentCols = `id,source,priority,symbol,side,hypothesis_qty,entry_hint,stop,target,
	confidence,dedup_key,reason,features,status,reject_reason,created_at,decided_at,expires_at`

func scanIntent(sc interface{ Scan(...any) error }) (Intent, error) {
	var it Intent
	var feats *string
	err := sc.Scan(&it.ID, &it.Source, &it.Priority, &it.Symbol, &it.Side, &it.HypothesisQty,
		&it.EntryHint, &it.Stop, &it.Target, &it.Confidence, &it.DedupKey, &it.Reason, &feats,
		&it.Status, &it.RejectReason, &it.CreatedAt, &it.DecidedAt, &it.ExpiresAt)
	if feats != nil {
		it.Features = json.RawMessage(*feats)
	}
	return it, err
}

// ClaimIntents returns pending intents, highest priority first, then oldest.
func (s *Store) ClaimIntents(ctx context.Context, limit int) ([]Intent, error) {
	return queryAll(ctx, s.q, scanIntent,
		"SELECT "+intentCols+" FROM intents WHERE status='pending' ORDER BY priority DESC, created_at LIMIT ?", limit)
}

// Intent fetches one intent by id; (nil, nil) when absent.
func (s *Store) Intent(ctx context.Context, id int64) (*Intent, error) {
	return queryOne(ctx, s.q, scanIntent, "SELECT "+intentCols+" FROM intents WHERE id=?", id)
}

// DecideIntent records the executor's verdict:
// approved | rejected | filled | expired | cancelled.
func (s *Store) DecideIntent(ctx context.Context, id int64, status string, rejectReason *string) error {
	return s.DecideIntentCoded(ctx, id, status, rejectReason, nil)
}

// DecideIntentCoded is DecideIntent with the reason also as a code
// (reasoncode.Code.JSON), which Flintrade.app says in its own language.
func (s *Store) DecideIntentCoded(ctx context.Context, id int64, status string, rejectReason, rejectCode *string) error {
	if err := s.require(PermIntentsDecide); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx,
		"UPDATE intents SET status=?, reject_reason=?, reject_code=?, decided_at=? WHERE id=?",
		status, rejectReason, rejectCode, Now(), id)
	return err
}

// ── positions ────────────────────────────────────────────────────────────

// PositionInput opens a position.
type PositionInput struct {
	Symbol     string
	Side       string // long | short
	Qty        int
	EntryPrice float64
	Stop       *float64
	Target     *float64
	Target2    *float64
	RiskAmt    *float64
	Source     *string
	IntentID   *int64
}

// Position is one row of the positions table.
type Position struct {
	ID         int64
	Symbol     string
	Side       string
	Qty        int
	EntryPrice float64
	Stop       *float64
	Target     *float64
	Target2    *float64
	T1Done     bool
	RiskAmt    *float64
	Source     *string
	IntentID   *int64
	OpenedAt   string
	Status     string
}

const positionCols = `id,symbol,side,qty,entry_price,stop,target,target2,t1_done,risk_amt,
	source,intent_id,opened_at,status`

func scanPosition(sc interface{ Scan(...any) error }) (Position, error) {
	var p Position
	err := sc.Scan(&p.ID, &p.Symbol, &p.Side, &p.Qty, &p.EntryPrice, &p.Stop, &p.Target,
		&p.Target2, &p.T1Done, &p.RiskAmt, &p.Source, &p.IntentID, &p.OpenedAt, &p.Status)
	return p, err
}

// OpenPosition inserts an open position and returns its id.
func (s *Store) OpenPosition(ctx context.Context, in PositionInput) (int64, error) {
	if err := s.require(PermPositions); err != nil {
		return 0, err
	}
	res, err := s.q.ExecContext(ctx,
		`INSERT INTO positions(symbol,side,qty,entry_price,stop,target,target2,
		     risk_amt,source,intent_id,opened_at,status)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,'open')`,
		in.Symbol, in.Side, in.Qty, in.EntryPrice, in.Stop, in.Target, in.Target2,
		in.RiskAmt, in.Source, in.IntentID, Now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ClosePosition marks a position closed.
func (s *Store) ClosePosition(ctx context.Context, id int64) error {
	if err := s.require(PermPositions); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, "UPDATE positions SET status='closed' WHERE id=?", id)
	return err
}

// ReducePosition shrinks a position after a partial exit. Any partial exit
// counts as the first take-profit having been taken (t1_done=1), which
// switches the take-profit guard over to target2.
func (s *Store) ReducePosition(ctx context.Context, id int64, qty int, riskAmt *float64) error {
	if err := s.require(PermPositions); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx,
		"UPDATE positions SET qty=?, risk_amt=?, t1_done=1 WHERE id=?", qty, riskAmt, id)
	return err
}

// OpenPositions lists every open position.
func (s *Store) OpenPositions(ctx context.Context) ([]Position, error) {
	return queryAll(ctx, s.q, scanPosition,
		"SELECT "+positionCols+" FROM positions WHERE status='open' ORDER BY id")
}

// PositionFor returns the open position in symbol; (nil, nil) when flat.
func (s *Store) PositionFor(ctx context.Context, symbol string) (*Position, error) {
	return queryOne(ctx, s.q, scanPosition,
		"SELECT "+positionCols+" FROM positions WHERE status='open' AND symbol=? ORDER BY id LIMIT 1", symbol)
}

// ── trades (append-only ledger) ──────────────────────────────────────────

// TradeInput appends one fill to the ledger.
type TradeInput struct {
	Symbol        string
	Action        string // BUY | SELL | SHORT | COVER
	Qty           int
	FillPrice     float64
	Commission    float64
	PnL           *float64 // only on closing legs
	Source        *string
	IntentID      *int64
	BrokerOrderID *string
	PositionID    *int64
	Features      any
	Reason        *string
	Attribution   *string
}

// Trade is one row of the trades table.
type Trade struct {
	ID            int64
	TS            string
	Symbol        string
	Action        string
	Qty           int
	FillPrice     float64
	Commission    float64
	PnL           *float64
	Source        *string
	IntentID      *int64
	BrokerOrderID *string
	PositionID    *int64
	Features      json.RawMessage
	Reason        *string
	Attribution   *string
}

const tradeCols = `id,ts,symbol,action,qty,fill_price,commission,pnl,source,intent_id,
	broker_order_id,position_id,features,reason,attribution`

func scanTrade(sc interface{ Scan(...any) error }) (Trade, error) {
	var t Trade
	var feats *string
	err := sc.Scan(&t.ID, &t.TS, &t.Symbol, &t.Action, &t.Qty, &t.FillPrice, &t.Commission,
		&t.PnL, &t.Source, &t.IntentID, &t.BrokerOrderID, &t.PositionID, &feats, &t.Reason,
		&t.Attribution)
	if feats != nil {
		t.Features = json.RawMessage(*feats)
	}
	return t, err
}

// AppendTrade writes one fill and returns its id.
func (s *Store) AppendTrade(ctx context.Context, in TradeInput) (int64, error) {
	if err := s.require(PermTrades); err != nil {
		return 0, err
	}
	feats, err := marshalJSON(in.Features)
	if err != nil {
		return 0, fmt.Errorf("features: %w", err)
	}
	res, err := s.q.ExecContext(ctx,
		`INSERT INTO trades(ts,symbol,action,qty,fill_price,commission,pnl,source,
		     intent_id,broker_order_id,position_id,features,reason,attribution)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		Now(), in.Symbol, in.Action, in.Qty, in.FillPrice, in.Commission, in.PnL, in.Source,
		in.IntentID, in.BrokerOrderID, in.PositionID, feats, in.Reason, in.Attribution)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RecentTrades returns the latest trades, newest first.
func (s *Store) RecentTrades(ctx context.Context, limit int) ([]Trade, error) {
	return queryAll(ctx, s.q, scanTrade, "SELECT "+tradeCols+" FROM trades ORDER BY id DESC LIMIT ?", limit)
}

// ── orders (broker mirror) ───────────────────────────────────────────────

// OrderInput creates a local order record before it goes to the broker.
type OrderInput struct {
	ClientOrderID string
	Symbol        string
	Side          string // BUY | SELL | SHORT | COVER
	Qty           int
	Price         *float64
	OutsideRTH    *string
	IntentID      *int64
}

// Order is one row of the orders table.
type Order struct {
	ID            int64
	ClientOrderID string
	BrokerOrderID *string
	Symbol        string
	Side          string
	Qty           int
	Price         *float64
	OutsideRTH    *string
	Status        string
	FilledQty     int
	AvgPrice      *float64
	IntentID      *int64
	CreatedAt     string
	UpdatedAt     *string
}

const orderCols = `id,client_order_id,broker_order_id,symbol,side,qty,price,outside_rth,status,
	filled_qty,avg_price,intent_id,created_at,updated_at`

func scanOrder(sc interface{ Scan(...any) error }) (Order, error) {
	var o Order
	err := sc.Scan(&o.ID, &o.ClientOrderID, &o.BrokerOrderID, &o.Symbol, &o.Side, &o.Qty,
		&o.Price, &o.OutsideRTH, &o.Status, &o.FilledQty, &o.AvgPrice, &o.IntentID,
		&o.CreatedAt, &o.UpdatedAt)
	return o, err
}

// CreateOrder records a submitted order and returns its id.
func (s *Store) CreateOrder(ctx context.Context, in OrderInput) (int64, error) {
	if err := s.require(PermOrders); err != nil {
		return 0, err
	}
	res, err := s.q.ExecContext(ctx,
		`INSERT INTO orders(client_order_id,symbol,side,qty,price,outside_rth,
		     intent_id,status,created_at)
		 VALUES(?,?,?,?,?,?,?,'submitted',?)`,
		in.ClientOrderID, in.Symbol, in.Side, in.Qty, in.Price, in.OutsideRTH, in.IntentID, Now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Fields is a column → value patch for UpdateOrder / UpdateRisk. Column names
// are checked against a whitelist (the Python version interpolated whatever
// keyword it was given straight into SQL).
type Fields map[string]any

var orderUpdatable = map[string]bool{
	"broker_order_id": true, "symbol": true, "side": true, "qty": true, "price": true,
	"outside_rth": true, "status": true, "filled_qty": true, "avg_price": true, "intent_id": true,
}

var riskUpdatable = map[string]bool{
	"equity": true, "day_start_equity": true, "day_realized_pnl": true,
	"open_risk": true, "halt": true, "halt_reason": true,
}

func setClause(f Fields, allowed map[string]bool, table string) (string, []any, error) {
	if len(f) == 0 {
		return "", nil, errors.New("empty update for " + table)
	}
	keys := make([]string, 0, len(f))
	for k := range f {
		if !allowed[k] {
			return "", nil, fmt.Errorf("%s.%s is not updatable", table, k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	args := make([]any, len(keys))
	for i, k := range keys {
		parts[i] = k + "=?"
		args[i] = f[k]
	}
	return strings.Join(parts, ", "), args, nil
}

// UpdateOrder patches an order by client_order_id and stamps updated_at.
func (s *Store) UpdateOrder(ctx context.Context, clientOrderID string, f Fields) error {
	if err := s.require(PermOrders); err != nil {
		return err
	}
	set, args, err := setClause(f, orderUpdatable, "orders")
	if err != nil {
		return err
	}
	args = append(args, Now(), clientOrderID)
	_, err = s.q.ExecContext(ctx, "UPDATE orders SET "+set+", updated_at=? WHERE client_order_id=?", args...)
	return err
}

// OpenOrders lists orders still working at the broker.
func (s *Store) OpenOrders(ctx context.Context) ([]Order, error) {
	return queryAll(ctx, s.q, scanOrder,
		"SELECT "+orderCols+" FROM orders WHERE status IN ('submitted','partial') ORDER BY id")
}

// ── risk state (single row id=1) ─────────────────────────────────────────

// RiskState is the portfolio risk row.
type RiskState struct {
	Equity         *float64
	DayStartEquity *float64
	DayRealizedPnL float64
	OpenRisk       float64
	Halt           bool
	HaltReason     *string
	UpdatedAt      *string
}

// GetRisk reads the risk row; (nil, nil) on an uninitialised database.
func (s *Store) GetRisk(ctx context.Context) (*RiskState, error) {
	return queryOne(ctx, s.q, func(sc interface{ Scan(...any) error }) (RiskState, error) {
		var r RiskState
		err := sc.Scan(&r.Equity, &r.DayStartEquity, &r.DayRealizedPnL, &r.OpenRisk,
			&r.Halt, &r.HaltReason, &r.UpdatedAt)
		return r, err
	}, `SELECT equity,day_start_equity,day_realized_pnl,open_risk,halt,halt_reason,updated_at
	    FROM risk_state WHERE id=1`)
}

// UpdateRisk patches the risk row and stamps updated_at.
func (s *Store) UpdateRisk(ctx context.Context, f Fields) error {
	if err := s.require(PermRiskState); err != nil {
		return err
	}
	set, args, err := setClause(f, riskUpdatable, "risk_state")
	if err != nil {
		return err
	}
	args = append(args, Now())
	_, err = s.q.ExecContext(ctx, "UPDATE risk_state SET "+set+", updated_at=? WHERE id=1", args...)
	return err
}

// SetHalt flips the global kill switch (risk_monitor / executor).
func (s *Store) SetHalt(ctx context.Context, halt bool, reason *string) error {
	if err := s.require(PermHalt); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx,
		"UPDATE risk_state SET halt=?, halt_reason=?, updated_at=? WHERE id=1", halt, reason, Now())
	return err
}

// IsHalted reports the kill switch.
func (s *Store) IsHalted(ctx context.Context) (bool, error) {
	r, err := s.GetRisk(ctx)
	if err != nil || r == nil {
		return false, err
	}
	return r.Halt, nil
}

// ── signals / events ─────────────────────────────────────────────────────

// SignalInput records a raw signal for provenance.
type SignalInput struct {
	Source  string // defaults to the Store's role
	Symbol  *string
	Kind    *string
	Payload any
}

// AddSignal appends a signal and returns its id.
func (s *Store) AddSignal(ctx context.Context, in SignalInput) (int64, error) {
	if err := s.require(PermSignals); err != nil {
		return 0, err
	}
	src := in.Source
	if src == "" {
		src = string(s.role)
	}
	payload, err := marshalJSON(in.Payload)
	if err != nil {
		return 0, fmt.Errorf("payload: %w", err)
	}
	res, err := s.q.ExecContext(ctx,
		"INSERT INTO signals(source,symbol,kind,payload,ts) VALUES(?,?,?,?,?)",
		src, in.Symbol, in.Kind, payload, Now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// EventInput tracks a catalyst (earnings, CPI, FOMC …).
type EventInput struct {
	Symbol  *string
	Kind    *string
	Title   *string
	FiresAt *string
	Payload any
}

// AddEvent appends a pending event and returns its id.
func (s *Store) AddEvent(ctx context.Context, in EventInput) (int64, error) {
	if err := s.require(PermEvents); err != nil {
		return 0, err
	}
	payload, err := marshalJSON(in.Payload)
	if err != nil {
		return 0, fmt.Errorf("payload: %w", err)
	}
	res, err := s.q.ExecContext(ctx,
		"INSERT INTO events(symbol,kind,title,fires_at,payload,status,ts) VALUES(?,?,?,?,?,'pending',?)",
		in.Symbol, in.Kind, in.Title, in.FiresAt, payload, Now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ── heartbeats ───────────────────────────────────────────────────────────

// Heartbeat is one row of the heartbeats table.
type Heartbeat struct {
	Process  string
	LastBeat string
	PID      *int
	Note     *string
}

// Beat upserts process's heartbeat (defaults to the role name) with this pid.
// Not role-guarded: every loop must be able to say it is alive.
func (s *Store) Beat(ctx context.Context, process string, note *string) error {
	if s.db.readOnly {
		return ErrReadOnly
	}
	if process == "" {
		process = string(s.role)
	}
	_, err := s.q.ExecContext(ctx,
		`INSERT INTO heartbeats(process,last_beat,pid,note) VALUES(?,?,?,?)
		 ON CONFLICT(process) DO UPDATE SET last_beat=excluded.last_beat,
		     pid=excluded.pid, note=excluded.note`,
		process, Now(), os.Getpid(), note)
	return err
}

// Heartbeats lists every process heartbeat.
func (s *Store) Heartbeats(ctx context.Context) ([]Heartbeat, error) {
	return queryAll(ctx, s.q, func(sc interface{ Scan(...any) error }) (Heartbeat, error) {
		var h Heartbeat
		err := sc.Scan(&h.Process, &h.LastBeat, &h.PID, &h.Note)
		return h, err
	}, "SELECT process,last_beat,pid,note FROM heartbeats ORDER BY process")
}

// ── kv ───────────────────────────────────────────────────────────────────

// KVGet reads a key; ok=false when absent.
func (s *Store) KVGet(ctx context.Context, key string) (value string, ok bool, err error) {
	var v *string
	err = s.q.QueryRowContext(ctx, "SELECT value FROM kv WHERE key=?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if v == nil {
		return "", true, nil
	}
	return *v, true, nil
}

// KVSet upserts a key. Not role-guarded (same as Python): kv holds loop
// bookkeeping such as last_dream_date and invariants_last.
func (s *Store) KVSet(ctx context.Context, key, value string) error {
	if s.db.readOnly {
		return ErrReadOnly
	}
	_, err := s.q.ExecContext(ctx,
		"INSERT INTO kv(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
		key, value)
	return err
}

// ── helpers ──────────────────────────────────────────────────────────────

func queryAll[T any](ctx context.Context, q querier, scan func(interface{ Scan(...any) error }) (T, error),
	query string, args ...any) ([]T, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func queryOne[T any](ctx context.Context, q querier, scan func(interface{ Scan(...any) error }) (T, error),
	query string, args ...any) (*T, error) {
	v, err := scan(q.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}
