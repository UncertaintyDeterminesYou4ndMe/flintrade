"""
Reconciler —— 系统的「自主神经系统」。持续对账进程。

为什么需要它:Executor 下限价单后可能未即时成交,留 order='submitted';
broker 端的 resting stop / take-profit 单也可能在我们不知情时成交。Reconciler
轮询 broker,把这些「带外」状态变化拉回数据库的单一真相:

  1. 结算挂单成交:open_orders() 里仍是 submitted/partial 的单,查 broker
     order_detail;若已 Filled 则回填 order + 用 settle.* 记账(与 Executor
     共用同一套会计逻辑,绝不重复实现 equity/position 数学)。
  2. 权益校准(仅 live):用 broker.assets() 的净值覆盖 risk_state.equity。
  3. 漂移检测(仅 live):比对 broker.positions() 与本地 open_positions();
     不一致时只 add_signal(kind='drift') 报警,绝不擅自改持仓。

安全性:默认 Broker() 走 dry-run(除非 FLINT_DRY_RUN=0),永不下单。每项职责
独立 try/except,一项失败不拖垮整轮循环。

避免重复结算:Executor 对即时成交的单会自己把 order 置 'filled' 并记账;
open_orders() 只返回 submitted/partial,所以我们天然不会重复结算它处理过的单。
仍加一道防线:读到单时再确认它当前确实是 submitted/partial 才结算。

角色:DB(role='reconciler') 可写 positions/trades/orders/risk_state(但不能
decide_intent —— 那是 Executor 的权力)。所以这里只结算成交,不裁决 intent。
"""
from __future__ import annotations

import time

from agent.broker import Broker
from agent.config import load_trading
from agent.db import DB


# broker order_detail 里可能出现的成交价字段名(各家/各版本不一,防御式逐个试)。
_PRICE_KEYS = ("executed_price", "avg_price", "average_price", "price", "fill_price")
# broker assets JSON 里净值/现金可能的键名(同样防御式探测)。
_EQUITY_KEYS = ("net_assets", "net_liquidation", "equity", "total_cash",
                "net_asset", "total_assets")


def _detail_fill_price(detail: dict, fallback: float | None) -> float | None:
    """从 broker order_detail 里挖一个成交价;挖不到回退到本地挂单价。"""
    for k in _PRICE_KEYS:
        v = detail.get(k)
        if v is not None:
            try:
                return float(v)
            except (TypeError, ValueError):
                continue
    return fallback


def _find_equity(assets) -> float | None:
    """从 longbridge assets JSON(list 或 dict,形状不定)里防御式抽净值。"""
    candidates: list[dict] = []
    if isinstance(assets, dict):
        candidates.append(assets)
        # 常见嵌套:{"data": {...}} 或 {"list": [{...}]} 或 {"cash_infos": [...]}
        for key in ("data", "account", "asset"):
            v = assets.get(key)
            if isinstance(v, dict):
                candidates.append(v)
            elif isinstance(v, list):
                candidates.extend(x for x in v if isinstance(x, dict))
        for key in ("list", "cash_infos", "items"):
            v = assets.get(key)
            if isinstance(v, list):
                candidates.extend(x for x in v if isinstance(x, dict))
    elif isinstance(assets, list):
        candidates.extend(x for x in assets if isinstance(x, dict))

    for d in candidates:
        for k in _EQUITY_KEYS:
            v = d.get(k)
            if v is not None:
                try:
                    return float(v)
                except (TypeError, ValueError):
                    continue
    return None


def _broker_positions_map(rows: list) -> dict[str, int]:
    """把 broker.positions() 的不定形状归一成 {symbol: signed_qty}。"""
    out: dict[str, int] = {}
    for r in rows:
        if not isinstance(r, dict):
            continue
        sym = r.get("symbol") or r.get("symbol_name") or r.get("code")
        if not sym:
            continue
        qty = None
        for k in ("quantity", "qty", "available_quantity", "holding_qty"):
            if r.get(k) is not None:
                qty = r.get(k)
                break
        try:
            q = int(float(qty)) if qty is not None else 0
        except (TypeError, ValueError):
            q = 0
        out[sym] = out.get(sym, 0) + q
    return out


def _broker_cost_map(rows: list) -> dict[str, float]:
    """{symbol: 成本价},供纳管未追踪仓位时定 entry/保护 stop。"""
    out: dict[str, float] = {}
    for r in rows:
        if not isinstance(r, dict):
            continue
        sym = r.get("symbol") or r.get("symbol_name") or r.get("code")
        if not sym:
            continue
        for k in ("cost_price", "avg_price", "average_cost", "cost"):
            v = r.get(k)
            if v not in (None, "", "-"):
                try:
                    out[sym] = float(v)
                    break
                except (TypeError, ValueError):
                    pass
    return out


class Reconciler:
    def __init__(self, broker: Broker | None = None):
        self.db = DB(role="reconciler")
        self.broker = broker or Broker()  # 默认 dry-run(env FLINT_DRY_RUN 控制)
        self.commission = load_trading()["execution"]["commission_per_share"]
        # symbol -> 上次上报的 delta(bq-dq)。用于 drift 信号去重:只在 delta 变化
        # 时才 add_signal,避免同一个未变化的漂移每轮都刷一条(31,606 条假信号的成因)。
        self._last_drift: dict[str, int] = {}

    # ── 职责 1:结算挂单成交 ────────────────────────────────────────────────
    def _settle_pending(self) -> list[str]:
        from agent import settle  # 局部 import,与 executor 同口径会计

        log: list[str] = []
        for row in self.db.open_orders():
            order = dict(row)
            # 防线:只结算当前确实是 submitted/partial 的单(避免与 executor 抢)。
            if order.get("status") not in ("submitted", "partial"):
                continue
            boid = order.get("broker_order_id")
            coid = order["client_order_id"]
            if not boid:
                continue  # 尚未拿到 broker 单号,下轮再说

            detail = self.broker.order_detail(boid)
            status = detail.get("status", "") or ""
            try:
                executed = int(detail.get("executed_quantity") or 0)
            except (ValueError, TypeError):
                executed = 0
            filled = (status == "Filled") or executed > 0
            terminal = any(k in status for k in ("Expired", "Reject", "Cancel"))

            if not filled:
                if terminal:
                    # 终态无成交:标记订单状态,移出 open_orders,停止每轮重复查询
                    self.db.update_order(coid, status=(status.lower() or "expired"))
                    log.append(f"[order-cleanup #{coid}] {order['symbol']} {order['side']} "
                               f"{status or 'terminal'} 无成交,清理")
                continue  # 否则仍在工作(New/WaitToNew),下轮再看

            # 幂等防线:同一 broker_order_id 若已有成交记录,说明这单已经结算过
            # (可能与 executor 的即时结算路径竞态),绝不重复记账。
            already = self.db.conn.execute(
                "SELECT 1 FROM trades WHERE broker_order_id=? LIMIT 1", (boid,)
            ).fetchone()
            if already:
                self.db.update_order(coid, status="filled")
                log.append(f"[settle-skip #{coid}] 已结算过(boid={boid}),防重跳过")
                continue

            # 成交(全部,或部分成交后终止)→ 按实际成交量结算
            qty = executed if executed > 0 else order["qty"]
            fill_price = _detail_fill_price(detail, order.get("price"))
            self.db.update_order(coid, status="filled", filled_qty=qty,
                                 avg_price=fill_price)

            side = order["side"]  # BUY | SHORT | SELL | COVER
            if side in ("BUY", "SHORT"):
                # 开仓:需要原始 intent 的 stop/target/source/reason
                intent_row = self.db.conn.execute(
                    "SELECT * FROM intents WHERE id=?", (order["intent_id"],)
                ).fetchone()
                intent = dict(intent_row) if intent_row else {}
                settle.settle_open(
                    self.db,
                    symbol=order["symbol"],
                    side=("long" if side == "BUY" else "short"),
                    qty=qty,
                    fill_price=fill_price,
                    stop=intent.get("stop"),
                    target=intent.get("target"),
                    commission_per_share=self.commission,
                    source=intent.get("source"),
                    intent_id=order["intent_id"],
                    broker_order_id=boid,
                    reason=intent.get("reason"),
                )
                log.append(
                    f"[settle-open #{coid}] {side} {qty} {order['symbol']} @ {fill_price}"
                )
            else:  # SELL | COVER → 平仓
                pos = self.db.position_for(order["symbol"])
                if pos is None:
                    log.append(
                        f"[settle-skip #{coid}] {side} {order['symbol']}:无对应持仓可平(可能已被结算)"
                    )
                    continue
                intent_row = self.db.conn.execute(
                    "SELECT reason FROM intents WHERE id=?", (order["intent_id"],)
                ).fetchone()
                intent_reason = intent_row["reason"] if intent_row else None
                pnl = settle.settle_close(
                    self.db,
                    held=dict(pos),
                    fill_price=fill_price,
                    qty=qty,
                    commission_per_share=self.commission,
                    intent_id=order["intent_id"],
                    broker_order_id=boid,
                    reason=(intent_reason or "reconciled close"),
                )
                log.append(
                    f"[settle-close #{coid}] {side} {qty} {order['symbol']} @ {fill_price}, pnl={pnl}"
                )
        return log

    # ── 职责 2:权益校准(仅 live)──────────────────────────────────────────
    def _sync_equity(self) -> list[str]:
        if self.broker.dry_run:
            return []
        # 固定 sleeve 模式:Flint 只用分配的本金,不随券商账户净值同步
        # (否则会把 $10K sleeve 刷成账户真实净值 $126K)。
        try:
            from agent.config import load_risk
            if load_risk().get("equity", {}).get("mode") == "fixed":
                return ["[equity-sync] mode=fixed,跳过(用固定 sleeve 本金)"]
        except Exception:
            pass
        assets = self.broker.assets()
        equity = _find_equity(assets)
        if equity is None:
            return ["[equity-sync] broker.assets() 未找到可识别的净值字段,跳过"]
        self.db.update_risk(equity=round(equity, 4))
        return [f"[equity-sync] risk_state.equity ← {round(equity, 4)}"]

    # ── 职责 3:漂移检测(仅 live)──────────────────────────────────────────
    def _detect_drift(self) -> list[str]:
        if self.broker.dry_run:
            return []
        from agent import settle
        from agent.config import load_risk
        log: list[str] = []
        raw = self.broker.positions()
        if raw is None:
            # broker.positions() 失败(API 抖动/超时),不能当成「空仓」处理——
            # 那会把每个本地持仓都判成漂移,刷出海量假信号(31,606 条的成因)。跳过本轮。
            return ["[drift] positions() 不可用(API失败),本轮跳过"]
        broker_map = _broker_positions_map(raw)
        cost_map = _broker_cost_map(raw)
        db_map: dict[str, int] = {}
        for r in self.db.open_positions():
            p = dict(r)
            signed = p["qty"] if p["side"] == "long" else -p["qty"]
            db_map[p["symbol"]] = db_map.get(p["symbol"], 0) + signed

        per_trade = load_risk()["per_trade"]
        stop_pct = float(per_trade.get("adopt_protective_stop_pct", 3)) / 100.0
        # 默认 false:决策回路从未批准的仓位不应被静默纳管、计入风控。只告警,交给人判断。
        adopt = bool(per_trade.get("adopt_untracked_drift", False))

        for sym in set(broker_map) | set(db_map):
            bq = broker_map.get(sym, 0)
            dq = db_map.get(sym, 0)
            if bq == dq:
                if sym in self._last_drift:
                    del self._last_drift[sym]
                    log.append(f"[drift-clear] {sym}: delta 已恢复为 0")
                continue
            untracked = bq - dq           # 券商相对库多出的(带符号)
            cost = cost_map.get(sym)

            # 安全网(仅在 adopt_untracked_drift=true 时启用):券商持有库未追踪的多/空头
            # (漏记成交,如丢确认)→ 主动纳管,补保护性 stop、计入风控,杜绝继续重复下单。
            if adopt and untracked > 0 and bq > 0 and cost:        # 未追踪多头
                qty, side = untracked, "long"
                stop = round(cost * (1 - stop_pct), 2)
            elif adopt and untracked < 0 and bq < 0 and cost:      # 未追踪空头
                qty, side = -untracked, "short"
                stop = round(cost * (1 + stop_pct), 2)
            else:
                # 默认路径:不擅自改持仓,只 add_signal 报警(库多于券商,或未开启 auto-adopt)。
                # 去重:同一 symbol 的 delta 没变化就不再重复报,否则每轮都刷同一条噪音。
                changed = self._last_drift.get(sym) != untracked
                if changed:
                    self.db.add_signal(source="reconciler", symbol=sym, kind="drift",
                                       payload={"broker_qty": bq, "db_qty": dq, "delta": untracked,
                                                "adopt_enabled": adopt})
                    note = ("库多于券商,仅告警(不自动删持仓)" if untracked < 0 or dq > bq
                            else "券商有未追踪仓位,仅告警(auto-adopt 关闭 → 需人工确认)")
                    log.append(f"[DRIFT] {sym}: broker={bq} db={dq} delta={untracked} — {note}")
                self._last_drift[sym] = untracked
                continue

            settle.settle_open(
                self.db, symbol=sym, side=side, qty=qty, fill_price=cost,
                stop=stop, target=None, commission_per_share=self.commission,
                source="reconciled-drift", intent_id=None, broker_order_id=None,
                reason="adopted untracked broker position (lost-confirmation safety net)",
            )
            self.db.add_signal(source="reconciler", symbol=sym, kind="drift_adopt",
                               payload={"qty": qty, "side": side, "cost": cost, "stop": stop})
            log.append(f"[DRIFT-ADOPT] {sym}: 纳管 {side} {qty}@{cost} 补保护 stop {stop} "
                       f"(未追踪成交→已止损保护+计入风控)")
        return log

    # ── 一轮 ────────────────────────────────────────────────────────────────
    def run_once(self) -> list[str]:
        try:
            self.db.beat(process="reconciler")  # 心跳:daemon 调 run_once,必须在此打
        except Exception:
            pass
        log: list[str] = []
        for name, duty in (("settle", self._settle_pending),
                           ("equity", self._sync_equity),
                           ("drift", self._detect_drift)):
            try:
                log.extend(duty())
            except Exception as exc:  # 一项失败不拖垮整轮
                log.append(f"[{name}-ERROR] {exc!r}")
        return log

    def run_forever(self):
        poll = load_trading()["cadence"].get("reconciler_poll_sec", 15)
        while True:
            try:
                self.db.beat(process="reconciler")
            except Exception as exc:
                print(f"[heartbeat-ERROR] {exc!r}", flush=True)
            for line in self.run_once():
                print(line, flush=True)
            time.sleep(poll)


_INSTANCE: Reconciler | None = None


def run_once() -> list[str]:
    """模块级便捷入口:复用线程内单例。

    每次调用都 new Reconciler() 会每 15s 泄漏一个 SQLite 连接:Python 3.13+
    的 sqlite3.Connection 参与引用环,引用计数不回收,只有分代 GC 才关 fd —
    安静期分配少、GC 不跑,连接堆满 launchd 默认 256 fd 上限(2026-07-26 停摆事故)。
    daemon 只从 reconciler 线程调本函数,单例的线程亲和性成立。
    """
    global _INSTANCE
    if _INSTANCE is None:
        _INSTANCE = Reconciler()
    return _INSTANCE.run_once()


def run_forever():
    Reconciler().run_forever()


if __name__ == "__main__":
    import sys

    if "--once" in sys.argv:
        for line in run_once():
            print(line)
    else:
        run_forever()
