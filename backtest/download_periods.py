#!/usr/bin/env python3
"""
Download 1m and 1h klines for period comparison backtest.
1h: one call per symbol (fits in 1000 bar limit).
1m: day-by-day (960 bars/day).
"""

import json
import os
import subprocess
import sys
from datetime import datetime, timedelta

BASE_DIR = os.path.dirname(__file__)

TRADE_SYMBOLS = [
    "AAPL.US", "MSFT.US", "GOOGL.US", "AMZN.US", "NVDA.US",
    "META.US", "TSLA.US", "GLD.US", "UGL.US", "SLV.US", "AGQ.US", "USO.US",
]
MARKET_SYMBOLS = ["QQQ.US", "SPY.US"]
ALL_SYMBOLS = MARKET_SYMBOLS + TRADE_SYMBOLS

START = "2026-02-25"
END = "2026-04-21"


def fetch(symbol, period, start, end):
    cmd = [
        "longbridge", "kline", "history", symbol,
        "--period", period,
        "--start", start, "--end", end,
        "--session", "all", "--format", "json",
    ]
    try:
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=30)
        if r.returncode == 0 and r.stdout.strip():
            data = json.loads(r.stdout)
            data.reverse()  # oldest first
            return data
    except Exception as e:
        print(f"  ERROR {symbol} {period} {start}~{end}: {e}", file=sys.stderr)
    return []


def normalize(bars):
    for bar in bars:
        bar["open"] = float(bar["open"])
        bar["high"] = float(bar["high"])
        bar["low"] = float(bar["low"])
        bar["close"] = float(bar["close"])
        bar["volume"] = int(bar["volume"])
        bar["turnover"] = float(bar.get("turnover", 0))
    return bars


def deduplicate(bars):
    seen = set()
    result = []
    for bar in bars:
        key = bar["time"]
        if key not in seen:
            seen.add(key)
            result.append(bar)
    return result


def download_1h():
    """1h data: entire period fits in one call per symbol."""
    out_dir = os.path.join(BASE_DIR, "data_1h")
    os.makedirs(out_dir, exist_ok=True)

    print("=== Downloading 1h klines ===")
    for sym in ALL_SYMBOLS:
        bars = fetch(sym, "1h", START, END)
        if bars:
            bars = normalize(deduplicate(bars))
            safe = sym.replace(".", "_")
            path = os.path.join(out_dir, f"{safe}.json")
            with open(path, "w") as f:
                json.dump(bars, f)
            days = len(set(b["time"][:10] for b in bars))
            print(f"  {sym}: {len(bars)} bars, {days} days")
        else:
            print(f"  {sym}: no data")

    print(f"1h done → {out_dir}/\n")


def download_1m():
    """1m data: day by day (960 bars/day, under 1000 limit)."""
    out_dir = os.path.join(BASE_DIR, "data_1m")
    os.makedirs(out_dir, exist_ok=True)

    start_dt = datetime.strptime(START, "%Y-%m-%d")
    end_dt = datetime.strptime(END, "%Y-%m-%d")

    print("=== Downloading 1m klines ===")
    for sym in ALL_SYMBOLS:
        all_bars = []
        current = start_dt
        while current <= end_dt:
            d = current.strftime("%Y-%m-%d")
            bars = fetch(sym, "1m", d, d)
            if bars:
                all_bars.extend(bars)
                print(f"  {sym} {d}: {len(bars)} bars")
            current += timedelta(days=1)

        if all_bars:
            all_bars = normalize(deduplicate(all_bars))
            safe = sym.replace(".", "_")
            path = os.path.join(out_dir, f"{safe}.json")
            with open(path, "w") as f:
                json.dump(all_bars, f)
            days = len(set(b["time"][:10] for b in all_bars))
            print(f"  {sym} TOTAL: {len(all_bars)} bars, {days} days")
        else:
            print(f"  {sym}: no data")
        print()

    print(f"1m done → {out_dir}/")


if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument("--period", choices=["1m", "1h", "both"], default="both")
    args = parser.parse_args()

    if args.period in ("1h", "both"):
        download_1h()
    if args.period in ("1m", "both"):
        download_1m()
