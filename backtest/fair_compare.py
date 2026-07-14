#!/usr/bin/env python3
"""
Fair comparison of 1m / 5m / 1h indicator signals under 30-min execution.

Key design: use 5m bars as the EXECUTION CLOCK for all three scenarios.
- Exit TP/SL always checked at every 5m bar (same responsiveness)
- Entry decisions every 6th 5m bar (= 30 min)
- At decision time, compute indicators from:
  a) last 50 bars of 1m data (50 min lookback)
  b) last 50 bars of 5m data (250 min lookback)
  c) last 50 bars of 1h data (50 hour lookback)
- Entry/exit PRICE always from 5m bar close

This isolates ONLY the signal quality difference.

Also does a parameter sweep per period to find optimal params for each.

Usage:
    python3 fair_compare.py
    python3 fair_compare.py --sweep     # parameter sweep per period
"""

import json
import os
import sys
import time
from dataclasses import dataclass, field
from typing import Optional

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "scripts"))
from indicators import analyze

BASE_DIR = os.path.dirname(__file__)

TRADE_SYMBOLS = [
    "AAPL.US", "MSFT.US", "GOOGL.US", "AMZN.US", "NVDA.US",
    "META.US", "TSLA.US", "GLD.US", "UGL.US", "SLV.US", "AGQ.US", "USO.US",
]
MARKET_SYMBOLS = ["QQQ.US", "SPY.US"]
ALL_SYMBOLS = MARKET_SYMBOLS + TRADE_SYMBOLS


@dataclass
class Trade:
    symbol: str
    side: str
    quantity: int
    entry_price: float
    exit_price: float
    entry_time: str
    exit_time: str
    pnl: float
    hold_bars: int
    exit_reason: str


def load_data(data_dir):
    data = {}
    for sym in ALL_SYMBOLS:
        safe = sym.replace(".", "_")
        path = os.path.join(data_dir, f"{safe}.json")
        if os.path.exists(path):
            with open(path) as f:
                bars = json.load(f)
            # Sort by time (files may not be chronological)
            bars.sort(key=lambda b: b["time"])
            data[sym] = bars
    return data


def build_timeline(data):
    all_times = set()
    sym_time_idx = {}
    for sym, bars in data.items():
        mapping = {}
        for i, bar in enumerate(bars):
            mapping[bar["time"]] = i
            all_times.add(bar["time"])
        sym_time_idx[sym] = mapping

    timeline = []
    for t in sorted(all_times):
        indices = {}
        for sym, mapping in sym_time_idx.items():
            if t in mapping:
                indices[sym] = mapping[t]
        timeline.append((t, indices))
    return timeline


def build_time_lookup(data):
    """Build time → bar_index lookup for each symbol."""
    lookup = {}
    for sym, bars in data.items():
        mapping = {}
        for i, bar in enumerate(bars):
            mapping[bar["time"]] = i
        lookup[sym] = mapping
    return lookup


def find_bars_up_to(data, sym, time_str, time_lookup, n=50):
    """Find the last N bars of `sym` that have time <= time_str."""
    if sym not in time_lookup or sym not in data:
        return []
    mapping = time_lookup[sym]
    bars = data[sym]

    # Find the latest bar at or before time_str
    best_idx = -1
    # Since bars are sorted by time, binary search would be better,
    # but for correctness let's iterate (data is modest size per call)
    for t, idx in mapping.items():
        if t <= time_str and idx > best_idx:
            best_idx = idx

    if best_idx < 0:
        return []

    start = max(0, best_idx - n + 1)
    return bars[start:best_idx + 1]


def check_market_regime(market_indicators):
    bullish = 0
    bearish = 0
    for sym in ["QQQ.US", "SPY.US"]:
        ind = market_indicators.get(sym)
        if not ind or ind.get("error"):
            return "mixed"
        if ind["price"] > ind["vwap"] and ind["macd"]["histogram"] > 0:
            bullish += 1
        elif ind["price"] < ind["vwap"] and ind["macd"]["histogram"] < 0:
            bearish += 1
    if bullish == 2:
        return "bullish"
    if bearish == 2:
        return "bearish"
    return "mixed"


def score_short(ind):
    score = 0
    if ind["price"] < ind["vwap"]:
        score += 1
    if ind["price"] < ind["ema20"]:
        score += 1
    if ind["macd"]["value"] < 0 or ind["macd"]["histogram"] < 0:
        score += 1
    if 35 <= ind["rsi"] <= 70:
        score += 1
    if ind["volume_ratio"] > 1.5:
        score += 1
    if ind["macd"]["histogram"] < 0 and ind["macd"].get("cross") == "bearish":
        score += 1
    return score


def run_fair_backtest(exec_data, signal_data, signal_time_lookup,
                      tp_pct=0.75, sl_pct=3.0, score_threshold=5,
                      offhours_threshold=6, indicator_window=50,
                      decision_interval=6):
    """
    Fair backtest:
    - exec_data (5m) drives the timeline, entry/exit prices, TP/SL checks
    - signal_data (1m/5m/1h) provides indicator computation
    """
    timeline = build_timeline(exec_data)
    capital = 1200.0
    commission = 0.02
    peak_capital = capital
    max_drawdown = 0.0
    position = None
    trades = []
    total_decisions = 0

    for bar_idx, (time_str, indices) in enumerate(timeline):
        # Check exit on EVERY 5m bar
        if position:
            sym = position["symbol"]
            if sym not in indices:
                continue
            price = exec_data[sym][indices[sym]]["close"]
            exit_reason = None

            if position["side"] == "long":
                if price <= position["stop_loss"]:
                    exit_reason = "stop_loss"
                elif price >= position["take_profit"]:
                    net = (price - position["entry_price"]) * position["qty"] - position["qty"] * commission * 2
                    if net > 0:
                        exit_reason = "take_profit"
            else:
                if price >= position["stop_loss"]:
                    exit_reason = "stop_loss"
                elif price <= position["take_profit"]:
                    net = (position["entry_price"] - price) * position["qty"] - position["qty"] * commission * 2
                    if net > 0:
                        exit_reason = "take_profit"

            if exit_reason:
                qty = position["qty"]
                if position["side"] == "long":
                    pnl = (price - position["entry_price"]) * qty - qty * commission * 2
                    capital += price * qty - qty * commission
                else:
                    pnl = (position["entry_price"] - price) * qty - qty * commission * 2
                    capital += (2 * position["entry_price"] - price) * qty - qty * commission
                trades.append(Trade(
                    symbol=sym, side=position["side"], quantity=qty,
                    entry_price=position["entry_price"], exit_price=price,
                    entry_time=position["entry_time"], exit_time=time_str,
                    pnl=pnl, hold_bars=bar_idx - position["entry_bar"],
                    exit_reason=exit_reason))
                position = None
                peak_capital = max(peak_capital, capital)
                dd = (peak_capital - capital) / peak_capital * 100
                max_drawdown = max(max_drawdown, dd)
                continue

        # Entry only at decision interval
        if bar_idx % decision_interval != 0:
            continue
        if position:
            continue

        total_decisions += 1

        ref_sym = list(indices.keys())[0] if indices else None
        if not ref_sym:
            continue
        ref_bar = exec_data[ref_sym][indices[ref_sym]]
        offhours = ref_bar.get("session", "Intraday") in ("Pre", "Post", "Overnight")
        threshold = offhours_threshold if offhours else score_threshold

        # Market regime from SIGNAL data
        market_ind = {}
        for msym in MARKET_SYMBOLS:
            sig_bars = find_bars_up_to(signal_data, msym, time_str,
                                       signal_time_lookup, indicator_window)
            if len(sig_bars) >= 30:
                market_ind[msym] = analyze(sig_bars, msym)
        regime = check_market_regime(market_ind)

        # Scan tradeable symbols using SIGNAL data for indicators
        candidates = []
        for sym in TRADE_SYMBOLS:
            if sym not in indices:
                continue
            sig_bars = find_bars_up_to(signal_data, sym, time_str,
                                       signal_time_lookup, indicator_window)
            if len(sig_bars) < 30:
                continue
            ind = analyze(sig_bars, sym)
            if ind.get("error"):
                continue

            score = ind["score"]
            # Entry price from EXECUTION data (5m bar close)
            entry_price = exec_data[sym][indices[sym]]["close"]

            if regime == "bullish" and score >= threshold:
                candidates.append(("long", sym, score, ind, entry_price))
            elif regime == "bearish":
                ss = score_short(ind)
                if ss >= threshold:
                    candidates.append(("short", sym, ss, ind, entry_price))

        if not candidates:
            continue

        candidates.sort(key=lambda x: x[2], reverse=True)
        side, sym, score, ind, price = candidates[0]
        if price <= 0:
            continue

        qty = int(capital / price)
        if qty <= 0:
            continue

        if side == "long":
            sl = price * (1 - sl_pct / 100)
            tp = price * (1 + tp_pct / 100)
        else:
            sl = price * (1 + sl_pct / 100)
            tp = price * (1 - tp_pct / 100)

        capital -= price * qty + qty * commission
        position = {
            "symbol": sym, "side": side, "qty": qty,
            "entry_price": price, "entry_bar": bar_idx, "entry_time": time_str,
            "stop_loss": sl, "take_profit": tp,
        }

    # Force close
    if position:
        sym = position["symbol"]
        if sym in exec_data and exec_data[sym]:
            price = exec_data[sym][-1]["close"]
            qty = position["qty"]
            if position["side"] == "long":
                pnl = (price - position["entry_price"]) * qty - qty * commission * 2
                capital += price * qty - qty * commission
            else:
                pnl = (position["entry_price"] - price) * qty - qty * commission * 2
                capital += (2 * position["entry_price"] - price) * qty - qty * commission
            trades.append(Trade(
                symbol=sym, side=position["side"], quantity=qty,
                entry_price=position["entry_price"], exit_price=price,
                entry_time=position["entry_time"], exit_time="END",
                pnl=pnl, hold_bars=len(timeline) - position["entry_bar"],
                exit_reason="end_of_data"))

    peak_capital = max(peak_capital, capital)

    return {
        "trades": trades, "capital": capital,
        "peak_capital": peak_capital, "max_drawdown": max_drawdown,
        "total_decisions": total_decisions,
    }


def summarize(label, r):
    trades = r["trades"]
    n = len(trades)
    wins = sum(1 for t in trades if t.pnl > 0)
    losses = n - wins
    total_pnl = sum(t.pnl for t in trades)
    wr = wins / n * 100 if n else 0
    gp = sum(t.pnl for t in trades if t.pnl > 0)
    gl = abs(sum(t.pnl for t in trades if t.pnl <= 0))
    pf = gp / gl if gl > 0 else 0
    aw = gp / wins if wins else 0
    al = -gl / losses if losses else 0
    ret = (r["capital"] - 1200) / 1200 * 100
    return {
        "label": label, "trades": n, "wins": wins, "losses": losses,
        "win_rate": round(wr, 1), "total_pnl": round(total_pnl, 2),
        "return_pct": round(ret, 2), "avg_win": round(aw, 2),
        "avg_loss": round(al, 2), "pf": round(pf, 2),
        "max_dd": round(r["max_drawdown"], 2),
        "decisions": r["total_decisions"],
        "final_capital": round(r["capital"], 2),
    }


def print_table(summaries):
    print(f"\n{'='*110}")
    print(f"{'Label':<30} {'Trades':>6} {'WinR%':>6} {'PnL$':>9} {'Ret%':>7} "
          f"{'AvgWin':>8} {'AvgLoss':>9} {'PF':>6} {'MaxDD%':>7} {'Decisions':>10}")
    print(f"{'-'*110}")
    for s in summaries:
        print(f"{s['label']:<30} {s['trades']:>6} {s['win_rate']:>5.1f}% "
              f"{s['total_pnl']:>+9.2f} {s['return_pct']:>6.2f}% "
              f"{s['avg_win']:>+8.2f} {s['avg_loss']:>+9.2f} "
              f"{s['pf']:>6.2f} {s['max_dd']:>6.2f}% {s['decisions']:>10}")
    print(f"{'='*110}")


def main():
    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument("--sweep", action="store_true", help="Run parameter sweep per period")
    args = parser.parse_args()

    # Load execution data (always 5m)
    print("Loading 5m execution data...")
    exec_data = load_data(os.path.join(BASE_DIR, "data"))
    print(f"  {len(exec_data)} symbols")

    # Load signal data for each period
    signal_sets = {}

    print("Loading 5m signal data...")
    sig_5m = load_data(os.path.join(BASE_DIR, "data"))
    if sig_5m:
        signal_sets["5m"] = (sig_5m, build_time_lookup(sig_5m))

    print("Loading 1h signal data...")
    sig_1h = load_data(os.path.join(BASE_DIR, "data_1h"))
    if sig_1h:
        signal_sets["1h"] = (sig_1h, build_time_lookup(sig_1h))

    print("Loading 1m signal data...")
    sig_1m = load_data(os.path.join(BASE_DIR, "data_1m"))
    if sig_1m and len(sig_1m) >= 10:  # need most symbols
        signal_sets["1m"] = (sig_1m, build_time_lookup(sig_1m))
    else:
        print(f"  Only {len(sig_1m)} symbols, skipping (need >= 10)")

    if not args.sweep:
        # Single run with current params for each period
        summaries = []
        for period, (sig_data, sig_lookup) in signal_sets.items():
            t0 = time.time()
            r = run_fair_backtest(exec_data, sig_data, sig_lookup,
                                  tp_pct=0.75, sl_pct=3.0)
            elapsed = time.time() - t0
            s = summarize(f"{period} (TP0.75/SL3.0)", r)
            summaries.append(s)
            print(f"  {period}: {elapsed:.1f}s, {s['trades']} trades, ${s['total_pnl']:+.2f}")

        print_table(summaries)
    else:
        # Parameter sweep for each period
        tp_values = [0, 0.3, 0.5, 0.75, 1.0, 1.5, 2.0]
        sl_values = [0.5, 1.0, 1.5, 2.0, 3.0]
        sc_values = [4, 5, 6]

        all_summaries = []

        for period, (sig_data, sig_lookup) in signal_sets.items():
            print(f"\n--- Sweeping {period} ({len(tp_values)*len(sl_values)*len(sc_values)} combos) ---")
            best_pnl = -9999
            best_s = None
            t0 = time.time()
            count = 0
            total = len(tp_values) * len(sl_values) * len(sc_values)

            for tp in tp_values:
                for sl in sl_values:
                    for sc in sc_values:
                        oh_sc = max(sc, 6)
                        r = run_fair_backtest(exec_data, sig_data, sig_lookup,
                                              tp_pct=tp, sl_pct=sl,
                                              score_threshold=sc,
                                              offhours_threshold=oh_sc)
                        label = f"{period}_tp{tp}_sl{sl}_sc{sc}"
                        s = summarize(label, r)

                        # Composite score: return + PF + WR - DD
                        if s["trades"] >= 10:
                            comp = s["return_pct"] * 0.4 + min(s["pf"], 5) * 10 * 0.25 + s["win_rate"] * 0.15 - s["max_dd"] * 0.2
                            if comp > best_pnl:
                                best_pnl = comp
                                best_s = s
                            all_summaries.append(s)

                        count += 1
                        if count % 30 == 0:
                            elapsed = time.time() - t0
                            rate = count / elapsed
                            print(f"  {count}/{total} ({rate:.1f}/s)")

            elapsed = time.time() - t0
            print(f"  {period} done: {elapsed:.1f}s")
            if best_s:
                print(f"  Best: {best_s['label']} → PnL ${best_s['total_pnl']:+.2f}, "
                      f"WR {best_s['win_rate']}%, PF {best_s['pf']}, DD {best_s['max_dd']}%")

        # Sort all by composite score and show top
        scored = []
        for s in all_summaries:
            comp = s["return_pct"] * 0.4 + min(s["pf"], 5) * 10 * 0.25 + s["win_rate"] * 0.15 - s["max_dd"] * 0.2
            scored.append((comp, s))
        scored.sort(key=lambda x: x[0], reverse=True)

        print("\n=== TOP 20 ACROSS ALL PERIODS ===")
        print_table([s for _, s in scored[:20]])

        # Best per period
        for period in signal_sets:
            period_best = [(c, s) for c, s in scored if s["label"].startswith(period)]
            if period_best:
                print(f"\n--- Best for {period} ---")
                print_table([s for _, s in period_best[:5]])

        # Save results
        os.makedirs(os.path.join(BASE_DIR, "results"), exist_ok=True)
        out = [s for _, s in scored[:50]]
        with open(os.path.join(BASE_DIR, "results", "fair_comparison.json"), "w") as f:
            json.dump(out, f, indent=2)
        print(f"\nSaved to results/fair_comparison.json")


if __name__ == "__main__":
    main()
