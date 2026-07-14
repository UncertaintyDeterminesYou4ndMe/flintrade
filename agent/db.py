"""
Flint Agent 数据访问层。SQLite + WAL,零外部依赖(stdlib only)。

核心不变量 —— 单一写者(role guard):
  生产者(technical/event/user/news)只能 submit_intent / add_signal / add_event。
  只有 executor / reconciler 写 positions / trades / orders。
  只有 executor / risk_monitor 写 risk_state.halt。
这把 Arena 时代「run.sh 是唯一写者」的性质,在多进程下用代码强制保住。

用法:
    from agent.db import DB, init_db
    init_db()                          # 幂等建表
    db = DB(role="technical")
    db.submit_intent(symbol="NVDA.US", side="long", stop=200.0, ...)

    ex = DB(role="executor")
    for it in ex.claim_intents(): ...
"""
from __future__ import annotations

import json
import os
import sqlite3
from contextlib import contextmanager
from datetime import datetime, timezone
from pathlib import Path

FLINT_DIR = Path(__file__).resolve().parent.parent
DB_PATH = Path(os.environ.get("FLINT_DB", FLINT_DIR / "flint.db"))
SCHEMA_PATH = Path(__file__).resolve().parent / "schema.sql"

# 角色 → 可写的受保护表。未列出的写操作会被 _require 拒绝。
_WRITE_PERMS = {
    "executor":     {"intents_decide", "positions", "trades", "orders", "risk_state", "halt"},
    "reconciler":   {"positions", "trades", "orders", "risk_state", "signals"},
    "risk_monitor": {"halt", "intents_submit"},
    "technical":    {"intents_submit", "signals", "events"},
    "event":        {"intents_submit", "signals", "events"},
    "user":         {"intents_submit"},
    "news":         {"signals", "events"},
    "reflect":      {"memory", "agg"},
    "migrate":      {"*"},          # 引导期全权
    "reader":       set(),          # dashboard / 只读
}


def now() -> str:
    """ISO8601 UTC,秒精度。全库统一时间格式。"""
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _j(v):
    return json.dumps(v, ensure_ascii=False) if v is not None else None


class DB:
    def __init__(self, role: str = "reader", path: Path | None = None):
        if role not in _WRITE_PERMS:
            raise ValueError(f"unknown role: {role!r} (valid: {sorted(_WRITE_PERMS)})")
        self.role = role
        self._txn_depth = 0  # 事务嵌套计数(见 transaction())
        self.path = Path(path) if path else DB_PATH
        self.conn = sqlite3.connect(self.path, timeout=10.0, isolation_level=None)
        self.conn.row_factory = sqlite3.Row
        self.conn.execute("PRAGMA journal_mode=WAL")
        self.conn.execute("PRAGMA busy_timeout=5000")
        self.conn.execute("PRAGMA foreign_keys=ON")
        self.conn.execute("PRAGMA synchronous=NORMAL")

    # ── role guard ────────────────────────────────────────────────────────
    def _require(self, perm: str):
        allowed = _WRITE_PERMS[self.role]
        if "*" in allowed or perm in allowed:
            return
        raise PermissionError(
            f"role={self.role!r} 无权执行 {perm!r}(单一写者不变量)。"
            f" 该角色可写: {sorted(allowed) or '只读'}"
        )

    def close(self):
        self.conn.close()

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()

    # ── 原子事务 ──────────────────────────────────────────────────────────
    @contextmanager
    def transaction(self):
        """把多步写包成一个原子单元。崩溃/异常 → 整体 ROLLBACK,绝不留半笔账。

        连接是 autocommit(isolation_level=None),所以每条语句默认各自提交;
        结算(settle_open/close)涉及 持仓+成交+风险状态 多步写,必须整体原子,
        否则进程在中途死掉会让账本撕裂(有持仓没扣风险、有成交没记盈亏)。

        WAL + ``BEGIN IMMEDIATE`` 立刻取写锁,配合 busy_timeout,天然串行化
        executor / reconciler 的并发结算。支持嵌套(内层复用外层事务)。
        """
        if self._txn_depth > 0:  # 已在事务中:复用,不重复 BEGIN/COMMIT
            self._txn_depth += 1
            try:
                yield
            finally:
                self._txn_depth -= 1
            return
        self.conn.execute("BEGIN IMMEDIATE")
        self._txn_depth = 1
        try:
            yield
            self.conn.execute("COMMIT")
        except Exception:
            self.conn.execute("ROLLBACK")
            raise
        finally:
            self._txn_depth = 0

    # ── 意图队列 ──────────────────────────────────────────────────────────
    def submit_intent(self, *, source: str | None = None, symbol: str, side: str,
                      priority: int = 0, hypothesis_qty: int | None = None,
                      entry_hint: float | None = None, stop: float | None = None,
                      target: float | None = None, confidence: int | None = None,
                      dedup_key: str | None = None, reason: str | None = None,
                      features: dict | None = None, expires_at: str | None = None) -> int | None:
        """生产者提交意图(pending)。dedup_key 冲突时静默跳过返回 None。"""
        self._require("intents_submit")
        src = source or self.role
        try:
            cur = self.conn.execute(
                """INSERT INTO intents(source,priority,symbol,side,hypothesis_qty,entry_hint,
                       stop,target,confidence,dedup_key,reason,features,status,created_at,expires_at)
                   VALUES(?,?,?,?,?,?,?,?,?,?,?,?, 'pending', ?, ?)""",
                (src, priority, symbol, side, hypothesis_qty, entry_hint, stop, target,
                 confidence, dedup_key, reason, _j(features), now(), expires_at),
            )
            return cur.lastrowid
        except sqlite3.IntegrityError:
            return None  # dedup_key 已存在

    def claim_intents(self, limit: int = 50) -> list[sqlite3.Row]:
        """Executor 取待裁决意图,按优先级 + 时间排序。"""
        return self.conn.execute(
            "SELECT * FROM intents WHERE status='pending' ORDER BY priority DESC, created_at LIMIT ?",
            (limit,),
        ).fetchall()

    def decide_intent(self, intent_id: int, status: str, reject_reason: str | None = None):
        """Executor 裁决意图:approved/rejected/filled/expired/cancelled。"""
        self._require("intents_decide")
        self.conn.execute(
            "UPDATE intents SET status=?, reject_reason=?, decided_at=? WHERE id=?",
            (status, reject_reason, now(), intent_id),
        )

    # ── 持仓 ──────────────────────────────────────────────────────────────
    def open_position(self, *, symbol: str, side: str, qty: int, entry_price: float,
                      stop: float | None = None, target: float | None = None,
                      risk_amt: float | None = None, source: str | None = None,
                      intent_id: int | None = None) -> int:
        self._require("positions")
        cur = self.conn.execute(
            """INSERT INTO positions(symbol,side,qty,entry_price,stop,target,risk_amt,
                   source,intent_id,opened_at,status)
               VALUES(?,?,?,?,?,?,?,?,?,?, 'open')""",
            (symbol, side, qty, entry_price, stop, target, risk_amt, source, intent_id, now()),
        )
        return cur.lastrowid

    def close_position(self, position_id: int):
        self._require("positions")
        self.conn.execute("UPDATE positions SET status='closed' WHERE id=?", (position_id,))

    def open_positions(self) -> list[sqlite3.Row]:
        return self.conn.execute("SELECT * FROM positions WHERE status='open'").fetchall()

    def position_for(self, symbol: str) -> sqlite3.Row | None:
        return self.conn.execute(
            "SELECT * FROM positions WHERE status='open' AND symbol=? LIMIT 1", (symbol,)
        ).fetchone()

    # ── 成交账本 ──────────────────────────────────────────────────────────
    def append_trade(self, *, symbol: str, action: str, qty: int, fill_price: float,
                     commission: float = 0.0, pnl: float | None = None,
                     source: str | None = None, intent_id: int | None = None,
                     broker_order_id: str | None = None, position_id: int | None = None,
                     features: dict | None = None, reason: str | None = None,
                     attribution: str | None = None) -> int:
        self._require("trades")
        cur = self.conn.execute(
            """INSERT INTO trades(ts,symbol,action,qty,fill_price,commission,pnl,source,
                   intent_id,broker_order_id,position_id,features,reason,attribution)
               VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
            (now(), symbol, action, qty, fill_price, commission, pnl, source,
             intent_id, broker_order_id, position_id, _j(features), reason, attribution),
        )
        return cur.lastrowid

    def recent_trades(self, limit: int = 20) -> list[sqlite3.Row]:
        return self.conn.execute(
            "SELECT * FROM trades ORDER BY id DESC LIMIT ?", (limit,)
        ).fetchall()

    # ── 订单 ──────────────────────────────────────────────────────────────
    def create_order(self, *, client_order_id: str, symbol: str, side: str, qty: int,
                     price: float | None = None, outside_rth: str | None = None,
                     intent_id: int | None = None) -> int:
        self._require("orders")
        cur = self.conn.execute(
            """INSERT INTO orders(client_order_id,symbol,side,qty,price,outside_rth,
                   intent_id,status,created_at)
               VALUES(?,?,?,?,?,?,?, 'submitted', ?)""",
            (client_order_id, symbol, side, qty, price, outside_rth, intent_id, now()),
        )
        return cur.lastrowid

    def update_order(self, client_order_id: str, **fields):
        self._require("orders")
        cols = ", ".join(f"{k}=?" for k in fields)
        self.conn.execute(
            f"UPDATE orders SET {cols}, updated_at=? WHERE client_order_id=?",
            (*fields.values(), now(), client_order_id),
        )

    def open_orders(self) -> list[sqlite3.Row]:
        return self.conn.execute(
            "SELECT * FROM orders WHERE status IN ('submitted','partial')"
        ).fetchall()

    # ── 风险状态 ──────────────────────────────────────────────────────────
    def get_risk(self) -> sqlite3.Row | None:
        return self.conn.execute("SELECT * FROM risk_state WHERE id=1").fetchone()

    def update_risk(self, **fields):
        self._require("risk_state")
        cols = ", ".join(f"{k}=?" for k in fields)
        self.conn.execute(
            f"UPDATE risk_state SET {cols}, updated_at=? WHERE id=1",
            (*fields.values(), now()),
        )

    def set_halt(self, halt: bool, reason: str | None = None):
        """risk_monitor / executor 拍急停。"""
        self._require("halt")
        self.conn.execute(
            "UPDATE risk_state SET halt=?, halt_reason=?, updated_at=? WHERE id=1",
            (1 if halt else 0, reason, now()),
        )

    def is_halted(self) -> bool:
        r = self.get_risk()
        return bool(r and r["halt"])

    # ── 信号 / 事件 ───────────────────────────────────────────────────────
    def add_signal(self, *, source: str | None = None, symbol: str | None = None,
                   kind: str | None = None, payload: dict | None = None) -> int:
        self._require("signals")
        cur = self.conn.execute(
            "INSERT INTO signals(source,symbol,kind,payload,ts) VALUES(?,?,?,?,?)",
            (source or self.role, symbol, kind, _j(payload), now()),
        )
        return cur.lastrowid

    def add_event(self, *, symbol: str | None = None, kind: str | None = None,
                  title: str | None = None, fires_at: str | None = None,
                  payload: dict | None = None) -> int:
        self._require("events")
        cur = self.conn.execute(
            "INSERT INTO events(symbol,kind,title,fires_at,payload,status,ts) VALUES(?,?,?,?,?, 'pending', ?)",
            (symbol, kind, title, fires_at, _j(payload), now()),
        )
        return cur.lastrowid

    # ── 心跳 ──────────────────────────────────────────────────────────────
    def beat(self, process: str | None = None, pid: int | None = None, note: str | None = None):
        proc = process or self.role
        self.conn.execute(
            """INSERT INTO heartbeats(process,last_beat,pid,note) VALUES(?,?,?,?)
               ON CONFLICT(process) DO UPDATE SET last_beat=excluded.last_beat,
                   pid=excluded.pid, note=excluded.note""",
            (proc, now(), pid or os.getpid(), note),
        )

    def heartbeats(self) -> list[sqlite3.Row]:
        return self.conn.execute("SELECT * FROM heartbeats").fetchall()

    # ── KV ────────────────────────────────────────────────────────────────
    def kv_get(self, key: str, default=None):
        r = self.conn.execute("SELECT value FROM kv WHERE key=?", (key,)).fetchone()
        return r["value"] if r else default

    def kv_set(self, key: str, value: str):
        self.conn.execute(
            "INSERT INTO kv(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
            (key, value),
        )


def init_db(path: Path | None = None):
    """幂等建表 + 确保 risk_state 单行存在。"""
    p = Path(path) if path else DB_PATH
    conn = sqlite3.connect(p, isolation_level=None)
    conn.execute("PRAGMA journal_mode=WAL")
    conn.executescript(SCHEMA_PATH.read_text())
    # 迁移:schema.sql 只对全新 db 生效;既有 db 的 trades 表可能建于 attribution
    # 列引入之前,这里幂等补列(列已存在时 sqlite3.OperationalError,吞掉即可)。
    try:
        conn.execute("ALTER TABLE trades ADD COLUMN attribution TEXT")
    except sqlite3.OperationalError:
        pass
    row = conn.execute("SELECT COUNT(*) FROM risk_state").fetchone()
    if row[0] == 0:
        conn.execute("INSERT INTO risk_state(id,equity,day_start_equity) VALUES(1, NULL, NULL)")
    conn.execute(
        "INSERT INTO kv(key,value) VALUES('schema_version','1') ON CONFLICT(key) DO NOTHING"
    )
    conn.close()


if __name__ == "__main__":
    init_db()
    print(f"initialized {DB_PATH}")
