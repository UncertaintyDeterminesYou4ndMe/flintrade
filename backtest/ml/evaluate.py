"""
Evaluation harness — given a trained factor + gate, compute P&L and IC stats
using HARD top-1 selection (matches flint's actual single-position decision).

Pure numpy. Imports from features.py for shared math, but not from torch.
"""

import math
import numpy as np


def _rank_avg(values):
    """Average-rank with tie handling (1-indexed)."""
    n = len(values)
    indexed = sorted(enumerate(values), key=lambda kv: kv[1])
    ranks = np.zeros(n)
    i = 0
    while i < n:
        j = i
        while j + 1 < n and indexed[j + 1][1] == indexed[i][1]:
            j += 1
        avg = (i + j) / 2.0 + 1.0
        for k in range(i, j + 1):
            ranks[indexed[k][0]] = avg
        i = j + 1
    return ranks


def _spearman(x, y):
    if len(x) != len(y) or len(x) < 3:
        return 0.0
    rx, ry = _rank_avg(x), _rank_avg(y)
    n = len(rx)
    mx, my = rx.mean(), ry.mean()
    num = ((rx - mx) * (ry - my)).sum()
    dx = math.sqrt(((rx - mx) ** 2).sum())
    dy = math.sqrt(((ry - my) ** 2).sum())
    if dx == 0 or dy == 0:
        return 0.0
    return num / (dx * dy)


def evaluate(
    factor_w: np.ndarray,        # (F,)
    gate_w: np.ndarray | None,   # (F_gate,) or None
    gate_b: float | None,        # bias scalar or None
    X: np.ndarray,               # (T, N, F)
    G: np.ndarray | None,        # (T, F_gate) or None
    fwd: np.ndarray,             # (T, N)
    valid_t: np.ndarray,         # (T,) bool
    *,
    gate_threshold: float = 0.5,
    commission_bps: float = 5.0,  # ~$0.02/share commission ≈ 5 bps at $40 avg price
):
    """Returns dict with hard-top-1 P&L stats and rank IC."""
    T, N, F_ = X.shape
    scores = np.einsum("tnf,f->tn", X, factor_w)  # (T, N)

    if gate_w is not None and G is not None:
        gate_logits = gate_b + G @ gate_w
        gate_prob = 1.0 / (1.0 + np.exp(-gate_logits))
        gate_open = gate_prob >= gate_threshold
    else:
        gate_prob = np.ones(T)
        gate_open = np.ones(T, dtype=bool)

    # Per-t Spearman rank IC of scores vs forward returns.
    ics = []
    for t in np.where(valid_t)[0]:
        s = scores[t]
        f = fwd[t]
        if not (np.isfinite(s).all() and np.isfinite(f).all()):
            continue
        if len(set(s.tolist())) <= 1:
            continue
        ics.append(_spearman(s, f))
    rank_ic = float(np.mean(ics)) if ics else 0.0

    # Hard top-1 selection: pick argmax score per t.
    top1_idx = scores.argmax(axis=1)  # (T,)
    top1_ret = fwd[np.arange(T), top1_idx]  # (T,) — return of the chosen stock next bar
    top2_score_diff = np.partition(scores, -2, axis=1)
    top1_score = top2_score_diff[:, -1]
    top2_score = top2_score_diff[:, -2]

    # Restrict to valid + gate-open timestamps.
    mask = valid_t & gate_open
    traded_returns = top1_ret[mask] - commission_bps / 1e4  # subtract round-trip cost
    mean_ret = float(traded_returns.mean()) if mask.any() else 0.0
    hit_rate = float((traded_returns > 0).mean()) if mask.any() else 0.0
    n_trades = int(mask.sum())
    n_eligible = int(valid_t.sum())
    coverage = n_trades / n_eligible if n_eligible else 0.0

    # Sharpe-ish: avg / std (per bar).
    if mask.any() and traded_returns.std() > 0:
        sharpe = float(mean_ret / traded_returns.std())
    else:
        sharpe = 0.0

    return {
        "rank_ic_all": rank_ic,            # all valid t, ignores gate
        "n_ic": len(ics),
        "mean_top1_ret_bps": mean_ret * 1e4,
        "hit_rate": hit_rate,
        "n_trades": n_trades,
        "coverage": coverage,
        "sharpe_per_bar": sharpe,
        "mean_gate_prob": float(gate_prob[valid_t].mean()) if valid_t.any() else 0.0,
    }


def format_eval(label, stats):
    return (
        f"{label:14s}  "
        f"IC={stats['rank_ic_all']:+.4f} (n={stats['n_ic']:>4d})  "
        f"top1_ret={stats['mean_top1_ret_bps']:+7.2f}bps  "
        f"hit={stats['hit_rate']*100:5.1f}%  "
        f"trades={stats['n_trades']:>5d}  "
        f"cov={stats['coverage']*100:5.1f}%  "
        f"sharpe/bar={stats['sharpe_per_bar']:+.4f}  "
        f"gate={stats['mean_gate_prob']:.2f}"
    )
