// Package reconciler is the ledger's autonomic nervous system, ported from
// agent/reconciler.py. Each round it pulls out-of-band broker state back into
// flintrade.db, the single source of truth:
//
//  1. settle resting orders: submitted/partial orders are looked up with
//     order_detail; fills are booked through internal/settle (the one
//     accounting path shared with the executor), terminal no-fills are closed
//     out — and in both cases the intent leaves 'approved'.
//  2. equity sync (live only, never in fixed-sleeve mode).
//  3. drift detection (live only): broker positions vs open positions. It
//     reports, it never invents positions (unless adopt_untracked_drift).
//
// Each duty fails on its own; one failing never stops the others.
package reconciler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"

	"flintrade/internal/db"
	"flintrade/internal/loop"
	"flintrade/internal/pyfloat"
	"flintrade/internal/settle"
)

// Reconciler implements loop.Loop.
type Reconciler struct {
	d     *loop.Deps
	store *db.Store

	mu sync.Mutex
	// lastDrift: symbol → last reported delta (broker − db). A drift is
	// reported only when its delta changes — reporting an unchanged drift
	// every 15 s is how 31,606 identical alerts piled up.
	lastDrift map[string]int
}

// New builds the reconciler with the reconciler role.
func New(d *loop.Deps) *Reconciler {
	return &Reconciler{d: d, store: d.DB.MustAs(db.RoleReconciler), lastDrift: map[string]int{}}
}

func (r *Reconciler) Name() string { return "reconciler" }

// RunOnce beats, then runs settle / equity / drift, each isolated.
func (r *Reconciler) RunOnce(ctx context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.store.Beat(ctx, "reconciler", nil)
	var log []string
	for _, duty := range []struct {
		name string
		fn   func(context.Context) ([]string, error)
	}{{"settle", r.SettlePending}, {"equity", r.syncEquity}, {"drift", r.detectDrift}} {
		lines, err := duty.fn(ctx)
		log = append(log, lines...)
		if err != nil {
			log = append(log, fmt.Sprintf("[%s-ERROR] %v", duty.name, err))
		}
	}
	return log, nil
}

// ── intent close-out ─────────────────────────────────────────────────────

// finishIntent moves an intent out of 'approved' when its order reaches a
// terminal state. Only 'approved' is touched: pending has not reached the
// executor yet, and an already-terminal intent is not rewritten (idempotent).
func finishIntent(ctx context.Context, s *db.Store, intentID *int64, status string, reason *string) error {
	if intentID == nil || *intentID == 0 {
		return nil
	}
	var cur string
	err := s.QueryRow(ctx, "SELECT status FROM intents WHERE id=?", *intentID).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if cur != "approved" {
		return nil
	}
	return s.DecideIntent(ctx, *intentID, status, reason)
}

// ── duty 1: settle resting orders ────────────────────────────────────────

var priceKeys = []string{"executed_price", "avg_price", "average_price", "price", "fill_price"}

// pyFloat is Python float(v) for the JSON shapes the CLI emits.
func pyFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

// pyInt is Python int(v): numbers truncate, strings must be integer literals.
func pyInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(x))
		return n, err == nil
	}
	return 0, false
}

// detailFillPrice digs a fill price out of order_detail, else the order's own
// limit price.
func detailFillPrice(detail map[string]any, fallback *float64) *float64 {
	for _, k := range priceKeys {
		v, ok := detail[k]
		if !ok || v == nil {
			continue
		}
		if f, ok := pyFloat(v); ok {
			return &f
		}
	}
	return fallback
}

// SettlePending settles resting orders that filled or died at the broker.
//
// Unlike Python, the order update, the settlement and the intent close-out
// commit as one transaction: Python committed update_order('filled') first,
// so a crash (or a missing fill price) in between left a filled order with
// no trade — the transient invariants has to grant 60 s of grace for.
func (r *Reconciler) SettlePending(ctx context.Context) ([]string, error) {
	trading, err := r.d.Config.Trading()
	if err != nil {
		return nil, err
	}
	commission := trading.Execution.CommissionPerShare
	orders, err := r.store.OpenOrders(ctx)
	if err != nil {
		return nil, err
	}
	var log []string
	for _, o := range orders {
		if o.Status != "submitted" && o.Status != "partial" {
			continue // only settle what is still ours to settle
		}
		if o.BrokerOrderID == nil || *o.BrokerOrderID == "" {
			continue // no broker id yet; next round
		}
		line, err := r.settleOne(ctx, o, commission)
		if err != nil {
			log = append(log, fmt.Sprintf("[settle-ERROR #%s] %v", o.ClientOrderID, err))
			continue
		}
		if line != "" {
			log = append(log, line)
		}
	}
	return log, nil
}

func (r *Reconciler) settleOne(ctx context.Context, o db.Order, commission float64) (string, error) {
	boid := *o.BrokerOrderID
	coid := o.ClientOrderID
	detail := r.d.Broker.OrderDetail(ctx, boid)
	status, _ := detail["status"].(string)
	executed, ok := pyInt(detail["executed_quantity"])
	if !ok || detail["executed_quantity"] == nil {
		executed = 0
	}
	filled := status == "Filled" || executed > 0
	terminal := status == "Filled" || strings.Contains(status, "Expired") || strings.Contains(status, "Reject") || strings.Contains(status, "Cancel")

	if !filled {
		if !terminal {
			return "", nil // still working (New / WaitToNew); next round
		}
		st := strings.ToLower(status)
		if st == "" {
			st = "expired"
		}
		istatus := "expired"
		if strings.Contains(status, "Reject") {
			istatus = "rejected"
		} else if strings.Contains(status, "Cancel") {
			istatus = "cancelled"
		}
		label := status
		if label == "" {
			label = "terminal"
		}
		err := r.store.Tx(ctx, func(tx *db.Store) error {
			if err := tx.UpdateOrder(ctx, coid, db.Fields{"status": st}); err != nil {
				return err
			}
			return finishIntent(ctx, tx, o.IntentID, istatus, db.Ptr(fmt.Sprintf("order %s 无成交", label)))
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("[order-cleanup #%s] %s %s %s 无成交,清理", coid, o.Symbol, o.Side, label), nil
	}
	if executed <= 0 {
		executed = o.Qty // Filled without executed_quantity: the whole order
	}

	// Partial fills are booked incrementally. What the ledger already holds
	// for this broker order (trades carrying its id) is the idempotency key:
	// only executed − booked is new, so neither a re-run nor the executor's
	// immediate path can book a share twice. The order stays 'partial' and
	// tracked while it still works at the broker; it becomes 'filled' (and its
	// intent is finished) only when the broker reports a terminal state.
	// Python booked the first partial as the whole order and stopped tracking,
	// so the remainder would have filled as untracked drift.
	var line string
	err := r.store.Tx(ctx, func(tx *db.Store) error {
		booked, err := tx.BookedFor(ctx, boid)
		if err != nil {
			return err
		}
		avg := detailFillPrice(detail, o.Price)
		orderFields := db.Fields{"status": "partial", "filled_qty": max(executed, booked.Qty)}
		if terminal {
			orderFields["status"] = "filled"
		}
		if avg != nil {
			orderFields["avg_price"] = *avg
		}
		delta := executed - booked.Qty
		if delta <= 0 {
			if err := tx.UpdateOrder(ctx, coid, orderFields); err != nil {
				return err
			}
			if terminal {
				line = fmt.Sprintf("[settle-skip #%s] 已结算过(boid=%s),防重跳过", coid, boid)
				return finishIntent(ctx, tx, o.IntentID, "filled", nil)
			}
			return nil // partial, nothing new since last round
		}
		if avg == nil {
			return fmt.Errorf("%s %s: broker reports a fill but no price, and the order has none", o.Side, o.Symbol)
		}
		// The broker reports the cumulative average; the new shares' price is
		// what that average implies beyond the shares already booked.
		price := *avg
		if booked.Qty > 0 {
			if p := (float64(*avg*float64(executed)) - booked.Notional) / float64(delta); p > 0 && !math.IsInf(p, 0) && !math.IsNaN(p) {
				price = pyfloat.Round(p, 4)
			}
		}
		if err := tx.UpdateOrder(ctx, coid, orderFields); err != nil {
			return err
		}
		tag := "settle"
		if !terminal {
			tag = "settle-partial"
		}

		switch o.Side {
		case "BUY", "SHORT":
			if booked.Qty > 0 && booked.PositionID != nil {
				pos, err := tx.Position(ctx, *booked.PositionID)
				if err != nil {
					return err
				}
				if pos == nil {
					return fmt.Errorf("position %d of the earlier fill is gone", *booked.PositionID)
				}
				if err := settle.Add(ctx, tx, settle.AddInput{Held: *pos, Qty: delta, FillPrice: price,
					CommissionPerShare: commission, IntentID: o.IntentID, BrokerOrderID: &boid}); err != nil {
					return err
				}
				line = fmt.Sprintf("[%s-add #%s] %s +%d %s @ %s (%d/%d)", tag, coid, o.Side, delta, o.Symbol, pyStr(price), executed, o.Qty)
				break
			}
			var it *db.Intent
			if o.IntentID != nil {
				if it, err = tx.Intent(ctx, *o.IntentID); err != nil {
					return err
				}
			}
			in := settle.OpenInput{Symbol: o.Symbol, Side: "long", Qty: delta, FillPrice: price,
				CommissionPerShare: commission, IntentID: o.IntentID, BrokerOrderID: &boid}
			if o.Side == "SHORT" {
				in.Side = "short"
			}
			if it != nil {
				in.Stop, in.Target, in.Source, in.Reason = it.Stop, it.Target, it.Source, it.Reason
				in.Target2 = target2(it.Features)
			}
			if _, err := settle.Open(ctx, tx, in); err != nil {
				return err
			}
			line = fmt.Sprintf("[%s-open #%s] %s %d %s @ %s", tag, coid, o.Side, delta, o.Symbol, pyStr(price))
			if !terminal {
				line += fmt.Sprintf(" (%d/%d)", executed, o.Qty)
			}
		default: // SELL | COVER
			pos, err := tx.PositionFor(ctx, o.Symbol)
			if err != nil {
				return err
			}
			if pos == nil {
				line = fmt.Sprintf("[settle-skip #%s] %s %s:无对应持仓可平(可能已被结算)", coid, o.Side, o.Symbol)
				if terminal {
					return finishIntent(ctx, tx, o.IntentID, "filled", nil)
				}
				return nil
			}
			reason := "reconciled close"
			if o.IntentID != nil {
				var rs *string
				err := tx.QueryRow(ctx, "SELECT reason FROM intents WHERE id=?", *o.IntentID).Scan(&rs)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				if rs != nil && *rs != "" {
					reason = *rs
				}
			}
			qty := min(delta, pos.Qty)
			// An order sized for the whole position (its size covers what is
			// held plus what it already took) is a full exit still filling;
			// a smaller one is the first take-profit and marks t1_done.
			fullExit := o.Qty >= pos.Qty+booked.Qty
			pnl, err := settle.Close(ctx, tx, settle.CloseInput{Held: *pos, FillPrice: price, Qty: qty,
				CommissionPerShare: commission, IntentID: o.IntentID, BrokerOrderID: &boid, Reason: &reason,
				FullExitFilling: fullExit})
			if err != nil {
				return err
			}
			line = fmt.Sprintf("[%s-close #%s] %s %d %s @ %s, pnl=%s", tag, coid, o.Side, qty, o.Symbol, pyStr(price), pyStr(pnl))
			if !terminal {
				line += fmt.Sprintf(" (%d/%d)", executed, o.Qty)
			}
		}
		if terminal {
			return finishIntent(ctx, tx, o.IntentID, "filled", nil)
		}
		return nil
	})
	return line, err
}

// target2 reads the staged second target from intent features.
func target2(raw json.RawMessage) *float64 {
	if len(raw) == 0 {
		return nil
	}
	var feats map[string]any
	if json.Unmarshal(raw, &feats) != nil {
		return nil
	}
	if f, ok := feats["target2"].(float64); ok {
		return &f
	}
	return nil
}

// ── duty 2: equity sync (live only) ──────────────────────────────────────

var equityKeys = []string{"net_assets", "net_liquidation", "equity", "total_cash", "net_asset", "total_assets"}

// FindEquity digs net equity out of the assets JSON, whatever its shape.
func FindEquity(assets any) (float64, bool) {
	var cands []map[string]any
	addList := func(v any) {
		if xs, ok := v.([]any); ok {
			for _, x := range xs {
				if m, ok := x.(map[string]any); ok {
					cands = append(cands, m)
				}
			}
		}
	}
	switch a := assets.(type) {
	case map[string]any:
		cands = append(cands, a)
		for _, k := range []string{"data", "account", "asset"} {
			if m, ok := a[k].(map[string]any); ok {
				cands = append(cands, m)
			} else {
				addList(a[k])
			}
		}
		for _, k := range []string{"list", "cash_infos", "items"} {
			addList(a[k])
		}
	case []any:
		addList(a)
	}
	for _, d := range cands {
		for _, k := range equityKeys {
			if v, ok := d[k]; ok && v != nil {
				if f, ok := pyFloat(v); ok {
					return f, true
				}
			}
		}
	}
	return 0, false
}

func (r *Reconciler) syncEquity(ctx context.Context) ([]string, error) {
	if r.d.Broker.DryRun() {
		return nil, nil
	}
	// Fixed sleeve: Flintrade trades only its allotted capital. Syncing would
	// turn a $10K sleeve into the account's $126K and multiply every risk
	// budget by 12.
	risk, err := r.d.Config.Risk()
	if err != nil {
		return nil, err
	}
	if risk.Equity.Fixed() {
		return []string{"[equity-sync] mode=fixed,跳过(用固定 sleeve 本金)"}, nil
	}
	eq, ok := FindEquity(r.d.Broker.Assets(ctx))
	if !ok {
		return []string{"[equity-sync] broker.assets() 未找到可识别的净值字段,跳过"}, nil
	}
	eq = pyfloat.Round(eq, 4)
	if err := r.store.UpdateRisk(ctx, db.Fields{"equity": eq}); err != nil {
		return nil, err
	}
	return []string{"[equity-sync] risk_state.equity ← " + pyStr(eq)}, nil
}

// ── duty 3: drift detection (live only) ──────────────────────────────────

func rowSymbol(r map[string]any) string {
	for _, k := range []string{"symbol", "symbol_name", "code"} {
		if s, ok := r[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// BrokerPositions normalises broker rows to symbol → signed qty.
func BrokerPositions(rows []map[string]any) map[string]int {
	out := map[string]int{}
	for _, r := range rows {
		sym := rowSymbol(r)
		if sym == "" {
			continue
		}
		var qty any
		for _, k := range []string{"quantity", "qty", "available_quantity", "holding_qty"} {
			if v, ok := r[k]; ok && v != nil {
				qty = v
				break
			}
		}
		q := 0
		if f, ok := pyFloat(qty); ok && !math.IsNaN(f) && !math.IsInf(f, 0) {
			q = int(f)
		}
		out[sym] += q
	}
	return out
}

// BrokerCosts maps symbol → cost price, for adopting an untracked position.
func BrokerCosts(rows []map[string]any) map[string]float64 {
	out := map[string]float64{}
	for _, r := range rows {
		sym := rowSymbol(r)
		if sym == "" {
			continue
		}
		for _, k := range []string{"cost_price", "avg_price", "average_cost", "cost"} {
			v, ok := r[k]
			if !ok || v == nil || v == "" || v == "-" {
				continue
			}
			if f, ok := pyFloat(v); ok {
				out[sym] = f
				break
			}
		}
	}
	return out
}

func (r *Reconciler) detectDrift(ctx context.Context) ([]string, error) {
	if r.d.Broker.DryRun() {
		return nil, nil
	}
	rows, ok := r.d.Broker.Positions(ctx)
	if !ok {
		// An API failure is not a flat book: reading it as empty flags every
		// local position as drift (the 31,606 false alerts). Skip this round.
		return []string{"[drift] positions() 不可用(API失败),本轮跳过"}, nil
	}
	risk, err := r.d.Config.Risk()
	if err != nil {
		return nil, err
	}
	trading, err := r.d.Config.Trading()
	if err != nil {
		return nil, err
	}
	brokerMap := BrokerPositions(rows)
	costMap := BrokerCosts(rows)
	local, err := r.store.OpenPositions(ctx)
	if err != nil {
		return nil, err
	}
	dbMap := map[string]int{}
	for _, p := range local {
		if p.Side == "long" {
			dbMap[p.Symbol] += p.Qty
		} else {
			dbMap[p.Symbol] -= p.Qty
		}
	}
	stopPct := risk.PerTrade.AdoptProtectiveStopPct / 100
	// Default false: a position the decision loop never approved must not be
	// silently adopted into risk accounting. Report and let a human decide.
	adopt := risk.PerTrade.AdoptUntrackedDrift

	syms := map[string]bool{}
	for s := range brokerMap {
		syms[s] = true
	}
	for s := range dbMap {
		syms[s] = true
	}
	order := make([]string, 0, len(syms))
	for s := range syms {
		order = append(order, s)
	}
	sort.Strings(order)

	var log []string
	for _, sym := range order {
		bq, dq := brokerMap[sym], dbMap[sym]
		if bq == dq {
			if _, was := r.lastDrift[sym]; was {
				delete(r.lastDrift, sym)
				log = append(log, fmt.Sprintf("[drift-clear] %s: delta 已恢复为 0", sym))
			}
			continue
		}
		untracked := bq - dq
		cost := costMap[sym]
		var qty int
		var side string
		var stop float64
		switch {
		case adopt && untracked > 0 && bq > 0 && cost != 0:
			qty, side, stop = untracked, "long", pyfloat.Round(float64(cost*(1-stopPct)), 2)
		case adopt && untracked < 0 && bq < 0 && cost != 0:
			qty, side, stop = -untracked, "short", pyfloat.Round(float64(cost*(1+stopPct)), 2)
		default:
			prev, seen := r.lastDrift[sym]
			if !seen || prev != untracked {
				if _, err := r.store.AddSignal(ctx, db.SignalInput{Source: "reconciler", Symbol: &sym, Kind: db.Ptr("drift"),
					Payload: map[string]any{"broker_qty": bq, "db_qty": dq, "delta": untracked, "adopt_enabled": adopt}}); err != nil {
					return log, err
				}
				note := "券商有未追踪仓位,仅告警(auto-adopt 关闭 → 需人工确认)"
				if untracked < 0 || dq > bq {
					note = "库多于券商,仅告警(不自动删持仓)"
				}
				log = append(log, fmt.Sprintf("[DRIFT] %s: broker=%d db=%d delta=%d — %s", sym, bq, dq, untracked, note))
			}
			r.lastDrift[sym] = untracked
			continue
		}
		err := r.store.Tx(ctx, func(tx *db.Store) error {
			if _, err := settle.Open(ctx, tx, settle.OpenInput{Symbol: sym, Side: side, Qty: qty, FillPrice: cost,
				Stop: &stop, CommissionPerShare: trading.Execution.CommissionPerShare, Source: "reconciled-drift",
				Reason: db.Ptr("adopted untracked broker position (lost-confirmation safety net)")}); err != nil {
				return err
			}
			_, err := tx.AddSignal(ctx, db.SignalInput{Source: "reconciler", Symbol: &sym, Kind: db.Ptr("drift_adopt"),
				Payload: map[string]any{"qty": qty, "side": side, "cost": cost, "stop": stop}})
			return err
		})
		if err != nil {
			return log, err
		}
		log = append(log, fmt.Sprintf("[DRIFT-ADOPT] %s: 纳管 %s %d@%s 补保护 stop %s (未追踪成交→已止损保护+计入风控)",
			sym, side, qty, pyStr(cost), pyStr(stop)))
	}
	return log, nil
}

// pyStr renders a float the way Python's str() does (3.0, not 3).
func pyStr(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".eEnN") {
		s += ".0"
	}
	return s
}
