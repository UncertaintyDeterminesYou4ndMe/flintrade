"""
Executor —— 单一执行权威。整个系统唯一写 positions/trades/orders 的进程,
唯一调用 broker 下单的进程。生产者只往 intents 表投递,这里串行裁决执行。

一轮 process_once():
  1. 心跳 + 读组合快照(equity/halt/持仓/最近成交)
  2. claim 待裁决 intents(按 priority DESC, created_at)
  3. 逐个:冲突检查 → RiskGate.evaluate → 下单 → 验单 → 写库
  每处理一笔都重新快照(后一笔能看到前一笔造成的持仓/敞口变化)。

权益口径(Phase 1,现金净值近似,持仓按入场价标记、开仓时未实现=0):
  开仓: equity -= 手续费; open_risk += risk_amt
  平仓: equity += 毛盈亏 - 平仓手续费; open_risk -= 持仓 risk_amt
Reconciler(Phase 2)接管后会用 broker.assets 校准真实权益。
"""
from __future__ import annotations

import time
from datetime import datetime, timezone

from agent.broker import Broker
from agent.config import load_risk, load_trading
from agent.db import DB
from agent.risk_gate import RiskGate
from agent import settle
from agent import session as sess


class Executor:
    def __init__(self, broker: Broker | None = None):
        self.db = DB(role="executor")
        self.broker = broker or Broker()  # 默认 dry-run(env FLINTRADE_DRY_RUN 控制)
        self.commission = load_trading()["execution"]["commission_per_share"]

    # ── 组合快照 ──────────────────────────────────────────────────────────
    def _positions(self) -> list[dict]:
        return [dict(r) for r in self.db.open_positions()]

    def _equity(self) -> float:
        r = self.db.get_risk()
        return float(r["equity"]) if r and r["equity"] is not None else 0.0

    # ── 冲突仲裁(Phase 1:持反向仓时拒开,需显式 close 先平)─────────────
    def _conflict(self, intent: dict, positions: list[dict]) -> str | None:
        held = next((p for p in positions if p["symbol"] == intent["symbol"]), None)
        if not held:
            return None
        want = "long" if intent["side"] == "long" else "short" if intent["side"] == "short" else None
        if want and held["side"] != want:
            return f"持有 {held['side']} {intent['symbol']},翻向需先平仓(close)再开(prompt 契约)"
        return None

    # ── 撤在途单(仅退出路径)────────────────────────────────────────────
    def _cancel_inflight(self, symbol: str, log: list[str]) -> bool:
        """撤掉该标的所有在途单,为新的平仓单清场。返回「可以继续下单」。

        券商会用挂着的卖单锁住持仓,所以第二张同向卖单会以「超卖」被拒 ——
        2026-07-31 就是这样:flintrade-198 还挂着,flintrade-199 直接被拒,行情继续下跌。
        平仓路径必须先清场再下单,否则改价追单永远追不上。

        撤单与成交是赛跑:撤之前那一刻单子可能刚成交。所以撤完必须用
        order_detail 复核,**绝不能凭撤单返回值就把本地订单标成 cancelled** ——
        真成交了却标 cancelled,Reconciler 就不会再结算它(它只认
        submitted/partial),账上持仓还在、券商已经空仓,静默撕裂。
        复核发现已成交 → 保持 submitted 交给 Reconciler 结算,并返回 False
        让本次平仓意图作废(仓已经在平了,不需要第二张单)。
        """
        proceed = True
        for o in self.db.open_orders():
            if o["symbol"] != symbol:
                continue
            coid, boid = o["client_order_id"], o["broker_order_id"]
            if not boid:
                # 只在本地建了行、没拿到券商单号 —— 券商侧无单可撤,本地标掉即可。
                self.db.update_order(coid, status="cancelled")
                continue

            res = self.broker.cancel(boid)
            detail = self.broker.order_detail(boid)
            status = detail.get("status", "") or ""
            try:
                executed = int(detail.get("executed_quantity") or 0)
            except (ValueError, TypeError):
                executed = 0

            if status == "Filled" or executed > 0:
                # 撤单没赶上成交。留 submitted,Reconciler 会按实际成交量结算。
                proceed = False
                log.append(f"[cancel #{coid}] {symbol} {o['side']} 撤单前已成交"
                           f"({status} executed={executed}),交给 Reconciler 结算")
                continue

            self.db.update_order(coid, status="cancelled")
            log.append(f"[cancel #{coid}] {symbol} {o['side']} "
                       f"{'已撤' if res.get('ok') else '撤单失败但券商侧无成交'}: {boid}")
        return proceed

    # ── 开仓挂单 TTL(安全网)─────────────────────────────────────────────
    def _expire_stale_entries(self, log: list[str]) -> None:
        """撤掉挂超过 TTL 未成交的开仓单,并把对应 intent 标 expired。

        开仓限价单的论点在下单那一刻定价;挂得越久,成交越可能发生在价格
        不利地穿过限价的时刻(逆向选择)。实例:flintrade-210 (NVDA) 2026-08-01
        周六下单躺过整个周末,周一开盘在自己止损位上方 4 美分成交,还连带
        让在途防线挡掉了 6 笔新 intent。平仓单不适用 TTL —— 挂着的平仓单由
        risk_monitor 重触发、_cancel_inflight 清场,超时撤掉反而让持仓裸奔。

        与 _cancel_inflight 同款竞态防线:撤单后必须 order_detail 复核,
        已成交的留 submitted 交给 Reconciler 结算,绝不标 cancelled。
        """
        ttl_min = load_trading()["execution"].get("entry_order_ttl_min", 45)
        if not ttl_min or ttl_min <= 0:
            return
        now = datetime.now(timezone.utc)
        for o in self.db.open_orders():
            if o["side"] not in ("BUY", "SHORT"):
                continue
            try:
                created = datetime.fromisoformat(
                    str(o["created_at"]).replace("Z", "+00:00"))
            except ValueError:
                continue
            age_min = (now - created).total_seconds() / 60
            if age_min < ttl_min:
                continue

            coid, boid = o["client_order_id"], o["broker_order_id"]
            if boid:
                self.broker.cancel(boid)
                detail = self.broker.order_detail(boid)
                status = detail.get("status", "") or ""
                try:
                    executed = int(detail.get("executed_quantity") or 0)
                except (ValueError, TypeError):
                    executed = 0
                if status == "Filled" or executed > 0:
                    log.append(f"[ttl #{coid}] {o['symbol']} {o['side']} 撤单前已成交"
                               f"({status} executed={executed}),交给 Reconciler 结算")
                    continue

            self.db.update_order(coid, status="cancelled")
            if o["intent_id"]:
                self.db.decide_intent(
                    o["intent_id"], "expired",
                    reject_reason=f"开仓挂单 {int(age_min)}min 未成交 > TTL {ttl_min}min,已撤单")
            log.append(f"[ttl #{coid}] {o['symbol']} {o['side']} 挂单 {int(age_min)}min "
                       f"未成交 > TTL {ttl_min}min,已撤")

    # ── 下单 + 验单 ───────────────────────────────────────────────────────
    def _execute(self, intent: dict, qty: int, is_exit: bool, positions: list[dict],
                 log: list[str]):
        symbol = intent["symbol"]
        session = sess.current_session()
        # 退出路径用 for_exit:会话辨识不出来时回退 ANY_TIME 而不是 RTH_ONLY,
        # 否则一张平仓单会静静躺到下一个常规开盘(见 session.outside_rth_for)。
        orth = sess.outside_rth_for(session, for_exit=is_exit)
        price = intent.get("entry_hint")
        coid = f"flintrade-{intent['id']}"

        if is_exit and not self._cancel_inflight(symbol, log):
            # 在途单已经成交 = 这个仓正在被平,再下一张就是反向裸开。
            self.db.decide_intent(intent["id"], "cancelled",
                                  reject_reason="在途平仓单已成交,本意图作废")
            return False, "在途平仓单已成交,跳过重复下单"

        # 决定 broker 方向与本地 action
        if is_exit:
            held = next(p for p in positions if p["symbol"] == symbol)
            pos_side, action = held["side"], ("SELL" if held["side"] == "long" else "COVER")
            broker_side = "sell" if held["side"] == "long" else "buy"
            price = price or held["entry_price"]
        else:
            pos_side = intent["side"]
            action = "BUY" if pos_side == "long" else "SHORT"
            broker_side = "buy" if pos_side == "long" else "sell"

        self.db.create_order(client_order_id=coid, symbol=symbol, side=action,
                             qty=qty, price=price, outside_rth=orth, intent_id=intent["id"])
        fn = self.broker.buy if broker_side == "buy" else self.broker.sell
        res = fn(symbol, qty, price, orth, client_order_id=coid)

        if not res.get("ok"):
            # 落库券商的原始报错,不只是 status。2026-07-31 复盘时三张被拒的单
            # 只留下 reject_reason="broker: Rejected",查不出到底为什么被拒 ——
            # 没有原因就没法区分「标志位错了」和「超卖」,只能靠推。
            raw = res.get("raw")
            detail = str(raw)[:400] if raw is not None else ""
            self.db.update_order(coid, status="rejected", broker_order_id=res.get("broker_order_id"))
            self.db.decide_intent(
                intent["id"], "rejected",
                reject_reason=f"broker[{res.get('status')}] orth={orth} sess={session}: {detail}")
            return False, f"broker 拒单/失败 (orth={orth} sess={session}): {raw}"

        boid = res.get("broker_order_id")
        # 验单:dry-run 立即 Filled;live 限价单可能未即时成交 → 交给 Reconciler
        status = res.get("status", "")
        filled = status == "Filled"
        if not filled:
            detail = self.broker.order_detail(boid)
            filled = detail.get("status") == "Filled"

        if not filled:
            # 订单已提交但未成交:留 submitted,Reconciler(Phase 2)完成回填
            self.db.update_order(coid, status="submitted", broker_order_id=boid)
            self.db.decide_intent(intent["id"], "approved")  # 已下单,待成交
            return True, f"已提交未即时成交(待对账): {boid}"

        fill_price = res.get("fill_price", price)
        self.db.update_order(coid, status="filled", broker_order_id=boid,
                             filled_qty=qty, avg_price=fill_price)

        if is_exit:
            held = next(p for p in positions if p["symbol"] == symbol)
            pnl_net = settle.settle_close(
                self.db, held=held, fill_price=fill_price, qty=qty,
                commission_per_share=self.commission, intent_id=intent["id"],
                broker_order_id=boid, reason=intent.get("reason"))
            self.db.decide_intent(intent["id"], "filled")
            return True, f"平仓 {symbol} {qty} @ {fill_price}, pnl={pnl_net}"

        # 开仓
        pos_id = settle.settle_open(
            self.db, symbol=symbol, side=pos_side, qty=qty, fill_price=fill_price,
            stop=intent.get("stop"), target=intent.get("target"),
            commission_per_share=self.commission, source=intent.get("source"),
            intent_id=intent["id"], broker_order_id=boid, reason=intent.get("reason"))
        risk_amt = round(qty * abs(fill_price - intent["stop"]), 4)
        self.db.decide_intent(intent["id"], "filled")
        return True, f"开仓 {pos_side} {symbol} {qty} @ {fill_price}, risk=${risk_amt}"

    # ── 一轮处理 ──────────────────────────────────────────────────────────
    def process_once(self) -> list[str]:
        log = []
        self.db.beat(process="executor")
        self._expire_stale_entries(log)
        session = sess.current_session()
        mins = sess.minutes_to_close(session)

        for it in self.db.claim_intents():
            intent = dict(it)
            positions = self._positions()
            equity = self._equity()
            halted = self.db.is_halted()
            is_exit = intent["side"] in ("flatten", "close")

            open_orders = [dict(o) for o in self.db.open_orders()]

            # 冲突仲裁(开仓且持反向仓)
            if not is_exit:
                conflict = self._conflict(intent, positions)
                if conflict:
                    self.db.decide_intent(intent["id"], "rejected", reject_reason=conflict)
                    log.append(f"[reject #{intent['id']} {intent['symbol']}] {conflict}")
                    continue

                # 在途重复下单防线:同标的已有 submitted/partial 的开仓单(BUY/SHORT)时,
                # 不再重复开仓——避免成交延迟期间(限价单未即时成交)被反复触发下单,
                # 堆出远超风控预算的敞口(实例:5 笔 16 股 AMZN 堆到 $19.4K/$10K sleeve)。
                inflight_same = any(
                    o["symbol"] == intent["symbol"] and o["side"] in ("BUY", "SHORT")
                    for o in open_orders
                )
                if inflight_same:
                    reason = "已有同标的在途开仓单(防止成交延迟期间重复下单)"
                    self.db.decide_intent(intent["id"], "rejected", reject_reason=reason)
                    log.append(f"[reject #{intent['id']} {intent['symbol']}] {reason}")
                    continue

            gate = RiskGate(
                equity=equity, halted=halted, open_positions=positions,
                recent_trades=[dict(t) for t in self.db.recent_trades(30)],
                session=session, minutes_to_close=mins,
                volume_ratio=(intent.get("features") or {}).get("volume_ratio")
                    if isinstance(intent.get("features"), dict) else None,
                open_orders=open_orders,
            )
            verdict = gate.evaluate(intent)
            if not verdict.approved:
                self.db.decide_intent(intent["id"], "rejected", reject_reason=verdict.reason)
                log.append(f"[reject #{intent['id']} {intent['symbol']}] {verdict.reason}")
                continue

            ok, msg = self._execute(intent, verdict.qty, is_exit, positions, log)
            log.append(f"[{'fill' if ok else 'fail'} #{intent['id']}] {msg} | gate: {verdict.reason}")

        return log

    def run_forever(self):
        cadence = load_trading()["cadence"].get("technical_loop_sec", 30)
        poll = min(15, cadence)  # executor 比生产者跑得勤,及时消费队列
        while True:
            for line in self.process_once():
                print(line, flush=True)
            time.sleep(poll)


if __name__ == "__main__":
    import sys
    ex = Executor()
    if "--once" in sys.argv:
        for line in ex.process_once():
            print(line)
    else:
        ex.run_forever()
