"""P0 修复冒烟测试 —— 临时 db + 桩 broker,零真实副作用。
跑法: FLINTRADE_DRY_RUN=1 FLINTRADE_DB=/tmp/flintrade_p0_test.db python3 -m agent.test_p0

覆盖 5 个 P0 修复:
  1. broker.positions() 返回 None(API 失败)时,drift 检测跳过,不落任何信号。
  2. broker.positions() 返回 [](确实空仓)+ 库内有仓位 → 恰好一次 drift 信号,
     第二轮(delta 未变)不再重复报。
  3. 结算幂等:同一 broker_order_id 已有成交记录时,不重复结算(不产生新成交/持仓)。
  4. reconciled close 携带原始 intent 的 reason,而不是硬编码 "reconciled close"。
  5. 在途重复下单:executor 层拒绝同标的已有在途开仓单的新 intent;
     risk_gate 层把在途开仓单计入 gross/symbol/cluster/并发仓 敞口。
"""
from __future__ import annotations

import os
import sys
import tempfile

# 强制临时 db + dry-run(在 import db 前设好 env)
os.environ.setdefault("FLINTRADE_DB", os.path.join(tempfile.gettempdir(), "flintrade_p0_test.db"))
os.environ["FLINTRADE_DRY_RUN"] = "1"
if os.path.exists(os.environ["FLINTRADE_DB"]):
    os.remove(os.environ["FLINTRADE_DB"])
for ext in ("-wal", "-shm"):
    p = os.environ["FLINTRADE_DB"] + ext
    if os.path.exists(p):
        os.remove(p)

from agent.db import DB, init_db                    # noqa: E402
from agent.reconciler import Reconciler              # noqa: E402
from agent.risk_gate import RiskGate                 # noqa: E402
from agent.broker import Broker                      # noqa: E402
from agent import session as S                       # noqa: E402
from agent.executor import Executor                  # noqa: E402

PASS, FAIL = [], []


def check(name, cond, detail=""):
    (PASS if cond else FAIL).append(name)
    print(f"  {'✓' if cond else '✗ FAIL'} {name}" + (f" — {detail}" if detail and not cond else ""))


def wipe():
    """每个测试段落之间清空受影响的表,保持互不干扰。"""
    boot = DB(role="migrate")
    for t in ("trades", "orders", "positions", "intents", "signals"):
        boot.conn.execute(f"DELETE FROM {t}")
    boot.update_risk(equity=10000.0, day_start_equity=10000.0, day_realized_pnl=0, open_risk=0)
    boot.close()


init_db()

# ─────────────────────────────────────────────────────────────────────────
# FIX 6: session 周末守卫 —— CLI 时刻表不含日期,周六中午不能匹配成 Intraday。
# 必须在下面全局 stub 掉 S.current_session 之前测(测的是真函数)。
# ─────────────────────────────────────────────────────────────────────────
print("=== FIX 6: session 周末守卫(周六不返回 Intraday)===")
from datetime import datetime as _real_datetime  # noqa: E402

_STATIC_SESSIONS = [{"market": "US", "sessions": [
    {"session": "Pre", "open": "4:00:00.0", "close": "9:30:00.0"},
    {"session": "Intraday", "open": "9:30:00.0", "close": "16:00:00.0"},
    {"session": "Post", "open": "16:00:00.0", "close": "20:00:00.0"},
]}]


class _FakeDT(_real_datetime):
    _now = None

    @classmethod
    def now(cls, tz=None):
        return cls._now


_orig_dt, _orig_fetch = S.datetime, S._get_session_json
S.datetime = _FakeDT
S._get_session_json = lambda: _STATIC_SESSIONS

_FakeDT._now = _real_datetime(2026, 8, 3, 12, 0, tzinfo=S.ET)   # 周一午间
check("阳性对照:周一 12:00 ET → Intraday", S.current_session() == "Intraday",
      f"got={S.current_session()}")
_FakeDT._now = _real_datetime(2026, 8, 1, 12, 0, tzinfo=S.ET)   # 周六午间(flintrade-210 事故时刻)
check("周六 12:00 ET → Closed(不再误判 Intraday)", S.current_session() == "Closed",
      f"got={S.current_session()}")
_FakeDT._now = _real_datetime(2026, 8, 2, 21, 0, tzinfo=S.ET)   # 周日晚(属周一的 Overnight)
check("周日 21:00 ET → Overnight(周末守卫不误伤)", S.current_session() == "Overnight",
      f"got={S.current_session()}")
_FakeDT._now = _real_datetime(2026, 8, 1, 21, 0, tzinfo=S.ET)   # 周六晚(次日周日,无 Overnight)
check("周六 21:00 ET → Closed", S.current_session() == "Closed",
      f"got={S.current_session()}")

S.datetime, S._get_session_json = _orig_dt, _orig_fetch

# 确定性 session(照抄 test_phase1 的桩法)
S.current_session = lambda: "Intraday"
S.minutes_to_close = lambda s: 300
S.outside_rth_for = lambda s, *, for_exit=False: "RTH_ONLY"


# ─────────────────────────────────────────────────────────────────────────
# 桩 broker:非 dry-run,positions()/assets() 行为可控;order_detail 防御式实现
# 避免 _settle_pending 在这些测试里意外触发真实 CLI 调用。
# ─────────────────────────────────────────────────────────────────────────
class BrokerPositionsNone:
    dry_run = False

    def positions(self):
        return None

    def assets(self):
        return []

    def order_detail(self, boid):
        return {"status": "Unknown"}


class BrokerPositionsFlat:
    dry_run = False

    def positions(self):
        return []

    def assets(self):
        return []

    def order_detail(self, boid):
        return {"status": "Unknown"}


print("=== FIX 1+2a: broker.positions() -> None 时 drift 跳过,不落信号 ===")
wipe()
recon = Reconciler(broker=BrokerPositionsNone())
log = recon.run_once()
sig_count = DB(role="reader").conn.execute("SELECT COUNT(*) c FROM signals").fetchone()["c"]
check("positions()->None: 无信号写入", sig_count == 0, f"signals={sig_count}")
check("positions()->None: 日志含跳过说明",
      any(("不可用" in l or "跳过" in l) for l in log), f"log={log}")

print("=== FIX 2b: drift 去重(同一 delta 只报一次)===")
wipe()
boot = DB(role="migrate")
boot.open_position(symbol="NVDA.US", side="long", qty=10, entry_price=100.0,
                    stop=98.0, target=None, source="technical")
boot.close()

recon2 = Reconciler(broker=BrokerPositionsFlat())
log1 = recon2.run_once()
sig_after_1 = DB(role="reader").conn.execute(
    "SELECT COUNT(*) c FROM signals WHERE kind='drift'"
).fetchone()["c"]
check("首轮:恰好 1 条 drift 信号", sig_after_1 == 1, f"count={sig_after_1} log={log1}")

log2 = recon2.run_once()
sig_after_2 = DB(role="reader").conn.execute(
    "SELECT COUNT(*) c FROM signals WHERE kind='drift'"
).fetchone()["c"]
check("次轮(delta 未变):无新增信号", sig_after_2 == sig_after_1,
      f"after1={sig_after_1} after2={sig_after_2} log2={log2}")


print("=== FIX 4: 结算幂等(同 broker_order_id 不重复结算)===")
wipe()
recon3 = Reconciler(broker=Broker(dry_run=True))
boot = DB(role="migrate")
pos_id = boot.open_position(symbol="TSLA.US", side="long", qty=5, entry_price=200.0,
                            stop=195.0, target=None, source="technical")
boot.append_trade(symbol="TSLA.US", action="BUY", qty=5, fill_price=200.0,
                  commission=0.1, source="technical", broker_order_id="DUPBOID-1",
                  position_id=pos_id, reason="original open")
order_id = boot.create_order(client_order_id="flintrade-idem-1", symbol="TSLA.US", side="BUY",
                             qty=5, price=200.0, outside_rth="RTH_ONLY")
boot.update_order("flintrade-idem-1", broker_order_id="DUPBOID-1")
boot.close()

trades_before = DB(role="reader").conn.execute("SELECT COUNT(*) c FROM trades").fetchone()["c"]
positions_before = DB(role="reader").conn.execute("SELECT COUNT(*) c FROM positions").fetchone()["c"]

log4 = recon3.run_once()

order_row = DB(role="reader").conn.execute(
    "SELECT status FROM orders WHERE client_order_id='flintrade-idem-1'"
).fetchone()
trades_after = DB(role="reader").conn.execute("SELECT COUNT(*) c FROM trades").fetchone()["c"]
positions_after = DB(role="reader").conn.execute("SELECT COUNT(*) c FROM positions").fetchone()["c"]

check("幂等:order 变 filled", order_row["status"] == "filled", f"status={order_row['status']}")
check("幂等:无新增成交", trades_after == trades_before,
      f"before={trades_before} after={trades_after}")
check("幂等:无新增持仓", positions_after == positions_before,
      f"before={positions_before} after={positions_after}")
check("幂等:日志含 settle-skip", any("settle-skip" in l for l in log4), f"log={log4}")


print("=== FIX 3: reconciled close 携带原始 intent reason ===")
wipe()
recon4 = Reconciler(broker=Broker(dry_run=True))
prod = DB(role="technical")
boot = DB(role="migrate")
pos_id2 = boot.open_position(symbol="AAPL.US", side="long", qty=10, entry_price=100.0,
                             stop=95.0, target=None, source="technical")
intent_id = prod.submit_intent(source="technical", symbol="AAPL.US", side="close",
                               entry_hint=105.0, reason="THESIS-XYZ")
boot.create_order(client_order_id="flintrade-close-1", symbol="AAPL.US", side="SELL",
                  qty=10, price=105.0, outside_rth="RTH_ONLY", intent_id=intent_id)
boot.update_order("flintrade-close-1", broker_order_id="CLOSEBOID-1")
boot.close()

log3 = recon4.run_once()

last_trade = dict(DB(role="reader").recent_trades(1)[0])
check("reconciled close: reason 取自原始 intent",
      last_trade["reason"] == "THESIS-XYZ", f"reason={last_trade['reason']} log={log3}")


print("=== FIX 5a: 在途重复下单 → executor 拒绝 ===")
wipe()
boot = DB(role="migrate")
boot.create_order(client_order_id="flintrade-inflight-1", symbol="NVDA.US", side="BUY",
                  qty=19, price=205.0, outside_rth="RTH_ONLY")
boot.close()

prod = DB(role="technical")
iid = prod.submit_intent(source="technical", symbol="NVDA.US", side="long",
                         entry_hint=205.0, stop=200.0, target=215.0,
                         confidence=70, reason="dup attempt")
ex = Executor()
ex.process_once()
row = DB(role="reader").conn.execute(
    "SELECT status, reject_reason FROM intents WHERE id=?", (iid,)
).fetchone()
check("同标的在途开仓单 → intent 拒绝", row["status"] == "rejected",
      f"status={row['status']}")
check("拒绝原因含 在途", "在途" in (row["reject_reason"] or ""),
      f"reason={row['reject_reason']}")


print("=== FIX 5b: risk_gate 计入在途敞口(qty 相应收紧)===")


def long_intent(symbol, entry, stop, target=None):
    return {"id": 999, "source": "technical", "symbol": symbol, "side": "long",
            "entry_hint": entry, "stop": stop, "target": target, "reason": "test"}


base_kwargs = dict(equity=10000.0, halted=False, open_positions=[], recent_trades=[],
                   session="Intraday", minutes_to_close=300, volume_ratio=1.0)

gate_no_inflight = RiskGate(**base_kwargs, open_orders=[])
v_no_inflight = gate_no_inflight.evaluate(long_intent("AAPL.US", 300.0, 294.0))

gate_with_inflight = RiskGate(**base_kwargs, open_orders=[
    {"symbol": "NVDA.US", "side": "BUY", "qty": 19, "price": 205.0},
])
v_with_inflight = gate_with_inflight.evaluate(long_intent("AAPL.US", 300.0, 294.0))

check("两种情况都批准", v_no_inflight.approved and v_with_inflight.approved,
      f"no_inflight={v_no_inflight} with_inflight={v_with_inflight}")
check("计入在途敞口后 qty 严格收紧",
      v_with_inflight.approved and v_no_inflight.approved and v_with_inflight.qty < v_no_inflight.qty,
      f"no_inflight.qty={v_no_inflight.qty} with_inflight.qty={v_with_inflight.qty}")


print("=== FIX 7: 开仓挂单 TTL 超时撤单(安全网)===")
wipe()


class BrokerCancelOk:
    """撤单成功、复核无成交的桩(TTL 主路径)。"""
    dry_run = False

    def cancel(self, boid):
        return {"ok": True}

    def order_detail(self, boid):
        return {"status": "Cancelled", "executed_quantity": 0}


class BrokerCancelRace:
    """撤单没赶上成交:复核发现已 Filled(竞态路径)。"""
    dry_run = False

    def cancel(self, boid):
        return {"ok": False}

    def order_detail(self, boid):
        return {"status": "Filled", "executed_quantity": 10}


prod = DB(role="technical")
iid_stale = prod.submit_intent(source="technical", symbol="NVDA.US", side="long",
                               entry_hint=200.75, stop=197.9, target=206.2,
                               confidence=73, reason="weekend stale order")
iid_fresh = prod.submit_intent(source="technical", symbol="GLD.US", side="long",
                               entry_hint=375.0, stop=372.0, target=382.0,
                               confidence=60, reason="fresh order")
prod.close()
dex = DB(role="executor")
dex.decide_intent(iid_stale, "approved")   # 已下单待成交的 intent 状态
dex.decide_intent(iid_fresh, "approved")
dex.close()

boot = DB(role="migrate")
boot.create_order(client_order_id="flintrade-stale-1", symbol="NVDA.US", side="BUY",
                  qty=11, price=200.75, outside_rth="RTH_ONLY", intent_id=iid_stale)
boot.update_order("flintrade-stale-1", broker_order_id="STALEBOID-1")
boot.conn.execute("UPDATE orders SET created_at='2026-08-01T16:01:24Z' "
                  "WHERE client_order_id='flintrade-stale-1'")   # 回填成陈年挂单
boot.create_order(client_order_id="flintrade-fresh-1", symbol="GLD.US", side="BUY",
                  qty=10, price=375.0, outside_rth="RTH_ONLY", intent_id=iid_fresh)
boot.update_order("flintrade-fresh-1", broker_order_id="FRESHBOID-1")
boot.close()

ex_ttl = Executor(broker=BrokerCancelOk())
log_ttl = ex_ttl.process_once()

reader = DB(role="reader")
row_stale = reader.conn.execute(
    "SELECT status FROM orders WHERE client_order_id='flintrade-stale-1'").fetchone()
row_fresh = reader.conn.execute(
    "SELECT status FROM orders WHERE client_order_id='flintrade-fresh-1'").fetchone()
row_intent = reader.conn.execute(
    "SELECT status, reject_reason FROM intents WHERE id=?", (iid_stale,)).fetchone()
check("超时开仓单已撤(order → cancelled)", row_stale["status"] == "cancelled",
      f"status={row_stale['status']} log={log_ttl}")
check("对应 intent → expired 且注明 TTL", row_intent["status"] == "expired"
      and "TTL" in (row_intent["reject_reason"] or ""),
      f"intent={dict(row_intent)}")
check("未超时挂单不受影响(仍 submitted)", row_fresh["status"] == "submitted",
      f"status={row_fresh['status']}")

print("=== FIX 7b: TTL 撤单竞态 —— 撤单前已成交则留给 Reconciler ===")
boot = DB(role="migrate")
boot.conn.execute("UPDATE orders SET created_at='2026-08-01T16:01:24Z' "
                  "WHERE client_order_id='flintrade-fresh-1'")   # 把它也变陈年
boot.close()

ex_race = Executor(broker=BrokerCancelRace())
log_race = ex_race.process_once()

row_race = DB(role="reader").conn.execute(
    "SELECT status FROM orders WHERE client_order_id='flintrade-fresh-1'").fetchone()
intent_race = DB(role="reader").conn.execute(
    "SELECT status FROM intents WHERE id=?", (iid_fresh,)).fetchone()
check("竞态:已成交的单保持 submitted(交 Reconciler 结算)",
      row_race["status"] == "submitted", f"status={row_race['status']} log={log_race}")
check("竞态:intent 不被误标 expired", intent_race["status"] == "approved",
      f"status={intent_race['status']}")

print("=== FIX 8: 做梦调度认 Overnight-Pre 为睡眠窗口 ===")
# 背景:Overnight-Pre 修复(03:50-04:00 ET 不再解析成 'Closed')让只认
# 'Closed' 的 should_dream 在工作日被静默饿死(2026-08-06 首个漏做梦日)。
import agent.reflect as R  # noqa: E402

_orig_cs = R.current_session
_kv = DB(role="migrate")
_kv.kv_set("last_dream_date", "2000-01-01")
_kv.close()

R.current_session = lambda: "Overnight-Pre"
check("Overnight-Pre + 今日未做梦 → 该做梦", R.should_dream() is True)
R.current_session = lambda: "Overnight"
check("Overnight(夜盘)→ 该做梦", R.should_dream() is True)
R.current_session = lambda: "Pre"
check("Pre(交易时段)→ 不做梦", R.should_dream() is False)
R.current_session = lambda: "Intraday"
check("Intraday(交易时段)→ 不做梦", R.should_dream() is False)
R.current_session = lambda: "Closed"
check("Closed(周末)→ 该做梦", R.should_dream() is True)

_kv = DB(role="migrate")
_kv.kv_set("last_dream_date", R._today_et(None))
_kv.close()
R.current_session = lambda: "Overnight-Pre"
check("今天已做过梦 → 不重复做", R.should_dream() is False)
R.current_session = _orig_cs

print(f"\n{'='*40}\nPASS={len(PASS)}  FAIL={len(FAIL)}")
if FAIL:
    print("FAILED:", FAIL)
    sys.exit(1)
print("P0 全绿 ✓")
