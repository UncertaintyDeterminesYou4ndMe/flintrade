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


def analyze(klines, symbol=""):
    """Run full analysis on kline data. Returns indicators + entry score."""
    if len(klines) < 30:
        return {"symbol": symbol, "error": f"Need >=30 bars, got {len(klines)}", "score": 0, "signal": "WAIT"}

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
    }


if __name__ == "__main__":
    symbol = ""
    args = sys.argv[1:]
    for i, arg in enumerate(args):
        if arg == "--symbol" and i + 1 < len(args):
            symbol = args[i + 1]

    try:
        data = json.loads(sys.stdin.read())
    except json.JSONDecodeError as e:
        print(json.dumps({"error": f"Invalid JSON: {e}"}))
        sys.exit(1)

    result = analyze(data, symbol)
    print(json.dumps(result, ensure_ascii=False, indent=2))
