"""Phase 2 冒烟测试 —— attribution + postmortem(cognitive iteration)层。
临时 db,canned LLM(不花真钱),零真实副作用。

跑法: FLINTRADE_DRY_RUN=1 python3 -m agent.test_p2
"""
from __future__ import annotations

import os
import sqlite3
import sys
import tempfile
from datetime import datetime, timedelta, timezone

# 强制临时 db + dry-run(在 import db 前设好 env)
os.environ.setdefault("FLINTRADE_DB", os.path.join(tempfile.gettempdir(), "flintrade_p2_test.db"))
os.environ["FLINTRADE_DRY_RUN"] = "1"
os.environ.setdefault("FLINTRADE_LLM_DRY", "1")
if os.path.exists(os.environ["FLINTRADE_DB"]):
    os.remove(os.environ["FLINTRADE_DB"])

from agent.db import DB, init_db, now           # noqa: E402
from agent import settle                        # noqa: E402
from agent import postmortem                    # noqa: E402
from agent import reflect                        # noqa: E402

PASS, FAIL = [], []


def check(name, cond, detail=""):
    (PASS if cond else FAIL).append(name)
    print(f"  {'✓' if cond else '✗ FAIL'} {name}" + (f" — {detail}" if detail and not cond else ""))


# ─────────────────────────────────────────────────────────────────────────
# 1. init_db 迁移:幂等 + ALTER TABLE 补列
# ─────────────────────────────────────────────────────────────────────────
print("=== init_db migration ===")

init_db()
init_db()  # 第二次调用不应抛异常
conn = sqlite3.connect(os.environ["FLINTRADE_DB"])
cols = [c[1] for c in conn.execute("PRAGMA table_info(trades)").fetchall()]
check("attribution column present after init_db (fresh db)", "attribution" in cols)
conn.close()

migrate_path = os.path.join(tempfile.gettempdir(), "flintrade_p2_migrate.db")
if os.path.exists(migrate_path):
    os.remove(migrate_path)
mconn = sqlite3.connect(migrate_path)
mconn.execute("""CREATE TABLE trades (
    id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, symbol TEXT NOT NULL,
    action TEXT NOT NULL, qty INTEGER NOT NULL, fill_price REAL NOT NULL,
    commission REAL NOT NULL DEFAULT 0, pnl REAL, source TEXT, intent_id INTEGER,
    broker_order_id TEXT, position_id INTEGER, features TEXT, reason TEXT
)""")
mconn.commit()
mconn.close()

init_db(migrate_path)  # 既有 db(无 attribution 列)→ 必须触发 ALTER TABLE
mconn = sqlite3.connect(migrate_path)
mcols = [c[1] for c in mconn.execute("PRAGMA table_info(trades)").fetchall()]
check("migration adds attribution column to pre-existing trades table",
      "attribution" in mcols, f"cols={mcols}")
mconn.close()
os.remove(migrate_path)


# ─────────────────────────────────────────────────────────────────────────
# 2. settle_open / settle_close 写 attribution
# ─────────────────────────────────────────────────────────────────────────
print("=== settle attribution ===")

db_ex = DB(role="executor")
db_ex.update_risk(equity=10000.0, day_start_equity=10000.0, day_realized_pnl=0, open_risk=0)

# strategy(source='technical')
pos_test = settle.settle_open(db_ex, symbol="TEST.US", side="long", qty=10, fill_price=100.0,
                              stop=98.0, target=110.0, commission_per_share=0.02,
                              source="technical", reason="entry technical")
open_row = db_ex.conn.execute("SELECT * FROM trades WHERE position_id=? AND pnl IS NULL",
                              (pos_test,)).fetchone()
check("settle_open source=technical -> attribution=strategy",
      open_row["attribution"] == "strategy", f"got={open_row['attribution']!r}")

held_test = dict(db_ex.conn.execute("SELECT * FROM positions WHERE id=?", (pos_test,)).fetchone())
settle.settle_close(db_ex, held=held_test, fill_price=105.0, qty=10,
                    commission_per_share=0.02, reason="target hit")
close_row = db_ex.conn.execute(
    "SELECT * FROM trades WHERE position_id=? AND pnl IS NOT NULL", (pos_test,)
).fetchone()
check("settle_close source=technical -> attribution=strategy",
      close_row["attribution"] == "strategy", f"got={close_row['attribution']!r}")
close_test_pnl = close_row["pnl"]

# manual(source='user')
pos_test2 = settle.settle_open(db_ex, symbol="TEST2.US", side="long", qty=5, fill_price=50.0,
                               stop=48.0, target=55.0, commission_per_share=0.02,
                               source="user", reason="manual buy")
open_row2 = db_ex.conn.execute("SELECT * FROM trades WHERE position_id=? AND pnl IS NULL",
                               (pos_test2,)).fetchone()
check("settle_open source=user -> attribution=manual",
      open_row2["attribution"] == "manual", f"got={open_row2['attribution']!r}")

held_test2 = dict(db_ex.conn.execute("SELECT * FROM positions WHERE id=?", (pos_test2,)).fetchone())
settle.settle_close(db_ex, held=held_test2, fill_price=52.0, qty=5,
                    commission_per_share=0.02, reason="manual close")
close_row2 = db_ex.conn.execute(
    "SELECT * FROM trades WHERE position_id=? AND pnl IS NOT NULL", (pos_test2,)
).fetchone()
check("settle_close source=user -> attribution=manual",
      close_row2["attribution"] == "manual", f"got={close_row2['attribution']!r}")
close_test2_pnl = close_row2["pnl"]


# ─────────────────────────────────────────────────────────────────────────
# 3. pair_roundtrip + code_metrics + review_new(canned good LLM)
# ─────────────────────────────────────────────────────────────────────────
print("=== pair_roundtrip + code_metrics + review_new ===")

# 测试用 escape hatch:role=migrate 只是拿一个可用连接,实际直接走 raw conn.execute
# 摆数据(需要精确控制 ts,append_trade/open_position 内部用 now() 做不到)。
seed_db = DB(role="migrate")

T0 = datetime(2026, 7, 10, 14, 0, 0, tzinfo=timezone.utc)
t0_str = T0.strftime("%Y-%m-%dT%H:%M:%SZ")
t_exit_str = (T0 + timedelta(hours=3)).strftime("%Y-%m-%dT%H:%M:%SZ")

cur = seed_db.conn.execute(
    """INSERT INTO intents(source,priority,symbol,side,confidence,reason,status,created_at)
       VALUES('technical',0,'ABC.US','long',70,'THESIS-ABC','approved',?)""",
    (t0_str,),
)
intent_id = cur.lastrowid

cur = seed_db.conn.execute(
    """INSERT INTO positions(symbol,side,qty,entry_price,stop,target,risk_amt,source,
           intent_id,opened_at,status)
       VALUES('ABC.US','long',35,208.9,205.7,220.0,112.0,'technical',?,?, 'closed')""",
    (intent_id, t0_str),
)
position_id = cur.lastrowid

cur = seed_db.conn.execute(
    """INSERT INTO trades(ts,symbol,action,qty,fill_price,commission,pnl,source,intent_id,
           position_id,reason,attribution)
       VALUES(?, 'ABC.US','SELL',35,203.0,0.7,-112.48,'technical',?,?,
              'Stop breach - thesis invalidated','strategy')""",
    (t_exit_str, intent_id, position_id),
)
abc_trade_id = cur.lastrowid

# 排除测试(test 5)的种子:attribution='test',永远不该被复盘
cur = seed_db.conn.execute(
    """INSERT INTO trades(ts,symbol,action,qty,fill_price,commission,pnl,source,reason,attribution)
       VALUES(?, 'ZTEST.US','SELL',1,10.0,0.02,1.0,'technical','test harness fill','test')""",
    (now(),),
)
ztest_trade_id = cur.lastrowid

# 直接单测 pair_roundtrip / code_metrics(不经 review_new)
abc_row = seed_db.conn.execute("SELECT * FROM trades WHERE id=?", (abc_trade_id,)).fetchone()
ctx = postmortem.pair_roundtrip(seed_db, abc_row)
check("pair_roundtrip found position", ctx is not None)
check("pair_roundtrip hold_minutes==180", ctx is not None and ctx["hold_minutes"] == 180,
      f"got={ctx and ctx['hold_minutes']}")
check("pair_roundtrip thesis==THESIS-ABC", ctx is not None and ctx["thesis"] == "THESIS-ABC")
check("pair_roundtrip confidence==70", ctx is not None and ctx["confidence"] == 70)

metrics = postmortem.code_metrics(ctx)
check("code_metrics exit_kind==stop", metrics["exit_kind"] == "stop", f"got={metrics}")
check("code_metrics slippage_vs_stop==2.7", metrics["slippage_vs_stop"] == 2.7, f"got={metrics}")

# pair_roundtrip(missing position) -> None
fake_row = dict(abc_row)
fake_row["position_id"] = 999999
none_ctx = postmortem.pair_roundtrip(seed_db, fake_row)
check("pair_roundtrip returns None for missing position", none_ctx is None)

# review_new,一次性复盘所有待复盘的 strategy 平仓交易(此刻:TEST.US close + ABC.US close)
postmortem.set_canned_llm_response(
    '```json\n{"thesis_verdict":"wrong","entry_grade":"ok","exit_grade":"right",'
    '"confidence_justified":false,"lesson":"L1"}\n```'
)
lines = postmortem.review_new(limit=10)
check("review_new processed exactly TEST.US + ABC.US (2 strategy closes)", len(lines) == 2,
      f"lines={lines}")

review_row = seed_db.conn.execute(
    "SELECT * FROM trade_reviews WHERE trade_id=?", (abc_trade_id,)
).fetchone()
check("trade_reviews row exists for ABC.US close", review_row is not None)
check("trade_reviews exit_kind==stop", review_row is not None and review_row["exit_kind"] == "stop")
check("trade_reviews slippage_vs_stop==2.7",
      review_row is not None and review_row["slippage_vs_stop"] == 2.7)
check("trade_reviews hold_minutes==180",
      review_row is not None and review_row["hold_minutes"] == 180)
check("trade_reviews thesis==THESIS-ABC",
      review_row is not None and review_row["thesis"] == "THESIS-ABC")
check("trade_reviews lesson==L1", review_row is not None and review_row["lesson"] == "L1")
check("trade_reviews thesis_verdict==wrong",
      review_row is not None and review_row["thesis_verdict"] == "wrong")
check("trade_reviews confidence_justified==0",
      review_row is not None and review_row["confidence_justified"] == 0)

# test 5: attribution='test' never reviewed
ztest_review = seed_db.conn.execute(
    "SELECT * FROM trade_reviews WHERE trade_id=?", (ztest_trade_id,)
).fetchone()
check("attribution='test' trade never reviewed", ztest_review is None)


# ─────────────────────────────────────────────────────────────────────────
# 4. Idempotency: review_new again -> 0 new rows
# ─────────────────────────────────────────────────────────────────────────
print("=== idempotency ===")
lines_again = postmortem.review_new(limit=10)
check("review_new second call finds 0 new trades", len(lines_again) == 0, f"lines={lines_again}")


# ─────────────────────────────────────────────────────────────────────────
# 6. LLM garbage: canned 'no json here' -> row still inserted, code metrics only
# ─────────────────────────────────────────────────────────────────────────
print("=== LLM garbage handling ===")

pos_garbage = settle.settle_open(db_ex, symbol="GARBAGE.US", side="long", qty=10, fill_price=50.0,
                                 stop=48.0, target=55.0, commission_per_share=0.02,
                                 source="technical", reason="entry")
held_garbage = dict(db_ex.conn.execute("SELECT * FROM positions WHERE id=?",
                                       (pos_garbage,)).fetchone())
settle.settle_close(db_ex, held=held_garbage, fill_price=53.0, qty=10,
                    commission_per_share=0.02, reason="target reached")
garbage_close = db_ex.conn.execute(
    "SELECT * FROM trades WHERE position_id=? AND pnl IS NOT NULL", (pos_garbage,)
).fetchone()
close_garbage_pnl = garbage_close["pnl"]

postmortem.set_canned_llm_response("no json here, sorry")
lines_garbage = postmortem.review_new(limit=10)
check("review_new processed the garbage-LLM trade", len(lines_garbage) == 1, f"lines={lines_garbage}")

garbage_review = seed_db.conn.execute(
    "SELECT * FROM trade_reviews WHERE trade_id=?", (garbage_close["id"],)
).fetchone()
check("garbage review row inserted", garbage_review is not None)
check("garbage review thesis_verdict is NULL",
      garbage_review is not None and garbage_review["thesis_verdict"] is None)
check("garbage review lesson is NULL",
      garbage_review is not None and garbage_review["lesson"] is None)
check("garbage review code metrics still computed (exit_kind==target)",
      garbage_review is not None and garbage_review["exit_kind"] == "target")


# ─────────────────────────────────────────────────────────────────────────
# 7. recall().self_assessment
# ─────────────────────────────────────────────────────────────────────────
print("=== recall().self_assessment ===")

expected_n = 3  # TEST.US close, ABC.US close, GARBAGE.US close (strategy, pnl not null)
strategy_pnls = [close_test_pnl, -112.48, close_garbage_pnl]  # TEST2.US is manual, excluded
expected_win_rate = round(sum(1 for p in strategy_pnls if p > 0) / len(strategy_pnls), 3)
expected_net_pnl = round(sum(strategy_pnls), 2)

sa = reflect.recall()["self_assessment"]
check("self_assessment n==3", sa["n"] == expected_n, f"got={sa}")
check("self_assessment win_rate matches", abs(sa["win_rate"] - expected_win_rate) < 1e-6,
      f"got={sa['win_rate']} want={expected_win_rate}")
check("self_assessment net_pnl matches", abs(sa["net_pnl"] - expected_net_pnl) < 1e-6,
      f"got={sa['net_pnl']} want={expected_net_pnl}")
check("self_assessment has 'note' for n<30", "note" in sa, f"got={sa}")


# ─────────────────────────────────────────────────────────────────────────
# 8. build_dream_prompt: HARD RULES present + non-strategy trades excluded
# ─────────────────────────────────────────────────────────────────────────
print("=== build_dream_prompt honesty rules ===")

seed_db.conn.execute(
    """INSERT INTO trades(ts,symbol,action,qty,fill_price,commission,pnl,source,reason,attribution)
       VALUES(?, 'OUT.US','SELL',10,10.0,0.1,5.0,'technical','OUTAGE-REASON-XYZ','outage-degraded')""",
    (now(),),
)

db_reader = DB(role="reflect")
trades_for_prompt = reflect._recent_closed_with_reasons(db_reader, limit=100)
reviews_for_prompt = reflect._recent_reviews(db_reader, limit=15)
prompt = reflect.build_dream_prompt([], trades_for_prompt, "2026-07-14", reviews_for_prompt)

check("HARD RULES text present in dream prompt", "HARD RULES" in prompt)
check("outage-degraded trade's reason excluded from prompt",
      "OUTAGE-REASON-XYZ" not in prompt)
check("manual trade's reason excluded from prompt", "manual close" not in prompt)
check("strategy trade's reason included in prompt", "Stop breach - thesis invalidated" in prompt)
check("recent trade_reviews block present (table has rows)",
      "RECENT TRADE POSTMORTEMS" in prompt and "L1" in prompt)

db_reader.close()
seed_db.close()
db_ex.close()


print(f"\n{'='*40}\nPASS={len(PASS)}  FAIL={len(FAIL)}")
if FAIL:
    print("FAILED:", FAIL)
    sys.exit(1)
print("Phase 2 端到端全绿 ✓")
