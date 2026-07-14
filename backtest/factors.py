"""
Cross-sectional factor for ranking the flint 12-symbol universe.

Final form chosen via cross-period validation:
- Discovered (and overfit) on 2026-02..04 hourly bars (50-iter autoresearch).
- Validated on 2025-01..12 hourly bars (4238-bar timeline, ~6× the 2026 sample).
- Survivors on both periods: simple multi-horizon mean reversion.
- The 2026-specific Williams %R / body-amplifier embellishments did NOT
  generalize to 2025 — discarded.

Contract:
- Pure stdlib only.
- score(panel, t) returns dict[symbol] -> float; higher = expected to outperform.
- Use only data at indices <= t. NO LOOKAHEAD.
"""

HORIZONS = (2, 3)


def score(panel, t):
    """Mean of (2,3)-bar reversals. Cross-period robust."""
    out = {}
    max_h = max(HORIZONS)
    for sym, bars in panel.items():
        if t < max_h:
            out[sym] = 0.0
            continue
        c_now = bars[t]["close"]
        rs = []
        for h in HORIZONS:
            c_lag = bars[t - h]["close"]
            if c_lag > 0:
                rs.append((c_now - c_lag) / c_lag)
        out[sym] = -sum(rs) / len(rs) if rs else 0.0
    return out
