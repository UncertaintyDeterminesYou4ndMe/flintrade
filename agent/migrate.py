"""state.json → flint.db 一次性迁移。幂等:重跑前清空 trades/positions 再导。
历史成交全部标记 source='technical'(cron 时代只有技术面一个来源)。"""
from __future__ import annotations

import json
import sys
from pathlib import Path

from agent.db import DB, FLINT_DIR, init_db, now

STATE_FILE = FLINT_DIR / "state.json"


def migrate():
    init_db()
    state = json.loads(STATE_FILE.read_text())
    db = DB(role="migrate")

    # 幂等:清空再导(迁移是一次性引导,不做增量)
    db.conn.execute("DELETE FROM trades")
    db.conn.execute("DELETE FROM positions")

    trades = state.get("trades", [])
    n = 0
    for t in trades:
        action = t.get("side", "").upper()  # 旧库 side = BUY/SELL
        db.append_trade(
            symbol=t.get("symbol"),
            action=action,
            qty=int(t.get("quantity", 0)),
            fill_price=float(t.get("fill_price", 0)),
            commission=float(t.get("commission", 0)),
            pnl=t.get("pnl"),
            source="technical",
            broker_order_id=str(t.get("order_id")) if t.get("order_id") else None,
            reason=t.get("reason"),
        )
        # 用旧 time 覆盖 ts(append_trade 写的是 now())
        db.conn.execute(
            "UPDATE trades SET ts=? WHERE id=last_insert_rowid()", (t.get("time", now()),)
        )
        n += 1

    # 当前持仓(现状为 null,但保留迁移逻辑)
    pos = state.get("position")
    if pos and pos.get("symbol"):
        db.open_position(
            symbol=pos["symbol"],
            side="long" if pos.get("side", "long").lower() in ("long", "buy") else "short",
            qty=int(pos.get("quantity", 0)),
            entry_price=float(pos.get("entry_price", 0)),
            stop=pos.get("stop"),
            target=pos.get("target"),
            source="technical",
        )

    # 权益基准
    capital = float(state.get("capital", 1200))
    db.update_risk(equity=capital, day_start_equity=capital,
                   day_realized_pnl=0, open_risk=0)
    db.kv_set("migrated_from_state", now())

    print(f"migrated {n} trades, capital={capital}, position={'yes' if pos and pos.get('symbol') else 'none'}")


if __name__ == "__main__":
    sys.exit(migrate())
