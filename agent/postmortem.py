"""
Flintrade Agent —— 逐笔平仓复盘(postmortem / "cognitive iteration")层。

这是 Dreaming 的姊妹层:reflect.py 做的是"跨交易的模式蒸馏"(聚合统计 + 语义
教训),这里做的是"单笔交易的诚实复盘" —— 每一笔已平仓、可归因为策略的交易,
配对它的建仓意图(thesis/confidence)与实际结果,先用纯代码算客观指标
(exit_kind/slippage),再用便宜档位 LLM 给一个诚实的判词(thesis_verdict/
entry_grade/exit_grade/confidence_justified/lesson)。

安全:从不交易、从不下单。写入表 trade_reviews 是"做梦产物",不是账本本身,
所以角色用 reflect,并对该表使用 raw conn.execute(见下方注释)。
纯 stdlib + agent.llm/agent.db。验证用 canned-response seam,不花真钱调模型。

CLI:
    python -m agent.postmortem --once      # 复盘至多 10 笔新交易并打印
    python -m agent.postmortem --stats     # 打印复盘统计
    python -m agent.postmortem --backfill  # 循环复盘直到没有新交易
"""
from __future__ import annotations

import json
import re
import sys
from datetime import datetime, timezone

from agent.db import DB, now

# ── 测试用 seam:置入一个 str(LLM 原始输出)即跳过真实 LLM 调用 ────────────
_CANNED_LLM_RESPONSE: str | None = None


def set_canned_llm_response(raw: str | None) -> None:
    """测试钩子:注入一段 LLM 原始输出,review_new() 将解析它而不调真实模型。"""
    global _CANNED_LLM_RESPONSE
    _CANNED_LLM_RESPONSE = raw


def _parse_ts(ts: str | None) -> datetime | None:
    """解析 db.now() 风格的 'YYYY-MM-DDTHH:MM:SSZ'(也容忍带 +00:00)。"""
    if not ts:
        return None
    try:
        s = ts.strip()
        if s.endswith("Z"):
            s = s[:-1] + "+00:00"
        dt = datetime.fromisoformat(s)
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=timezone.utc)
        return dt
    except (ValueError, TypeError):
        return None


# ─────────────────────────────────────────────────────────────────────────
# 配对 + 代码指标(纯代码,无 LLM)
# ─────────────────────────────────────────────────────────────────────────
def pair_roundtrip(db: DB, closing_trade_row) -> dict | None:
    """把一笔平仓 trade 与它的建仓 position + 建仓 intent 配对成一份复盘上下文。

    closing_trade_row: trades 表的一行(sqlite3.Row),pnl 非空、position_id 非空。
    返回 None 表示找不到对应 position(数据不完整,调用方应跳过)。
    """
    pos = db.conn.execute(
        "SELECT * FROM positions WHERE id=?", (closing_trade_row["position_id"],)
    ).fetchone()
    if pos is None:
        return None

    intent = None
    if pos["intent_id"] is not None:
        intent = db.conn.execute(
            "SELECT * FROM intents WHERE id=?", (pos["intent_id"],)
        ).fetchone()

    opened_dt = _parse_ts(pos["opened_at"])
    exit_dt = _parse_ts(closing_trade_row["ts"])
    hold_minutes = None
    if opened_dt is not None and exit_dt is not None:
        hold_minutes = round((exit_dt - opened_dt).total_seconds() / 60.0)

    return {
        "trade_id": closing_trade_row["id"],
        "position_id": pos["id"],
        "symbol": closing_trade_row["symbol"],
        "attribution": closing_trade_row["attribution"],
        "side": pos["side"],
        "entry_price": pos["entry_price"],
        "exit_price": closing_trade_row["fill_price"],
        "qty": closing_trade_row["qty"],
        "pnl": closing_trade_row["pnl"],
        "stop": pos["stop"],
        "target": pos["target"],
        "hold_minutes": hold_minutes,
        "confidence": intent["confidence"] if intent is not None else None,
        "thesis": intent["reason"] if intent is not None else None,
        "exit_reason": closing_trade_row["reason"],
    }


def code_metrics(ctx: dict) -> dict:
    """从复盘上下文纯代码推 exit_kind + slippage_vs_stop(不依赖 LLM)。"""
    reason = (ctx.get("exit_reason") or "").lower()
    if "target" in reason:
        exit_kind = "target"
    elif "stop" in reason:
        exit_kind = "stop"
    else:
        exit_kind = "discretionary"

    slippage = None
    if exit_kind == "stop" and ctx.get("stop") is not None and ctx.get("exit_price") is not None:
        if ctx.get("side") == "long":
            slippage = round(ctx["stop"] - ctx["exit_price"], 4)
        else:  # short
            slippage = round(ctx["exit_price"] - ctx["stop"], 4)

    return {"exit_kind": exit_kind, "slippage_vs_stop": slippage}


# ─────────────────────────────────────────────────────────────────────────
# LLM 判词(flash 档,便宜)
# ─────────────────────────────────────────────────────────────────────────
def _build_review_prompt(ctx: dict, metrics: dict) -> str:
    return f"""You are the trade-postmortem ("cognitive iteration") layer for an autonomous short-term US-equities paper-trading agent. A trade has just closed. Review it honestly and critically — you are not the trader, you are its skeptical supervisor.

Symbol: {ctx.get('symbol')}
Side: {ctx.get('side')}
Entry price: {ctx.get('entry_price')}
Stop: {ctx.get('stop')}
Target: {ctx.get('target')}
Exit price: {ctx.get('exit_price')}
Realized PnL: {ctx.get('pnl')}
Hold time: {ctx.get('hold_minutes')} minutes
Exit kind (code-derived from exit reason text): {metrics.get('exit_kind')}
Slippage vs stop (code-derived; positive = worse than stop, only set when exit_kind=stop): {metrics.get('slippage_vs_stop')}

Entry thesis / reasoning, in the trader's own words at entry time:
{ctx.get('thesis') or '(none recorded)'}

Entry confidence stated at entry time (0-100): {ctx.get('confidence')}

Exit reason, in the trader's own words at exit time:
{ctx.get('exit_reason') or '(none recorded)'}

Judge, in hindsight, with the full outcome known:
- thesis_verdict: was the entry thesis actually correct? correct | partial | wrong | unknowable
- entry_grade: was the entry itself well-timed/well-structured? good | ok | bad
- exit_grade: was the exit timing right? early | right | late
- confidence_justified: true/false — was the stated entry confidence warranted given the setup quality and outcome?
- lesson: ONE specific, reusable lesson (<=140 chars) — durable and actionable, not just a restatement of the pnl.

Respond with ONE fenced ```json block, exactly this shape, nothing else:
```json
{{"thesis_verdict":"correct","entry_grade":"ok","exit_grade":"right","confidence_justified":false,"lesson":"..."}}
```"""


def _call_review_llm(prompt: str) -> str:
    """复盘走 flash 档(便宜模型)。若设置了 canned response,直接返回它(测试不花钱)。"""
    if _CANNED_LLM_RESPONSE is not None:
        return _CANNED_LLM_RESPONSE
    from agent import llm
    return llm.complete(prompt, tier="flash", max_budget_usd=0.30, tag="postmortem")


def _parse_review(text: str) -> dict | None:
    """从 LLM 输出抽 JSON。容忍 fenced block 与裸 JSON(同 reflect._parse_dream 思路)。"""
    m = re.search(r"```json\s*(\{.*?\})\s*```", text, re.DOTALL)
    raw = m.group(1) if m else None
    if not raw:
        start = text.find("{")
        end = text.rfind("}")
        if start != -1 and end > start:
            raw = text[start:end + 1]
    if not raw:
        return None
    try:
        obj = json.loads(raw)
        return obj if isinstance(obj, dict) else None
    except (ValueError, TypeError):
        return None


_VALID_VERDICT = {"correct", "partial", "wrong", "unknowable"}
_VALID_ENTRY_GRADE = {"good", "ok", "bad"}
_VALID_EXIT_GRADE = {"early", "right", "late"}


def _review_via_llm(ctx: dict, metrics: dict) -> dict:
    """调 LLM 拿判词,防御性解析。解析失败/字段无效 → 对应字段直接缺席(None)。
    绝不抛异常、绝不阻塞:任何问题都以"字段为空"收场,由调用方落 NULL。"""
    prompt = _build_review_prompt(ctx, metrics)
    try:
        text = _call_review_llm(prompt)
    except Exception:
        return {}
    parsed = _parse_review(text)
    if not isinstance(parsed, dict):
        return {}

    out: dict = {}
    verdict = parsed.get("thesis_verdict")
    if verdict in _VALID_VERDICT:
        out["thesis_verdict"] = verdict
    entry_grade = parsed.get("entry_grade")
    if entry_grade in _VALID_ENTRY_GRADE:
        out["entry_grade"] = entry_grade
    exit_grade = parsed.get("exit_grade")
    if exit_grade in _VALID_EXIT_GRADE:
        out["exit_grade"] = exit_grade
    cj = parsed.get("confidence_justified")
    if isinstance(cj, bool):
        out["confidence_justified"] = cj
    lesson = parsed.get("lesson")
    if isinstance(lesson, str) and lesson.strip():
        out["lesson"] = lesson.strip()[:140]
    return out


# ─────────────────────────────────────────────────────────────────────────
# review_new() —— 找未复盘的已平仓策略交易,逐笔复盘
# ─────────────────────────────────────────────────────────────────────────
def review_new(limit: int = 10) -> list[str]:
    """复盘至多 limit 笔尚无 trade_reviews 行的已平仓策略交易(最旧优先)。

    只处理 attribution IN ('strategy') 或 attribution IS NULL(向前兼容:老数据
    没打过标,按 strategy 处理)。manual/test/outage-degraded 一律跳过 ——
    不进复盘,也就不会污染绩效结论。返回人类可读的日志行列表。
    """
    with DB(role="reflect") as db:  # 显式生命周期:LLM 中途抛异常也不泄漏连接
        return _review_new(db, limit)


def _review_new(db: DB, limit: int) -> list[str]:
    rows = db.conn.execute(
        """SELECT t.* FROM trades t
           LEFT JOIN trade_reviews r ON r.trade_id = t.id
           WHERE t.pnl IS NOT NULL
             AND (t.attribution IS NULL OR t.attribution = 'strategy')
             AND r.id IS NULL
           ORDER BY t.id ASC
           LIMIT ?""",
        (limit,),
    ).fetchall()

    lines: list[str] = []
    for row in rows:
        ctx = pair_roundtrip(db, row)
        if ctx is None:
            # 配对失败(如旧时代迁移成交没有 position 链路)也必须落一条 stub 行:
            # 否则这些 trade 永远匹配查询、堵死 oldest-first 队列头,新成交永远轮不到复盘,
            # --backfill 也会死循环。UNIQUE(trade_id) 让它们从此出队。
            db.conn.execute(
                """INSERT OR IGNORE INTO trade_reviews(trade_id, symbol, attribution, pnl,
                       exit_kind, reviewed_at)
                   VALUES(?,?,?,?, 'unpairable', ?)""",
                (row["id"], row["symbol"], row["attribution"], row["pnl"], now()),
            )
            lines.append(f"trade {row['id']} ({row['symbol']}): no matching position → stub(unpairable)")
            continue

        metrics = code_metrics(ctx)
        llm_fields = _review_via_llm(ctx, metrics)
        cj = llm_fields.get("confidence_justified")
        cj_int = 1 if cj is True else (0 if cj is False else None)

        # trade_reviews 是 dreaming 产物表,role='reflect' 的白名单只覆盖
        # memory/agg(见 db.py _WRITE_PERMS);这里走 raw conn.execute 作为
        # established escape hatch,UNIQUE(trade_id) 保证 INSERT OR IGNORE 幂等。
        db.conn.execute(
            """INSERT OR IGNORE INTO trade_reviews(
                   trade_id, position_id, symbol, attribution, entry_price, exit_price,
                   qty, pnl, stop, target, hold_minutes, exit_kind, slippage_vs_stop,
                   confidence, thesis, exit_reason, thesis_verdict, entry_grade, exit_grade,
                   confidence_justified, lesson, reviewed_at)
               VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
            (ctx["trade_id"], ctx["position_id"], ctx["symbol"], ctx["attribution"],
             ctx["entry_price"], ctx["exit_price"], ctx["qty"], ctx["pnl"], ctx["stop"],
             ctx["target"], ctx["hold_minutes"], metrics["exit_kind"], metrics["slippage_vs_stop"],
             ctx["confidence"], ctx["thesis"], ctx["exit_reason"],
             llm_fields.get("thesis_verdict"), llm_fields.get("entry_grade"),
             llm_fields.get("exit_grade"), cj_int, llm_fields.get("lesson"), now()),
        )
        lines.append(
            f"trade {ctx['trade_id']} ({ctx['symbol']}): exit_kind={metrics['exit_kind']} "
            f"pnl={ctx['pnl']} verdict={llm_fields.get('thesis_verdict')}"
        )
    return lines


# ─────────────────────────────────────────────────────────────────────────
# stats() —— 纯代码汇总
# ─────────────────────────────────────────────────────────────────────────
def stats() -> dict:
    """从 trade_reviews(join trades 取权威 pnl)汇总:n/win_rate/net_pnl/
    avg slippage_vs_stop/exit_kind 计数/按 confidence 分桶的胜率。"""
    db = DB(role="reader")
    rows = db.conn.execute(
        """SELECT r.exit_kind, r.slippage_vs_stop, r.confidence, t.pnl AS pnl
           FROM trade_reviews r JOIN trades t ON t.id = r.trade_id"""
    ).fetchall()
    db.close()

    n = len(rows)
    wins = sum(1 for r in rows if (r["pnl"] or 0.0) > 0)
    win_rate = round(wins / n, 3) if n else None
    net_pnl = round(sum(r["pnl"] or 0.0 for r in rows), 2)

    slips = [r["slippage_vs_stop"] for r in rows if r["slippage_vs_stop"] is not None]
    avg_slippage = round(sum(slips) / len(slips), 4) if slips else None

    exit_kind_counts: dict = {}
    for r in rows:
        k = r["exit_kind"] or "unknown"
        exit_kind_counts[k] = exit_kind_counts.get(k, 0) + 1

    buckets = {"<60": [], "60-70": [], ">70": []}
    for r in rows:
        c = r["confidence"]
        if c is None:
            continue
        if c < 60:
            buckets["<60"].append(r)
        elif c <= 70:
            buckets["60-70"].append(r)
        else:
            buckets[">70"].append(r)
    confidence_buckets = {}
    for name, lst in buckets.items():
        n_b = len(lst)
        wins_b = sum(1 for r in lst if (r["pnl"] or 0.0) > 0)
        confidence_buckets[name] = {
            "n": n_b,
            "win_rate": round(wins_b / n_b, 3) if n_b else None,
        }

    return {
        "n": n,
        "win_rate": win_rate,
        "net_pnl": net_pnl,
        "avg_slippage_vs_stop": avg_slippage,
        "exit_kind_counts": exit_kind_counts,
        "confidence_buckets": confidence_buckets,
    }


if __name__ == "__main__":
    if "--stats" in sys.argv:
        print(json.dumps(stats(), ensure_ascii=False, indent=2))
    elif "--backfill" in sys.argv:
        total = 0
        while True:
            lines = review_new(limit=10)
            if not lines:
                break
            for line in lines:
                print(line)
            total += len(lines)
        print(f"backfill done: {total} reviewed")
    elif "--once" in sys.argv:
        lines = review_new(limit=10)
        for line in lines:
            print(line)
        print(f"{len(lines)} reviewed")
    else:
        print("usage: python -m agent.postmortem [--once|--stats|--backfill]")
