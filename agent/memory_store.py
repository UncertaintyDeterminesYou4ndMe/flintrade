"""
语义记忆 —— LanceDB 后端(嵌入式磁盘向量库)。

把成交/新闻的 setup 嵌入向量,按"与此刻最像的历史情形"召回(及其结果 pnl)。
嵌入走 agent.llm.embed(默认 fastembed 进程内 ONNX 小模型,同那个 app)。
公共接口与早期 BLOB+cosine 版完全一致 —— reflect / loop_technical 的 wiring 不用改:
    index(kind, ref_id, text, *, meta=None)
    index_new_trades() / recall_similar(query, kind, k) / embeddings_ready() / stats()

依赖(lancedb)只在 .venv 内;缺失或嵌入后端不可用时**全部优雅降级**
(index→False,index_new_trades→0,recall_similar→[]),绝不抛错拖垮决策。

向量库落盘在 FLINTRADE_DIR/flintrade_lance(可用 FLINTRADE_LANCE 覆盖),与 flintrade.db 并列。
"""
from __future__ import annotations

import os
import sys

from agent import llm
from agent.db import DB, FLINTRADE_DIR, now

LANCE_PATH = os.environ.get("FLINTRADE_LANCE", str(FLINTRADE_DIR / "flintrade_lance"))
TABLE = "memory"


def _log(msg: str):
    print(f"[memory_store] {msg}", file=sys.stderr)


def _connect():
    import lancedb  # 仅 .venv 内可用
    return lancedb.connect(LANCE_PATH)


def _ensure_table(conn, dim: int):
    import pyarrow as pa
    if TABLE in conn.table_names():
        return conn.open_table(TABLE)
    schema = pa.schema([
        pa.field("vector", pa.list_(pa.float32(), dim)),
        pa.field("kind", pa.string()),
        pa.field("ref_id", pa.int64()),
        pa.field("text", pa.string()),
        pa.field("symbol", pa.string()),
        pa.field("action", pa.string()),
        pa.field("pnl", pa.float64()),
        pa.field("created", pa.string()),
    ])
    return conn.create_table(TABLE, schema=schema)


def _open(conn):
    """打开已存在的表;不存在返回 None。"""
    return conn.open_table(TABLE) if TABLE in conn.table_names() else None


# ── 索引 ──────────────────────────────────────────────────────────────────
def index(kind: str, ref_id: int, text: str, *, meta: dict | None = None) -> bool:
    """嵌入 text 并写入向量库(同 kind+ref_id 先删后写,幂等)。后端不可用→False。"""
    try:
        vecs = llm.embed([text])
        if not vecs:
            _log("嵌入后端不可用,index 跳过")
            return False
        vec = [float(x) for x in vecs[0]]
        m = meta or {}
        row = {
            "vector": vec, "kind": kind, "ref_id": int(ref_id), "text": text,
            "symbol": str(m.get("symbol", "")), "action": str(m.get("action", "")),
            "pnl": float(m.get("pnl") or 0.0), "created": now(),
        }
        conn = _connect()
        tbl = _ensure_table(conn, len(vec))
        try:
            tbl.delete(f"kind = '{kind}' AND ref_id = {int(ref_id)}")
        except Exception:
            pass
        tbl.add([row])
        return True
    except Exception as e:
        _log(f"index 失败: {e!r}")
        return False


def _trade_text(t) -> str:
    """成交 → 可嵌入的 setup 描述(相似 setup → 相近向量)。"""
    import json
    feats = {}
    if t["features"]:
        try:
            feats = json.loads(t["features"])
        except (ValueError, TypeError):
            feats = {}
    sess = _session_of(t["ts"])
    bits = [f"{t['symbol']} {('long' if t['action'] in ('BUY', 'SHORT') else 'exit')}", sess]
    if feats.get("rsi") is not None:
        bits.append(f"RSI {feats['rsi']}")
    if feats.get("volume_ratio") is not None:
        bits.append(f"vol {feats['volume_ratio']}x")
    if t["reason"]:
        bits.append(t["reason"][:160])
    out = f"{t['pnl']:+.2f}" if t["pnl"] is not None else "open"
    return ", ".join(b for b in bits if b) + f"; outcome {out}"


def _session_of(ts: str) -> str:
    """从 UTC 成交时间粗分 ET 时段(嵌入文本用)。"""
    try:
        from datetime import datetime
        from zoneinfo import ZoneInfo
        dt = datetime.strptime(ts, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=ZoneInfo("UTC"))
        et = dt.astimezone(ZoneInfo("America/New_York"))
        h = et.hour + et.minute / 60
        if 4 <= h < 9.5:
            return "Pre"
        if 9.5 <= h < 16:
            return "Intraday"
        if 16 <= h < 20:
            return "Post"
        return "Overnight"
    except Exception:
        return "unknown"


def _indexed_ref_ids(conn, kind: str) -> set:
    tbl = _open(conn)
    if tbl is None:
        return set()
    try:
        d = tbl.to_arrow().to_pydict()
        return {rid for rid, k in zip(d.get("ref_id", []), d.get("kind", [])) if k == kind}
    except Exception:
        return set()


def index_new_trades() -> int:
    """把尚未索引的已平仓成交嵌入向量库。嵌入后端没起→0。"""
    if not embeddings_ready():
        _log("嵌入后端不可用,index_new_trades 跳过")
        return 0
    with DB(role="reflect") as db:  # Row 已物化,关闭连接后仍可用
        rows = db.conn.execute("SELECT * FROM trades WHERE pnl IS NOT NULL ORDER BY id").fetchall()
    try:
        existing = _indexed_ref_ids(_connect(), "trade")
    except Exception:
        existing = set()
    n = 0
    for t in rows:
        if t["id"] in existing:
            continue
        if index("trade", t["id"], _trade_text(t),
                 meta={"symbol": t["symbol"], "action": t["action"], "pnl": t["pnl"]}):
            n += 1
    return n


# ── 召回 ──────────────────────────────────────────────────────────────────
def recall_similar(query_text: str, kind: str = "trade", k: int = 5) -> list[dict]:
    """召回与 query 最像的历史条目(及其结果)。后端不可用→[]。"""
    try:
        vecs = llm.embed([query_text])
        if not vecs:
            return []
        conn = _connect()
        tbl = _open(conn)
        if tbl is None:
            return []
        q = tbl.search([float(x) for x in vecs[0]])
        try:
            q = q.metric("cosine")
        except Exception:
            pass
        rows = q.where(f"kind = '{kind}'").limit(k).to_list()
        out = []
        for r in rows:
            d = r.get("_distance", 0.0)
            out.append({
                "ref_id": r.get("ref_id"), "text": r.get("text"),
                "score": round(1.0 / (1.0 + d), 4),
                "symbol": r.get("symbol") or None, "action": r.get("action") or None,
                "pnl": r.get("pnl"),
            })
        return out
    except Exception as e:
        _log(f"recall 失败: {e!r}")
        return []


def embeddings_ready() -> bool:
    return llm.embeddings_available()


def stats() -> dict:
    try:
        tbl = _open(_connect())
        if tbl is None:
            return {"_total": 0, "_embeddings_ready": embeddings_ready()}
        d = tbl.to_arrow().to_pydict()
        per = {}
        for kk in d.get("kind", []):
            per[kk] = per.get(kk, 0) + 1
        per["_total"] = sum(v for k, v in per.items() if not k.startswith("_"))
        per["_embeddings_ready"] = embeddings_ready()
        return per
    except Exception as e:
        return {"_error": repr(e)}


if __name__ == "__main__":
    print("stats:", stats())
    if len(sys.argv) > 1:
        for h in recall_similar(sys.argv[1]):
            print(f"  {h['score']:.3f}  {h.get('symbol')} pnl={h.get('pnl')}  {h['text'][:60]}")
