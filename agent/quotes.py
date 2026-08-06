"""共享实时报价读取 —— daemon 各 loop 的唯一取价入口。

为什么单独一个模块:止损守卫(risk_monitor)和舆情 loop 都要取价,而
`longbridge quote` 的 JSON 形状随版本/会话而变。这里把「防御式挖字段」集中
一处,沿用 reconciler.py 里 _PRICE_KEYS / _EQUITY_KEYS 的同一套做法。

**顶层 last_done 在非常规时段是陈旧的** —— 这是 2026-07-31 AAPL 事故的核心:
夜盘 AAPL 实际已经 311.01,而顶层 last_done 仍停在前一日 regular close 333.43。
所以取价必须先看当前会话对应的子报价(pre/post/overnight),取不到才回退顶层。
拿错了价,止损守卫就会在最需要它的那一刻看不见跌幅。

一次 CLI 调用可查多个标的(与 scripts/collect.sh 的 `lb quote $SYMBOLS` 一致),
避免持仓多时把速率预算打满。
"""
from __future__ import annotations

# quote JSON 里「最新价」可能的键名(各版本不一,防御式逐个试)。
# 实测 longbridge CLI 用的是 `last`,值是字符串("211.940")。
_LAST_KEYS = ("last", "last_done", "last_price", "price", "close")

# 会话 → 该会话报价子对象的候选键名。第一个是实测键名,后面是防版本漂移的别名。
#
# 实测结构(2026-08-05 夜盘 NVDA):
#   {"last": "211.940",                         ← 前一个 regular close,陈旧!
#    "overnight":   {"last": "217.140", ...},   ← 当前真实价
#    "pre_market":  {"last": "211.650", ...},
#    "post_market": {"last": "216.500", ...}}
# 顶层 last 与夜盘实际价差了 5.20 —— 这正是 07-31 那次跳空「在快照里看不见」的
# 机制。键名写错会静默回退到顶层,守卫照跑不报错,只是永远看着昨天的价格。
_SESSION_SUBQUOTE = {
    "Pre":           ("pre_market", "pre_market_quote", "premarket_quote"),
    "Post":          ("post_market", "post_market_quote", "postmarket_quote"),
    "Overnight":     ("overnight", "overnight_quote"),
    "Overnight-Pre": ("overnight", "overnight_quote"),
}


def _to_float(v) -> float | None:
    try:
        f = float(v)
    except (TypeError, ValueError):
        return None
    return f if f > 0 else None  # 0 / 负数视为「无报价」,不是有效价格


def _dig_last(obj) -> float | None:
    """从一个报价 dict 里挖最新价;挖不到返回 None。"""
    if not isinstance(obj, dict):
        return None
    for k in _LAST_KEYS:
        f = _to_float(obj.get(k))
        if f is not None:
            return f
    return None


def _rows(payload) -> list[dict]:
    """把 quote 返回体(list / dict / {data:[...]})normalise 成 dict 列表。"""
    if isinstance(payload, list):
        return [r for r in payload if isinstance(r, dict)]
    if isinstance(payload, dict):
        for key in ("quotes", "data", "items", "list"):
            v = payload.get(key)
            if isinstance(v, list):
                return [r for r in v if isinstance(r, dict)]
        return [payload]
    return []


def quotes(symbols: list[str], *, timeout: int = 20) -> dict[str, dict]:
    """批量取报价,返回 {symbol: quote_dict}。任何失败都返回 {}(不抛)。

    取价失败绝不能拖垮调用方的循环 —— 熔断器宁可这一轮看不见价,也不能死。
    """
    syms = [s for s in dict.fromkeys(symbols) if s]  # 去重保序
    if not syms:
        return {}
    try:
        from agent import lb
        ok, data, _ = lb.run(["quote", *syms, "--format", "json"], timeout=timeout)
        if not ok or data is None:
            return {}
        out: dict[str, dict] = {}
        for row in _rows(data):
            sym = row.get("symbol") or row.get("code")
            if sym:
                out[str(sym)] = row
        # 单标的查询时某些版本不回 symbol 字段 —— 只有一行就直接认领。
        if not out and len(syms) == 1:
            rows = _rows(data)
            if rows:
                out[syms[0]] = rows[0]
        return out
    except Exception:
        return {}


def last_price_from(q: dict, session: str | None = None) -> float | None:
    """从单个报价 dict 里取「当前会话」的最新价。

    先查会话对应的子报价(盘前/盘后/夜盘),再回退顶层 last_done —— 顺序不能
    反,顶层在非常规时段会停在上一个 regular close 上(见模块 docstring)。
    """
    for key in _SESSION_SUBQUOTE.get(session or "", ()):
        f = _dig_last(q.get(key))
        if f is not None:
            return f
    return _dig_last(q)


def last_prices(symbols: list[str], session: str | None = None) -> dict[str, float]:
    """批量取「当前会话」最新价,返回 {symbol: price}。取不到的标的直接缺席。"""
    out: dict[str, float] = {}
    for sym, q in quotes(symbols).items():
        p = last_price_from(q, session)
        if p is not None:
            out[sym] = p
    return out


def last_price(symbol: str, session: str | None = None) -> float | None:
    """单标的最新价;取不到返回 None。"""
    return last_prices([symbol], session).get(symbol)
