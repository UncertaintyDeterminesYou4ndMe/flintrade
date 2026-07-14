"""
Cross-period comparison of candidate factors that emerged from the 2026 autoresearch run.

Runs all candidates on 2025 (full timeline) and 2026 (current holdout) in one go.
This is pure validation — no model selection on these results.
"""

import json
import math
import sys
from pathlib import Path

UNIVERSE = [
    "AAPL_US", "MSFT_US", "GOOGL_US", "AMZN_US", "NVDA_US", "META_US",
    "TSLA_US", "GLD_US", "UGL_US", "SLV_US", "AGQ_US", "USO_US",
]
HERE = Path(__file__).parent


# ---------- factor candidates ----------

def factor_a_rev5(panel, t):
    """A: 5-bar reversal (original baseline)."""
    out = {}
    for sym, bars in panel.items():
        if t < 5:
            out[sym] = 0.0; continue
        cl = bars[t - 5]["close"]
        if cl <= 0:
            out[sym] = 0.0; continue
        out[sym] = -(bars[t]["close"] - cl) / cl
    return out


def factor_b_rev3(panel, t):
    """B: 3-bar reversal alone (2026 iter 1 winner)."""
    out = {}
    for sym, bars in panel.items():
        if t < 3:
            out[sym] = 0.0; continue
        cl = bars[t - 3]["close"]
        if cl <= 0:
            out[sym] = 0.0; continue
        out[sym] = -(bars[t]["close"] - cl) / cl
    return out


def factor_c_rev23(panel, t):
    """C: mean of (2,3)-bar reversals (2026 iter 11)."""
    out = {}
    for sym, bars in panel.items():
        if t < 3:
            out[sym] = 0.0; continue
        c = bars[t]["close"]
        rs = []
        for h in (2, 3):
            cl = bars[t - h]["close"]
            if cl > 0:
                rs.append((c - cl) / cl)
        out[sym] = -sum(rs) / len(rs) if rs else 0.0
    return out


def factor_d_rev3_pos(panel, t):
    """D: rev_3 * intra-bar pos (2026 iter 34 — clean winner)."""
    out = {}
    for sym, bars in panel.items():
        if t < 3:
            out[sym] = 0.0; continue
        c = bars[t]["close"]
        cl = bars[t - 3]["close"]
        if cl <= 0:
            out[sym] = 0.0; continue
        rev = -(c - cl) / cl
        b = bars[t]
        rng = b["high"] - b["low"]
        pos = (b["high"] - c) / rng if rng > 0 else 0.5
        out[sym] = rev * pos
    return out


def factor_e_rev3_pos_body(panel, t):
    """E: rev_3 * pos * (1 + 15*body) (2026 iter 41 — body-amped, suspected overfit)."""
    out = {}
    for sym, bars in panel.items():
        if t < 3:
            out[sym] = 0.0; continue
        c = bars[t]["close"]
        cl = bars[t - 3]["close"]
        if cl <= 0:
            out[sym] = 0.0; continue
        rev = -(c - cl) / cl
        b = bars[t]
        rng = b["high"] - b["low"]
        pos = (b["high"] - c) / rng if rng > 0 else 0.5
        body = (b["open"] - c) / c if c > 0 else 0.0
        out[sym] = rev * pos * (1.0 + 15.0 * body)
    return out


CANDIDATES = [
    ("A_rev5", factor_a_rev5, "5-bar reversal (original baseline)"),
    ("B_rev3", factor_b_rev3, "3-bar reversal"),
    ("C_rev23", factor_c_rev23, "mean reversal (2,3)"),
    ("D_rev3_pos", factor_d_rev3_pos, "rev_3 × intra-bar pos"),
    ("E_rev3_pos_body", factor_e_rev3_pos_body, "rev_3 × pos × (1 + 15·body)"),
]


# ---------- evaluation harness ----------

def load_panel(data_dir):
    panel = {}
    for sym in UNIVERSE:
        bars = json.load(open(data_dir / f"{sym}.json"))
        bars.sort(key=lambda b: b["time"])
        panel[sym] = bars
    common = set(b["time"] for b in panel[UNIVERSE[0]])
    for sym in UNIVERSE[1:]:
        common &= set(b["time"] for b in panel[sym])
    common = sorted(common)
    cs = set(common)
    aligned = {sym: [b for b in panel[sym] if b["time"] in cs] for sym in UNIVERSE}
    return aligned, common


def rank_avg(values):
    indexed = sorted(enumerate(values), key=lambda kv: kv[1])
    ranks = [0.0] * len(values)
    n = len(indexed); i = 0
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
    mx = sum(rx) / n; my = sum(ry) / n
    num = sum((rx[i] - mx) * (ry[i] - my) for i in range(n))
    dx = math.sqrt(sum((rx[i] - mx) ** 2 for i in range(n)))
    dy = math.sqrt(sum((ry[i] - my) ** 2 for i in range(n)))
    if dx == 0 or dy == 0:
        return 0.0
    return num / (dx * dy)


def evaluate(score_fn, panel, t_lo, t_hi):
    """Mean Spearman rank IC across [t_lo, t_hi)."""
    ics = []
    for t in range(t_lo, t_hi):
        if t + 1 >= len(panel[UNIVERSE[0]]):
            break
        scores = score_fn(panel, t)
        fwd = {}
        for sym in UNIVERSE:
            c0 = panel[sym][t]["close"]
            c1 = panel[sym][t + 1]["close"]
            if c0 > 0:
                fwd[sym] = (c1 - c0) / c0
        syms = [s for s in UNIVERSE if s in scores and s in fwd]
        sv = [scores[s] for s in syms]
        fv = [fwd[s] for s in syms]
        if not all(math.isfinite(v) for v in sv + fv):
            continue
        if len(set(sv)) <= 1:
            continue
        ics.append(spearman(sv, fv))
    return (sum(ics) / len(ics) if ics else 0.0), len(ics)


def main():
    panel_25, tl_25 = load_panel(HERE / "data_1h_2025")
    panel_26, tl_26 = load_panel(HERE / "data_1h")

    print(f"2025 dataset: {len(tl_25)} bars  ({tl_25[0]} -> {tl_25[-1]})")
    print(f"2026 holdout: {len(tl_26)} bars  ({tl_26[0]} -> {tl_26[-1]})")
    print()

    # 70/30 split on 2025 to mirror 2026 search protocol.
    n = len(tl_25)
    split = int(n * 0.7)

    print(f"{'Factor':18s}  {'Description':38s}  {'2025-trn':>9s}  {'2025-tst':>9s}  {'2026-OOS':>9s}")
    print("-" * 100)
    for label, fn, desc in CANDIDATES:
        ic_train, n_train = evaluate(fn, panel_25, 0, split)
        ic_test, n_test = evaluate(fn, panel_25, split, n - 1)
        ic_2026, n_2026 = evaluate(fn, panel_26, 0, len(tl_26) - 1)
        print(f"{label:18s}  {desc:38s}  {ic_train:+9.4f}  {ic_test:+9.4f}  {ic_2026:+9.4f}")
    print()
    print(f"(N: 2025-trn={n_train}, 2025-tst={n_test}, 2026={n_2026})")


if __name__ == "__main__":
    main()
