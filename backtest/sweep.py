#!/usr/bin/env python3
"""
Parameter sweep for Flint trading strategy.

Two-phase approach:
1. Broad sweep: stop_loss x take_profit x score_threshold (most impactful)
2. Fine-tune: trailing_stop, max_hold, sessions, market_regime

Usage:
    python3 sweep.py                    # full sweep
    python3 sweep.py --phase 1          # broad sweep only
    python3 sweep.py --phase 2          # fine-tune (uses phase 1 top params)
    python3 sweep.py --top 20           # show top 20 results
"""

import json
import os
import sys
import time
from itertools import product

from engine import (
    StrategyParams, BacktestResult, run_backtest,
    load_data, build_timeline
)

RESULTS_DIR = os.path.join(os.path.dirname(__file__), "results")


def phase1_sweep(data, timeline):
    """Broad sweep: the 3 most impactful parameters."""
    stop_losses = [0.3, 0.5, 0.75, 1.0, 1.5, 2.0, 3.0]
    take_profits = [0, 0.2, 0.3, 0.5, 0.75, 1.0, 1.5, 2.0]
    score_thresholds = [4, 5, 6, 7]

    combos = list(product(stop_losses, take_profits, score_thresholds))
    total = len(combos)
    print(f"Phase 1: {total} combinations (SL x TP x Score)")

    results = []
    t0 = time.time()

    for i, (sl, tp, sc) in enumerate(combos):
        params = StrategyParams(
            stop_loss_pct=sl,
            take_profit_pct=tp,
            score_threshold=sc,
            offhours_score_threshold=max(sc, 6),  # at least 6 for offhours
        )
        result = run_backtest(params, data, timeline)
        results.append(result)

        if (i + 1) % 50 == 0:
            elapsed = time.time() - t0
            rate = (i + 1) / elapsed
            eta = (total - i - 1) / rate
            print(f"  {i+1}/{total} ({rate:.1f}/s, ETA {eta:.0f}s)")

    elapsed = time.time() - t0
    print(f"Phase 1 done: {total} combos in {elapsed:.1f}s ({total/elapsed:.1f}/s)")
    return results


def phase2_sweep(data, timeline, base_params_list):
    """Fine-tune: trailing stop, max hold, sessions, market regime."""
    trailing_stops = [0, 0.3, 0.5, 0.75, 1.0]
    max_hold_bars = [0, 6, 12, 24, 48]  # 0=unlimited, 6=30min, 12=1h, 24=2h, 48=4h
    sessions_list = ["all", "intraday"]
    market_regimes = [True, False]

    results = []
    total = len(base_params_list) * len(trailing_stops) * len(max_hold_bars) * len(sessions_list) * len(market_regimes)
    print(f"Phase 2: {total} combinations (from {len(base_params_list)} base configs)")

    t0 = time.time()
    count = 0

    for base in base_params_list:
        for ts, hb, sess, mr in product(trailing_stops, max_hold_bars, sessions_list, market_regimes):
            params = StrategyParams(
                stop_loss_pct=base.stop_loss_pct,
                take_profit_pct=base.take_profit_pct,
                score_threshold=base.score_threshold,
                offhours_score_threshold=base.offhours_score_threshold,
                trailing_stop_pct=ts,
                max_hold_bars=hb,
                sessions=sess,
                market_regime=mr,
            )
            result = run_backtest(params, data, timeline)
            results.append(result)
            count += 1

            if count % 100 == 0:
                elapsed = time.time() - t0
                rate = count / elapsed
                eta = (total - count) / rate
                print(f"  {count}/{total} ({rate:.1f}/s, ETA {eta:.0f}s)")

    elapsed = time.time() - t0
    print(f"Phase 2 done: {count} combos in {elapsed:.1f}s")
    return results


def rank_results(results, min_trades=10):
    """Rank results by a composite score. Filter out low-trade-count noise."""
    scored = []
    for r in results:
        if len(r.trades) < min_trades:
            continue

        # Composite score: balance profitability, risk, and consistency
        # Weights: return (40%), profit_factor (25%), win_rate (15%), -drawdown (20%)
        pf = r.profit_factor if r.profit_factor != float('inf') else 10
        composite = (
            r.return_pct * 0.40 +
            min(pf, 5) * 10 * 0.25 +  # cap PF at 5 to avoid outlier bias
            r.win_rate * 100 * 0.15 +
            -r.max_drawdown * 0.20
        )
        scored.append((composite, r))

    scored.sort(key=lambda x: x[0], reverse=True)
    return scored


def print_results(scored, top_n=30):
    """Print ranked results as a table."""
    print(f"\n{'='*120}")
    print(f"{'Rank':>4} {'Label':<40} {'Trades':>6} {'WinR%':>6} {'PnL$':>8} {'Ret%':>7} "
          f"{'AvgWin':>7} {'AvgLoss':>8} {'PF':>6} {'MaxDD%':>7} {'AvgHold':>8}")
    print(f"{'-'*120}")

    for i, (score, r) in enumerate(scored[:top_n]):
        s = r.summary()
        pf = s['profit_factor'] if isinstance(s['profit_factor'], str) else f"{s['profit_factor']:.2f}"
        print(f"{i+1:>4} {s['label']:<40} {s['trades']:>6} {s['win_rate']:>5.1f}% "
              f"{s['total_pnl']:>+8.2f} {s['return_pct']:>6.2f}% "
              f"{s['avg_win']:>+7.2f} {s['avg_loss']:>+8.2f} {pf:>6} "
              f"{s['max_drawdown']:>6.2f}% {s['avg_hold_bars']:>7.1f}b")

    print(f"{'='*120}")


def print_current_vs_best(scored):
    """Compare current Flint params with the best found."""
    # Find current params result
    current = None
    for _, r in scored:
        p = r.params
        if (p.stop_loss_pct == 1.0 and p.take_profit_pct == 0 and
            p.score_threshold == 5 and p.trailing_stop_pct == 0 and
            p.max_hold_bars == 0 and p.sessions == "all" and p.market_regime):
            current = r
            break

    if scored:
        best = scored[0][1]
        print(f"\n{'='*80}")
        print(f"CURRENT vs BEST")
        print(f"{'='*80}")

        headers = ["Metric", "Current (Flint)", "Best Found", "Delta"]
        rows = []

        def add_row(metric, curr_val, best_val, fmt=".2f", suffix=""):
            if curr_val is not None:
                delta = best_val - curr_val
                rows.append([
                    metric,
                    f"{curr_val:{fmt}}{suffix}",
                    f"{best_val:{fmt}}{suffix}",
                    f"{delta:+{fmt}}{suffix}",
                ])

        if current:
            cs, bs = current.summary(), best.summary()
            add_row("Trades", cs['trades'], bs['trades'], "d")
            add_row("Win Rate", cs['win_rate'], bs['win_rate'], ".1f", "%")
            add_row("Total PnL", cs['total_pnl'], bs['total_pnl'], ".2f", "$")
            add_row("Return", cs['return_pct'], bs['return_pct'], ".2f", "%")
            add_row("Avg Win", cs['avg_win'], bs['avg_win'], ".2f", "$")
            add_row("Avg Loss", cs['avg_loss'], bs['avg_loss'], ".2f", "$")
            add_row("Max DD", cs['max_drawdown'], bs['max_drawdown'], ".2f", "%")
        else:
            print("(Current Flint params not found in sweep)")
            bs = best.summary()

        print(f"\nBest strategy: {best.params.label()}")
        print(f"  stop_loss_pct: {best.params.stop_loss_pct}")
        print(f"  take_profit_pct: {best.params.take_profit_pct}")
        print(f"  score_threshold: {best.params.score_threshold}")
        print(f"  trailing_stop_pct: {best.params.trailing_stop_pct}")
        print(f"  max_hold_bars: {best.params.max_hold_bars}")
        print(f"  sessions: {best.params.sessions}")
        print(f"  market_regime: {best.params.market_regime}")

        if current:
            for h, *_ in [headers]:
                print(f"\n{h[0]:<15} {h[1]:<18} {h[2]:<18} {h[3]}")
            print("-" * 70)
            for row in rows:
                print(f"{row[0]:<15} {row[1]:<18} {row[2]:<18} {row[3]}")


def main():
    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument("--phase", type=int, default=0, help="1=broad, 2=fine, 0=both")
    parser.add_argument("--top", type=int, default=30)
    parser.add_argument("--min-trades", type=int, default=10)
    parser.add_argument("--top-n-phase2", type=int, default=5,
                        help="Number of top phase1 results to carry into phase2")
    args = parser.parse_args()

    print("Loading data...")
    data = load_data()
    timeline = build_timeline(data)
    print(f"Loaded {len(data)} symbols, {len(timeline)} bars")
    print(f"Period: {timeline[0][0]} to {timeline[-1][0]}")
    print()

    os.makedirs(RESULTS_DIR, exist_ok=True)

    all_results = []

    # Phase 1
    if args.phase in (0, 1):
        p1_results = phase1_sweep(data, timeline)
        all_results.extend(p1_results)

        scored = rank_results(p1_results, args.min_trades)
        print("\n--- Phase 1 Top Results ---")
        print_results(scored, args.top)

        # Save phase 1 results
        p1_data = [r.summary() for _, r in scored]
        with open(os.path.join(RESULTS_DIR, "phase1.json"), "w") as f:
            json.dump(p1_data, f, indent=2)

    # Phase 2
    if args.phase in (0, 2):
        if args.phase == 2:
            # Load phase 1 results to get top params
            p1_path = os.path.join(RESULTS_DIR, "phase1.json")
            if not os.path.exists(p1_path):
                print("Phase 1 results not found. Run phase 1 first.")
                sys.exit(1)
            # Re-run phase 1 to get StrategyParams objects
            p1_results = phase1_sweep(data, timeline)
            scored = rank_results(p1_results, args.min_trades)
        else:
            scored = rank_results(all_results, args.min_trades)

        # Take top N base configs
        top_base = []
        for _, r in scored[:args.top_n_phase2]:
            top_base.append(r.params)

        print(f"\nPhase 2: fine-tuning top {len(top_base)} strategies...")
        p2_results = phase2_sweep(data, timeline, top_base)
        all_results.extend(p2_results)

        all_scored = rank_results(all_results, args.min_trades)
        print("\n--- Final Rankings (All Phases) ---")
        print_results(all_scored, args.top)
        print_current_vs_best(all_scored)

        # Save final results
        final_data = [r.summary() for _, r in all_scored[:100]]
        with open(os.path.join(RESULTS_DIR, "final.json"), "w") as f:
            json.dump(final_data, f, indent=2)

        # Save detailed trades for top 3
        for i, (_, r) in enumerate(all_scored[:3]):
            trades_data = [{
                "symbol": t.symbol, "side": t.side, "qty": t.quantity,
                "entry": t.entry_price, "exit": t.exit_price,
                "entry_time": t.entry_time, "exit_time": t.exit_time,
                "pnl": round(t.pnl, 2), "hold_bars": t.hold_bars,
                "exit_reason": t.exit_reason,
            } for t in r.trades]
            with open(os.path.join(RESULTS_DIR, f"top{i+1}_trades.json"), "w") as f:
                json.dump(trades_data, f, indent=2)

    print(f"\nResults saved to {RESULTS_DIR}/")


if __name__ == "__main__":
    main()
