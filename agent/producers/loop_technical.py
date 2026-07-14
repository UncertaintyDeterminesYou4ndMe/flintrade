"""
技术面信号生产者(Arena 模式)。collect.sh 采数据 → claude -p 纯分析 → 输出 intent。
绝不下单、绝不写持仓 —— 只 submit_intent 投递队列,Executor 负责风控与执行。

与旧 run.sh 的区别:LLM 的 system prompt 换成 technical_intent.md(只产假设,不算量、
不下单);LLM 无工具权限(纯文本 in/out);持仓真相从 flint.db 注入(不再读过时的
state.json)。
"""
from __future__ import annotations

import json
import os
import re
import subprocess
import time

from agent.config import load_risk, load_trading
from agent.db import DB, FLINT_DIR
from agent import session as sess

COLLECT = FLINT_DIR / "scripts" / "collect.sh"
PROMPT = FLINT_DIR / "agent" / "prompts" / "technical_intent.md"

# LLM action → intent.side
_SIDE = {"BUY": "long", "SHORT": "short", "CLOSE": "close", "SELL": "close"}


def collect_data() -> dict:
    """跑 collect.sh 拿市场数据,再用 db 的持仓真相覆盖 flint_positions。"""
    # 把当前时段 + outside_rth 传给 collect.sh(同旧 run.sh),让 payload 的 session 准确。
    s = sess.current_session()
    env = {**os.environ, "MARKET_STATUS": s, "OUTSIDE_RTH": sess.outside_rth_for(s)}
    out = subprocess.run(["bash", str(COLLECT)], capture_output=True, text=True,
                         timeout=120, env=env)
    try:
        data = json.loads(out.stdout)
    except (ValueError, TypeError):
        data = {"error": "collect failed", "stderr": out.stderr[:500]}
    db = DB(role="reader")
    data["flint_positions"] = [dict(p) for p in db.open_positions()]
    r = db.get_risk()
    data["flint_risk"] = {"equity": r["equity"], "open_risk": r["open_risk"],
                          "halt": bool(r["halt"])} if r else {}
    # 唤醒回忆:先定向时间,再调取做梦合成的 lessons + 未过期 plans。
    # recall() 纯读库、不调 broker,以当前 ET 过滤过期计划。
    try:
        from agent import reflect
        data["recall"] = reflect.recall()
    except Exception as e:
        data["recall"] = {"error": f"recall unavailable: {e!r}"}
    # 语义记忆:按当前最高分候选的 setup,召回历史上最像的成交及其结果。
    # 嵌入后端(Ollama)没起时静默跳过,不影响决策。
    try:
        from agent import memory_store
        if memory_store.embeddings_ready():
            inds = data.get("indicators") or []
            best = max((i for i in inds if isinstance(i, dict)),
                       key=lambda i: i.get("score", 0), default=None)
            if best and best.get("symbol"):
                q = (f"{best['symbol']} score {best.get('score')} "
                     f"RSI {best.get('rsi')} vol {best.get('volume_ratio')} "
                     f"{'above' if best.get('above_vwap') else 'below'} VWAP")
                data["similar_history"] = memory_store.recall_similar(q, kind="trade", k=5)
    except Exception as e:
        print(f"[loop_technical] similar_history skipped: {e}", file=__import__('sys').stderr)
    return data


def call_llm(data: dict) -> str:
    """Arena:无工具的纯分析调用。走 llm 路由(trader 档),provider 可后期切换。"""
    from agent import llm
    return llm.complete(json.dumps(data, ensure_ascii=False),
                        system_prompt=PROMPT.read_text(), tier="trader")


def parse_intent(text: str) -> dict | None:
    """从 LLM 输出抽 JSON。容忍 fenced block 与裸行两种形式。"""
    m = re.search(r"```json\s*(\{.*?\})\s*```", text, re.DOTALL)
    raw = m.group(1) if m else None
    if not raw:
        for line in text.strip().splitlines():
            line = line.strip()
            if line.startswith("{") and "action" in line:
                raw = line
                break
    if not raw:
        return None
    try:
        return json.loads(raw)
    except (ValueError, TypeError):
        return None


def to_intent(decision: dict) -> dict | None:
    """LLM 决策 → submit_intent 参数。WAIT/HOLD 返回 None。"""
    action = (decision.get("action") or "").upper()
    if action in ("WAIT", "HOLD", ""):
        return None
    side = _SIDE.get(action)
    if not side or not decision.get("symbol"):
        return None
    feats = {}
    if decision.get("volume_ratio") is not None:
        feats["volume_ratio"] = decision["volume_ratio"]
    return {
        "symbol": decision["symbol"],
        "side": side,
        "entry_hint": decision.get("entry_hint"),
        "stop": decision.get("stop"),
        "target": decision.get("target"),
        "confidence": decision.get("confidence"),
        "reason": (decision.get("reasoning") or "")[:1000],
        "features": feats or None,
    }


def run_once() -> str:
    db = DB(role="technical")
    db.beat(process="loop_technical")
    if db.is_halted():
        return "halted — 跳过技术面信号产出"

    data = collect_data()
    text = call_llm(data)
    decision = parse_intent(text)
    if not decision:
        return f"无法解析 LLM 输出(前80字: {text[:80]!r})"
    intent = to_intent(decision)
    if not intent:
        return f"WAIT — {decision.get('reasoning','')[:80]}"

    prio = load_risk()["priority"]["technical"]
    # 同一周期内同票去重(executor 轮询快于本 loop,正常不会堆积)
    cycle = time.strftime("%Y%m%d%H%M", time.gmtime())
    dedup = f"technical:{intent['symbol']}:{cycle}"
    iid = db.submit_intent(source="technical", priority=prio, dedup_key=dedup, **intent)
    if iid is None:
        return f"重复 intent 已去重({intent['symbol']})"
    return f"intent #{iid}: {intent['side']} {intent['symbol']} @ {intent['entry_hint']} stop {intent['stop']} conf {intent['confidence']}"


def run_forever():
    cadence = load_trading()["cadence"]["technical_loop_sec"]
    while True:
        try:
            print(run_once(), flush=True)
        except Exception as e:  # 生产者崩溃不应拖垮系统
            print(f"loop_technical error: {e!r}", flush=True)
        time.sleep(cadence)


if __name__ == "__main__":
    import sys
    if "--once" in sys.argv:
        print(run_once())
    else:
        run_forever()
