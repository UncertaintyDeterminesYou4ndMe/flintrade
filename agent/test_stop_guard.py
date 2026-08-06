"""2026-07-31 AAPL 事故的三个修复的回归测试 —— 临时 db + 桩 broker,零真实副作用。
跑法: FLINTRADE_DRY_RUN=1 python3 -m agent.test_stop_guard

事故回顾:AAPL 多头持仓在财报跳空中被穿掉止损,从消息落库到成交隔了 81 分钟,
期间三张平仓单连废(expired / rejected / rejected),价格从 312.30 跌到 308.55。

覆盖:
  A. session 03:50-04:00 死区:此前 current_session() 在这十分钟返回 'Closed'
     → outside_rth=RTH_ONLY → 券商拒单(flintrade-200 的死因)。
  B. 平仓前撤在途单:券商用挂着的卖单锁住持仓,第二张卖单会以超卖被拒
     (flintrade-199 的死因);以及撤单与成交赛跑时绝不能把已成交的单标成 cancelled。
  C. risk_monitor 逐仓止损守卫:此前 positions.stop 只有每 1800 秒的 LLM 会看。
"""
from __future__ import annotations

import datetime as _dt
import os
import tempfile

os.environ.setdefault("FLINTRADE_DB", os.path.join(tempfile.gettempdir(), "flintrade_stopguard_test.db"))
os.environ["FLINTRADE_DRY_RUN"] = "1"
for suffix in ("", "-wal", "-shm"):
    p = os.environ["FLINTRADE_DB"] + suffix
    if os.path.exists(p):
        os.remove(p)

from agent.db import DB, init_db                    # noqa: E402
from agent.executor import Executor                 # noqa: E402
from agent.risk_monitor import RiskMonitor          # noqa: E402
from agent import session as S                      # noqa: E402
from agent import quotes as Q                       # noqa: E402

PASS, FAIL = [], []


def check(name, cond, detail=""):
    (PASS if cond else FAIL).append(name)
    print(f"  {'✓' if cond else '✗ FAIL'} {name}" + (f" — {detail}" if detail and not cond else ""))


def wipe():
    boot = DB(role="migrate")
    for t in ("trades", "orders", "positions", "intents", "signals"):
        boot.conn.execute(f"DELETE FROM {t}")
    boot.update_risk(equity=10000.0, day_start_equity=10000.0, day_realized_pnl=0, open_risk=0)
    boot.close()


init_db()


# ══════════════════════════════════════════════════════════════════════════
print("=== A. session 03:50-04:00 死区 ===")
# ══════════════════════════════════════════════════════════════════════════
_real_datetime = S.datetime


class FakeDT(_dt.datetime):
    _now = None

    @classmethod
    def now(cls, tz=None):
        return cls._now.replace(tzinfo=tz)


def session_at(month, day, hh, mm):
    """打桩时钟 + 清空 CLI 缓存,只走 overnight 启发式。"""
    S.datetime = FakeDT
    S._cache_value, S._cache_ts = [], 1e18
    FakeDT._now = _dt.datetime(2026, month, day, hh, mm, 0)
    try:
        return S.current_session()
    finally:
        S.datetime = _real_datetime


# 2026-07-31 是周五,凌晨属于周五的交易日 → 应有夜盘
for hh, mm, want in [(3, 49, "Overnight"), (3, 50, "Overnight-Pre"),
                     (3, 55, "Overnight-Pre"), (3, 59, "Overnight-Pre")]:
    got = session_at(7, 31, hh, mm)
    check(f"{hh:02d}:{mm:02d} → {want}", got == want, f"得到 {got}")

check("死区映射到 OVERNIGHT 而不是 RTH_ONLY",
      S.outside_rth_for("Overnight-Pre") == "OVERNIGHT")
check("会话辨识不出时,平仓回退 ANY_TIME(不是 RTH_ONLY)",
      S.outside_rth_for("Closed", for_exit=True) == "ANY_TIME"
      and S.outside_rth_for("Closed") == "RTH_ONLY")
# 周五夜里没有次日交易日 → 仍应是 Closed,别把周末也当夜盘
check("周五 20:00 仍为 Closed(次日周六)", session_at(7, 31, 20, 0) == "Closed",
      f"得到 {session_at(7, 31, 20, 0)}")
check("周四 20:00 为 Overnight", session_at(7, 30, 20, 0) == "Overnight")

S._cache_value, S._cache_ts = [], 0.0  # 还原缓存,后续测试用桩函数


# ══════════════════════════════════════════════════════════════════════════
print("=== A2. 会话取价(顶层 last 在非常规时段是陈旧的)===")
# ══════════════════════════════════════════════════════════════════════════
# 实测 payload 形状(2026-08-05 夜盘 NVDA):顶层 last 是前一个 regular close,
# 当前真实价在 overnight 子对象里。键名写错会静默回退到顶层 —— 不报错,
# 只是守卫永远看着昨天的价格,正好在跳空那一刻失明。
REAL_SHAPE = {
    "symbol": "NVDA.US", "last": "211.940", "prev_close": "206.640",
    "overnight":   {"last": "217.140", "prev_close": "211.940"},
    "pre_market":  {"last": "211.650", "prev_close": "206.640"},
    "post_market": {"last": "216.500", "prev_close": "211.940"},
}
check("Overnight 取 overnight.last 而不是顶层 last",
      Q.last_price_from(REAL_SHAPE, "Overnight") == 217.14,
      f"得到 {Q.last_price_from(REAL_SHAPE, 'Overnight')}(顶层是 211.94)")
check("Overnight-Pre 同样取 overnight.last",
      Q.last_price_from(REAL_SHAPE, "Overnight-Pre") == 217.14)
check("Pre 取 pre_market.last", Q.last_price_from(REAL_SHAPE, "Pre") == 211.65)
check("Post 取 post_market.last", Q.last_price_from(REAL_SHAPE, "Post") == 216.50)
check("Intraday 取顶层 last", Q.last_price_from(REAL_SHAPE, "Intraday") == 211.94)
check("子对象缺席时回退顶层",
      Q.last_price_from({"last": "100.5"}, "Overnight") == 100.5)
check("价格是字符串也能解析", isinstance(Q.last_price_from(REAL_SHAPE, "Pre"), float))
check("0 / 缺失视为无报价,不是 0 元",
      Q.last_price_from({"last": "0"}, "Intraday") is None
      and Q.last_price_from({}, "Intraday") is None)


# 后续测试用确定性 session 桩
S.current_session = lambda: "Intraday"
S.minutes_to_close = lambda s: 300
S.outside_rth_for = lambda s, *, for_exit=False: "RTH_ONLY"


# ══════════════════════════════════════════════════════════════════════════
print("=== B. 平仓前撤在途单 ===")
# ══════════════════════════════════════════════════════════════════════════
class StubBroker:
    """可控桩:cancel 记账,order_detail 按 detail_status 回答。"""
    dry_run = False

    def __init__(self, detail_status="Cancelled", executed=0):
        self.detail_status, self.executed = detail_status, executed
        self.cancelled, self.placed = [], []

    def cancel(self, boid):
        self.cancelled.append(boid)
        return {"ok": True, "raw": {}}

    def order_detail(self, boid):
        return {"status": self.detail_status, "executed_quantity": self.executed}

    def _place(self, side, symbol, qty, price):
        self.placed.append((side, symbol, qty, price))
        return {"ok": True, "broker_order_id": f"B{len(self.placed)}",
                "status": "Filled", "fill_price": price, "raw": {}}

    def buy(self, symbol, qty, price, orth, client_order_id=None):
        return self._place("buy", symbol, qty, price)

    def sell(self, symbol, qty, price, orth, client_order_id=None):
        return self._place("sell", symbol, qty, price)


def seed_position_with_resting_order(db):
    """一个多头持仓 + 一张还挂着的卖单(复刻 flintrade-198 的状态)。"""
    pid = db.open_position(symbol="AAPL.US", side="long", qty=12, entry_price=331.21,
                           stop=328.0, target=337.6, risk_amt=38.52, source="technical")
    db.create_order(client_order_id="flintrade-old", symbol="AAPL.US", side="SELL",
                    qty=12, price=312.30, outside_rth="OVERNIGHT")
    db.update_order("flintrade-old", broker_order_id="B-OLD", status="submitted")
    return pid


# B1:在途单确实撤掉了 → 撤旧单 + 下新单
wipe()
db = DB(role="migrate")
seed_position_with_resting_order(db)
iid = db.submit_intent(source="risk_monitor", priority=101, symbol="AAPL.US",
                       side="close", entry_hint=308.55, reason="stop breached")
db.close()

brk = StubBroker(detail_status="Cancelled", executed=0)
ex = Executor(broker=brk)
ex.process_once()
rdb = DB(role="reader")
old = rdb.conn.execute("SELECT status FROM orders WHERE client_order_id='flintrade-old'").fetchone()
intent = rdb.conn.execute("SELECT status FROM intents WHERE id=?", (iid,)).fetchone()
n_pos = len(rdb.open_positions())
rdb.close()
check("撤掉了在途单", brk.cancelled == ["B-OLD"], f"cancelled={brk.cancelled}")
check("在途单落库为 cancelled", old["status"] == "cancelled", f"得到 {old['status']}")
check("撤单后照常下平仓单", len(brk.placed) == 1 and brk.placed[0][0] == "sell",
      f"placed={brk.placed}")
check("持仓已平", n_pos == 0, f"仍有 {n_pos} 个持仓")
check("意图落为 filled", intent["status"] == "filled", f"得到 {intent['status']}")

# B2:撤单没赶上成交 → 绝不能标 cancelled,也绝不能再下一张单
wipe()
db = DB(role="migrate")
seed_position_with_resting_order(db)
iid = db.submit_intent(source="risk_monitor", priority=101, symbol="AAPL.US",
                       side="close", entry_hint=308.55, reason="stop breached")
db.close()

brk = StubBroker(detail_status="Filled", executed=12)
ex = Executor(broker=brk)
ex.process_once()
rdb = DB(role="reader")
old = rdb.conn.execute("SELECT status FROM orders WHERE client_order_id='flintrade-old'").fetchone()
intent = rdb.conn.execute("SELECT status FROM intents WHERE id=?", (iid,)).fetchone()
n_pos = len(rdb.open_positions())
rdb.close()
check("赛跑输了:在途单保持 submitted(留给 Reconciler 结算)",
      old["status"] == "submitted", f"得到 {old['status']} —— 标成别的会静默丢掉这笔成交")
check("赛跑输了:不再下第二张单", brk.placed == [], f"placed={brk.placed}")
check("赛跑输了:意图作废为 cancelled", intent["status"] == "cancelled",
      f"得到 {intent['status']}")
check("赛跑输了:持仓保持 open,等 Reconciler 结算", n_pos == 1, f"得到 {n_pos}")


# ══════════════════════════════════════════════════════════════════════════
print("=== C. risk_monitor 逐仓止损守卫 ===")
# ══════════════════════════════════════════════════════════════════════════
def run_guard(price_map):
    """打桩报价,跑一轮 risk_monitor,返回它产生的日志。"""
    Q.last_prices = lambda syms, session=None: {s: price_map[s] for s in syms if s in price_map}
    rm = RiskMonitor(db=DB(role="risk_monitor"))
    try:
        return rm.run_once()
    finally:
        rm.db.close()


def pending_closes(symbol="AAPL.US"):
    rdb = DB(role="reader")
    n = rdb.conn.execute(
        """SELECT count(*) c FROM intents
           WHERE symbol=? AND side='close' AND status='pending'""", (symbol,)).fetchone()["c"]
    rdb.close()
    return n


# C1:多头跌破止损 → 投 close 意图
wipe()
db = DB(role="migrate")
db.open_position(symbol="AAPL.US", side="long", qty=12, entry_price=331.21,
                 stop=328.0, target=337.6, risk_amt=38.52, source="technical")
db.kv_set("risk_day", _dt.datetime.now().strftime("%Y-%m-%d"))  # 抑制日切噪声
db.close()
log = run_guard({"AAPL.US": 308.55})
check("多头跌破止损 → 投 close 意图", pending_closes() == 1,
      f"log={log}")
check("日志点名了价格与止损", any("308.55" in l and "328.0" in l for l in log), f"log={log}")

# C2:同一持仓已有在途平仓意图 → 不重复投
log = run_guard({"AAPL.US": 307.00})
check("已有在途平仓意图 → 不重复投", pending_closes() == 1, f"log={log}")

# C3:取不到价 → 什么都不做(绝不拿陈旧价触发)
wipe()
db = DB(role="migrate")
db.open_position(symbol="AAPL.US", side="long", qty=12, entry_price=331.21,
                 stop=328.0, source="technical")
db.kv_set("risk_day", _dt.datetime.now().strftime("%Y-%m-%d"))
db.close()
log = run_guard({})           # 报价缺席
check("取不到价 → 不投意图", pending_closes() == 0, f"log={log}")

# C4:价格在止损之上 → 不投
log = run_guard({"AAPL.US": 330.00})
check("价格高于止损 → 不投", pending_closes() == 0, f"log={log}")

# C5:空头涨破止损 → 投(方向不能搞反)
wipe()
db = DB(role="migrate")
db.open_position(symbol="TSLA.US", side="short", qty=5, entry_price=300.0,
                 stop=310.0, source="event")
db.kv_set("risk_day", _dt.datetime.now().strftime("%Y-%m-%d"))
db.close()
log = run_guard({"TSLA.US": 311.5})
check("空头涨破止损 → 投 close 意图", pending_closes("TSLA.US") == 1, f"log={log}")

log = run_guard({"TSLA.US": 305.0})
check("空头价格低于止损 → 不额外投", pending_closes("TSLA.US") == 1, f"log={log}")

# C6:陈年的 approved 平仓意图不得锁死守卫
# (真实库里躺着十几条 —— 订单走到终态时 intents 行没被回写,永远停在 approved。
#  若把 approved 当在途,NVDA/AAPL/AMZN 的止损守卫会被永久上锁。)
wipe()
db = DB(role="migrate")
db.open_position(symbol="NVDA.US", side="long", qty=18, entry_price=215.6,
                 stop=208.5, source="event")
old_iid = db.submit_intent(source="technical", symbol="NVDA.US", side="close",
                           reason="昨天那笔,订单早已 filled")
db.conn.execute("UPDATE intents SET status='approved' WHERE id=?", (old_iid,))
db.create_order(client_order_id="flintrade-orphan", symbol="NVDA.US", side="SELL",
                qty=18, price=210.0, intent_id=old_iid)
db.update_order("flintrade-orphan", status="filled", broker_order_id="B-ORPHAN")
db.kv_set("risk_day", _dt.datetime.now().strftime("%Y-%m-%d"))
db.close()
log = run_guard({"NVDA.US": 207.0})
check("陈年 approved 意图(订单已终态)不锁死守卫", pending_closes("NVDA.US") == 1,
      f"log={log}")

# C6b:但真挂着的平仓单必须算在途
wipe()
db = DB(role="migrate")
db.open_position(symbol="NVDA.US", side="long", qty=18, entry_price=215.6,
                 stop=208.5, source="event")
db.create_order(client_order_id="flintrade-live", symbol="NVDA.US", side="SELL",
                qty=18, price=208.0)
db.update_order("flintrade-live", status="submitted", broker_order_id="B-LIVE")
db.kv_set("risk_day", _dt.datetime.now().strftime("%Y-%m-%d"))
db.close()
log = run_guard({"NVDA.US": 207.0})
check("真挂着的平仓单 → 算在途,不重复投", pending_closes("NVDA.US") == 0, f"log={log}")

# C7:stop 为 NULL 的持仓不参与
wipe()
db = DB(role="migrate")
db.open_position(symbol="GLD.US", side="long", qty=3, entry_price=250.0,
                 stop=None, source="technical")
db.kv_set("risk_day", _dt.datetime.now().strftime("%Y-%m-%d"))
db.close()
log = run_guard({"GLD.US": 1.0})
check("stop 为 NULL → 跳过,不投意图", pending_closes("GLD.US") == 0, f"log={log}")


# ══════════════════════════════════════════════════════════════════════════
print("\n" + "=" * 40)
print(f"PASS={len(PASS)}  FAIL={len(FAIL)}")
if FAIL:
    for f in FAIL:
        print(f"  ✗ {f}")
    raise SystemExit(1)
print("止损守卫 / 撤单 / 会话死区 全绿 ✓")
