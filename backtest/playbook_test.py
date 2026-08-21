#!/usr/bin/env python3
"""
4h 播放手册回测 —— 验证「趋势过滤 + MACD/WR/MTM 三重共振」在 SNDK/MU 上的历史表现。

规则(与 live 完全同源:直接调 scripts/indicators.h4_snapshot):
  入场  h4.confluence == True(4h 站稳上升的 SMA20 + MACD 翻红 + WR 超卖回升 + MTM 金叉)
        → 下一根 1h 开盘价买入,风险定额 2% × $10K / 3% 止损距离
  离场  T1 = entry×1.05 平半仓;T2 = entry×1.08 清仓;stop = entry×0.97
        同一根 K 线内 stop 与 target 都触及时,保守假设 stop 先成交
  佣金  $0.02/股(与 live 一致)

用法: python3 backtest/playbook_test.py [--refresh]
"""
from __future__ import annotations

import json
import sys
from datetime import date, timedelta
from pathlib import Path

BT_DIR = Path(__file__).resolve().parent
sys.path.insert(0, str(BT_DIR))
sys.path.insert(0, str(BT_DIR.parent / "scripts"))

from indicators import h4_snapshot, aggregate_4h, calc_macd, calc_wr, calc_mtm, sma  # noqa: E402
from download_data import fetch_klines  # noqa: E402


def confluence_within(bars_1h: list[dict], lookback: int) -> bool:
    """放宽版共振:三个触发各自发生在最近 lookback 根 4h 内,且当前状态仍成立
    (MACD hist>0、WR<80、MTM>MA),外加趋势过滤。lookback=2 ≈ 严格同步版。"""
    bars = aggregate_4h(bars_1h)
    if len(bars) < 36:
        return False
    highs = [b["high"] for b in bars]
    lows = [b["low"] for b in bars]
    closes = [b["close"] for b in bars]
    ma20 = sma(closes, 20)
    if ma20[-1] is None or closes[-1] <= ma20[-1] or ma20[-1] < (ma20[-4] or ma20[-1]):
        return False
    _line, _sig, hist = calc_macd(closes)
    wr = calc_wr(highs, lows, closes)
    mtm, mtm_ma = calc_mtm(closes)

    def crossed_up_within(series, ref, k):
        for j in range(1, k + 1):
            if j + 1 >= len(series):
                break
            cur, prev = series[-j], series[-j - 1]
            rc = 0 if ref is None else ref[-j]
            rp = 0 if ref is None else ref[-j - 1]
            if None in (cur, prev, rc, rp):
                continue
            if prev <= rp and cur > rc:
                return True
        return False

    # 当前状态
    if hist[-1] is None or hist[-1] <= 0:
        return False
    if wr[-1] is None or wr[-1] >= 80:
        return False
    if mtm[-1] is None or mtm_ma[-1] is None or mtm[-1] <= mtm_ma[-1]:
        return False
    # 近期触发
    macd_trig = crossed_up_within(hist, None, lookback)
    wr_trig = any(v is not None and v >= 90 for v in wr[-(lookback + 5):-1]) and wr[-1] < 80
    mtm_trig = crossed_up_within(mtm, mtm_ma, lookback)
    return macd_trig and wr_trig and mtm_trig

DATA_DIR = BT_DIR / "data_playbook"
START = "2025-09-01"

# 参数与标的从 strategies.toml 读 —— live 生效的和这里验证的永远是同一份数字。
import tomllib  # noqa: E402
_PB = tomllib.load(open(BT_DIR.parent / "agent" / "config" / "strategies.toml", "rb")
                   )["playbooks"]["h4_confluence"]
SYMBOLS = _PB["symbols"]
LOOKBACK = int(_PB["lookback"])
STOP_PCT, T1_PCT, T2_PCT = _PB["stop_pct"] / 100, _PB["t1_pct"] / 100, _PB["t2_pct"] / 100
EQUITY, RISK_PCT = 10_000.0, 0.02   # 与 risk.toml 的 sleeve/max_risk_pct 一致
COMM = 0.02
WARMUP = 200          # 首个可评估信号前的最少 1h 根数
WINDOW = 720          # 每次评估回看的 1h 根数(≈ live 的 240 根之上放宽,喂饱 4h MACD)


def load_bars(symbol: str, refresh: bool) -> list[dict]:
    DATA_DIR.mkdir(exist_ok=True)
    f = DATA_DIR / f"{symbol.replace('.', '_')}.json"
    if f.exists() and not refresh:
        return json.loads(f.read_text())
    bars, cur, today = [], date.fromisoformat(START), date.today()
    while cur < today:
        end = min(cur + timedelta(days=45), today)
        bars += fetch_klines(symbol, cur.isoformat(), end.isoformat(), period="1h")
        cur = end + timedelta(days=1)
    # 去重(分块边界可能重叠)并按时间排序
    seen, out = set(), []
    for b in bars:
        t = b.get("time")
        if t not in seen:
            seen.add(t)
            out.append(b)
    out.sort(key=lambda b: b["time"])
    f.write_text(json.dumps(out))
    return out


def run(symbol: str, bars: list[dict], signal_fn=None) -> dict:
    trades = []           # 每笔: dict(entry, exit_legs=[(qty,price,tag)], pnl)
    pos = None            # dict(qty, entry, stop, t1, t2, t1_done, opened_i)
    signals = 0

    for i in range(WARMUP, len(bars) - 1):
        px_next_open = float(bars[i + 1]["open"])
        if pos:
            hi, lo = float(bars[i + 1]["high"]), float(bars[i + 1]["low"])
            legs = []
            # 保守顺序:先 stop 后 target
            if lo <= pos["stop"]:
                legs.append((pos["qty"], pos["stop"], "stop"))
            else:
                if not pos["t1_done"] and hi >= pos["t1"]:
                    half = pos["qty"] // 2 or pos["qty"]
                    legs.append((half, pos["t1"], "T1"))
                    pos["qty"] -= half
                    pos["t1_done"] = True
                if pos["qty"] and pos["t1_done"] and hi >= pos["t2"]:
                    legs.append((pos["qty"], pos["t2"], "T2"))
                    pos["qty"] = 0
            for qty, px, tag in legs:
                pnl = (px - pos["entry"]) * qty - COMM * qty
                pos["legs"].append((qty, px, tag, round(pnl, 2)))
            if legs and (pos["qty"] == 0 or legs[0][2] == "stop"):
                trades.append({
                    "entry_i": pos["opened_i"], "exit_i": i + 1,
                    "entry": pos["entry"], "legs": pos["legs"],
                    "pnl": round(sum(l[3] for l in pos["legs"]) - COMM * pos["qty0"], 2),
                    "hold_h": i + 1 - pos["opened_i"],
                })
                pos = None
            continue

        window = bars[max(0, i - WINDOW):i + 1]
        if signal_fn is not None:
            if not signal_fn(window):
                continue
        else:
            snap = h4_snapshot(window, lookback=LOOKBACK)
            if not snap or not snap["confluence"]:
                continue
        signals += 1
        qty = int(EQUITY * RISK_PCT / (STOP_PCT * px_next_open))
        if qty < 1:
            continue
        pos = {"qty": qty, "qty0": qty, "entry": px_next_open,
               "stop": px_next_open * (1 - STOP_PCT),
               "t1": px_next_open * (1 + T1_PCT),
               "t2": px_next_open * (1 + T2_PCT),
               "t1_done": False, "opened_i": i + 1, "legs": []}

    if pos:  # 期末未平:按最后收盘 mark
        px = float(bars[-1]["close"])
        pnl = (px - pos["entry"]) * pos["qty"] - COMM * pos["qty"]
        pos["legs"].append((pos["qty"], px, "eod", round(pnl, 2)))
        trades.append({"entry_i": pos["opened_i"], "exit_i": len(bars) - 1,
                       "entry": pos["entry"], "legs": pos["legs"],
                       "pnl": round(sum(l[3] for l in pos["legs"]) - COMM * pos["qty0"], 2),
                       "hold_h": len(bars) - 1 - pos["opened_i"]})

    wins = [t for t in trades if t["pnl"] > 0]
    bh_qty = int(EQUITY * RISK_PCT / (STOP_PCT * float(bars[WARMUP]["close"])))
    buyhold = (float(bars[-1]["close"]) - float(bars[WARMUP]["close"])) * bh_qty
    return {"symbol": symbol, "bars": len(bars), "signals": signals,
            "trades": trades, "n": len(trades), "wins": len(wins),
            "winrate": round(len(wins) / len(trades) * 100, 1) if trades else None,
            "pnl": round(sum(t["pnl"] for t in trades), 2),
            "buyhold_same_size": round(buyhold, 2)}


def main():
    refresh = "--refresh" in sys.argv
    variants = [("strict(同步触发)", None)] + \
               [(f"lookback={L}根4h", (lambda L: lambda w: confluence_within(w, L))(L))
                for L in (4, 8, 12)]
    for sym in SYMBOLS:
        bars = load_bars(sym, refresh)
        if len(bars) < WARMUP + 50:
            print(f"{sym}: 数据不足({len(bars)} bars),跳过")
            continue
        print(f"\n═══ {sym} ═══  {len(bars)} bars  ({bars[0]['time'][:10]} → {bars[-1]['time'][:10]})")
        for name, fn in variants:
            r = run(sym, bars, signal_fn=fn)
            print(f"  [{name:16}] 信号 {r['signals']:3} → 成交 {r['n']:3} 笔 | "
                  f"胜率 {r['winrate']}% | pnl ${r['pnl']}")
            if "--trades" in sys.argv:
                for t in r["trades"]:
                    legs = ", ".join(f"{tag}:{qty}@{px:.2f}({pnl:+.2f})"
                                     for qty, px, tag, pnl in t["legs"])
                    print(f"      entry {t['entry']:.2f} @bar{t['entry_i']} "
                          f"hold {t['hold_h']}h → {legs}  Σ{t['pnl']:+.2f}")
        bh = run(sym, bars, signal_fn=lambda w: False)["buyhold_same_size"]
        print(f"  [同等仓位买入持有 ] ${bh}")


if __name__ == "__main__":
    main()
