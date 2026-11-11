// Package executor is the single execution authority, ported from
// agent/executor.py: the only loop that places orders and the only writer of
// positions / trades / orders on the entry path. Producers only queue
// intents; this loop rules on them serially.
//
// One RunOnce:
//  1. heartbeat, expire stale entry orders (TTL)
//  2. claim pending intents (priority DESC, created_at)
//  3. per intent: conflict check → in-flight duplicate check → risk gate →
//     order → verify → settle. The portfolio is re-read before every intent
//     so the next one sees the exposure the previous one created.
package executor

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"flintrade/internal/broker"
	"flintrade/internal/db"
	"flintrade/internal/loop"
	"flintrade/internal/pyfloat"
	"flintrade/internal/reasoncode"
	"flintrade/internal/riskgate"
	"flintrade/internal/session"
	"flintrade/internal/settle"
)

// claimLimit matches claim_intents' default.
const claimLimit = 50

// Executor implements loop.Loop.
type Executor struct {
	d  *loop.Deps
	st *db.Store
}

// New builds the executor on the executor role.
func New(d *loop.Deps) *Executor {
	return &Executor{d: d, st: d.DB.MustAs(db.RoleExecutor)}
}

func (e *Executor) Name() string { return "executor" }

// RunOnce processes the queue once.
func (e *Executor) RunOnce(ctx context.Context) ([]string, error) {
	var log []string
	if err := e.st.Beat(ctx, "executor", nil); err != nil {
		return nil, err
	}
	trading, err := e.d.Config.Trading()
	if err != nil {
		return nil, err
	}
	if err := e.expireStaleEntries(ctx, trading.Execution.EntryOrderTTLMin, &log); err != nil {
		return log, err
	}
	sess := e.d.Clock.Current(ctx)
	var mins *int
	if m, ok := e.d.Clock.MinutesToClose(ctx, sess); ok {
		mins = &m
	}

	intents, err := e.st.ClaimIntents(ctx, claimLimit)
	if err != nil {
		return log, err
	}
	for _, row := range intents {
		in := riskgate.FromDB(row)
		positions, err := e.st.OpenPositions(ctx)
		if err != nil {
			return log, err
		}
		risk, err := e.st.GetRisk(ctx)
		if err != nil {
			return log, err
		}
		equity, halted := 0.0, false
		if risk != nil {
			if risk.Equity != nil {
				equity = *risk.Equity
			}
			halted = risk.Halt
		}
		openOrders, err := e.st.OpenOrders(ctx)
		if err != nil {
			return log, err
		}
		isExit := riskgate.IsExit(in.Side)

		if !isExit {
			// Phase 1 arbitration: holding the opposite side → refuse; the
			// producer must close first (prompt contract).
			if reason, code := conflict(in, positions); reason != "" {
				if err := e.reject(ctx, row.ID, reason, code); err != nil {
					return log, err
				}
				log = append(log, fmt.Sprintf("[reject #%d %s] %s", row.ID, in.Symbol, reason))
				continue
			}
			// In-flight duplicate guard: while an entry order for the symbol
			// rests, do not stack another (5 × 16 AMZN = $19.4K on a $10K sleeve).
			if entryInflight(openOrders, in.Symbol) {
				reason := "已有同标的在途开仓单(防止成交延迟期间重复下单)"
				if err := e.reject(ctx, row.ID, reason, reasoncode.New("entryInFlight")); err != nil {
					return log, err
				}
				log = append(log, fmt.Sprintf("[reject #%d %s] %s", row.ID, in.Symbol, reason))
				continue
			}
		}
		riskCfg, err := e.d.Config.Risk()
		if err != nil {
			return log, err
		}
		strategies, err := e.d.Config.Strategies()
		if err != nil {
			return log, err
		}
		trades, err := e.st.RecentTrades(ctx, 30)
		if err != nil {
			return log, err
		}
		gate := &riskgate.Gate{Risk: riskCfg, Strategies: strategies, Equity: equity, Halted: halted,
			Positions: positions, RecentTrades: trades, Session: sess, MinutesToClose: mins,
			VolumeRatio: number(in.Features, "volume_ratio"), Now: e.d.Time(), OpenOrders: openOrders}
		v := gate.Evaluate(in)
		if !v.Approved {
			if err := e.reject(ctx, row.ID, v.Reason, v.Code); err != nil {
				return log, err
			}
			log = append(log, fmt.Sprintf("[reject #%d %s] %s", row.ID, in.Symbol, v.Reason))
			continue
		}
		ok, msg, err := e.execute(ctx, row, in, v.Qty, isExit, positions, trading.Execution.CommissionPerShare, &log)
		if err != nil {
			return log, err
		}
		tag := "fail"
		if ok {
			tag = "fill"
		}
		log = append(log, fmt.Sprintf("[%s #%d] %s | gate: %s", tag, row.ID, msg, v.Reason))
	}
	return log, nil
}

func entryInflight(orders []db.Order, symbol string) bool {
	for _, o := range orders {
		if o.Symbol == symbol && (o.Side == "BUY" || o.Side == "SHORT") {
			return true
		}
	}
	return false
}

func conflict(in riskgate.Intent, positions []db.Position) (string, reasoncode.Code) {
	for _, p := range positions {
		if p.Symbol != in.Symbol {
			continue
		}
		if (in.Side == "long" || in.Side == "short") && p.Side != in.Side {
			return fmt.Sprintf("持有 %s %s,翻向需先平仓(close)再开(prompt 契约)", p.Side, in.Symbol),
				reasoncode.New("flipNeedsClose", "side", p.Side, "symbol", in.Symbol)
		}
		return "", reasoncode.Code{}
	}
	return "", reasoncode.Code{}
}

func (e *Executor) reject(ctx context.Context, id int64, reason string, code reasoncode.Code) error {
	return e.st.DecideIntentCoded(ctx, id, "rejected", &reason, code.JSON())
}

// finishIntent closes out the intent riding on an order that reached a
// terminal state. Only 'approved' is overwritten, so it is idempotent (same
// discipline as the reconciler).
func (e *Executor) finishIntent(ctx context.Context, intentID *int64, status, reason string, code reasoncode.Code) error {
	if intentID == nil || *intentID == 0 {
		return nil
	}
	it, err := e.st.Intent(ctx, *intentID)
	if err != nil || it == nil || it.Status != "approved" {
		return err
	}
	return e.st.DecideIntentCoded(ctx, *intentID, status, &reason, code.JSON())
}

// filledAtBroker re-checks an order after a cancel. Cancel and fill race:
// the order may have filled the instant before. It must never be marked
// cancelled on the strength of the cancel reply — the reconciler only settles
// submitted/partial orders, so a filled order marked cancelled would leave
// the ledger holding a position the broker already closed.
func (e *Executor) filledAtBroker(ctx context.Context, boid string) (bool, string, int) {
	detail := e.d.Broker.OrderDetail(ctx, boid)
	status, _ := detail["status"].(string)
	executed := pyIntOr0(detail["executed_quantity"])
	return status == "Filled" || executed > 0, status, executed
}

// cancelInflight clears every working order on symbol before an exit. The
// broker locks shares against a resting sell, so a second sell is rejected
// as oversell (2026-07-31: flintrade-198 resting, flintrade-199 rejected, price kept
// falling). Returns false when a working order had already filled: the
// position is being closed, a second order would open the other way.
func (e *Executor) cancelInflight(ctx context.Context, symbol string, log *[]string) (bool, error) {
	orders, err := e.st.OpenOrders(ctx)
	if err != nil {
		return false, err
	}
	proceed := true
	for _, o := range orders {
		if o.Symbol != symbol {
			continue
		}
		coid := o.ClientOrderID
		if o.BrokerOrderID == nil || *o.BrokerOrderID == "" {
			// Only a local row, never reached the broker: nothing to cancel there.
			if err := e.st.UpdateOrder(ctx, coid, db.Fields{"status": "cancelled"}); err != nil {
				return false, err
			}
			if err := e.finishIntent(ctx, o.IntentID, "cancelled", "被新平仓意图清场撤单(未获券商单号)", reasoncode.New("cancelledForClose")); err != nil {
				return false, err
			}
			continue
		}
		boid := *o.BrokerOrderID
		res := e.d.Broker.Cancel(ctx, boid)
		if filled, status, executed := e.filledAtBroker(ctx, boid); filled {
			// Lost the race: stays submitted, the reconciler settles the real
			// fill and finishes the intent.
			proceed = false
			*log = append(*log, fmt.Sprintf("[cancel #%s] %s %s 撤单前已成交(%s executed=%d),交给 Reconciler 结算",
				coid, symbol, o.Side, status, executed))
			continue
		}
		if err := e.st.UpdateOrder(ctx, coid, db.Fields{"status": "cancelled"}); err != nil {
			return false, err
		}
		if err := e.finishIntent(ctx, o.IntentID, "cancelled", "被新平仓意图清场撤单", reasoncode.New("cancelledForClose")); err != nil {
			return false, err
		}
		how := "撤单失败但券商侧无成交"
		if res.OK {
			how = "已撤"
		}
		*log = append(*log, fmt.Sprintf("[cancel #%s] %s %s %s: %s", coid, symbol, o.Side, how, boid))
	}
	return proceed, nil
}

// expireStaleEntries cancels entry orders resting longer than the TTL and
// marks their intents expired. A limit entry is priced at the moment of the
// thesis; the longer it rests the more likely it fills exactly when price
// runs through it (flintrade-210 NVDA rested over a weekend and filled 4 cents
// above its own stop). Exit orders never expire: cancelling one leaves the
// position naked; the risk monitor re-triggers exits instead.
func (e *Executor) expireStaleEntries(ctx context.Context, ttlMin float64, log *[]string) error {
	if ttlMin <= 0 {
		return nil
	}
	orders, err := e.st.OpenOrders(ctx)
	if err != nil {
		return err
	}
	now := e.d.Time()
	for _, o := range orders {
		if o.Side != "BUY" && o.Side != "SHORT" {
			continue
		}
		created, err := db.ParseTime(o.CreatedAt)
		if err != nil {
			continue
		}
		ageMin := now.Sub(created).Seconds() / 60
		if ageMin < ttlMin {
			continue
		}
		coid := o.ClientOrderID
		if o.BrokerOrderID != nil && *o.BrokerOrderID != "" {
			e.d.Broker.Cancel(ctx, *o.BrokerOrderID)
			if filled, status, executed := e.filledAtBroker(ctx, *o.BrokerOrderID); filled {
				*log = append(*log, fmt.Sprintf("[ttl #%s] %s %s 撤单前已成交(%s executed=%d),交给 Reconciler 结算",
					coid, o.Symbol, o.Side, status, executed))
				continue
			}
		}
		if err := e.st.UpdateOrder(ctx, coid, db.Fields{"status": "cancelled"}); err != nil {
			return err
		}
		ttl := riskgate.TomlNum(ttlMin)
		if o.IntentID != nil && *o.IntentID != 0 {
			reason := fmt.Sprintf("开仓挂单 %dmin 未成交 > TTL %smin,已撤单", int(ageMin), ttl)
			code := reasoncode.New("ttlExpired", "age", strconv.Itoa(int(ageMin)), "ttl", ttl)
			if err := e.st.DecideIntentCoded(ctx, *o.IntentID, "expired", &reason, code.JSON()); err != nil {
				return err
			}
		}
		*log = append(*log, fmt.Sprintf("[ttl #%s] %s %s 挂单 %dmin 未成交 > TTL %smin,已撤", coid, o.Symbol, o.Side, int(ageMin), ttl))
	}
	return nil
}

// execute places the order, verifies it and settles an immediate fill.
func (e *Executor) execute(ctx context.Context, row db.Intent, in riskgate.Intent, qty int, isExit bool,
	positions []db.Position, commission float64, log *[]string) (bool, string, error) {
	symbol := in.Symbol
	sess := e.d.Clock.Current(ctx)
	// Exits resolve an unknown session to ANY_TIME, not RTH_ONLY, or a close
	// order sits until the next regular open.
	orth := session.OutsideRTHFor(sess, isExit)
	coid := fmt.Sprintf("flintrade-%d", row.ID)
	intentID := row.ID

	if isExit {
		proceed, err := e.cancelInflight(ctx, symbol, log)
		if err != nil {
			return false, "", err
		}
		if !proceed {
			reason := "在途平仓单已成交,本意图作废"
			if err := e.st.DecideIntentCoded(ctx, row.ID, "cancelled", &reason, reasoncode.New("closeAlreadyFilled").JSON()); err != nil {
				return false, "", err
			}
			return false, "在途平仓单已成交,跳过重复下单", nil
		}
	}

	var held *db.Position
	var price float64
	if in.EntryHint != nil {
		price = *in.EntryHint
	}
	posSide, action, brokerSide := in.Side, "BUY", "buy"
	if isExit {
		for i := range positions {
			if positions[i].Symbol == symbol {
				held = &positions[i]
				break
			}
		}
		if held == nil { // the gate checked; guard anyway
			return false, "", fmt.Errorf("exit intent #%d: no %s position in snapshot", row.ID, symbol)
		}
		posSide = held.Side
		if held.Side == "long" {
			action, brokerSide = "SELL", "sell"
		} else {
			action, brokerSide = "COVER", "buy"
		}
		if price == 0 {
			price = held.EntryPrice
		}
	} else if posSide != "long" {
		action, brokerSide = "SHORT", "sell"
	}

	if _, err := e.st.CreateOrder(ctx, db.OrderInput{ClientOrderID: coid, Symbol: symbol, Side: action, Qty: qty,
		Price: &price, OutsideRTH: &orth, IntentID: &intentID}); err != nil {
		return false, "", err
	}
	place := e.d.Broker.Buy
	if brokerSide == "sell" {
		place = e.d.Broker.Sell
	}
	res := place(ctx, symbol, qty, price, orth, coid)

	if !res.OK {
		// Store the broker's raw error, not just a status: the three rejected
		// orders of 2026-07-31 kept only "Rejected" and could not be diagnosed.
		var boid any
		if res.BrokerOrderID != "" {
			boid = res.BrokerOrderID
		}
		if err := e.st.UpdateOrder(ctx, coid, db.Fields{"status": "rejected", "broker_order_id": boid}); err != nil {
			return false, "", err
		}
		reason := fmt.Sprintf("broker[%s] orth=%s sess=%s: %s", res.Status, orth, sess, runes(res.Raw, 400))
		code := reasoncode.New("brokerRejected", "status", res.Status, "orth", orth, "session", sess, "raw", runes(res.Raw, 400))
		if err := e.st.DecideIntentCoded(ctx, row.ID, "rejected", &reason, code.JSON()); err != nil {
			return false, "", err
		}
		return false, fmt.Sprintf("broker 拒单/失败 (orth=%s sess=%s): %s", orth, sess, res.Raw), nil
	}

	boid := res.BrokerOrderID
	// Dry run fills at once; a live limit order may not — the reconciler
	// settles it later.
	filled := res.Status == "Filled"
	if !filled {
		st, _ := e.d.Broker.OrderDetail(ctx, boid)["status"].(string)
		filled = st == "Filled"
	}
	if !filled {
		if err := e.st.UpdateOrder(ctx, coid, db.Fields{"status": "submitted", "broker_order_id": boid}); err != nil {
			return false, "", err
		}
		if err := e.st.DecideIntent(ctx, row.ID, "approved", nil); err != nil {
			return false, "", err
		}
		return true, "已提交未即时成交(待对账): " + boid, nil
	}

	fill := price
	if res.FillPrice != nil {
		fill = *res.FillPrice
	}
	if err := e.st.UpdateOrder(ctx, coid, db.Fields{"status": "filled", "broker_order_id": boid,
		"filled_qty": qty, "avg_price": fill}); err != nil {
		return false, "", err
	}

	if isExit {
		pnl, err := settle.Close(ctx, e.st, settle.CloseInput{Held: *held, FillPrice: fill, Qty: qty,
			CommissionPerShare: commission, IntentID: &intentID, BrokerOrderID: &boid, Reason: row.Reason})
		if err != nil {
			return false, "", err
		}
		if err := e.st.DecideIntent(ctx, row.ID, "filled", nil); err != nil {
			return false, "", err
		}
		return true, fmt.Sprintf("平仓 %s %d @ %s, pnl=%s", symbol, qty, riskgate.PyRepr(fill), riskgate.PyRepr(pnl)), nil
	}

	if _, err := settle.Open(ctx, e.st, settle.OpenInput{Symbol: symbol, Side: posSide, Qty: qty, FillPrice: fill,
		Stop: in.Stop, Target: row.Target, Target2: number(in.Features, "target2"), CommissionPerShare: commission,
		Source: row.Source, IntentID: &intentID, BrokerOrderID: &boid, Reason: row.Reason}); err != nil {
		return false, "", err
	}
	riskAmt := 0.0
	if in.Stop != nil {
		riskAmt = round4(float64(float64(qty) * abs(fill-*in.Stop)))
	}
	if err := e.st.DecideIntent(ctx, row.ID, "filled", nil); err != nil {
		return false, "", err
	}
	return true, fmt.Sprintf("开仓 %s %s %d @ %s, risk=$%s", posSide, symbol, qty, riskgate.PyRepr(fill), riskgate.PyRepr(riskAmt)), nil
}

// number reads a numeric feature. Anything but a JSON number reads as
// absent; for volume_ratio that is fail-closed at the gate.
func number(feats map[string]any, key string) *float64 {
	if f, ok := feats[key].(float64); ok {
		return &f
	}
	return nil
}

// pyIntOr0 is `int(x or 0)` with ValueError/TypeError → 0.
func pyIntOr0(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case bool:
		if x {
			return 1
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(x)); err == nil {
			return n
		}
	}
	return 0
}

func runes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func round4(x float64) float64 { return pyfloat.Round(x, 4) }

var _ loop.Loop = (*Executor)(nil)
var _ broker.Broker = (*broker.CLI)(nil)
