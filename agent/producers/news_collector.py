"""
资讯采集生产者(纯数据摄取,Arena 模式)。

职责单一:轮询交易标的的资讯(longbridge news CLI),把新资讯写成 signals,
并对命中催化剂关键词的条目额外写 events(kind="news_catalyst")。
绝不下单、绝不调用 LLM —— 只读市场、纯 stdlib。它同时喂养事件交易回路与 prompt 上下文。

底层 CLI(已实测确认):
    longbridge news <SYMBOL> --count N --format json
    → [{"id","title","published_at","likes_count","comments_count","url"}, ...]
detail/search 子命令存在但本生产者不依赖。

去重:每只标的在 kv 维护一个滚动「已见 id」集合(key="news_seen_<SYMBOL>"),
仅对真正的新条目产出 signal。CLI 形态未知或报错时优雅降级(日志 + 跳过),绝不崩。

从项目根运行:
    python3 -m agent.producers.news_collector --once
    python3 -m agent.producers.news_collector            # run_forever
"""
from __future__ import annotations

import json
import subprocess
import time

from agent.config import load_trading
from agent.db import DB, now

# 每只标的拉取的条数;够覆盖一个轮询周期内的增量即可。
NEWS_COUNT = 20
# CLI 调用超时(秒)。
FETCH_TIMEOUT = 30
# 每只标的「已见 id」滚动窗口上限,防止 kv 无界增长。
SEEN_CAP = 500

# 催化剂关键词(轻量、关键词匹配,无 LLM)。中英混合 —— longbridge 资讯标题常为中文。
# 命中即额外落一条 news_catalyst event。保持精简、含义明确。
CATALYST_KEYWORDS = (
    # 业绩 / 指引
    "earnings", "guidance", "revenue", "profit warning", "outlook",
    "财报", "业绩", "指引", "预期", "盈利预警",
    # 监管 / 法律
    "fda", "sec", "doj", "antitrust", "lawsuit", "subpoena", "investigation",
    "recall", "probe",
    "监管", "诉讼", "调查", "反垄断", "召回", "处罚", "罚款",
    # 评级
    "upgrade", "downgrade", "initiates", "price target", "rating",
    "上调", "下调", "评级", "目标价", "增持", "减持", "买入", "卖出",
    # 并购 / 资本动作
    "merger", "acquisition", "acquire", "buyout", "takeover", "stake",
    "spinoff", "ipo", "buyback", "dividend",
    "并购", "收购", "重组", "回购", "分拆", "分红", "入股",
    # 突发
    "bankruptcy", "default", "resign", "ceo", "halt", "delist",
    "破产", "违约", "辞职", "停牌", "退市",
)


def _matched_keywords(text: str) -> list[str]:
    """返回 text(小写)命中的催化剂关键词列表。空表示未命中。"""
    low = (text or "").lower()
    return [kw for kw in CATALYST_KEYWORDS if kw in low]


def fetch_news(symbol: str) -> list[dict]:
    """
    调 longbridge news 取某标的资讯,解析 JSON 返回规范化 dict 列表。
    防御式:超时/非零退出/非 JSON/形态异常 一律返回 [] 并打日志,绝不抛。
    subprocess.run + timeout,绝不 shell=True,继承环境以复用凭据。
    """
    from agent import lb
    ok, data, raw = lb.run(["news", symbol, "--count", str(NEWS_COUNT), "--format", "json"],
                           timeout=FETCH_TIMEOUT)
    if not ok:
        print(f"[news_collector] fetch {symbol} 失败: {str(raw)[:200]}", flush=True)
        return []
    if data is None:
        return []

    # 形态容忍:期望 list;若是 {"items":[...]} / {"news":[...]} 之类也兼容。
    if isinstance(data, dict):
        for k in ("items", "news", "data", "list"):
            if isinstance(data.get(k), list):
                data = data[k]
                break
        else:
            data = [data]  # 单对象包成单元素列表
    if not isinstance(data, list):
        return []

    out: list[dict] = []
    for it in data:
        if isinstance(it, dict):
            out.append(_normalize(it))
    return out


def _first(d: dict, *keys, default=None):
    """从多个候选键名里取第一个非空值(容忍不同 CLI 字段命名)。"""
    for k in keys:
        v = d.get(k)
        if v not in (None, ""):
            return v
    return default


def _normalize(item: dict) -> dict:
    """规范成统一形状,但完整保留原始字段(payload 里下游可用)。"""
    nid = _first(item, "id", "news_id", "article_id", "uuid")
    title = _first(item, "title", "headline", "name", default="")
    published = _first(item, "published_at", "publish_time", "time", "date", "ts")
    url = _first(item, "url", "link", "href")
    norm = dict(item)  # 原样保留所有字段
    norm["_id"] = str(nid) if nid is not None else None
    norm["_title"] = title
    norm["_published_at"] = published
    norm["_url"] = url
    return norm


def _item_key(item: dict) -> str | None:
    """去重键:优先 id,缺失时退化为 title+time 的稳定串。"""
    if item.get("_id"):
        return item["_id"]
    title = item.get("_title") or ""
    pub = item.get("_published_at") or ""
    if title or pub:
        return f"{title}|{pub}"
    return None


def _load_seen(db: DB, symbol: str) -> list[str]:
    raw = db.kv_get(f"news_seen_{symbol}")
    if not raw:
        return []
    try:
        v = json.loads(raw)
        return v if isinstance(v, list) else []
    except (ValueError, TypeError):
        return []


def _save_seen(db: DB, symbol: str, seen: list[str]):
    # 滚动窗口:只留最近 SEEN_CAP 个。
    trimmed = seen[-SEEN_CAP:]
    db.kv_set(f"news_seen_{symbol}", json.dumps(trimmed, ensure_ascii=False))


def _collect_symbol(db: DB, symbol: str) -> tuple[int, int]:
    """采集单标的,返回 (新增 signal 数, 新增 catalyst event 数)。"""
    items = fetch_news(symbol)
    if not items:
        return 0, 0

    seen = _load_seen(db, symbol)
    seen_set = set(seen)
    new_count = 0
    cat_count = 0

    for item in items:
        key = _item_key(item)
        if key is None:
            continue  # 无法去重的条目跳过,避免重复刷屏
        if key in seen_set:
            continue

        db.add_signal(source="news_collector", symbol=symbol, kind="news", payload=item)
        new_count += 1
        seen_set.add(key)
        seen.append(key)

        kws = _matched_keywords(item.get("_title", ""))
        if kws:
            payload = dict(item)
            payload["catalyst_keywords"] = kws
            db.add_event(
                symbol=symbol,
                kind="news_catalyst",
                title=item.get("_title") or "",
                fires_at=item.get("_published_at") or now(),
                payload=payload,
            )
            cat_count += 1

    if new_count:
        _save_seen(db, symbol, seen)
    return new_count, cat_count


def run_once() -> list[str]:
    """
    一轮采集。心跳 → 逐标的拉取 → 新条目写 signal(命中关键词额外写 event)。
    返回每标的新增数量的日志行列表。单标的异常被隔离,不影响其余标的。
    """
    with DB(role="news") as db:
        return _run_once(db)


def _run_once(db: DB) -> list[str]:
    db.beat(process="news_collector")

    symbols = load_trading()["universe"]["symbols"]
    throttle = load_trading()["cadence"].get("news_symbol_throttle_sec", 0.5)
    log: list[str] = []

    for i, sym in enumerate(symbols):
        try:
            new_count, cat_count = _collect_symbol(db, sym)
            if new_count:
                suffix = f" ({cat_count} catalyst)" if cat_count else ""
                log.append(f"{sym}: +{new_count} news{suffix}")
        except Exception as e:  # 单标的失败隔离
            print(f"[news_collector] {sym} 采集异常: {e!r}", flush=True)
            log.append(f"{sym}: error {e!r}")
        if i < len(symbols) - 1:
            time.sleep(throttle)  # 节流,避开 longbridge 429 限流

    if not log:
        log.append("no new news")
    return log


def run_forever():
    cadence = load_trading()["cadence"]["news_poll_sec"]
    while True:
        try:
            for line in run_once():
                print(line, flush=True)
        except Exception as e:  # 生产者崩溃不应拖垮系统
            print(f"news_collector error: {e!r}", flush=True)
        time.sleep(cadence)


if __name__ == "__main__":
    import sys
    if "--once" in sys.argv:
        for line in run_once():
            print(line)
    else:
        run_forever()
