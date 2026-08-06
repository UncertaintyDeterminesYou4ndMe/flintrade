"""Phase 1 冒烟测试 —— 临时 db + dry-run broker,零真实副作用。
跑法: FLINTRADE_DRY_RUN=1 FLINTRADE_DB=/tmp/flintrade_test.db python3 -m agent.test_phase1
"""
from __future__ import annotations

import os
import sys
import tempfile
from datetime import datetime, timedelta, timezone

# 强制临时 db + dry-run(在 import db 前设好 env)
os.environ.setdefault("FLINTRADE_DB", os.path.join(tempfile.gettempdir(), "flintrade_test.db"))
os.environ["FLINTRADE_DRY_RUN"] = "1"
if os.path.exists(os.environ["FLINTRADE_DB"]):
    os.remove(os.environ["FLINTRADE_DB"])

from agent.db import DB, init_db, now              # noqa: E402
from agent.risk_gate import RiskGate                # noqa: E402
from agent import session as S                      # noqa: E402
from agent.executor import Executor                 # noqa: E402

PASS, FAIL = [], []


def check(name, cond, detail=""):
    (PASS if cond else FAIL).append(name)
    print(f"  {'✓' if cond else '✗ FAIL'} {name}" + (f" — {detail}" if detail and not cond else ""))


def gate(**over):
    base = dict(equity=10000.0, halted=False, open_positions=[], recent_trades=[],
                session="Intraday", minutes_to_close=300, volume_ratio=1.0)
    base.update(over)
    return RiskGate(**base)


def long_intent(symbol="NVDA.US", entry=100.0, stop=98.0, target=110.0, side="long"):
    return {"id": 1, "source": "technical", "symbol": symbol, "side": side,
            "entry_hint": entry, "stop": stop, "target": target, "reason": "test"}


print("=== RiskGate 单元 ===")
# 风险定额:2%*10000=200 预算,每股风险 2 → 100 股,但 per_symbol 40% → 40 股封顶
v = gate().evaluate(long_intent())
check("风险定额+per_symbol封顶 → 40股", v.approved and v.qty == 40, f"got approved={v.approved} qty={v.qty}")

# volume 过滤
v = gate(volume_ratio=0.2).evaluate(long_intent())
check("volume_ratio<0.3 拒", not v.approved and "volume" in v.reason)

# 收盘黑窗
v = gate(minutes_to_close=10).evaluate(long_intent())
check("距收盘<30min 拒", not v.approved and "黑窗" in v.reason)

# 无效止损
v = gate().evaluate(long_intent(stop=99.99))
check("止损过近 拒", not v.approved and "止损" in v.reason)

# no-revenge 冷却:同票 30min 前刚平亏损单
recent = [{"symbol": "NVDA.US", "action": "SELL", "pnl": -5.0,
           "ts": (datetime.now(timezone.utc) - timedelta(minutes=30)).strftime("%Y-%m-%dT%H:%M:%SZ")}]
v = gate(recent_trades=recent).evaluate(long_intent())
check("no-revenge 冷却 拒", not v.approved and "revenge" in v.reason)

# 并发仓上限(配置=3:已持 3 个不同票,开第 4 个新票应拒)
held3 = [{"symbol": "AAPL.US", "side": "long", "qty": 1, "entry_price": 200, "risk_amt": 2},
         {"symbol": "MSFT.US", "side": "long", "qty": 1, "entry_price": 400, "risk_amt": 2},
         {"symbol": "GOOGL.US", "side": "long", "qty": 1, "entry_price": 300, "risk_amt": 2}]
v = gate(open_positions=held3).evaluate(long_intent())
check("并发仓上限3 → 第4票拒", not v.approved and "并发" in v.reason)

# 做空允许(配置 allow_short=true)
v = gate().evaluate(long_intent(side="short", entry=100, stop=102))
check("允许做空 → 批准", v.approved and v.qty == 40)

print("=== Executor 端到端(dry-run) ===")
# 确定性 session
S.current_session = lambda: "Intraday"
S.minutes_to_close = lambda s: 300
S.outside_rth_for = lambda s, *, for_exit=False: "RTH_ONLY"

init_db()
boot = DB(role="migrate")
boot.conn.execute("DELETE FROM intents"); boot.conn.execute("DELETE FROM positions")
boot.conn.execute("DELETE FROM trades"); boot.conn.execute("DELETE FROM orders")
boot.update_risk(equity=10000.0, day_start_equity=10000.0, day_realized_pnl=0, open_risk=0)

prod = DB(role="technical")
ex = Executor()

# 1) 开多 NVDA
prod.submit_intent(source="technical", symbol="NVDA.US", side="long",
                   entry_hint=100.0, stop=98.0, target=110.0, confidence=70, reason="open long")
ex.process_once()
pos = DB(role="reader").position_for("NVDA.US")
r = DB(role="reader").get_risk()
check("开多建仓 40 股", pos and pos["qty"] == 40, f"pos={dict(pos) if pos else None}")
check("开仓后 open_risk=80", abs(r["open_risk"] - 80.0) < 1e-6, f"open_risk={r['open_risk']}")
check("开仓扣手续费 equity=9999.2", abs(r["equity"] - 9999.2) < 1e-6, f"equity={r['equity']}")

# 2) 冲突:持多仓时投做空 → 拒
sid = prod.submit_intent(source="technical", symbol="NVDA.US", side="short",
                         entry_hint=100.0, stop=102.0, reason="conflict short")
ex.process_once()
row = DB(role="reader").conn.execute("SELECT status,reject_reason FROM intents WHERE id=?", (sid,)).fetchone()
check("持多仓投做空 → 冲突拒", row["status"] == "rejected" and "翻向" in (row["reject_reason"] or ""),
      f"status={row['status']} reason={row['reject_reason']}")

# 3) 平仓 NVDA @105 → pnl=(105-100)*40 - 0.8 = 199.2
prod.submit_intent(source="technical", symbol="NVDA.US", side="close",
                   entry_hint=105.0, reason="take profit")
ex.process_once()
pos = DB(role="reader").position_for("NVDA.US")
r = DB(role="reader").get_risk()
last = dict(DB(role="reader").recent_trades(1)[0])
check("平仓后无持仓", pos is None)
check("平仓 pnl=199.2", abs(last["pnl"] - 199.2) < 1e-6, f"pnl={last['pnl']}")
check("平仓后 equity=10198.4", abs(r["equity"] - 10198.4) < 1e-6, f"equity={r['equity']}")
check("平仓后 open_risk=0", abs(r["open_risk"]) < 1e-6, f"open_risk={r['open_risk']}")

print(f"\n{'='*40}\nPASS={len(PASS)}  FAIL={len(FAIL)}")
if FAIL:
    print("FAILED:", FAIL)
    sys.exit(1)
print("Phase 1 端到端全绿 ✓")
