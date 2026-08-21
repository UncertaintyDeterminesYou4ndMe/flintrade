"""
运行时不变量自检 —— 账本一致性的独立守夜人。

设计参考 deepseek-harness 的 package-owned runtime invariants:每条检查对应
一类真实会发生的「账本撕裂」,违反就大声报,而不是等坏账在某天复盘时才被发现。

只读、观察不干预:违反 → 打日志(daemon 前缀 [invariants])+ 结果落 kv
('invariants_last'),不熔断、不修数据。要不要联动 halt,等有了误报率数据再定。

检查清单(每条注明它防的事故类):
  open_risk_ledger    risk_state.open_risk == Σ 开仓 risk_amt。
                      settle_close 用 max(0,·) 钳位,漂移会被悄悄吞掉,这里兜底。
  filled_has_trade    intent 说成交了,trades 里必须有那笔成交。
  approved_has_order  approved intent 必须有对应 order(2026-08 孤儿 intent 事故类:
                      十几条 intent 永远停在 approved,止损守卫的在途判断被锁死)。
  filled_order_trade  filled order 必须有同 broker_order_id 的成交记录。
  position_sane       开仓行 qty>0 / entry_price>0 / side 合法。
  no_hedged_symbol    同一标的不允许同时持有多空(broker 侧会净额,库里出现=撕裂)。
  trade_leg_pnl       平仓腿(SELL/COVER)必有 pnl,开仓腿(BUY/SHORT)必无。
  equity_sane         equity 一旦初始化就必须 > 0。

带时间窗的检查只看近 14 天:老数据的历史遗留问题一次性人工清理,不在这里反复报警。
"""
from __future__ import annotations

import json
from datetime import datetime, timedelta, timezone

from agent.db import DB, now

# 浮点容差:settle 全程 round(·,4),多笔累计后 1 分钱级别的差异不算撕裂
_RISK_TOLERANCE = 0.05
_WINDOW_DAYS = 14          # 带时间窗检查的回看范围
_APPROVED_GRACE_MIN = 30   # approved intent 允许无单悬挂的宽限(等 reconciler)


def _ts(dt: datetime) -> str:
    return dt.strftime("%Y-%m-%dT%H:%M:%SZ")


def _check_all(db: DB) -> list[str]:
    """跑全部断言,返回违反清单(空 = 健康)。调用方需保证读一致性。"""
    v: list[str] = []
    utcnow = datetime.now(timezone.utc)
    window = _ts(utcnow - timedelta(days=_WINDOW_DAYS))
    grace = _ts(utcnow - timedelta(minutes=_APPROVED_GRACE_MIN))

    # ── open_risk_ledger ──────────────────────────────────────────────────
    risk = db.get_risk()
    if risk and risk["equity"] is not None:  # equity 为 NULL = 账本未初始化,全跳过
        ledger = db.conn.execute(
            "SELECT COALESCE(SUM(COALESCE(risk_amt,0)),0) AS s FROM positions WHERE status='open'"
        ).fetchone()["s"]
        if abs((risk["open_risk"] or 0.0) - ledger) > _RISK_TOLERANCE:
            v.append(f"open_risk_ledger: risk_state.open_risk={risk['open_risk']} "
                     f"≠ Σ开仓risk_amt={round(ledger, 4)}")
        if risk["equity"] <= 0:
            v.append(f"equity_sane: equity={risk['equity']} ≤ 0(会计已崩)")

    # ── filled_has_trade ──────────────────────────────────────────────────
    rows = db.conn.execute(
        """SELECT i.id FROM intents i
           WHERE i.status='filled' AND i.decided_at >= ?
             AND NOT EXISTS (SELECT 1 FROM trades t WHERE t.intent_id = i.id)""",
        (window,)).fetchall()
    if rows:
        ids = [r["id"] for r in rows]
        v.append(f"filled_has_trade: intent {ids} 标记 filled 但 trades 无对应成交")

    # ── approved_has_order ────────────────────────────────────────────────
    rows = db.conn.execute(
        """SELECT i.id FROM intents i
           WHERE i.status='approved' AND i.decided_at < ?
             AND NOT EXISTS (SELECT 1 FROM orders o WHERE o.intent_id = i.id)""",
        (grace,)).fetchall()
    if rows:
        ids = [r["id"] for r in rows]
        v.append(f"approved_has_order: intent {ids} approved 超过 "
                 f"{_APPROVED_GRACE_MIN}min 却没有任何 order(孤儿)")

    # ── filled_order_trade ────────────────────────────────────────────────
    # 60s 宽限:executor/reconciler 的 update_order(filled) 与随后的 settle 是
    # 两条独立提交的语句,快照卡在中间会看到"filled 但还没记账"的合法瞬态。
    settle_grace = _ts(utcnow - timedelta(seconds=60))
    rows = db.conn.execute(
        """SELECT o.client_order_id AS coid FROM orders o
           WHERE o.status='filled' AND o.broker_order_id IS NOT NULL
             AND o.created_at >= ?
             AND COALESCE(o.updated_at, o.created_at) < ?
             AND NOT EXISTS (SELECT 1 FROM trades t
                             WHERE t.broker_order_id = o.broker_order_id)""",
        (window, settle_grace)).fetchall()
    if rows:
        coids = [r["coid"] for r in rows]
        v.append(f"filled_order_trade: order {coids} 状态 filled 但账本无同 broker_order_id 成交")

    # ── position_sane ─────────────────────────────────────────────────────
    rows = db.conn.execute(
        """SELECT id, symbol, side, qty, entry_price FROM positions
           WHERE status='open'
             AND (qty <= 0 OR entry_price <= 0 OR side NOT IN ('long','short'))"""
    ).fetchall()
    for r in rows:
        v.append(f"position_sane: position #{r['id']} {r['symbol']} "
                 f"side={r['side']} qty={r['qty']} entry={r['entry_price']}")

    # ── no_hedged_symbol ──────────────────────────────────────────────────
    rows = db.conn.execute(
        """SELECT symbol FROM positions WHERE status='open'
           GROUP BY symbol HAVING COUNT(DISTINCT side) > 1"""
    ).fetchall()
    for r in rows:
        v.append(f"no_hedged_symbol: {r['symbol']} 同时持有多空开仓行")

    # ── trade_leg_pnl ─────────────────────────────────────────────────────
    # test 归因的回填行不受实时结算路径约束,豁免
    rows = db.conn.execute(
        """SELECT id, action FROM trades
           WHERE ts >= ? AND COALESCE(attribution,'') != 'test'
             AND ((action IN ('SELL','COVER') AND pnl IS NULL)
               OR (action IN ('BUY','SHORT') AND pnl IS NOT NULL))""",
        (window,)).fetchall()
    for r in rows:
        v.append(f"trade_leg_pnl: trade #{r['id']} action={r['action']} 的 pnl 语义不符")

    return v


def run_once() -> list[str]:
    """daemon loop 入口。返回违反清单(daemon 逐行打印);健康返回 []。"""
    with DB(role="reader") as db:
        # 多条 SELECT 必须看同一快照,否则 executor/reconciler 写到一半会造成假撕裂。
        # WAL 下 deferred BEGIN + 首条读即固定快照,不占写锁。
        db.conn.execute("BEGIN")
        try:
            violations = _check_all(db)
        finally:
            db.conn.execute("COMMIT")
        db.kv_set("invariants_last", json.dumps(
            {"ts": now(), "ok": not violations, "violations": violations},
            ensure_ascii=False))
        db.beat(process="invariants",
                note=("ok" if not violations else f"{len(violations)} violations"))
    return [f"VIOLATION {x}" for x in violations]


if __name__ == "__main__":
    out = run_once()
    if out:
        for line in out:
            print(line)
        raise SystemExit(1)
    print("invariants: ok")
