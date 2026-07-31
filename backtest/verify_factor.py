"""
Verify a factor formula against OOS Rank IC on the flintrade 12-symbol universe.

Loads 1h panel data, aligns timestamps across all symbols, splits 70/30
chronologically, imports `score` from factors.py, computes Spearman rank IC
of factor vs 1-bar-ahead return at every cross-section, averages.

Output (last line on success):  METRIC: <float>     (OOS mean Rank IC)
Exit 0 on success.
Exit 1 on guard failure (NaN/inf in scores OR train Rank IC <= 0).
"""

import argparse
import json
import math
import sys
from pathlib import Path

UNIVERSE = [
    "AAPL_US", "MSFT_US", "GOOGL_US", "AMZN_US", "NVDA_US", "META_US",
    "TSLA_US", "GLD_US", "UGL_US", "SLV_US", "AGQ_US", "USO_US",
]
HERE = Path(__file__).parent
DEFAULT_DATA_DIR = HERE / "data_1h"
TRAIN_FRAC = 0.70

sys.path.insert(0, str(HERE))
from factors import score  # noqa: E402


def load_panel(data_dir):
    panel = {}
    for sym in UNIVERSE:
        bars = json.load(open(data_dir / f"{sym}.json"))
        bars.sort(key=lambda b: b["time"])  # chronological
        panel[sym] = bars
    common = set(b["time"] for b in panel[UNIVERSE[0]])
    for sym in UNIVERSE[1:]:
        common &= set(b["time"] for b in panel[sym])
    common = sorted(common)
    common_set = set(common)
    aligned = {
        sym: [b for b in panel[sym] if b["time"] in common_set]
        for sym in UNIVERSE
    }
    for sym in UNIVERSE:
        if len(aligned[sym]) != len(common):
            raise SystemExit(f"alignment failed for {sym}")
    return aligned, common


def rank_avg(values):
    """Average-rank with tie handling. Stable; pure stdlib."""
    indexed = sorted(enumerate(values), key=lambda kv: kv[1])
    ranks = [0.0] * len(values)
    i = 0
    n = len(indexed)
    while i < n:
        j = i
        while j + 1 < n and indexed[j + 1][1] == indexed[i][1]:
            j += 1
        avg = (i + j) / 2.0 + 1.0
        for k in range(i, j + 1):
            ranks[indexed[k][0]] = avg
        i = j + 1
    return ranks


def spearman(x, y):
    if len(x) != len(y) or len(x) < 3:
        return 0.0
    rx, ry = rank_avg(x), rank_avg(y)
    n = len(rx)
    mx = sum(rx) / n
    my = sum(ry) / n
    num = sum((rx[i] - mx) * (ry[i] - my) for i in range(n))
    dx = math.sqrt(sum((rx[i] - mx) ** 2 for i in range(n)))
    dy = math.sqrt(sum((ry[i] - my) ** 2 for i in range(n)))
    if dx == 0 or dy == 0:
        return 0.0
    return num / (dx * dy)


def rank_ic_window(panel, t_lo, t_hi):
    """Mean Spearman rank IC across [t_lo, t_hi). Also reports bad rows."""
    ics = []
    bad = 0
    no_var = 0
    for t in range(t_lo, t_hi):
        # Need t+1 for forward return.
        if t + 1 >= len(panel[UNIVERSE[0]]):
            break
        scores = score(panel, t)
        fwd = {}
        for sym in UNIVERSE:
            c0 = panel[sym][t]["close"]
            c1 = panel[sym][t + 1]["close"]
            if c0 <= 0:
                continue
            fwd[sym] = (c1 - c0) / c0
        syms = [s for s in UNIVERSE if s in scores and s in fwd]
        sv = [scores[s] for s in syms]
        fv = [fwd[s] for s in syms]
        finite_ok = all(math.isfinite(v) for v in sv + fv)
        if not finite_ok:
            bad += 1
            continue
        if len(set(sv)) <= 1:
            no_var += 1
            continue
        ics.append(spearman(sv, fv))
    mean_ic = sum(ics) / len(ics) if ics else 0.0
    return mean_ic, len(ics), bad, no_var


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--data-dir", type=str, default=str(DEFAULT_DATA_DIR),
                    help="Directory with {SYMBOL}.json bars (default: data_1h)")
    ap.add_argument("--holdout", action="store_true",
                    help="Use ALL bars as one window (no train/test split, no guard). "
                         "For final scoring on a held-out dataset.")
    args = ap.parse_args()

    data_dir = Path(args.data_dir)
    panel, timeline = load_panel(data_dir)
    n = len(timeline)

    if args.holdout:
        ic, n_ic, bad, nv = rank_ic_window(panel, 0, n - 1)
        print(f"HOLDOUT mode | Data: {data_dir}")
        print(f"Timeline: {n} bars  ({timeline[0]} -> {timeline[-1]})")
        print(f"Rank IC: {ic:+.4f}   ({n_ic} valid, "
              f"invalid={bad}, no-variance={nv})")
        if bad > 0:
            print("WARN: NaN/inf in score outputs.")
            sys.exit(1)
        print(f"METRIC: {ic:.6f}")
        sys.exit(0)

    split = int(n * TRAIN_FRAC)
    train_ic, n_train, train_bad, train_nv = rank_ic_window(panel, 0, split)
    test_ic, n_test, test_bad, test_nv = rank_ic_window(panel, split, n - 1)

    print(f"Data: {data_dir}")
    print(f"Timeline: {n} bars  ({timeline[0]} -> {timeline[-1]})")
    print(f"Split @ {split}  ->  train={n_train} ICs, test={n_test} ICs")
    print(f"Train Rank IC: {train_ic:+.4f}   "
          f"(invalid={train_bad}, no-variance={train_nv})")
    print(f"Test  Rank IC: {test_ic:+.4f}   "
          f"(invalid={test_bad}, no-variance={test_nv})")

    if train_bad > 0 or test_bad > 0:
        print("GUARD FAIL: NaN/inf in score outputs.")
        sys.exit(1)
    if not math.isfinite(train_ic) or train_ic <= 0:
        print(f"GUARD FAIL: train Rank IC = {train_ic:+.4f} (must be > 0).")
        sys.exit(1)

    print(f"METRIC: {test_ic:.6f}")
    sys.exit(0)


if __name__ == "__main__":
    main()
