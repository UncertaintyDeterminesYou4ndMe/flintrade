"""
Flint Agent —— "Dreaming" / 记忆合成层(reflect)。

这是 agent 的「睡眠」。它在**休市窗口**(session.current_session() == 'Closed')运行,
把原始交易历史蒸馏成有界、可复用的记忆,再通过 recall() 把记忆带着**时间纵深感**
注回决策。

设计隐喻(全部要点都在这):agent 应当像一个人一样醒来 —— 先在时间里定位
(「今天几号、距我上次行动/上次和人说话有多久」),再回想昨天的小结 + 今天的计划。
记忆合成发生在睡眠期(休市)。两类记忆,对应人脑两种记忆:

  - LESSONS(语义记忆,持久、不绑日期、带 confidence)
      例:「Intraday 收盘前 30min 内开仓:n=47,胜率 31% vs 全天 54%」。
  - PLANS(情景记忆,会过期)
      例:「关注 NVDA 6/12 财报」。醒来时按当日 ET 过滤,过期的丢弃。

四个动作:
  aggregate()          纯代码:把已平仓 trade 滚进 agg 表。
  synthesize_lessons() Haiku「做梦」:有界重写 <=20 条 lessons + 若干 plans。
  decay()              纯代码:时间衰减 confidence、过期 plans。
  recall()             唤醒/定向:返回注入 prompt 的紧凑 dict(含 time_anchor)。

安全:从不交易、从不下单。纯 stdlib(Python 3.14)。验证用 canned-response seam,
不花真钱调 claude。
"""
from __future__ import annotations

import json
import re
import subprocess
import sys
import time
from datetime import datetime, timedelta, timezone
from zoneinfo import ZoneInfo

from agent.db import DB, FLINT_DIR, now
from agent.session import current_session

ET = ZoneInfo("America/New_York")

MAX_LESSONS = 20          # 有界重写上限:压缩而非累积
RECALL_LESSONS_CAP = 12   # 注入 prompt 的 lessons 上限
DECAY_STALE_DAYS = 7      # 每 stale 一周 confidence *0.9
DECAY_FACTOR = 0.9
ARCHIVE_BELOW = 0.2       # confidence 低于此值 → archived
DREAM_MODEL = "claude-haiku-4-5"
DREAM_BUDGET_USD = "0.50"
DREAM_LOG_DIR = FLINT_DIR / "logs"

# ── 测试用 seam:置入一个 str(LLM 原始输出)即跳过真实 claude 调用 ──────────
_CANNED_LLM_RESPONSE: str | None = None


def set_canned_llm_response(raw: str | None) -> None:
    """测试钩子:注入一段 LLM 原始输出,synthesize_lessons() 将解析它而不调 claude。"""
    global _CANNED_LLM_RESPONSE
    _CANNED_LLM_RESPONSE = raw


# ─────────────────────────────────────────────────────────────────────────
# 时间工具
# ─────────────────────────────────────────────────────────────────────────
def _parse_utc(ts: str | None) -> datetime | None:
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


def _today_et(now_et: datetime | None = None) -> str:
    """当前 ET 日期 'YYYY-MM-DD'。"""
    dt = now_et or datetime.now(ET)
    return dt.strftime("%Y-%m-%d")


def _session_for_ts(ts: str | None) -> str:
    """从 trade 时间戳(UTC)推 session,按 ET 小时分桶。run.sh 的 session 切分:
    Pre 04:00-09:30, Intraday 09:30-16:00, Post 16:00-20:00, 其余 Overnight。"""
    dt = _parse_utc(ts)
    if dt is None:
        return "Overnight"
    et = dt.astimezone(ET)
    mins = et.hour * 60 + et.minute
    if 4 * 60 <= mins < 9 * 60 + 30:
        return "Pre"
    if 9 * 60 + 30 <= mins < 16 * 60:
        return "Intraday"
    if 16 * 60 <= mins < 20 * 60:
        return "Post"
    return "Overnight"


def _rsi_bucket(features_json: str | None) -> str:
    """从 trade.features json 取 rsi → 桶。无则 'na'。"""
    if not features_json:
        return "na"
    try:
        feats = json.loads(features_json)
    except (ValueError, TypeError):
        return "na"
    if not isinstance(feats, dict):
        return "na"
    rsi = feats.get("rsi")
    if rsi is None:
        return "na"
    try:
        r = float(rsi)
    except (ValueError, TypeError):
        return "na"
    if r < 30:
        return "<30"
    if r < 50:
        return "30-50"
    if r < 70:
        return "50-70"
    return ">70"


def _human_gap(then: datetime | None, ref: datetime | None = None) -> str:
    """人读时间差:'45 minutes' / '3.2 days' / 'never' / 'just now'。"""
    if then is None:
        return "never"
    ref = ref or datetime.now(timezone.utc)
    if then.tzinfo is None:
        then = then.replace(tzinfo=timezone.utc)
    secs = (ref - then).total_seconds()
    if secs < 0:
        return "in the future"
    if secs < 90:
        return "just now"
    mins = secs / 60.0
    if mins < 90:
        return f"{round(mins)} minutes"
    hours = mins / 60.0
    if hours < 36:
        return f"{hours:.1f} hours"
    days = hours / 24.0
    return f"{days:.1f} days"


# ─────────────────────────────────────────────────────────────────────────
# 1. aggregate() —— 纯代码:已平仓 trade 滚进 agg
# ─────────────────────────────────────────────────────────────────────────
def aggregate() -> int:
    """把所有 pnl 非空的 trade 按 (symbol, session, setup, rsi_bucket) 聚合进 agg。

    setup 取 trade.source(technical/event/user;缺省 'unknown')。
    pl_ratio = avg_win / avg_loss(亏损取绝对值;无亏损则 None)。
    全量重算该格子(幂等):把命中同一 key 的所有已平仓 trade 重新统计后 upsert。
    返回写入/更新的 agg 行数。
    """
    db = DB(role="reflect")
    rows = db.conn.execute(
        "SELECT id, symbol, ts, source, features, pnl FROM trades WHERE pnl IS NOT NULL"
    ).fetchall()

    # key -> {trips, wins, losses, win_sum, loss_sum}
    buckets: dict[tuple, dict] = {}
    for r in rows:
        symbol = r["symbol"]
        session = _session_for_ts(r["ts"])
        setup = r["source"] or "unknown"
        rsi_b = _rsi_bucket(r["features"])
        key = (symbol, session, setup, rsi_b)
        b = buckets.setdefault(
            key, {"trips": 0, "wins": 0, "losses": 0, "win_sum": 0.0, "loss_sum": 0.0}
        )
        pnl = float(r["pnl"])
        b["trips"] += 1
        if pnl > 0:
            b["wins"] += 1
            b["win_sum"] += pnl
        elif pnl < 0:
            b["losses"] += 1
            b["loss_sum"] += abs(pnl)
        # pnl == 0 计入 trips 但不计 win/loss

    ts = now()
    n = 0
    for (symbol, session, setup, rsi_b), b in buckets.items():
        avg_win = b["win_sum"] / b["wins"] if b["wins"] else 0.0
        avg_loss = b["loss_sum"] / b["losses"] if b["losses"] else 0.0
        pl_ratio = (avg_win / avg_loss) if avg_loss > 0 else None
        db.conn.execute(
            """INSERT INTO agg(symbol,session,setup,rsi_bucket,trips,wins,losses,pl_ratio,updated_at)
               VALUES(?,?,?,?,?,?,?,?,?)
               ON CONFLICT(symbol,session,setup,rsi_bucket) DO UPDATE SET
                   trips=excluded.trips, wins=excluded.wins, losses=excluded.losses,
                   pl_ratio=excluded.pl_ratio, updated_at=excluded.updated_at""",
            (symbol, session, setup, rsi_b, b["trips"], b["wins"], b["losses"], pl_ratio, ts),
        )
        n += 1
    db.close()
    return n


# ─────────────────────────────────────────────────────────────────────────
# 2. synthesize_lessons() —— Haiku「做梦」:有界重写
# ─────────────────────────────────────────────────────────────────────────
def _agg_summary(db: DB, limit: int = 60) -> list[dict]:
    rows = db.conn.execute(
        """SELECT symbol,session,setup,rsi_bucket,trips,wins,losses,pl_ratio
           FROM agg ORDER BY trips DESC LIMIT ?""",
        (limit,),
    ).fetchall()
    out = []
    for r in rows:
        trips = r["trips"] or 0
        win_rate = round(r["wins"] / trips, 3) if trips else None
        out.append({
            "symbol": r["symbol"], "session": r["session"], "setup": r["setup"],
            "rsi_bucket": r["rsi_bucket"], "n": trips, "win_rate": win_rate,
            "pl_ratio": round(r["pl_ratio"], 2) if r["pl_ratio"] is not None else None,
        })
    return out


def _recent_closed_with_reasons(db: DB, limit: int = 40) -> list[dict]:
    """已平仓交易,喂给做梦 LLM 找模式。诚实性规则(3a-i):只喂 strategy 归因的
    交易 —— manual/test/outage-degraded 会污染"我的策略表现如何"这个问题的
    答案,必须在源头排除,而不是指望 LLM 自己识别。"""
    rows = db.conn.execute(
        """SELECT id,ts,symbol,action,qty,fill_price,pnl,source,reason
           FROM trades
           WHERE pnl IS NOT NULL
             AND (attribution IS NULL OR attribution = 'strategy')
           ORDER BY id DESC LIMIT ?""",
        (limit,),
    ).fetchall()
    out = []
    for r in rows:
        out.append({
            "id": r["id"], "ts": r["ts"], "symbol": r["symbol"], "action": r["action"],
            "qty": r["qty"], "pnl": round(float(r["pnl"]), 2),
            "source": r["source"], "reason": (r["reason"] or "")[:400],
        })
    return out


def _recent_reviews(db: DB, limit: int = 15) -> list[dict]:
    """近期逐笔复盘(postmortem 产物),压缩喂给做梦 LLM:symbol/verdict/grades/lesson。"""
    rows = db.conn.execute(
        """SELECT symbol, thesis_verdict, entry_grade, exit_grade, lesson
           FROM trade_reviews ORDER BY id DESC LIMIT ?""",
        (limit,),
    ).fetchall()
    out = []
    for r in rows:
        out.append({
            "symbol": r["symbol"], "thesis_verdict": r["thesis_verdict"],
            "entry_grade": r["entry_grade"], "exit_grade": r["exit_grade"],
            "lesson": r["lesson"],
        })
    return out


HARD_RULES = (
    "HARD RULES: (1) Any statistic with n<10 must be phrased as a preliminary "
    "observation \"(n=X, low sample)\" and must NEVER be stated as a rule, a "
    "win-rate verdict, or an avoid-list entry. (2) Never derive symbol-avoidance "
    "rules from n<10. (3) Trades tagged test/outage-degraded/manual are excluded "
    "from performance conclusions."
)


def build_dream_prompt(agg_rows: list[dict], trades: list[dict], today_et: str,
                       reviews: list[dict] | None = None) -> str:
    """构造给 Haiku 的「做梦」提示。要求 REWRITE(非 append)<=20 条 lessons + plans。"""
    reviews_block = ""
    if reviews:
        reviews_block = f"""

3. RECENT TRADE POSTMORTEMS — per-trade hindsight verdicts from the postmortem layer (symbol, thesis_verdict, entry_grade, exit_grade, one-line lesson). These are already-distilled per-trade judgments; use them to corroborate or challenge patterns you see in the raw trades above.
{json.dumps(reviews, ensure_ascii=False)}"""

    return f"""You are the memory-consolidation ("dreaming") layer of an autonomous short-term US-equities paper-trading agent. The market is CLOSED; this is the agent's sleep. Your job is to DISTILL raw trade history into a small, durable, reusable memory — like a human consolidating the day into long-term memory.

Today (ET) is {today_et}.

You are given two inputs:

1. AGGREGATE STATS — rolled-up performance by (symbol, session, setup, rsi_bucket). win_rate is fraction; pl_ratio = avg_win/avg_loss. n is sample size. Treat small-n rows skeptically.
{json.dumps(agg_rows, ensure_ascii=False)}

2. RECENT CLOSED TRADES with the trader's own reasoning text (these reasons are rich — they already contain the trader's reflections). Use them to find patterns, mistakes, and recurring setups.
{json.dumps(trades, ensure_ascii=False)}{reviews_block}

Produce a COMPLETE REWRITE of the agent's memory (NOT an append). This is compression, not accumulation. Output AT MOST {MAX_LESSONS} durable lessons. Each lesson must be:
  - durable & semantic (NOT date-bound) — a reusable rule, not "today I did X";
  - grounded in evidence (cite trade ids when possible);
  - quantified where the stats support it (cite n, win_rate, pl_ratio);
  - assigned a confidence 0.0-1.0 reflecting sample size and consistency.

Separately, extract any DATED, EPISODIC plans (things to watch on a specific future date — earnings, macro events, "revisit X after Y"). Each plan needs an expires_at date (YYYY-MM-DD); it will be auto-dropped once that date passes.

Respond with ONE fenced ```json block, exactly this shape, nothing else:
```json
{{"lessons":[{{"text":"...","confidence":0.0,"evidence":[1,2],"tags":{{"symbol":"NVDA.US","session":"Intraday","setup":"technical"}}}}],"plans":[{{"text":"watch NVDA earnings 6/12","expires_at":"2026-06-13","tags":{{"symbol":"NVDA.US"}}}}]}}
```
If there is no evidence for plans, return an empty plans list. Do not exceed {MAX_LESSONS} lessons.

{HARD_RULES}"""


def _call_dream_llm(prompt: str) -> str:
    """做梦走 flash 档(便宜模型)。若设置了 canned response,直接返回它(测试不花钱)。"""
    if _CANNED_LLM_RESPONSE is not None:
        return _CANNED_LLM_RESPONSE
    from agent import llm
    return llm.complete(prompt, tier="flash", max_budget_usd=float(DREAM_BUDGET_USD))


def _parse_dream(text: str) -> dict | None:
    """从 LLM 输出抽 JSON。容忍 fenced block 与裸 JSON。"""
    m = re.search(r"```json\s*(\{.*?\})\s*```", text, re.DOTALL)
    raw = m.group(1) if m else None
    if not raw:
        # 退而求其次:抓第一个平衡的大括号块
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


def _write_digest(today_et: str, lessons: list[dict], plans: list[dict]) -> None:
    """可选:写一份 markdown digest 到 logs/dream-<date>.md(best-effort)。"""
    try:
        DREAM_LOG_DIR.mkdir(parents=True, exist_ok=True)
        lines = [f"# Dream digest — {today_et}", "",
                 f"## Lessons ({len(lessons)})", ""]
        for ls in lessons:
            conf = ls.get("confidence", "?")
            lines.append(f"- ({conf}) {ls.get('text','')}")
        lines += ["", f"## Plans ({len(plans)})", ""]
        for pl in plans:
            lines.append(f"- [exp {pl.get('expires_at','?')}] {pl.get('text','')}")
        (DREAM_LOG_DIR / f"dream-{today_et}.md").write_text("\n".join(lines) + "\n")
    except Exception:
        pass  # digest 是装饰品,失败不影响记忆写入


def synthesize_lessons(now_et: datetime | None = None) -> dict:
    """Haiku 做梦 → 有界重写 memory_lessons + upsert memory_plans。

    REPLACE 语义:把现有 active lessons 全部 archive,再插入新的(<=20)。
    plans 用 text 去重 upsert。返回 {'lessons': n, 'plans': n, 'parsed': bool}。
    """
    db = DB(role="reflect")
    agg_rows = _agg_summary(db)
    trades = _recent_closed_with_reasons(db)
    reviews = _recent_reviews(db)
    today = _today_et(now_et)

    prompt = build_dream_prompt(agg_rows, trades, today, reviews)
    text = _call_dream_llm(prompt)
    parsed = _parse_dream(text)
    if parsed is None:
        db.close()
        return {"lessons": 0, "plans": 0, "parsed": False}

    raw_lessons = parsed.get("lessons") or []
    raw_plans = parsed.get("plans") or []
    if not isinstance(raw_lessons, list):
        raw_lessons = []
    if not isinstance(raw_plans, list):
        raw_plans = []
    raw_lessons = raw_lessons[:MAX_LESSONS]   # 有界

    ts = now()

    # ── lessons:先备好新集,再原子替换 ──
    # 防御 1:解析成功但没产出有效 lesson 时,保留旧 active,绝不清空成空记忆。
    # 防御 2:archive 旧 + 插入新 放进同一事务,recall() 永远看不到中间的空窗。
    prepared = []
    for ls in raw_lessons:
        if not isinstance(ls, dict):
            continue
        text_ls = (ls.get("text") or "").strip()
        if not text_ls:
            continue
        try:
            conf = float(ls.get("confidence", 0.5))
        except (ValueError, TypeError):
            conf = 0.5
        conf = max(0.0, min(1.0, conf))
        evidence = ls.get("evidence") if isinstance(ls.get("evidence"), list) else []
        tags = ls.get("tags") if isinstance(ls.get("tags"), dict) else {}
        prepared.append((text_ls, json.dumps(evidence, ensure_ascii=False), conf,
                         json.dumps(tags, ensure_ascii=False)))

    n_lessons = 0
    if prepared:
        db.conn.execute("BEGIN IMMEDIATE")  # 原子替换
        try:
            db.conn.execute("UPDATE memory_lessons SET status='archived' WHERE status='active'")
            for text_ls, ev, conf, tg in prepared:
                db.conn.execute(
                    """INSERT INTO memory_lessons(text,evidence,confidence,tags,status,created,last_confirmed)
                       VALUES(?,?,?,?, 'active', ?, ?)""",
                    (text_ls, ev, conf, tg, ts, ts),
                )
                n_lessons += 1
            db.conn.execute("COMMIT")
        except Exception:
            db.conn.execute("ROLLBACK")
            raise
    else:
        print("[reflect] 本次梦未产出有效 lesson,保留现有 active 集(不清空)", flush=True)

    # ── plans:按 text upsert(无唯一约束,手动查重)──
    n_plans = 0
    for pl in raw_plans:
        if not isinstance(pl, dict):
            continue
        text_pl = (pl.get("text") or "").strip()
        if not text_pl:
            continue
        expires_at = pl.get("expires_at")
        tags = pl.get("tags") if isinstance(pl.get("tags"), dict) else {}
        existing = db.conn.execute(
            "SELECT id FROM memory_plans WHERE text=? AND status='active'", (text_pl,)
        ).fetchone()
        if existing:
            db.conn.execute(
                "UPDATE memory_plans SET expires_at=?, tags=? WHERE id=?",
                (expires_at, json.dumps(tags, ensure_ascii=False), existing["id"]),
            )
        else:
            db.conn.execute(
                """INSERT INTO memory_plans(text,tags,status,created,expires_at)
                   VALUES(?,?, 'active', ?, ?)""",
                (text_pl, json.dumps(tags, ensure_ascii=False), ts, expires_at),
            )
        n_plans += 1

    db.close()
    _write_digest(today, raw_lessons, raw_plans)
    return {"lessons": n_lessons, "plans": n_plans, "parsed": True}


# ─────────────────────────────────────────────────────────────────────────
# 3. decay() —— 纯代码:时间衰减 + 过期
# ─────────────────────────────────────────────────────────────────────────
def decay(now_et: datetime | None = None) -> dict:
    """confidence 时间衰减,plans 过期。纯代码,无 LLM。

    每个 active lesson:以 last_confirmed(无则 created)为基准,过期周数
    w = floor(stale_days / 7),confidence *= 0.9**w。低于 0.2 → archived。
    plans:expires_at < 今日 ET → expired。
    返回 {'decayed': n, 'archived': n, 'expired': n}。
    """
    db = DB(role="reflect")
    ref = datetime.now(timezone.utc)
    today = _today_et(now_et)

    decayed = archived = expired = 0
    rows = db.conn.execute(
        "SELECT id,confidence,created,last_confirmed FROM memory_lessons WHERE status='active'"
    ).fetchall()
    for r in rows:
        base = _parse_utc(r["last_confirmed"]) or _parse_utc(r["created"])
        if base is None:
            continue
        stale_days = (ref - base).total_seconds() / 86400.0
        weeks = int(stale_days // DECAY_STALE_DAYS)
        if weeks <= 0:
            continue
        new_conf = float(r["confidence"]) * (DECAY_FACTOR ** weeks)
        if new_conf < ARCHIVE_BELOW:
            db.conn.execute(
                "UPDATE memory_lessons SET confidence=?, status='archived' WHERE id=?",
                (round(new_conf, 4), r["id"]),
            )
            archived += 1
        else:
            db.conn.execute(
                "UPDATE memory_lessons SET confidence=? WHERE id=?",
                (round(new_conf, 4), r["id"]),
            )
            decayed += 1

    # plans 过期:expires_at < today(字符串日期比较,'YYYY-MM-DD' 字典序即时序)
    prows = db.conn.execute(
        "SELECT id,expires_at FROM memory_plans WHERE status='active'"
    ).fetchall()
    for r in prows:
        exp = (r["expires_at"] or "").strip()
        if exp and exp[:10] < today:
            db.conn.execute("UPDATE memory_plans SET status='expired' WHERE id=?", (r["id"],))
            expired += 1

    db.close()
    return {"decayed": decayed, "archived": archived, "expired": expired}


# ─────────────────────────────────────────────────────────────────────────
# 4. recall() —— 唤醒/定向:注入决策的紧凑记忆
# ─────────────────────────────────────────────────────────────────────────
def recall(now_et: datetime | None = None) -> dict:
    """醒来。先在时间里定位(time_anchor),再取 active lessons + 非过期 plans + 头条 stats。

    返回的 dict 小而紧凑(会注入每次决策 prompt)。
    """
    db = DB(role="reflect")
    et = now_et or datetime.now(ET)
    today = et.strftime("%Y-%m-%d")
    ref_utc = et.astimezone(timezone.utc)

    # ── time_anchor:时间纵深感 ──
    last_dream_date = db.kv_get("last_dream_date")
    # last_dream_date 是 'YYYY-MM-DD'(ET);转成当日 00:00 ET 估算 gap
    last_dream_dt = None
    if last_dream_date:
        try:
            last_dream_dt = datetime.strptime(last_dream_date, "%Y-%m-%d").replace(tzinfo=ET)
        except ValueError:
            last_dream_dt = None
    last_user = _parse_utc(db.kv_get("last_user_seen"))
    last_trade_row = db.conn.execute(
        "SELECT ts FROM trades ORDER BY id DESC LIMIT 1"
    ).fetchone()
    last_trade = _parse_utc(last_trade_row["ts"]) if last_trade_row else None

    time_anchor = {
        "now_et": et.isoformat(),
        "today_et": today,
        "since_last_dream": _human_gap(last_dream_dt, ref_utc) if last_dream_dt else "never",
        "since_last_user": _human_gap(last_user, ref_utc),
        "since_last_trade": _human_gap(last_trade, ref_utc),
    }

    # ── lessons:active,按 confidence desc,capped ──
    lrows = db.conn.execute(
        """SELECT text,confidence,evidence FROM memory_lessons
           WHERE status='active' ORDER BY confidence DESC LIMIT ?""",
        (RECALL_LESSONS_CAP,),
    ).fetchall()
    lessons = []
    for r in lrows:
        try:
            ev = json.loads(r["evidence"]) if r["evidence"] else []
        except (ValueError, TypeError):
            ev = []
        n = len(ev) if isinstance(ev, list) else 0
        lessons.append({
            "text": r["text"],
            "confidence": round(float(r["confidence"]), 2),
            "n": n,
        })

    # ── plans:active 且 非过期(expires_at >= today,或无到期日)──
    prows = db.conn.execute(
        "SELECT text,expires_at,tags FROM memory_plans WHERE status='active' ORDER BY expires_at"
    ).fetchall()
    plans = []
    for r in prows:
        exp = (r["expires_at"] or "").strip()
        if exp and exp[:10] < today:
            continue  # 过期的不召回(即便 decay 还没跑)
        try:
            tags = json.loads(r["tags"]) if r["tags"] else {}
        except (ValueError, TypeError):
            tags = {}
        plans.append({"text": r["text"], "expires_at": exp or None, "tags": tags})

    # ── stats:头条 agg(按来源汇总胜率)──
    srows = db.conn.execute(
        """SELECT setup AS source,
                  SUM(trips) AS trips, SUM(wins) AS wins, SUM(losses) AS losses
           FROM agg GROUP BY setup ORDER BY trips DESC"""
    ).fetchall()
    stats = []
    for r in srows:
        trips = r["trips"] or 0
        stats.append({
            "source": r["source"],
            "n": trips,
            "win_rate": round(r["wins"] / trips, 3) if trips else None,
        })

    # ── self_assessment:诚实性层(3b)—— 我的真实战绩,纯代码算,绝不让
    # LLM 替我编 —— 只看 strategy 归因(NULL 向前兼容当 strategy),排除
    # manual/test/outage-degraded ──
    arows = db.conn.execute(
        """SELECT pnl FROM trades
           WHERE (attribution IS NULL OR attribution = 'strategy') AND pnl IS NOT NULL"""
    ).fetchall()
    n_sa = len(arows)
    wins_sa = sum(1 for r in arows if r["pnl"] > 0)
    win_rate_sa = round(wins_sa / n_sa, 3) if n_sa else None
    net_pnl_sa = round(sum(r["pnl"] for r in arows), 2)
    self_assessment = {"n": n_sa, "win_rate": win_rate_sa, "net_pnl": net_pnl_sa}
    if n_sa < 30:
        pct = round(win_rate_sa * 100, 1) if win_rate_sa is not None else 0.0
        self_assessment["note"] = (
            f"{n_sa}-trade sample, win {pct}%, net {net_pnl_sa} — no proven edge yet; "
            f"treat your own confidence as uncalibrated"
        )
    else:
        try:
            from agent import postmortem
            self_assessment["confidence_buckets"] = postmortem.stats().get("confidence_buckets")
        except Exception:
            pass

    db.close()
    return {"time_anchor": time_anchor, "lessons": lessons, "plans": plans, "stats": stats,
            "self_assessment": self_assessment}


# ─────────────────────────────────────────────────────────────────────────
# 5. 调度:should_dream / run_once / run_forever
# ─────────────────────────────────────────────────────────────────────────
def should_dream(now_et: datetime | None = None) -> bool:
    """睡眠窗口(market Closed)且今天还没做过梦 → True。"""
    if current_session() != "Closed":
        return False
    today = _today_et(now_et)
    db = DB(role="reflect")
    last = db.kv_get("last_dream_date")
    db.close()
    return last != today


def run_once(force: bool = False, now_et: datetime | None = None) -> str:
    """一次反思周期。force=True 则无视 session 强制做梦。"""
    if not (force or should_dream(now_et)):
        return "not sleeping / already dreamed today"

    db = DB(role="reflect")
    db.beat(process="reflect")
    db.close()

    n_agg = aggregate()
    d = decay(now_et)

    # 逐笔复盘(postmortem):必须在 synthesize_lessons() 之前跑完,这样
    # build_dream_prompt 才能把新鲜的 trade_reviews 一并喂给做梦 LLM。
    # 复盘层的任何异常都不该拖垮做梦 —— 打日志、跳过,继续往下走。
    review_lines: list[str] = []
    try:
        from agent import postmortem
        review_lines = postmortem.review_new(limit=10)
        for line in review_lines:
            print(f"[reflect] postmortem: {line}", flush=True)
    except Exception as e:
        print(f"[reflect] postmortem error: {e!r}", file=sys.stderr, flush=True)

    syn = synthesize_lessons(now_et)

    # 把新平仓的成交嵌入语义记忆(Ollama 没起时返回 0,优雅降级)
    try:
        from agent import memory_store
        n_vec = memory_store.index_new_trades()
    except Exception as e:
        n_vec = 0
        print(f"[reflect] memory index skipped: {e}", file=__import__("sys").stderr)

    today = _today_et(now_et)
    db = DB(role="reflect")
    db.kv_set("last_dream_date", today)
    db.close()

    return (f"dreamed {today}: agg={n_agg} rows, "
            f"decay(decayed={d['decayed']},archived={d['archived']},expired={d['expired']}), "
            f"reviews={len(review_lines)}, "
            f"lessons={syn['lessons']} plans={syn['plans']} parsed={syn['parsed']}, "
            f"vectors+={n_vec}")


def run_forever() -> None:
    """每 ~600s 检查一次是否该做梦。"""
    while True:
        try:
            print(run_once(), flush=True)
        except Exception as e:  # 反思崩溃不应拖垮系统
            print(f"reflect error: {e!r}", flush=True)
        time.sleep(600)


if __name__ == "__main__":
    if "--recall" in sys.argv:
        print(json.dumps(recall(), ensure_ascii=False, indent=2))
    elif "--force" in sys.argv:
        print(run_once(force=True))
    elif "--once" in sys.argv:
        print(run_once())
    else:
        run_forever()
