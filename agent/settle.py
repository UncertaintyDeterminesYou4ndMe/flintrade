"""
成交结算 —— Executor 与 Reconciler 共用的唯一会计逻辑。

为什么独立成模块:limit 单可能不即时成交。Executor 下单后若已成交则当场结算;
若未成交则留 order='submitted',由 Reconciler 在成交回来时结算。两条路径必须用
**同一套** 建仓/平仓/改 equity 逻辑,否则账会对不上。这里就是那唯一一套。

权益口径(现金净值近似,持仓按入场价标记):
  开仓: equity -= 手续费; open_risk += risk_amt
  平仓: equity += 毛盈亏 - 平仓手续费; open_risk -= 持仓 risk_amt; day_realized_pnl += 净盈亏

调用方必须用有写权限的 DB(role='executor' 或 'reconciler')。
"""
from __future__ import annotations


def _round(x, n=4):
    return round(x, n) if x is not None else None


def _attribution_for(source: str | None) -> str:
    """来源 → 归因标签。taxonomy:strategy(technical/event 等自动信号) vs
    manual(user_cli 手动单)。test/outage-degraded 只由回填脚本/运维直接写库,
    这里的实时结算路径永远不产出它们。"""
    return "manual" if source == "user" else "strategy"


def settle_open(db, *, symbol: str, side: str, qty: int, fill_price: float,
                stop: float | None, target: float | None, commission_per_share: float,
                target2: float | None = None,
                source: str | None = None, intent_id: int | None = None,
                broker_order_id: str | None = None, reason: str | None = None,
                features: dict | None = None) -> int:
    """建仓结算。side ∈ {long, short}。返回 position_id。
    target/target2 = 分批止盈的第一/第二目标位(由 risk_monitor 守卫执行)。"""
    comm = _round(qty * commission_per_share)
    risk_amt = _round(qty * abs(fill_price - stop)) if stop else None
    action = "BUY" if side == "long" else "SHORT"

    # 持仓 + 成交 + 风险状态三步写必须原子:中途崩溃不能留半笔账。
    with db.transaction():
        pos_id = db.open_position(symbol=symbol, side=side, qty=qty, entry_price=fill_price,
                                  stop=stop, target=target, target2=target2,
                                  risk_amt=risk_amt,
                                  source=source, intent_id=intent_id)
        db.append_trade(symbol=symbol, action=action, qty=qty, fill_price=fill_price,
                        commission=comm, source=source, intent_id=intent_id,
                        broker_order_id=broker_order_id, position_id=pos_id,
                        features=features, reason=reason,
                        attribution=_attribution_for(source))
        r = db.get_risk()
        db.update_risk(equity=_round(r["equity"] - comm),
                       open_risk=_round(r["open_risk"] + (risk_amt or 0.0)))
    return pos_id


def settle_close(db, *, held: dict, fill_price: float, qty: int,
                 commission_per_share: float, intent_id: int | None = None,
                 broker_order_id: str | None = None, reason: str | None = None) -> float:
    """平仓结算。held 为持仓 dict(含 side/entry_price/risk_amt/id/qty)。返回净 pnl。

    qty < held.qty = 部分平仓(分批止盈的第一目标):持仓行不关闭,收缩 qty 并按
    平仓比例释放 risk_amt,t1_done 置 1;qty >= held.qty = 整仓平(原路径)。
    """
    held_qty = int(held["qty"])
    partial = 0 < qty < held_qty
    comm = _round(qty * commission_per_share)
    gross = ((fill_price - held["entry_price"]) * qty if held["side"] == "long"
             else (held["entry_price"] - fill_price) * qty)
    pnl_net = _round(gross - comm)
    action = "SELL" if held["side"] == "long" else "COVER"

    held_risk = held.get("risk_amt") or 0.0
    # 释放的风险额按平仓比例;整仓平释放全部(避免比例舍入留尾差)。
    risk_released = _round(held_risk * qty / held_qty) if partial else held_risk

    # 平仓 + 成交 + 风险状态三步写必须原子:中途崩溃不能留半笔账。
    with db.transaction():
        if partial:
            remaining_risk = _round(held_risk - risk_released) if held.get("risk_amt") is not None else None
            db.reduce_position(held["id"], qty=held_qty - qty, risk_amt=remaining_risk)
        else:
            db.close_position(held["id"])
        db.append_trade(symbol=held["symbol"], action=action, qty=qty, fill_price=fill_price,
                        commission=comm, pnl=pnl_net, source=held.get("source"),
                        intent_id=intent_id, broker_order_id=broker_order_id,
                        position_id=held["id"], reason=reason,
                        attribution=_attribution_for(held.get("source")))
        r = db.get_risk()
        db.update_risk(equity=_round(r["equity"] + gross - comm),
                       open_risk=_round(max(0.0, r["open_risk"] - (risk_released or 0.0))),
                       day_realized_pnl=_round(r["day_realized_pnl"] + pnl_net))
    return pnl_net
