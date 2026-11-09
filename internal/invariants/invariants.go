// Package invariants is the ledger's independent night watch, ported from
// agent/invariants.py. Read-only: a violation is logged and recorded in kv
// 'invariants_last'; nothing is halted or repaired.
//
// Each check names a real class of torn ledger:
//
//	open_risk_ledger    risk_state.open_risk == Σ open risk_amt (settle's
//	                    max(0,·) clamp would otherwise swallow drift)
//	filled_has_trade    a filled intent has its trade
//	approved_has_order  an approved intent has an order (the 2026-08 orphans)
//	filled_order_trade  a filled order has a trade with its broker_order_id
//	position_sane       open rows: qty > 0, entry > 0, side long|short
//	no_hedged_symbol    never long and short the same symbol
//	trade_leg_pnl       closing legs carry pnl, opening legs do not
//	equity_sane         initialised equity is > 0
//
// Windowed checks look back 14 days; old residue is cleaned by hand once,
// not re-reported forever.
package invariants

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"flintrade/internal/db"
	"flintrade/internal/loop"
	"flintrade/internal/pyfloat"
)

const (
	riskTolerance    = 0.05 // settle rounds to 4 dp; cent-level drift is not a tear
	windowDays       = 14
	approvedGraceMin = 30               // approved may wait this long for its order
	settleGrace      = 60 * time.Second // update_order(filled) → settle transient
)

// Checker implements loop.Loop.
type Checker struct {
	d     *loop.Deps
	store *db.Store
}

// New builds the checker with the read-only reader role.
func New(d *loop.Deps) *Checker { return &Checker{d: d, store: d.DB.MustAs(db.RoleReader)} }

func (c *Checker) Name() string { return "invariants" }

// RunOnce runs every check on one snapshot, records the result in kv and
// returns "VIOLATION …" lines (none when healthy).
func (c *Checker) RunOnce(ctx context.Context) ([]string, error) {
	var v []string
	// All SELECTs must see one snapshot, or a writer half-way through a
	// settlement reads as a tear. ReadTx is deferred: no write lock taken.
	err := c.store.ReadTx(ctx, func(s *db.Store) error {
		var err error
		v, err = Check(ctx, s, c.d.Time())
		return err
	})
	if err != nil {
		return nil, err
	}
	rec, _ := json.Marshal(map[string]any{"ts": db.Now(), "ok": len(v) == 0, "violations": orEmpty(v)})
	if err := c.store.KVSet(ctx, "invariants_last", string(rec)); err != nil {
		return nil, err
	}
	note := "ok"
	if len(v) > 0 {
		note = fmt.Sprintf("%d violations", len(v))
	}
	if err := c.store.Beat(ctx, "invariants", &note); err != nil {
		return nil, err
	}
	out := make([]string, len(v))
	for i, x := range v {
		out[i] = "VIOLATION " + x
	}
	return out, nil
}

func orEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// Check runs every assertion and returns the violations. The caller provides
// read consistency.
func Check(ctx context.Context, s *db.Store, now time.Time) ([]string, error) {
	var v []string
	window := db.FormatTime(now.AddDate(0, 0, -windowDays))
	grace := db.FormatTime(now.Add(-approvedGraceMin * time.Minute))

	r, err := s.GetRisk(ctx)
	if err != nil {
		return nil, err
	}
	if r != nil && r.Equity != nil { // NULL equity = ledger not initialised
		// SUM over no rows is the integer 0 in SQLite; keep its type for the
		// message (Python printed "0", not "0.0").
		var raw any
		if err := s.QueryRow(ctx, `SELECT COALESCE(SUM(COALESCE(risk_amt,0)),0) FROM positions WHERE status='open'`).Scan(&raw); err != nil {
			return nil, err
		}
		var ledger float64
		shown := ""
		switch x := raw.(type) {
		case int64:
			ledger, shown = float64(x), strconv.FormatInt(x, 10)
		case float64:
			ledger, shown = x, pyStr(pyfloat.Round(x, 4))
		}
		if math.Abs(r.OpenRisk-ledger) > riskTolerance {
			v = append(v, fmt.Sprintf("open_risk_ledger: risk_state.open_risk=%s ≠ Σ开仓risk_amt=%s", pyStr(r.OpenRisk), shown))
		}
		if *r.Equity <= 0 {
			v = append(v, fmt.Sprintf("equity_sane: equity=%s ≤ 0(会计已崩)", pyStr(*r.Equity)))
		}
	}

	ids, err := ints(ctx, s, `SELECT i.id FROM intents i
		WHERE i.status='filled' AND i.decided_at >= ?
		  AND NOT EXISTS (SELECT 1 FROM trades t WHERE t.intent_id = i.id)`, window)
	if err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		v = append(v, fmt.Sprintf("filled_has_trade: intent %s 标记 filled 但 trades 无对应成交", pyList(ids)))
	}

	ids, err = ints(ctx, s, `SELECT i.id FROM intents i
		WHERE i.status='approved' AND i.decided_at < ?
		  AND NOT EXISTS (SELECT 1 FROM orders o WHERE o.intent_id = i.id)`, grace)
	if err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		v = append(v, fmt.Sprintf("approved_has_order: intent %s approved 超过 %dmin 却没有任何 order(孤儿)", pyList(ids), approvedGraceMin))
	}

	coids, err := strs(ctx, s, `SELECT o.client_order_id FROM orders o
		WHERE o.status='filled' AND o.broker_order_id IS NOT NULL
		  AND o.created_at >= ?
		  AND COALESCE(o.updated_at, o.created_at) < ?
		  AND NOT EXISTS (SELECT 1 FROM trades t WHERE t.broker_order_id = o.broker_order_id)`,
		window, db.FormatTime(now.Add(-settleGrace)))
	if err != nil {
		return nil, err
	}
	if len(coids) > 0 {
		v = append(v, fmt.Sprintf("filled_order_trade: order %s 状态 filled 但账本无同 broker_order_id 成交", pyStrList(coids)))
	}

	rows, err := s.Query(ctx, `SELECT id, symbol, side, qty, entry_price FROM positions
		WHERE status='open' AND (qty <= 0 OR entry_price <= 0 OR side NOT IN ('long','short'))`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var sym, side string
		var qty int
		var entry float64
		if err := rows.Scan(&id, &sym, &side, &qty, &entry); err != nil {
			rows.Close()
			return nil, err
		}
		v = append(v, fmt.Sprintf("position_sane: position #%d %s side=%s qty=%d entry=%s", id, sym, side, qty, pyStr(entry)))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	hedged, err := strs(ctx, s, `SELECT symbol FROM positions WHERE status='open'
		GROUP BY symbol HAVING COUNT(DISTINCT side) > 1`)
	if err != nil {
		return nil, err
	}
	for _, sym := range hedged {
		v = append(v, fmt.Sprintf("no_hedged_symbol: %s 同时持有多空开仓行", sym))
	}

	// Backfilled rows attributed 'test' are not bound by the live path.
	rows, err = s.Query(ctx, `SELECT id, action FROM trades
		WHERE ts >= ? AND COALESCE(attribution,'') != 'test'
		  AND ((action IN ('SELL','COVER') AND pnl IS NULL)
		    OR (action IN ('BUY','SHORT') AND pnl IS NOT NULL))`, window)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var action string
		if err := rows.Scan(&id, &action); err != nil {
			return nil, err
		}
		v = append(v, fmt.Sprintf("trade_leg_pnl: trade #%d action=%s 的 pnl 语义不符", id, action))
	}
	return v, rows.Err()
}

func ints(ctx context.Context, s *db.Store, q string, args ...any) ([]int64, error) {
	rows, err := s.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var x int64
		if err := rows.Scan(&x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func strs(ctx context.Context, s *db.Store, q string, args ...any) ([]string, error) {
	rows, err := s.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var x string
		if err := rows.Scan(&x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// pyList / pyStrList render like Python's list repr, as the old messages did.
func pyList(xs []int64) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.FormatInt(x, 10)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func pyStrList(xs []string) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = "'" + x + "'"
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func pyStr(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".eEnN") {
		s += ".0"
	}
	return s
}
