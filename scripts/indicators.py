#!/usr/bin/env python3
"""
Technical indicator calculator for the scalper bot.
Pure Python, no dependencies. Reads kline JSON from stdin, outputs indicators + score.

Usage:
    echo '[{"open":110,"high":112,"low":109,"close":111,"volume":1000}, ...]' | python indicators.py
    python indicators.py --symbol NVDA.US < klines.json
"""

import json
import sys


def ema(values, period):
    """Exponential Moving Average."""
    if len(values) < period:
        return [None] * len(values)
    k = 2 / (period + 1)
    result = [None] * (period - 1)
    result.append(sum(values[:period]) / period)
    for i in range(period, len(values)):
        result.append(values[i] * k + result[-1] * (1 - k))
    return result


def sma(values, period):
    """Simple Moving Average."""
    result = [None] * (period - 1)
    for i in range(period - 1, len(values)):
        result.append(sum(values[i - period + 1 : i + 1]) / period)
    return result


def calc_vwap(highs, lows, closes, volumes):
    """Volume Weighted Average Price (cumulative)."""
    cum_tp_vol = 0
    cum_vol = 0
    vwaps = []
    for h, l, c, v in zip(highs, lows, closes, volumes):
        tp = (h + l + c) / 3
        cum_tp_vol += tp * v
        cum_vol += v
        vwaps.append(cum_tp_vol / cum_vol if cum_vol > 0 else 0)
    return vwaps


def calc_rsi(closes, period=14):
    """Relative Strength Index."""
    if len(closes) < period + 1:
        return [None] * len(closes)

    deltas = [closes[i] - closes[i - 1] for i in range(1, len(closes))]
    gains = [max(d, 0) for d in deltas]
    losses = [abs(min(d, 0)) for d in deltas]

    result = [None] * period
    avg_gain = sum(gains[:period]) / period
    avg_loss = sum(losses[:period]) / period

    if avg_loss == 0:
        result.append(100.0)
    else:
        rs = avg_gain / avg_loss
        result.append(100 - (100 / (1 + rs)))

    for i in range(period, len(deltas)):
        avg_gain = (avg_gain * (period - 1) + gains[i]) / period
        avg_loss = (avg_loss * (period - 1) + losses[i]) / period
        if avg_loss == 0:
            result.append(100.0)
        else:
            rs = avg_gain / avg_loss
            result.append(100 - (100 / (1 + rs)))

    return result


def calc_macd(closes, fast=12, slow=26, signal_period=9):
    """MACD: line, signal, histogram."""
    ema_fast = ema(closes, fast)
    ema_slow = ema(closes, slow)

    macd_line = []
    for f, s in zip(ema_fast, ema_slow):
        if f is not None and s is not None:
            macd_line.append(f - s)
        else:
            macd_line.append(None)

    valid_macd = [v for v in macd_line if v is not None]
    signal_raw = ema(valid_macd, signal_period) if len(valid_macd) >= signal_period else [None] * len(valid_macd)
    signal_line = [None] * (len(macd_line) - len(signal_raw)) + signal_raw

    histogram = []
    for m, s in zip(macd_line, signal_line):
        if m is not None and s is not None:
            histogram.append(m - s)
        else:
            histogram.append(None)

    return macd_line, signal_line, histogram


def calc_wr(highs, lows, closes, period=14):
    """Williams %R, positive 0-100 convention (CN charting style):
    WR = (HH - C) / (HH - LL) * 100.  >80 = oversold, <20 = overbought."""
    result = [None] * len(closes)
    for i in range(period - 1, len(closes)):
        hh = max(highs[i - period + 1: i + 1])
        ll = min(lows[i - period + 1: i + 1])
        result[i] = 50.0 if hh == ll else (hh - closes[i]) / (hh - ll) * 100.0
    return result


def calc_mtm(closes, period=12, ma_period=6):
    """Momentum: MTM = close - close[period ago]; MTMMA = SMA(MTM, ma_period)."""
    mtm = [None] * min(period, len(closes)) + \
          [closes[i] - closes[i - period] for i in range(period, len(closes))]
    valid = [v for v in mtm if v is not None]
    ma_valid = sma(valid, ma_period) if len(valid) >= ma_period else [None] * len(valid)
    mtm_ma = [None] * (len(mtm) - len(ma_valid)) + ma_valid
    return mtm, mtm_ma


def _ts_epoch(k):
    """kline timestamp → epoch seconds. Accepts int/float epoch or ISO8601 string."""
    t = k.get("timestamp") or k.get("ts") or k.get("time")
    if t is None:
        return None
    if isinstance(t, (int, float)):
        return float(t)
    from datetime import datetime
    try:
        return datetime.fromisoformat(str(t).replace("Z", "+00:00")).timestamp()
    except ValueError:
        return None


def aggregate_4h(klines_1h):
    """Aggregate 1h bars into 4h bars by wall-clock 4h buckets (epoch // 14400).
    Falls back to fixed groups of 4 when timestamps are absent."""
    buckets, order = {}, []
    for i, k in enumerate(klines_1h):
        ts = _ts_epoch(k)
        key = int(ts // 14400) if ts is not None else i // 4
        if key not in buckets:
            buckets[key] = {"open": float(k["open"]), "high": float(k["high"]),
                            "low": float(k["low"]), "close": float(k["close"]),
                            "volume": int(k["volume"])}
            order.append(key)
        else:
            b = buckets[key]
            b["high"] = max(b["high"], float(k["high"]))
            b["low"] = min(b["low"], float(k["low"]))
            b["close"] = float(k["close"])
            b["volume"] += int(k["volume"])
    return [buckets[k] for k in order]


def h4_snapshot(klines_1h, lookback=12):
    """4h 趋势 + 三指标共振快照(MACD / WR / MTM 播放手册的机器可读输入)。

    Playbook: trend filter = close above rising SMA20 on 4h; entry needs all three
    triggers to have fired within the last LOOKBACK(12) 4h bars (~2 交易日确认窗)
    with their bullish state still holding NOW:
      macd.turned_bull  — histogram crossed >0 within lookback AND is >0 now
      wr.recovering     — WR reached >=90 (oversold) within lookback AND is <80 now
      mtm.golden_cross  — MTM crossed above its MA within lookback AND is above now
    `confluence` = trend_ok AND all three.

    这套参数不是拍的:backtest/playbook_test.py 在 SNDK/MU 2025-09→2026-08 的
    1h 数据上扫过 {同步触发, 4, 8, 12} —— 严格同步触发一年仅 0-1 个信号(不可用);
    lookback=12 在两个标的上均为正(合计 +$2380 / 73 笔 / 胜率 ~41%)。改这里的
    语义前先重跑那个脚本。
    Returns None when there are not enough 4h bars (need ~36 for MACD warm-up).
    """
    LOOKBACK = lookback
    bars = aggregate_4h(klines_1h)
    if len(bars) < 36:
        return None
    highs = [b["high"] for b in bars]
    lows = [b["low"] for b in bars]
    closes = [b["close"] for b in bars]

    sma20 = sma(closes, 20)
    macd_line, _sig, hist = calc_macd(closes)
    wr = calc_wr(highs, lows, closes)
    mtm, mtm_ma = calc_mtm(closes)

    price, ma20 = closes[-1], sma20[-1]
    ma20_prev = sma20[-4] if len(sma20) >= 4 and sma20[-4] is not None else ma20
    trend_ok = ma20 is not None and price > ma20 and ma20 >= ma20_prev

    def _crossed_up(series, ref=None, lookback=LOOKBACK):
        """series crossed above ref (another series, or 0) within last `lookback` bars."""
        for j in range(1, lookback + 1):
            if j + 1 > len(series) - 1:
                break
            cur, prev = series[-j], series[-j - 1]
            rc = 0 if ref is None else ref[-j]
            rp = 0 if ref is None else ref[-j - 1]
            if None in (cur, prev, rc, rp):
                continue
            if prev <= rp and cur > rc:
                return True
        return False

    macd_turned_bull = (hist[-1] is not None and hist[-1] > 0 and _crossed_up(hist))
    wr_recent_max = max((v for v in wr[-(LOOKBACK + 5):-1] if v is not None), default=None)
    wr_recovering = (wr[-1] is not None and wr_recent_max is not None
                     and wr_recent_max >= 90 and wr[-1] < 80)
    mtm_golden = (mtm[-1] is not None and mtm_ma[-1] is not None
                  and mtm[-1] > mtm_ma[-1] and _crossed_up(mtm, ref=mtm_ma))

    r = lambda v, n=4: round(v, n) if v is not None else None
    return {
        "bars": len(bars),
        "close": r(price),
        "sma20": r(ma20),
        "trend_ok": trend_ok,
        "macd": {"dif": r(macd_line[-1], 6), "hist": r(hist[-1], 6),
                 "hist_prev": r(hist[-2], 6) if len(hist) > 1 else None,
                 "above_zero": macd_line[-1] is not None and macd_line[-1] > 0,
                 "turned_bull": macd_turned_bull},
        "wr": {"value": r(wr[-1], 2), "recent_max": r(wr_recent_max, 2),
               "recovering": wr_recovering},
        "mtm": {"value": r(mtm[-1]), "ma": r(mtm_ma[-1]), "golden_cross": mtm_golden},
        "confluence": bool(trend_ok and macd_turned_bull and wr_recovering and mtm_golden),
    }


def calc_atr(highs, lows, closes, period=14):
    """Average True Range."""
    if len(closes) < 2:
        return [None] * len(closes)
    true_ranges = [highs[0] - lows[0]]
    for i in range(1, len(closes)):
        tr = max(highs[i] - lows[i], abs(highs[i] - closes[i - 1]), abs(lows[i] - closes[i - 1]))
        true_ranges.append(tr)
    return sma(true_ranges, period)


def score_entry(price, vwap, ema20, macd_val, macd_sig, macd_hist, macd_hist_prev, rsi, vol_ratio, atr, candle_body, upper_shadow):
    """Score long entry opportunity, max 8."""
    score = 0
    reasons = []

    if price > vwap:
        score += 1
        reasons.append("above_vwap")

    if price > ema20:
        score += 1
        reasons.append("above_ema20")

    if macd_val > 0 or (macd_val is not None and macd_sig is not None and macd_val > macd_sig and macd_hist > 0):
        score += 1
        reasons.append("macd_bullish")

    if 30 <= rsi <= 65:
        score += 1
        reasons.append("rsi_in_range")

    if vol_ratio > 1.5:
        score += 1
        reasons.append("high_volume")

    if macd_hist is not None and macd_hist_prev is not None and macd_hist > 0 and macd_hist > macd_hist_prev:
        score += 1
        reasons.append("macd_accelerating")

    if candle_body > upper_shadow:
        score += 1
        reasons.append("clean_candle")

    if atr is not None and atr > 0 and abs(price - ema20) < 1.5 * atr:
        score += 1
        reasons.append("not_extended")

    return score, reasons


def analyze(klines, symbol="", h4_lookback=12):
    """Run full analysis on kline data. Returns indicators + entry score.

    1h indicators are computed on the LAST 50 bars regardless of input length —
    callers may pass a longer history (e.g. 240 bars) purely to feed the 4h
    aggregation; the cumulative VWAP and score semantics must not silently
    stretch from a 50-bar window to a monthly window because of that.
    """
    if len(klines) < 30:
        return {"symbol": symbol, "error": f"Need >=30 bars, got {len(klines)}", "score": 0, "signal": "WAIT"}

    # 4h 播放手册快照,仅当调用方要求(lookback>0)。非手册标的传 0 → h4=None,
    # 其 payload 与手册引入之前完全等价(策略隔离:手册不影响旧 universe)。
    h4 = h4_snapshot(klines, lookback=h4_lookback) if h4_lookback else None
    klines = klines[-50:]

    opens = [float(k["open"]) for k in klines]
    highs = [float(k["high"]) for k in klines]
    lows = [float(k["low"]) for k in klines]
    closes = [float(k["close"]) for k in klines]
    volumes = [int(k["volume"]) for k in klines]

    vwaps = calc_vwap(highs, lows, closes, volumes)
    ema20s = ema(closes, 20)
    macd_line, signal_line, histogram = calc_macd(closes)
    rsis = calc_rsi(closes)
    atrs = calc_atr(highs, lows, closes)
    vol_sma20 = sma(volumes, 20)

    price = closes[-1]
    vwap_val = vwaps[-1]
    ema20_val = ema20s[-1] if ema20s[-1] is not None else price
    macd_val = macd_line[-1] if macd_line[-1] is not None else 0
    macd_sig = signal_line[-1] if signal_line[-1] is not None else 0
    macd_hist = histogram[-1] if histogram[-1] is not None else 0
    macd_hist_prev = histogram[-2] if len(histogram) > 1 and histogram[-2] is not None else 0
    rsi_val = rsis[-1] if rsis[-1] is not None else 50
    atr_val = atrs[-1]
    vol_avg = vol_sma20[-1] if vol_sma20[-1] is not None else 1
    vol_ratio = volumes[-1] / vol_avg if vol_avg > 0 else 1

    candle_body = abs(closes[-1] - opens[-1])
    upper_shadow = highs[-1] - max(opens[-1], closes[-1])

    # MACD cross detection
    cross = "none"
    if len(macd_line) >= 2 and macd_line[-2] is not None and signal_line[-2] is not None:
        prev_diff = macd_line[-2] - signal_line[-2]
        curr_diff = macd_val - macd_sig
        if prev_diff <= 0 and curr_diff > 0:
            cross = "bullish"
        elif prev_diff >= 0 and curr_diff < 0:
            cross = "bearish"

    score, reasons = score_entry(
        price, vwap_val, ema20_val,
        macd_val, macd_sig, macd_hist, macd_hist_prev,
        rsi_val, vol_ratio, atr_val,
        candle_body, upper_shadow,
    )

    return {
        "symbol": symbol,
        "price": round(price, 4),
        "vwap": round(vwap_val, 4),
        "ema20": round(ema20_val, 4),
        "macd": {
            "value": round(macd_val, 6),
            "signal": round(macd_sig, 6),
            "histogram": round(macd_hist, 6),
            "cross": cross,
        },
        "rsi": round(rsi_val, 2),
        "volume_ratio": round(vol_ratio, 2),
        "atr": round(atr_val, 4) if atr_val is not None else None,
        "score": score,
        "max_score": 8,
        "reasons": reasons,
        "signal": "BUY" if score >= 5 else "WAIT",
        "h4": h4,
    }


if __name__ == "__main__":
    symbol = ""
    h4_lookback = 12
    args = sys.argv[1:]
    for i, arg in enumerate(args):
        if arg == "--symbol" and i + 1 < len(args):
            symbol = args[i + 1]
        if arg == "--h4-lookback" and i + 1 < len(args):
            h4_lookback = int(args[i + 1])

    try:
        data = json.loads(sys.stdin.read())
    except json.JSONDecodeError as e:
        print(json.dumps({"error": f"Invalid JSON: {e}"}))
        sys.exit(1)

    result = analyze(data, symbol, h4_lookback=h4_lookback)
    print(json.dumps(result, ensure_ascii=False, indent=2))
