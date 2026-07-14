"""
Cross-sectional factor for ranking the flint 12-symbol universe.

autoresearch evolves score() to maximize OOS Rank IC vs 1-bar-ahead returns.

Contract:
- Pure stdlib only.
- score(panel, t) returns dict[symbol] -> float; higher = expected to outperform.
- Use only data at indices <= t. NO LOOKAHEAD.
"""

HORIZONS = (3,)


def score(panel, t):
    """Reversal(2,3) * intra-bar-position. Big revert candidates closing near low."""
    out = {}
    max_h = max(HORIZONS)
    for sym, bars in panel.items():
        if t < max_h:
            out[sym] = 0.0
            continue
        c_now = bars[t]["close"]
        rs = []
        for h in HORIZONS:
            cl = bars[t - h]["close"]
            if cl > 0:
                rs.append((c_now - cl) / cl)
        rev = -sum(rs) / len(rs) if rs else 0.0
        b = bars[t]
        rng = b["high"] - b["low"]
        pos = (b["high"] - c_now) / rng if rng > 0 else 0.5
        body = (b["open"] - c_now) / c_now if c_now > 0 else 0.0  # +ve for red bar
        out[sym] = rev * pos * (1.0 + body * 15.0)
    return out
