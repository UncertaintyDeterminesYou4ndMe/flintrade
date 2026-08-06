"""
Risk Monitor —— 独立熔断器 / kill-switch(系统的「前额叶」)。

它与 Executor 分进程运行,这样即使 Executor 卡死、死循环或下单逻辑出 bug,
这个进程仍能独立拍下急停(set_halt)并(可选)把现有持仓平掉。它本身从不直接
下单 —— 只往 intents 队列投递 close 意图,由 Executor 串行执行(Executor 的
风险闸门对「退出」永远放行,即使 halt)。

每轮 run_once():
  1. 日切重置:ET 日期跨日 → 重设 day_start_equity / day_realized_pnl,并清 halt
     (这是唯一自动清 halt 的地方:盘中熔断会一直 halt 到下一交易日)。
  2. 逐仓止损:持仓现价越过 positions.stop → 投 close 意图(见下)。
  3. 回撤熔断:当日权益自 day_start 回撤 ≥ daily_loss_limit_pct → set_halt。
  4. halt 时平仓:若刚熔断且 flatten_on_halt=true → 给每个持仓投 close 意图。

逐仓止损为什么在这里:在此之前 `positions.stop` 只是个咨询性字段 —— 唯一会
看它的是 loop_technical,而那是**每 1800 秒一次的 LLM 调用**。一个 30 分钟
才检查一次、还要等模型想明白的止损,不是止损。2026-07-31 AAPL 跳空那次,
从消息落库到真正成交隔了 81 分钟。止损属于「不需要判断力的机械纪律」,应该
由这个 10 秒的独立循环执行,和熔断器同级 —— 它本来就是为「Executor 卡死也
要能自救」而存在的进程。

它仍然遵守「自己从不下单」的边界:只投 close 意图,由 Executor 串行执行
(风险闸门对退出永远放行,即使 halt)。

角色边界(db.py 的 _WRITE_PERMS):risk_monitor 仅允许 {'halt','intents_submit'}。
  * set_halt(...)        —— 受 'halt' 许可保护,允许。✓
  * submit_intent(...)   —— 受 'intents_submit' 许可保护,允许。✓
  * update_risk(...)     —— 受 'risk_state' 许可保护,risk_monitor 无权!✗
所以日切重置 day_start_equity / day_realized_pnl 无法走 update_risk 助手。
role guard 只拦截助手方法,直接对 db.conn 执行 raw SQL 不受拦截(设计如此:
guard 是助手层的纪律,不是连接层的 ACL)。这是熔断器维持自身基准的必要、
范围极小的旁路 —— 仅触碰熔断基准列,不碰 positions/trades/orders。
"""
from __future__ import annotations

import sys
from datetime import datetime
from zoneinfo import ZoneInfo

from agent.config import load_risk, load_trading
from agent.db import DB, now
from agent import session as sess

ET = ZoneInfo("America/New_York")


def _et_today() -> str:
    """当前 ET 交易日日期串 'YYYY-MM-DD'(日切判据)。"""
    return datetime.now(ET).strftime("%Y-%m-%d")


class RiskMonitor:
    def __init__(self, db: DB | None = None):
        self.db = db or DB(role="risk_monitor")

    # ── 日切重置(唯一自动清 halt 处)──────────────────────────────────────
    def _daily_reset(self, log: list[str]):
        today = _et_today()
        if self.db.kv_get("risk_day") == today:
            return  # 同一交易日,无需重置

        r = self.db.get_risk()
        equity = r["equity"] if r else None

        # day_start_equity / day_realized_pnl 重置:risk_monitor 无 'risk_state'
        # 写权限(只有 executor/reconciler 有),不能走 db.update_risk 助手。
        # role guard 仅护助手层,raw SQL 不受拦 —— 这是熔断器维持自身基准的
        # 范围极小、必要的旁路,仅触碰熔断基准列。
        self.db.conn.execute(
            "UPDATE risk_state SET day_start_equity=?, day_realized_pnl=0, updated_at=? WHERE id=1",
            (equity, now()),
        )
        # set_halt 是 risk_monitor 的合法助手(受 'halt' 许可保护),正常调用。
        self.db.set_halt(False, "daily reset")
        self.db.kv_set("risk_day", today)
        log.append(
            f"[daily-reset] 新交易日 {today}: day_start_equity={equity}, "
            f"day_realized_pnl=0, halt 已清"
        )

    # ── 逐仓止损守卫 ────────────────────────────────────────────────────────
    def _exit_in_flight(self, symbol: str) -> bool:
        """该标的是否已有平仓动作在路上:排队中的意图,或挂着的平仓单。

        注意「在路上」只认这两样,**不认 status='approved' 的意图**。approved
        的含义是「已下单、等成交」,但 Executor 之后再没回来改过它 —— 订单走到
        终态(filled/rejected/expired)时只有 orders 行被更新,intents 行永远停在
        approved。库里此刻就躺着十几条这样的孤儿(NVDA #230、AAPL #198/199/200…)。
        把 approved 算作在途,等于给这些标的的止损守卫永久上锁 —— 修了等于没修。
        订单是否真的活着,以 orders 表为准。
        """
        row = self.db.conn.execute(
            """SELECT 1 FROM intents
               WHERE symbol=? AND side IN ('close','flatten')
                 AND status='pending' LIMIT 1""",
            (symbol,),
        ).fetchone()
        if row:
            return True
        return self.db.conn.execute(
            """SELECT 1 FROM orders
               WHERE symbol=? AND side IN ('SELL','COVER')
                 AND status IN ('submitted','partial') LIMIT 1""",
            (symbol,),
        ).fetchone() is not None

    def _stop_guard(self, log: list[str]):
        """现价越过 stop 的持仓 → 投 close 意图。"""
        positions = [dict(p) for p in self.db.open_positions()]
        guarded = [p for p in positions if p.get("stop") is not None]
        if not guarded:
            return

        session = sess.current_session()
        if session == "Closed":
            return  # 无盘可交易,省下取价配额;开盘瞬间会立刻再查

        from agent import quotes
        # 一次 CLI 调用查完所有持仓。取价必须按会话取(顶层 last_done 在盘前/
        # 夜盘会停在上一个 regular close 上,正是这个陈旧价让 07-31 那次
        # 跳空在指标快照里完全看不见)。
        prices = quotes.last_prices([p["symbol"] for p in guarded], session)

        prio = int(load_risk()["priority"]["user"]) + 1
        for pos in guarded:
            price = prices.get(pos["symbol"])
            if price is None:
                continue  # 取不到价这轮就不判,下轮再说 —— 绝不拿陈旧价触发止损
            stop = float(pos["stop"])
            breached = price <= stop if pos["side"] == "long" else price >= stop
            if not breached:
                continue
            if self._exit_in_flight(pos["symbol"]):
                continue  # 已经在平了,别再投

            # dedup_key 带分钟桶:UNIQUE 索引不区分意图状态,固定 key 一旦被拒
            # 就永远无法重投。分钟桶让它每分钟最多重试一次 —— 既不刷屏,
            # 又不会因为一次拒单就把止损永久哑掉。
            bucket = datetime.now(ET).strftime("%Y%m%d%H%M")
            iid = self.db.submit_intent(
                source="risk_monitor",
                priority=prio,
                symbol=pos["symbol"],
                side="close",
                entry_hint=price,
                confidence=100,
                dedup_key=f"stop-{pos['id']}-{bucket}",
                reason=(f"stop breached: {pos['side']} {pos['symbol']} "
                        f"last {price} vs stop {stop} (session={session})"),
            )
            if iid is not None:
                log.append(f"[stop-guard] {pos['symbol']} {pos['side']} 现价 {price} "
                           f"越过止损 {stop} → 投 close 意图 #{iid}")

    # ── 回撤熔断 ────────────────────────────────────────────────────────────
    def _drawdown_breaker(self, log: list[str]) -> bool:
        """检查当日回撤;触发则 set_halt 并返回 True(表示「刚刚熔断」)。"""
        r = self.db.get_risk()
        if r is None:
            return False
        if r["halt"]:
            return False  # 已经 halt,不重复触发(盘中熔断保持到日切)

        ds = r["day_start_equity"]
        eq = r["equity"]
        if ds is None or eq is None or ds <= 0:
            return False  # 基准未就绪,无法计算回撤

        limit = float(load_risk()["circuit_breaker"]["daily_loss_limit_pct"])
        drawdown_pct = (float(ds) - float(eq)) / float(ds) * 100.0
        if drawdown_pct >= limit:
            reason = f"daily drawdown {drawdown_pct:.2f}% >= {limit}%"
            self.db.set_halt(True, reason=reason)
            log.append(f"[!! CIRCUIT BREAKER !!] HALT — {reason} "
                       f"(day_start={ds}, equity={eq})")
            return True
        return False

    # ── 熔断后平仓 ──────────────────────────────────────────────────────────
    def _flatten(self, log: list[str]):
        if not load_risk()["circuit_breaker"].get("flatten_on_halt", False):
            log.append("[flatten] flatten_on_halt=false:仅停新单,现有持仓交给人/Executor")
            return
        # 用户级 +1 优先级,确保强平意图压过任何排队的入场意图。
        prio = int(load_risk()["priority"]["user"]) + 1
        positions = self.db.open_positions()
        if not positions:
            log.append("[flatten] 无持仓需平")
            return
        for pos in positions:
            iid = self.db.submit_intent(
                source="risk_monitor",
                priority=prio,
                symbol=pos["symbol"],
                side="close",
                reason="circuit breaker flatten",
                dedup_key=f"cb-flatten-{pos['id']}",  # 同一持仓只投一次强平意图
            )
            if iid is not None:
                log.append(f"[flatten] 投递 close 意图 #{iid} {pos['symbol']} "
                           f"(priority={prio})")
            else:
                log.append(f"[flatten] {pos['symbol']} 强平意图已存在(去重),跳过")

    # ── 一轮 ────────────────────────────────────────────────────────────────
    def run_once(self) -> list[str]:
        log: list[str] = []
        try:
            self.db.beat(process="risk_monitor")  # 心跳:daemon 调 run_once,必须在此打
        except Exception:
            pass
        # 每项职责各自 try/except,任一出错不拖垮整轮 / 整个进程。
        try:
            self._daily_reset(log)
        except Exception as e:
            log.append(f"[error] daily_reset: {e!r}")

        try:
            self._stop_guard(log)
        except Exception as e:
            log.append(f"[error] stop_guard: {e!r}")

        just_halted = False
        try:
            just_halted = self._drawdown_breaker(log)
        except Exception as e:
            log.append(f"[error] drawdown_breaker: {e!r}")

        if just_halted:
            try:
                self._flatten(log)
            except Exception as e:
                log.append(f"[error] flatten: {e!r}")

        return log

    def run_forever(self):
        import time
        poll = int(load_trading()["cadence"].get("risk_monitor_sec", 10))
        while True:
            try:
                self.db.beat(process="risk_monitor")
            except Exception as e:
                print(f"[error] heartbeat: {e!r}", flush=True)
            try:
                for line in self.run_once():
                    print(line, flush=True)
            except Exception as e:
                print(f"[error] run_once 整轮: {e!r}", flush=True)
            time.sleep(poll)

    # ── 手动开关(供 CLI / 人工干预)────────────────────────────────────────
    def halt(self, reason: str = "manual halt") -> list[str]:
        self.db.set_halt(True, reason=reason)
        return [f"[manual] HALT — {reason}"]

    def resume(self, reason: str = "manual resume") -> list[str]:
        self.db.set_halt(False, reason=reason)
        return [f"[manual] RESUME — {reason}"]

    def status(self) -> list[str]:
        r = self.db.get_risk()
        n_pos = len(self.db.open_positions())
        out = ["── risk_state ──"]
        if r is None:
            out.append("(risk_state 行缺失;请先 init_db)")
        else:
            for k in ("equity", "day_start_equity", "day_realized_pnl",
                      "open_risk", "halt", "halt_reason", "updated_at"):
                out.append(f"  {k:18} = {r[k]!r}")
        out.append(f"  risk_day(kv)       = {self.db.kv_get('risk_day')!r}")
        out.append(f"  open_positions     = {n_pos}")
        out.append(f"  session(now)       = {sess.current_session()!r}")
        return out


def main(argv: list[str] | None = None):
    argv = sys.argv[1:] if argv is None else argv
    rm = RiskMonitor()

    if "--halt" in argv:
        i = argv.index("--halt")
        reason = argv[i + 1] if i + 1 < len(argv) and not argv[i + 1].startswith("--") \
            else "manual halt"
        for line in rm.halt(reason):
            print(line)
    elif "--resume" in argv:
        i = argv.index("--resume")
        reason = argv[i + 1] if i + 1 < len(argv) and not argv[i + 1].startswith("--") \
            else "manual resume"
        for line in rm.resume(reason):
            print(line)
    elif "--status" in argv:
        for line in rm.status():
            print(line)
    elif "--once" in argv:
        for line in rm.run_once():
            print(line)
    else:
        rm.run_forever()


if __name__ == "__main__":
    main()
