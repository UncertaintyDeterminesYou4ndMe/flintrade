"""
舆情/催化剂信号生产者(Arena 模式)。消费近期检测到的新闻 catalyst(events 表里
status='pending' 的 news_catalyst / macro),让 claude -p 判断方向与强度 → 产出
HIGH-PRIORITY intent。与技术面 loop 同构,但驱动源是新闻事件而非指标。

绝不下单、绝不写持仓 —— 只 submit_intent 投递队列,Executor 负责风控与执行。

幂等性是关键:同一条新闻在多次轮询里反复出现,必须只触发一次 intent。
两道保险:
  1. kv 游标 "event_cursor" 记录已处理的最大 event id,gather 只取 id > 游标的新事件;
  2. dedup_key = f"event:{symbol}:{event_id}" 绑定到具体 catalyst,DB 唯一索引兜底。
"""
from __future__ import annotations

import json
import subprocess
import time

from agent.config import load_risk, load_trading
from agent.db import DB, FLINTRADE_DIR
from agent import session as sess

# 复用技术面 loop 的解析/转换,保证 Executor 看到的 intent 形态完全一致。
from agent.producers.loop_technical import parse_intent, to_intent

PROMPT = FLINTRADE_DIR / "agent" / "prompts" / "event_intent.md"

# 哪些 event.kind 算可交易的舆情催化剂。
_CATALYST_KINDS = ("news_catalyst", "macro", "earnings", "news")
_CURSOR_KEY = "event_cursor"
_MAX_CATALYSTS = 8   # 单轮最多处理的新事件数(防止积压时一次喷太多)


def _quote(symbol: str) -> dict | None:
    """尽力获取实时报价;失败返回 None(防御性,不让取价拖垮 loop)。"""
    try:
        from agent import lb
        ok, data, _ = lb.run(["quote", symbol, "--format", "json"], timeout=20)
        if not ok or data is None:
            return None
        # quote 可能返回 list 或单对象,两种都容忍。
        if isinstance(data, list):
            return data[0] if data else None
        return data
    except Exception:
        return None


def gather_catalysts(db: DB | None = None) -> list[dict]:
    """读取 id 大于游标的新 pending 催化剂事件,附上同票近期 news signals,并推进游标。

    返回 [{event: {...}, signals: [{...}, ...]}, ...]。无新事件返回 []。
    游标在「读到事件」后即推进 —— 即使后续 LLM 判定 WAIT,也不再重复点火同一新闻。
    """
    own = db is None
    if own:
        db = DB(role="event")
    try:
        cursor = int(db.kv_get(_CURSOR_KEY, "0") or "0")
        placeholders = ",".join("?" for _ in _CATALYST_KINDS)
        rows = db.conn.execute(
            f"""SELECT * FROM events
                WHERE status='pending' AND id > ? AND kind IN ({placeholders})
                ORDER BY id DESC LIMIT ?""",
            (cursor, *_CATALYST_KINDS, _MAX_CATALYSTS),
        ).fetchall()
        if not rows:
            return []

        catalysts = []
        max_id = cursor
        for ev in rows:
            evd = dict(ev)
            max_id = max(max_id, evd["id"])
            sigs = db.conn.execute(
                """SELECT * FROM signals
                   WHERE kind='news' AND symbol IS ? ORDER BY id DESC LIMIT 5""",
                (evd.get("symbol"),),
            ).fetchall()
            catalysts.append({"event": evd, "signals": [dict(s) for s in sigs]})

        # 推进游标到本批最大 id,确保同一事件不再被重复 gather。
        if max_id > cursor:
            db.kv_set(_CURSOR_KEY, str(max_id))
        return catalysts
    finally:
        if own:
            db.close()


def _build_payload(catalysts: list[dict]) -> dict:
    """组装 LLM 数据载荷:催化剂 + 受影响标的的现报价。"""
    symbols = []
    for c in catalysts:
        sym = c["event"].get("symbol")
        if sym and sym not in symbols:
            symbols.append(sym)
    quotes = {s: _quote(s) for s in symbols}
    return {
        "now_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "catalysts": catalysts,
        "quotes": quotes,
    }


def call_llm(data: dict) -> str:
    """Arena:无工具的纯分析调用(同 loop_technical)。走 llm 路由(trader 档)。"""
    from agent import llm
    return llm.complete(json.dumps(data, ensure_ascii=False),
                        system_prompt=PROMPT.read_text(), tier="trader")


def run_once() -> str | list[str]:
    """入口:连接生命周期显式管理(不能依赖 GC 关闭,见 2026-07-26 fd 泄漏事故)。"""
    with DB(role="event") as db:
        return _run_once(db)


def _run_once(db: DB) -> str | list[str]:
    db.beat(process="loop_event")
    if db.is_halted():
        return "halted — 跳过舆情信号产出"
    # 非交易时段不消费催化剂:游标不推进,事件留在队列里,开盘后由 LLM 判断新鲜度。
    # 休市时产出的 intent 只会变成躺到下个开盘的挂单(见 loop_technical 同款注释)。
    session = sess.current_session()
    if not sess.is_tradeable(session):
        return f"session={session} — 非交易时段,跳过"

    catalysts = gather_catalysts(db)
    if not catalysts:
        return "no new catalysts"

    data = _build_payload(catalysts)
    text = call_llm(data)
    decision = parse_intent(text)
    if not decision:
        return [f"无法解析 LLM 输出(前80字: {text[:80]!r})"]

    intent = to_intent(decision)
    if not intent:
        return [f"WAIT — {decision.get('reasoning', '')[:80]}"]

    # 把 intent 绑定到具体催化剂(同票优先用 symbol 匹配的事件,否则取首个)。
    sym = intent["symbol"]
    event_id = next(
        (c["event"]["id"] for c in catalysts if c["event"].get("symbol") == sym),
        catalysts[0]["event"]["id"],
    )
    prio = load_risk()["priority"]["event"]
    dedup = f"event:{sym}:{event_id}"
    iid = db.submit_intent(source="event", priority=prio, dedup_key=dedup, **intent)
    if iid is None:
        return [f"重复 catalyst 已去重({dedup})"]
    return [
        f"intent #{iid}: {intent['side']} {sym} @ {intent['entry_hint']} "
        f"stop {intent['stop']} conf {intent['confidence']} (event #{event_id})"
    ]


def run_forever():
    cadence = load_trading()["cadence"]["event_loop_sec"]
    while True:
        try:
            out = run_once()
            for line in (out if isinstance(out, list) else [out]):
                print(line, flush=True)
        except Exception as e:  # 生产者崩溃不应拖垮系统
            print(f"loop_event error: {e!r}", flush=True)
        time.sleep(cadence)


if __name__ == "__main__":
    import sys
    if "--once" in sys.argv:
        out = run_once()
        for line in (out if isinstance(out, list) else [out]):
            print(line)
    else:
        run_forever()
