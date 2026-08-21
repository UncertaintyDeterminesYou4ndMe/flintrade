#!/usr/bin/env python3
"""
Test impact of three new rules on Flintrade's strategy:
1. Volume filter: min volume_ratio to enter
2. No entry near session close (last N minutes)
3. Track hold duration per trade

Uses 1h signals on 5m execution (proven best combo).
Compares: baseline vs each rule vs all rules combined.
"""

import json
import os
import sys
import time
from itertools import product

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "scripts"))
from indicators import analyze

BASE_DIR = os.path.dirname(__file__)

TRADE_SYMBOLS = [
    "AAPL.US", "MSFT.US", "GOOGL.US", "AMZN.US", "NVDA.US",
    "META.US", "TSLA.US", "GLD.US", "UGL.US", "SLV.US", "AGQ.US", "USO.US",
]
MARKET_SYMBOLS = ["QQQ.US", "SPY.US"]
ALL_SYMBOLS = MARKET_SYMBOLS + TRADE_SYMBOLS

# Session close times (ET, as HH:MM in the time string format "YYYY-MM-DD HH:MM:SS")
SESSION_CLOSES = {
    "Pre": "09:30",       # pre-market ends
    "Intraday": "16:00",  # regular close
    "Post": "20:00",      # post-market ends
}


def load_data(data_dir):
    data = {}
    for sym in ALL_SYMBOLS:
        safe = sym.replace(".", "_")
        path = os.path.join(data_dir, f"{safe}.json")
        if os.path.exists(path):
            with open(path) as f:
                bars = json.load(f)
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
    lookup = {}
    for sym, bars in data.items():
        mapping = {}
        for i, bar in enumerate(bars):
            mapping[bar["time"]] = i
        lookup[sym] = mapping
    return lookup


def find_bars_up_to(data, sym, time_str, time_lookup, n=50):
    if sym not in time_lookup or sym not in data:
        return []
    mapping = time_lookup[sym]
    bars = data[sym]
    best_idx = -1
    for t, idx in mapping.items():
        if t <= time_str and idx > best_idx:
            best_idx = idx
    if best_idx < 0:
        return []
    start = max(0, best_idx - n + 1)
    return bars[start:best_idx + 1]


def check_market_regime(market_indicators):
    bullish = bearish = 0
    for sym in ["QQQ.US", "SPY.US"]:
        ind = market_indicators.get(sym)
        if not ind or ind.get("error"):
            return "mixed"
        if ind["price"] > ind["vwap"] and ind["macd"]["histogram"] > 0:
            bullish += 1
        elif ind["price"] < ind["vwap"] and ind["macd"]["histogram"] < 0:
            bearish += 1
    if bullish == 2: return "bullish"
    if bearish == 2: return "bearish"
    return "mixed"


def score_short(ind):
    score = 0
    if ind["price"] < ind["vwap"]: score += 1
    if ind["price"] < ind["ema20"]: score += 1
    if ind["macd"]["value"] < 0 or ind["macd"]["histogram"] < 0: score += 1
    if 35 <= ind["rsi"] <= 70: score += 1
    if ind["volume_ratio"] > 1.5: score += 1
    if ind["macd"]["histogram"] < 0 and ind["macd"].get("cross") == "bearish": score += 1
    return score


def minutes_to_session_close(time_str, session):
    """How many minutes until this session closes. Returns None if unknown."""
    close_hhmm = SESSION_CLOSES.get(session)
    if not close_hhmm:
        return None
    # time_str = "2026-04-21 14:35:00"
    try:
        hh, mm = int(time_str[11:13]), int(time_str[14:16])
        ch, cm = int(close_hhmm[:2]), int(close_hhmm[3:5])
        return (ch * 60 + cm) - (hh * 60 + mm)
    except:
        return None


def run_backtest(exec_data, signal_data, signal_time_lookup,
                 tp_pct=0.75, sl_pct=1.0, score_threshold=4,
                 offhours_threshold=6,
                 # New rules
                 min_volume_ratio=0.0,       # 0 = disabled
                 no_entry_before_close=0,     # minutes, 0 = disabled
                 max_hold_min=0,              # 0 = disabled; force exit at market after N minutes
                 reeval_losing_min=0,         # 0 = disabled; exit if held >= N min AND unrealized net < 0
                                              # (pessimistic proxy for "re-evaluate losing holds":
                                              #  always closes, real re-eval would sometimes keep)
                 bar_minutes=5,               # minutes per exec bar (5 for data/, 60 for data_1h*)
                 entry_every_bars=6,          # decision cadence in bars (6x5m=30min; use 1 for 1h)
                 label=""):
    timeline = build_timeline(exec_data)
    capital = 1200.0
    commission = 0.02
    peak_capital = capital
    max_drawdown = 0.0
    position = None
    trades = []
    total_decisions = 0
    skipped_volume = 0
    skipped_close = 0

    for bar_idx, (time_str, indices) in enumerate(timeline):
        # Exit check every 5m bar
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

            # RULES: hold-duration exits (stop/tp take precedence)
            if exit_reason is None and (max_hold_min > 0 or reeval_losing_min > 0):
                held_min = (bar_idx - position["entry_bar"]) * bar_minutes
                if max_hold_min > 0 and held_min >= max_hold_min:
                    exit_reason = "max_hold"
                elif reeval_losing_min > 0 and held_min >= reeval_losing_min:
                    if position["side"] == "long":
                        unreal = (price - position["entry_price"]) * position["qty"]
                    else:
                        unreal = (position["entry_price"] - price) * position["qty"]
                    if unreal - position["qty"] * commission * 2 < 0:
                        exit_reason = "reeval_losing"

            if exit_reason:
                qty = position["qty"]
                if position["side"] == "long":
                    pnl = (price - position["entry_price"]) * qty - qty * commission * 2
                    capital += price * qty - qty * commission
                else:
                    pnl = (position["entry_price"] - price) * qty - qty * commission * 2
                    capital += (2 * position["entry_price"] - price) * qty - qty * commission

                hold_bars = bar_idx - position["entry_bar"]
                trades.append({
                    "symbol": sym, "side": position["side"], "qty": qty,
                    "entry_price": position["entry_price"], "exit_price": price,
                    "entry_time": position["entry_time"], "exit_time": time_str,
                    "pnl": pnl, "hold_bars": hold_bars,
                    "hold_minutes": hold_bars * bar_minutes,
                    "exit_reason": exit_reason,
                })
                position = None
                peak_capital = max(peak_capital, capital)
                dd = (peak_capital - capital) / peak_capital * 100
                max_drawdown = max(max_drawdown, dd)
                continue

        # Entry only every `entry_every_bars` bars (default 6x5m = 30 min)
        if bar_idx % entry_every_bars != 0:
            continue
        if position:
            continue

        total_decisions += 1

        ref_sym = list(indices.keys())[0] if indices else None
        if not ref_sym:
            continue
        ref_bar = exec_data[ref_sym][indices[ref_sym]]
        session = ref_bar.get("session", "Intraday")
        offhours = session in ("Pre", "Post", "Overnight")
        threshold = offhours_threshold if offhours else score_threshold

        # RULE: No entry near session close
        if no_entry_before_close > 0:
            mins_left = minutes_to_session_close(time_str, session)
            if mins_left is not None and 0 < mins_left <= no_entry_before_close:
                skipped_close += 1
                continue

        # Market regime
        market_ind = {}
        for msym in MARKET_SYMBOLS:
            sig_bars = find_bars_up_to(signal_data, msym, time_str,
                                       signal_time_lookup, 50)
            if len(sig_bars) >= 30:
                market_ind[msym] = analyze(sig_bars, msym)
        regime = check_market_regime(market_ind)

        # Scan symbols
        candidates = []
        for sym in TRADE_SYMBOLS:
            if sym not in indices:
                continue
            sig_bars = find_bars_up_to(signal_data, sym, time_str,
                                       signal_time_lookup, 50)
            if len(sig_bars) < 30:
                continue
            ind = analyze(sig_bars, sym)
            if ind.get("error"):
                continue

            score = ind["score"]
            vol_ratio = ind.get("volume_ratio", 0)

            # RULE: Volume filter
            if min_volume_ratio > 0 and vol_ratio < min_volume_ratio:
                skipped_volume += 1
                continue

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
            trades.append({
                "symbol": sym, "side": position["side"], "qty": qty,
                "entry_price": position["entry_price"], "exit_price": price,
                "entry_time": position["entry_time"], "exit_time": "END",
                "pnl": pnl, "hold_bars": len(timeline) - position["entry_bar"],
                "hold_minutes": (len(timeline) - position["entry_bar"]) * bar_minutes,
                "exit_reason": "end_of_data",
            })

    peak_capital = max(peak_capital, capital)
    n = len(trades)
    wins = [t for t in trades if t["pnl"] > 0]
    losses = [t for t in trades if t["pnl"] <= 0]
    gp = sum(t["pnl"] for t in wins)
    gl = abs(sum(t["pnl"] for t in losses))
    total_pnl = sum(t["pnl"] for t in trades)

    return {
        "label": label,
        "trades": n,
        "wins": len(wins), "losses": len(losses),
        "win_rate": round(len(wins) / n * 100, 1) if n else 0,
        "total_pnl": round(total_pnl, 2),
        "return_pct": round((capital - 1200) / 1200 * 100, 2),
        "avg_win": round(gp / len(wins), 2) if wins else 0,
        "avg_loss": round(-gl / len(losses), 2) if losses else 0,
        "pf": round(gp / gl, 2) if gl > 0 else 0,
        "max_dd": round(max_drawdown, 2),
        "final_capital": round(capital, 2),
        "skipped_volume": skipped_volume,
        "skipped_close": skipped_close,
        "avg_hold_min": round(sum(t["hold_minutes"] for t in trades) / n, 0) if n else 0,
        "win_hold_min": round(sum(t["hold_minutes"] for t in wins) / len(wins), 0) if wins else 0,
        "loss_hold_min": round(sum(t["hold_minutes"] for t in losses) / len(losses), 0) if losses else 0,
        "all_trades": trades,
    }


def print_table(results):
    print(f"\n{'='*130}")
    print(f"{'Label':<45} {'Trades':>6} {'WinR%':>6} {'PnL$':>9} {'Ret%':>7} "
          f"{'AvgWin':>8} {'AvgLoss':>9} {'PF':>6} {'MaxDD%':>7} "
          f"{'AvgHold':>8} {'WinHold':>8} {'LossHold':>9} {'SkipVol':>8} {'SkipClose':>10}")
    print(f"{'-'*130}")
    for s in results:
        print(f"{s['label']:<45} {s['trades']:>6} {s['win_rate']:>5.1f}% "
              f"{s['total_pnl']:>+9.2f} {s['return_pct']:>6.2f}% "
              f"{s['avg_win']:>+8.2f} {s['avg_loss']:>+9.2f} "
              f"{s['pf']:>6.2f} {s['max_dd']:>6.2f}% "
              f"{s['avg_hold_min']:>7.0f}m {s['win_hold_min']:>7.0f}m {s['loss_hold_min']:>8.0f}m "
              f"{s['skipped_volume']:>8} {s['skipped_close']:>10}")
    print(f"{'='*130}")


def main():
    print("Loading data...")
    exec_data = load_data(os.path.join(BASE_DIR, "data"))
    sig_1h = load_data(os.path.join(BASE_DIR, "data_1h"))
    sig_lookup = build_time_lookup(sig_1h)
    print(f"  {len(exec_data)} symbols (exec), {len(sig_1h)} symbols (1h signal)")

    # Best params from previous sweep: 1h TP 0.75% SL 1.0% SC 4
    base_tp, base_sl, base_sc = 0.75, 1.0, 4

    results = []

    # =============================================
    # 1. BASELINE (no new rules)
    # =============================================
    print("\n--- Baseline ---")
    r = run_backtest(exec_data, sig_1h, sig_lookup,
                     tp_pct=base_tp, sl_pct=base_sl, score_threshold=base_sc,
                     label="BASELINE (tp0.75 sl1.0 sc4)")
    results.append({k: v for k, v in r.items() if k != "all_trades"})

    # =============================================
    # 2. VOLUME FILTER sweep
    # =============================================
    print("\n--- Volume Filter ---")
    for min_vol in [0.3, 0.5, 0.7, 1.0, 1.5, 2.0]:
        r = run_backtest(exec_data, sig_1h, sig_lookup,
                         tp_pct=base_tp, sl_pct=base_sl, score_threshold=base_sc,
                         min_volume_ratio=min_vol,
                         label=f"vol >= {min_vol}")
        results.append({k: v for k, v in r.items() if k != "all_trades"})

    # =============================================
    # 3. NO ENTRY BEFORE CLOSE sweep
    # =============================================
    print("\n--- No Entry Before Close ---")
    for mins in [15, 30, 45, 60, 90, 120]:
        r = run_backtest(exec_data, sig_1h, sig_lookup,
                         tp_pct=base_tp, sl_pct=base_sl, score_threshold=base_sc,
                         no_entry_before_close=mins,
                         label=f"no entry {mins}min before close")
        results.append({k: v for k, v in r.items() if k != "all_trades"})

    # =============================================
    # 4. COMBINED: best vol + best close_buffer
    # =============================================
    print("\n--- Combined Rules ---")
    vol_values = [0, 0.3, 0.5, 0.7, 1.0]
    close_values = [0, 30, 60, 90]
    for vol, close_min in product(vol_values, close_values):
        if vol == 0 and close_min == 0:
            continue  # skip baseline duplicate
        r = run_backtest(exec_data, sig_1h, sig_lookup,
                         tp_pct=base_tp, sl_pct=base_sl, score_threshold=base_sc,
                         min_volume_ratio=vol,
                         no_entry_before_close=close_min,
                         label=f"vol>={vol} close{close_min}m")
        results.append({k: v for k, v in r.items() if k != "all_trades"})

    # Sort by composite score
    scored = []
    for r in results:
        if r["trades"] >= 5:
            comp = r["return_pct"] * 0.4 + min(r["pf"], 5) * 10 * 0.25 + r["win_rate"] * 0.15 - r["max_dd"] * 0.2
            scored.append((comp, r))
    scored.sort(key=lambda x: x[0], reverse=True)

    print("\n=== TOP 20 RESULTS ===")
    print_table([s for _, s in scored[:20]])

    # Show baseline vs best
    baseline = results[0]
    best = scored[0][1] if scored else None

    if best and best["label"] != baseline["label"]:
        print(f"\n{'='*60}")
        print(f"BASELINE vs BEST")
        print(f"{'='*60}")
        for key in ["trades", "win_rate", "total_pnl", "return_pct", "avg_win", "avg_loss",
                     "pf", "max_dd", "avg_hold_min", "win_hold_min", "loss_hold_min"]:
            bv = baseline[key]
            bev = best[key]
            delta = bev - bv if isinstance(bv, (int, float)) else ""
            print(f"  {key:<15} {str(bv):>12} → {str(bev):>12}  ({delta:+.2f})" if delta != "" else f"  {key:<15} {str(bv):>12} → {str(bev):>12}")
        print(f"\n  Best config: {best['label']}")

    # Hold duration analysis
    print(f"\n{'='*60}")
    print("HOLD DURATION ANALYSIS (Baseline)")
    print(f"{'='*60}")
    r = run_backtest(exec_data, sig_1h, sig_lookup,
                     tp_pct=base_tp, sl_pct=base_sl, score_threshold=base_sc,
                     label="baseline_detail")
    wins = [t for t in r["all_trades"] if t["pnl"] > 0]
    losses = [t for t in r["all_trades"] if t["pnl"] <= 0]
    print(f"  Winning trades: avg hold {sum(t['hold_minutes'] for t in wins)/len(wins):.0f} min" if wins else "  No wins")
    print(f"  Losing trades:  avg hold {sum(t['hold_minutes'] for t in losses)/len(losses):.0f} min" if losses else "  No losses")

    if wins:
        print(f"\n  Win hold distribution:")
        for bucket in [(0, 30), (30, 60), (60, 120), (120, 240), (240, 480), (480, 9999)]:
            count = sum(1 for t in wins if bucket[0] <= t["hold_minutes"] < bucket[1])
            pnl = sum(t["pnl"] for t in wins if bucket[0] <= t["hold_minutes"] < bucket[1])
            label = f"{bucket[0]}-{bucket[1]}min" if bucket[1] < 9999 else f"{bucket[0]}+min"
            print(f"    {label:<12} {count:>3} trades  ${pnl:>+8.2f}")

    if losses:
        print(f"\n  Loss hold distribution:")
        for bucket in [(0, 30), (30, 60), (60, 120), (120, 240), (240, 480), (480, 9999)]:
            count = sum(1 for t in losses if bucket[0] <= t["hold_minutes"] < bucket[1])
            pnl = sum(t["pnl"] for t in losses if bucket[0] <= t["hold_minutes"] < bucket[1])
            label = f"{bucket[0]}-{bucket[1]}min" if bucket[1] < 9999 else f"{bucket[0]}+min"
            print(f"    {label:<12} {count:>3} trades  ${pnl:>+8.2f}")

    # Save
    os.makedirs(os.path.join(BASE_DIR, "results"), exist_ok=True)
    out = [s for _, s in scored[:50]]
    with open(os.path.join(BASE_DIR, "results", "rule_test.json"), "w") as f:
        json.dump(out, f, indent=2)
    print(f"\nSaved to results/rule_test.json")


if __name__ == "__main__":
    main()
