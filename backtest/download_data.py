#!/usr/bin/env python3
"""
Download historical 5m klines for backtesting.
Pulls data week-by-week to stay under 1000-bar API limit.
Includes pre/post/overnight sessions.

Usage:
    python3 download_data.py                          # last 8 weeks
    python3 download_data.py --weeks 12               # last 12 weeks
    python3 download_data.py --start 2026-01-01       # from date to today
"""

import json
import os
import subprocess
import sys
from datetime import datetime, timedelta

# Defaults; can be overridden via --period and --outdir.
DEFAULT_PERIOD = "5m"
DEFAULT_DATA_DIR = os.path.join(os.path.dirname(__file__), "data")

# Same symbols as Flint
TRADE_SYMBOLS = [
    "AAPL.US", "MSFT.US", "GOOGL.US", "AMZN.US", "NVDA.US",
    "META.US", "TSLA.US", "GLD.US", "UGL.US", "SLV.US", "AGQ.US", "USO.US",
]
MARKET_SYMBOLS = ["QQQ.US", "SPY.US"]
ALL_SYMBOLS = MARKET_SYMBOLS + TRADE_SYMBOLS


def fetch_klines(symbol, start_date, end_date, period=DEFAULT_PERIOD,
                  adjust="forward_adjust"):
    """Fetch klines for a symbol in a date range. Defaults to forward-adjusted
    prices (continuous across splits/dividends — required for clean backtests)."""
    cmd = [
        "longbridge", "kline-history", symbol,
        "--period", period,
        "--start", start_date,
        "--end", end_date,
        "--session", "all",
        "--adjust", adjust,
        "--format", "json",
    ]
    try:
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=30)
        if r.returncode == 0 and r.stdout.strip():
            data = json.loads(r.stdout)
            # API returns newest-first, we want oldest-first for backtesting
            data.reverse()
            return data
    except Exception as e:
        print(f"  ERROR fetching {symbol} {start_date}~{end_date}: {e}", file=sys.stderr)
    return []


def download_symbol(symbol, start_date, end_date, period=DEFAULT_PERIOD,
                    existing_min=None, existing_max=None,
                    adjust="forward_adjust"):
    """Download all klines for a symbol, splitting into weekly chunks.

    existing_min/max: YYYY-MM-DD string range already present in the local store.
    Any week whose [s, e] range falls entirely inside [existing_min, existing_max]
    is skipped (treated as already-attempted; holidays/gaps inside that span
    are accepted as 'we already tried'). To force a re-pull, delete the file.
    """
    all_bars = []
    skipped = 0
    current = datetime.strptime(start_date, "%Y-%m-%d")
    end = datetime.strptime(end_date, "%Y-%m-%d")

    while current < end:
        week_end = min(current + timedelta(days=6), end)
        s = current.strftime("%Y-%m-%d")
        e = week_end.strftime("%Y-%m-%d")

        if existing_min and existing_max and s >= existing_min and e <= existing_max:
            print(f"  {symbol} {s}~{e}: SKIP (within existing {existing_min}~{existing_max})")
            skipped += 1
            current = week_end + timedelta(days=1)
            continue

        bars = fetch_klines(symbol, s, e, period=period, adjust=adjust)
        if bars:
            all_bars.extend(bars)
            print(f"  {symbol} {s}~{e}: {len(bars)} bars")
        else:
            print(f"  {symbol} {s}~{e}: 0 bars (market closed?)")

        current = week_end + timedelta(days=1)

    if skipped:
        print(f"  ({skipped} weeks skipped — API calls saved)")
    return all_bars


def deduplicate(bars):
    """Remove duplicate bars (overlapping week boundaries)."""
    seen = set()
    result = []
    for bar in bars:
        key = bar["time"]
        if key not in seen:
            seen.add(key)
            result.append(bar)
    return result


def main():
    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument("--weeks", type=int, default=8)
    parser.add_argument("--start", type=str, default=None)
    parser.add_argument("--end", type=str, default=None,
                        help="End date YYYY-MM-DD, default=today")
    parser.add_argument("--period", type=str, default=DEFAULT_PERIOD,
                        help="Kline period (5m, 1h, 1d, etc)")
    parser.add_argument("--outdir", type=str, default=None,
                        help="Output directory, default=backtest/data")
    parser.add_argument("--symbols", type=str, default=None,
                        help="Comma-separated symbols, default=all")
    parser.add_argument("--adjust", type=str, default="forward_adjust",
                        choices=["forward_adjust", "no_adjust"],
                        help="Price adjustment (default forward_adjust — splits/dividends).")
    args = parser.parse_args()

    end_date = args.end if args.end else datetime.now().strftime("%Y-%m-%d")

    if args.start:
        start_date = args.start
    else:
        start_date = (datetime.now() - timedelta(weeks=args.weeks)).strftime("%Y-%m-%d")

    symbols = args.symbols.split(",") if args.symbols else ALL_SYMBOLS
    out_dir = args.outdir if args.outdir else DEFAULT_DATA_DIR

    os.makedirs(out_dir, exist_ok=True)

    print(f"Downloading {args.period} klines: {start_date} to {end_date}")
    print(f"Symbols: {len(symbols)}")
    print(f"Output: {out_dir}/")
    print()

    summary = {}
    for sym in symbols:
        print(f"[{sym}]")
        safe_name = sym.replace(".", "_")
        out_file = os.path.join(out_dir, f"{safe_name}.json")

        # Load any existing bars so we can skip already-covered weeks.
        existing = []
        emin = emax = None
        if os.path.exists(out_file):
            try:
                existing = json.load(open(out_file))
                if existing:
                    dates = [b["time"][:10] for b in existing]
                    emin, emax = min(dates), max(dates)
                    print(f"  loaded {len(existing)} existing bars "
                          f"({emin} → {emax})")
            except Exception:
                existing = []
                emin = emax = None

        # If existing fully covers requested range (within 3-day holiday tolerance),
        # skip this symbol entirely — saves all API calls for re-runs.
        if emin and emax:
            req_s = datetime.strptime(start_date, "%Y-%m-%d")
            req_e = datetime.strptime(end_date, "%Y-%m-%d")
            ex_s = datetime.strptime(emin, "%Y-%m-%d")
            ex_e = datetime.strptime(emax, "%Y-%m-%d")
            if ex_s - timedelta(days=3) <= req_s and req_e <= ex_e + timedelta(days=3):
                print(f"  SKIP {sym} — existing [{emin}~{emax}] covers requested [{start_date}~{end_date}]")
                summary[sym] = {"bars": len(existing), "days": len(set(b["time"][:10] for b in existing)),
                                "from": existing[0]["time"], "to": existing[-1]["time"],
                                "skipped": True}
                print()
                continue

        new_bars = download_symbol(sym, start_date, end_date,
                                    period=args.period,
                                    existing_min=emin, existing_max=emax,
                                    adjust=args.adjust)
        bars = deduplicate(existing + new_bars)

        if bars:
            # Normalize field types (API returns strings on freshly-fetched bars,
            # but cached existing bars are already typed — be defensive).
            for bar in bars:
                bar["open"] = float(bar["open"])
                bar["high"] = float(bar["high"])
                bar["low"] = float(bar["low"])
                bar["close"] = float(bar["close"])
                bar["volume"] = int(bar["volume"])
                bar["turnover"] = float(bar.get("turnover", 0))

            with open(out_file, "w") as f:
                json.dump(bars, f)

            days = len(set(bar["time"][:10] for bar in bars))
            summary[sym] = {"bars": len(bars), "days": days,
                            "from": bars[0]["time"], "to": bars[-1]["time"]}
            print(f"  Total: {len(bars)} bars, {days} days → {out_file}")
        else:
            summary[sym] = {"bars": 0, "days": 0}
            print(f"  No data")
        print()

    # Save manifest
    manifest = {
        "start": start_date,
        "end": end_date,
        "period": args.period,
        "session": "all",
        "symbols": summary,
        "downloaded_at": datetime.now().isoformat(),
    }
    with open(os.path.join(out_dir, "manifest.json"), "w") as f:
        json.dump(manifest, f, indent=2)

    print("=" * 60)
    total_bars = sum(s["bars"] for s in summary.values())
    total_days = max((s["days"] for s in summary.values()), default=0)
    print(f"Done. {total_bars} total bars across {len(symbols)} symbols, ~{total_days} trading days")


if __name__ == "__main__":
    main()
