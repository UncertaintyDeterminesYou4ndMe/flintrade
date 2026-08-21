"""不变量自检 + LLM 调用留痕 冒烟测试 —— 临时 db + 临时日志目录,零真实副作用。
跑法: python3 -m agent.test_invariants

覆盖:
  1. 干净账本 → 零违反;正常结算一笔后仍零违反(open_risk 对账走真实 settle 路径)。
  2. 每类不变量的「注入撕裂 → 被点名」:open_risk 漂移 / filled 无成交 /
     approved 孤儿(含宽限期内不报)/ filled order 无成交 / 多空对冲 / 平仓腿无 pnl。
  3. 结果落 kv('invariants_last')。
  4. llm._log_call 逐字落盘可重建;FLINTRADE_LLM_DRY=1 的 complete() 不落盘;过期日志被清理。
"""
from __future__ import annotations

import json
import os
import tempfile

# 强制临时 db + dry-run + 临时留痕目录(在 import 前设好 env)
os.environ.setdefault("FLINTRADE_DB", os.path.join(tempfile.gettempdir(), "flintrade_inv_test.db"))
os.environ["FLINTRADE_DRY_RUN"] = "1"
os.environ["FLINTRADE_LLM_DRY"] = "1"
_LLM_LOG_DIR = tempfile.mkdtemp(prefix="flintrade_llm_test_")
os.environ["FLINTRADE_LLM_LOG_DIR"] = _LLM_LOG_DIR
if os.path.exists(os.environ["FLINTRADE_DB"]):
    os.remove(os.environ["FLINTRADE_DB"])
for ext in ("-wal", "-shm"):
    p = os.environ["FLINTRADE_DB"] + ext
    if os.path.exists(p):
        os.remove(p)

from agent import invariants, llm, settle          # noqa: E402
from agent.db import DB, init_db                   # noqa: E402

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


def violations() -> list[str]:
    return [x.removeprefix("VIOLATION ") for x in invariants.run_once()]


init_db()

# ─────────────────────────────────────────────────────────────────────────
print("=== 1. 干净账本零违反;真实结算路径自洽 ===")
wipe()
check("干净账本零违反", violations() == [], str(violations()))

ex = DB(role="executor")
pos_id = settle.settle_open(ex, symbol="NVDA.US", side="long", qty=10, fill_price=200.0,
                            stop=195.0, target=210.0, commission_per_share=0.02)
v = violations()
check("结算一笔开仓后仍零违反(open_risk 对账)", v == [], str(v))

# ─────────────────────────────────────────────────────────────────────────
print("=== 2. open_risk 漂移被点名 ===")
boot = DB(role="migrate")
boot.update_risk(open_risk=999.0)   # 注入漂移:与 Σrisk_amt=50 不符
boot.close()
v = violations()
check("open_risk_ledger 违反被点名", any("open_risk_ledger" in x for x in v), str(v))
ex.update_risk(open_risk=50.0)      # 修回

# ─────────────────────────────────────────────────────────────────────────
print("=== 3. filled intent 无成交被点名;宽限期内 approved 不报 ===")
tech = DB(role="technical")
iid = tech.submit_intent(symbol="AAPL.US", side="long", stop=180.0)
ex.decide_intent(iid, "filled")     # 说成交了但没有 trade
v = violations()
check("filled_has_trade 违反被点名", any("filled_has_trade" in x and str(iid) in x for x in v), str(v))

iid2 = tech.submit_intent(symbol="MSFT.US", side="long", stop=400.0)
ex.decide_intent(iid2, "approved")  # 刚 approved,decided_at=now → 宽限期内
v = violations()
check("宽限期内的 approved 无单不报", not any("approved_has_order" in x for x in v), str(v))

boot = DB(role="migrate")
boot.conn.execute("UPDATE intents SET decided_at='2026-01-01T00:00:00Z' WHERE id=?", (iid2,))
boot.close()
v = violations()
check("超宽限的 approved 孤儿被点名", any("approved_has_order" in x and str(iid2) in x for x in v), str(v))

# ─────────────────────────────────────────────────────────────────────────
print("=== 4. filled order 无成交 / 多空对冲 / 平仓腿 pnl 语义 ===")
wipe()
ex2 = DB(role="executor")
ex2.create_order(client_order_id="test-coid-1", symbol="TSLA.US", side="BUY", qty=5)
ex2.update_order("test-coid-1", status="filled", broker_order_id="B999")
v = violations()
check("60s 结算宽限内不报(合法瞬态)", not any("filled_order_trade" in x for x in v), str(v))
boot = DB(role="migrate")
boot.conn.execute("UPDATE orders SET updated_at='2026-01-01T00:00:00Z' "
                  "WHERE client_order_id='test-coid-1'")
boot.close()
v = violations()
check("filled_order_trade 违反被点名", any("filled_order_trade" in x for x in v), str(v))

wipe()
ex3 = DB(role="executor")
ex3.open_position(symbol="GLD.US", side="long", qty=5, entry_price=250.0, risk_amt=10.0)
ex3.open_position(symbol="GLD.US", side="short", qty=5, entry_price=251.0, risk_amt=10.0)
ex3.update_risk(open_risk=20.0)
v = violations()
check("no_hedged_symbol 违反被点名", any("no_hedged_symbol" in x and "GLD" in x for x in v), str(v))

wipe()
ex4 = DB(role="executor")
ex4.append_trade(symbol="USO.US", action="SELL", qty=3, fill_price=80.0)  # 平仓腿无 pnl
tid = ex4.append_trade(symbol="USO.US", action="SELL", qty=3, fill_price=80.0,
                       attribution="test")  # test 归因豁免
v = violations()
check("trade_leg_pnl 违反被点名", any("trade_leg_pnl" in x for x in v), str(v))
check("test 归因的行豁免", sum("trade_leg_pnl" in x for x in v) == 1, str(v))

# ─────────────────────────────────────────────────────────────────────────
print("=== 5. 结果落 kv ===")
wipe()
violations()
last = json.loads(DB(role="reader").kv_get("invariants_last") or "{}")
check("kv invariants_last 写入且 ok=true", last.get("ok") is True, str(last))

# ─────────────────────────────────────────────────────────────────────────
print("=== 6. LLM 调用留痕 ===")
rec = {"ts": "2026-08-14T00:00:00Z", "tag": "test", "tier": "trader",
       "provider": "deepseek", "model": "x", "wire": "openai", "duration_ms": 1,
       "ok": True, "system_prompt": "SYS", "user_prompt": "USER 中文", "response": "RESP"}
llm._log_call(rec)
files = [f for f in os.listdir(_LLM_LOG_DIR) if f.endswith(".jsonl")]
check("留痕文件生成", len(files) == 1, str(files))
if files:
    with open(os.path.join(_LLM_LOG_DIR, files[0]), encoding="utf-8") as f:
        back = json.loads(f.readlines()[-1])
    check("留痕逐字可重建", back == rec, str(back))

n_before = sum(1 for f in os.listdir(_LLM_LOG_DIR) for _ in open(os.path.join(_LLM_LOG_DIR, f)))
out = llm.complete("ping", tier="trader")   # FLINTRADE_LLM_DRY=1 → 不真调、不落盘
n_after = sum(1 for f in os.listdir(_LLM_LOG_DIR) for _ in open(os.path.join(_LLM_LOG_DIR, f)))
check("dry-run 的 complete 不落盘", n_after == n_before and out != "", f"{n_before}->{n_after}")

old = os.path.join(_LLM_LOG_DIR, "20250101.jsonl")
open(old, "w").close()
llm._purge_done = False                      # 触发下一次写时的清理
llm._log_call(rec)
check("过期留痕被清理", not os.path.exists(old))

# ─────────────────────────────────────────────────────────────────────────
print("=== 7. CLI 子进程环境洗白 ===")
os.environ["LONGBRIDGE_APP_KEY"] = "fake-key"
os.environ["LONGBRIDGE_APP_SECRET"] = "fake-secret"
os.environ["LONGBRIDGE_ACCESS_TOKEN"] = "fake-token"
os.environ["DEEPSEEK_API_KEY"] = "fake-ds"
os.environ["CLAUDE_CODE_OAUTH_TOKEN"] = "fake-oauth"
os.environ["ANTHROPIC_API_KEY"] = "fake-ant"
env_claude = llm._cli_env("claude")
check("券商凭据被剔除", not any(k.startswith("LONGBRIDGE_") for k in env_claude))
check("他厂 key 被剔除", "DEEPSEEK_API_KEY" not in env_claude)
check("claude 自身鉴权放行", "CLAUDE_CODE_OAUTH_TOKEN" in env_claude
      and "ANTHROPIC_API_KEY" in env_claude)
check("非敏感变量保留", "HOME" in env_claude and "PATH" in env_claude)
env_kimi = llm._cli_env("kimi")
check("kimi 不放行 claude 鉴权", "ANTHROPIC_API_KEY" not in env_kimi
      and not any(k.startswith("LONGBRIDGE_") for k in env_kimi))
for k in ("LONGBRIDGE_APP_KEY", "LONGBRIDGE_APP_SECRET", "LONGBRIDGE_ACCESS_TOKEN",
          "DEEPSEEK_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"):
    del os.environ[k]

# ─────────────────────────────────────────────────────────────────────────
print("=== 8. 分批止盈:部分平仓账本闭环 ===")
wipe()
ex5 = DB(role="executor")
pid = settle.settle_open(ex5, symbol="NVDA.US", side="long", qty=10, fill_price=200.0,
                         stop=194.0, target=210.0, target2=216.0,
                         commission_per_share=0.02)
pos = dict(DB(role="reader").conn.execute("SELECT * FROM positions WHERE id=?", (pid,)).fetchone())
check("开仓落 target2", pos["target2"] == 216.0 and pos["t1_done"] == 0)
check("开仓后 open_risk=60", abs(DB(role="reader").get_risk()["open_risk"] - 60.0) < 0.01)

pnl1 = settle.settle_close(ex5, held=pos, fill_price=211.0, qty=5,
                           commission_per_share=0.02)  # T1: 平半仓
pos = dict(DB(role="reader").conn.execute("SELECT * FROM positions WHERE id=?", (pid,)).fetchone())
check("部分平仓后仓位仍 open 且减半", pos["status"] == "open" and pos["qty"] == 5)
check("t1_done 置位", pos["t1_done"] == 1)
check("risk_amt 按比例释放", abs(pos["risk_amt"] - 30.0) < 0.01)
check("部分平仓 pnl 正确", abs(pnl1 - (11.0 * 5 - 0.1)) < 0.01, str(pnl1))
check("部分平仓后不变量全绿", violations() == [], str(violations()))

pnl2 = settle.settle_close(ex5, held=pos, fill_price=216.5, qty=5,
                           commission_per_share=0.02)  # T2: 清仓
pos = dict(DB(role="reader").conn.execute("SELECT * FROM positions WHERE id=?", (pid,)).fetchone())
r = DB(role="reader").get_risk()
check("清仓后 closed 且 open_risk 归零", pos["status"] == "closed" and abs(r["open_risk"]) < 0.01)
check("清仓后不变量全绿", violations() == [], str(violations()))

# ─────────────────────────────────────────────────────────────────────────
print("=== 9. 止盈守卫(risk_monitor)分批触发 ===")
from agent import quotes as Q                       # noqa: E402
from agent import session as S                      # noqa: E402
from agent.risk_monitor import RiskMonitor          # noqa: E402
from agent.risk_gate import RiskGate                # noqa: E402
S.current_session = lambda: "Intraday"              # 守卫需要可交易时段

def guard_at(price_map):
    Q.last_prices = lambda syms, session=None: {s: price_map[s] for s in syms if s in price_map}
    log = []
    rm = RiskMonitor(db=DB(role="risk_monitor"))
    rm._position_guards(log)
    rm.db.close()
    return log

def pending(symbol):
    return [dict(r) for r in DB(role="reader").conn.execute(
        "SELECT * FROM intents WHERE symbol=? AND status='pending'", (symbol,)).fetchall()]

def clear_pending():
    boot = DB(role="migrate")
    boot.conn.execute("UPDATE intents SET status='cancelled' WHERE status='pending'")
    boot.close()

wipe()
ex6 = DB(role="executor")
pid = settle.settle_open(ex6, symbol="MU.US", side="long", qty=8, fill_price=900.0,
                         stop=873.0, target=945.0, target2=990.0,
                         commission_per_share=0.02)
log = guard_at({"MU.US": 920.0})
check("未触及任何位:不投意图", not pending("MU.US"), str(log))

log = guard_at({"MU.US": 946.0})
p = pending("MU.US")
check("T1 命中:投半仓 close 意图", len(p) == 1 and p[0]["side"] == "close"
      and p[0]["hypothesis_qty"] == 4, str(p))
log = guard_at({"MU.US": 946.0})
check("在途平仓意图挡住重复触发", len(pending("MU.US")) == 1, str(log))
clear_pending()

# 模拟 T1 已成交:部分平仓 → t1_done=1
pos = dict(DB(role="reader").conn.execute("SELECT * FROM positions WHERE id=?", (pid,)).fetchone())
settle.settle_close(ex6, held=pos, fill_price=946.0, qty=4, commission_per_share=0.02)
log = guard_at({"MU.US": 947.0})
check("T1 已兑现后同价不再触发", not pending("MU.US"), str(log))
log = guard_at({"MU.US": 991.0})
p = pending("MU.US")
check("T2 命中:清剩余仓(无 hypothesis_qty)", len(p) == 1
      and p[0]["hypothesis_qty"] is None, str(p))
clear_pending()

# 止损优先于止盈;qty=1 时 T1 直接清仓
wipe()
ex7 = DB(role="executor")
settle.settle_open(ex7, symbol="SNDK.US", side="long", qty=1, fill_price=1500.0,
                   stop=1455.0, target=1575.0, target2=1650.0,
                   commission_per_share=0.02)
log = guard_at({"SNDK.US": 1450.0})
p = pending("SNDK.US")
check("止损优先触发", len(p) == 1 and "stop breached" in (p[0]["reason"] or ""), str(p))
clear_pending()
log = guard_at({"SNDK.US": 1580.0})
p = pending("SNDK.US")
check("qty=1 时 T1 整仓平", len(p) == 1 and p[0]["hypothesis_qty"] is None, str(p))
clear_pending()

# 策略隔离:非手册标的(mg7/金银油)的 target 保持咨询语义,守卫不碰;stop 照常机械
wipe()
ex8 = DB(role="executor")
settle.settle_open(ex8, symbol="AAPL.US", side="long", qty=8, fill_price=300.0,
                   stop=294.0, target=306.0, commission_per_share=0.02)
log = guard_at({"AAPL.US": 307.0})
check("非手册标的 target 命中不触发", not pending("AAPL.US"), str(log))
log = guard_at({"AAPL.US": 293.0})
p = pending("AAPL.US")
check("非手册标的 stop 照常触发", len(p) == 1 and "stop breached" in (p[0]["reason"] or ""), str(p))
clear_pending()

# risk_gate:平仓意图带 hypothesis_qty → 部分放行
gate = RiskGate(equity=10000, halted=False,
                open_positions=[{"symbol": "MU.US", "side": "long", "qty": 8,
                                 "entry_price": 900.0, "risk_amt": 216.0}],
                recent_trades=[], session="Intraday", minutes_to_close=120)
v = gate.evaluate({"symbol": "MU.US", "side": "close", "hypothesis_qty": 4})
check("gate 平仓尊重 hypothesis_qty", v.approved and v.qty == 4, str(v))
v = gate.evaluate({"symbol": "MU.US", "side": "close", "hypothesis_qty": None})
check("gate 无 hypothesis_qty 整仓平", v.approved and v.qty == 8, str(v))

# ─────────────────────────────────────────────────────────────────────────
print("=== 10. 策略配置化:playbook 单一真相 + gate 机械强制 ===")
from agent.config import playbook_for  # noqa: E402

pb = playbook_for("SNDK.US")
check("playbook_for 解析 SNDK", pb is not None and pb["name"] == "h4_confluence"
      and pb["binding"] and pb["t1_pct"] == 5, str(pb))
check("非手册标的返回 None", playbook_for("AAPL.US") is None)

def fresh_gate():
    return RiskGate(equity=10000, halted=False, open_positions=[], recent_trades=[],
                    session="Intraday", minutes_to_close=120)

base = {"symbol": "MU.US", "side": "long", "source": "technical",
        "entry_hint": 900.0, "stop": 855.0, "confidence": 80}
v = fresh_gate().evaluate({**base, "features": None})
check("binding: 无 h4 证据拒", not v.approved and "共振确认" in v.reason, str(v))
v = fresh_gate().evaluate({**base, "features": {"h4_confluence": False}})
check("binding: 共振为假拒", not v.approved, str(v))
v = fresh_gate().evaluate({**base, "features": {"h4_confluence": True}})
check("binding: 共振为真放行", v.approved and v.qty > 0, str(v))
v = fresh_gate().evaluate({**base, "side": "short",
                           "features": {"h4_confluence": True}})
check("binding: long_only 拒做空", not v.approved and "多头" in v.reason, str(v))
v = fresh_gate().evaluate({**base, "source": "user", "features": None})
check("binding: 人工单豁免", v.approved, str(v))

# ─────────────────────────────────────────────────────────────────────────
print()
print(f"PASS {len(PASS)} / FAIL {len(FAIL)}")
if FAIL:
    print("失败: " + ", ".join(FAIL))
    raise SystemExit(1)
