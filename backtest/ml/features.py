"""
Feature engineering for the cross-sectional factor model.

Pure stdlib — no torch. Outputs numpy arrays sized (T, N) where
T = aligned timeline length, N = universe size.

Adding new features: just add a `_feat_*` function and register in FEATURES.
"""

import json
import math
from pathlib import Path

import numpy as np

UNIVERSE = [
    "AAPL_US", "MSFT_US", "GOOGL_US", "AMZN_US", "NVDA_US", "META_US",
    "TSLA_US", "GLD_US", "UGL_US", "SLV_US", "AGQ_US", "USO_US",
]
BENCH = "QQQ_US"  # used for universe-level gate features


# ---------- panel loading ----------

def load_panel(data_dir: Path):
    """Load all symbols, align to common timeline, return arrays of shape (T, N).

    Returns:
        arrays: dict of "open"/"high"/"low"/"close"/"volume"/"turnover" -> (T, N)
        bench_arrays: same fields for BENCH symbol -> (T,)
        timeline: list[str] of length T
    """
    raw = {}
    for sym in UNIVERSE + [BENCH]:
        bars = json.load(open(data_dir / f"{sym}.json"))
        bars.sort(key=lambda b: b["time"])
        raw[sym] = bars

    # Align across the universe + bench.
    common = set(b["time"] for b in raw[UNIVERSE[0]])
    for sym in UNIVERSE[1:] + [BENCH]:
        common &= set(b["time"] for b in raw[sym])
    timeline = sorted(common)
    cs = set(common)

    fields = ["open", "high", "low", "close", "volume", "turnover"]
    T, N = len(timeline), len(UNIVERSE)
    arrays = {f: np.zeros((T, N), dtype=np.float64) for f in fields}
    bench_arrays = {f: np.zeros(T, dtype=np.float64) for f in fields}

    time_idx = {t: i for i, t in enumerate(timeline)}
    for j, sym in enumerate(UNIVERSE):
        for b in raw[sym]:
            if b["time"] in cs:
                i = time_idx[b["time"]]
                for f in fields:
                    arrays[f][i, j] = b.get(f, 0.0)
    for b in raw[BENCH]:
        if b["time"] in cs:
            i = time_idx[b["time"]]
            for f in fields:
                bench_arrays[f][i] = b.get(f, 0.0)

    return arrays, bench_arrays, timeline


# ---------- per-symbol features (T, N) ----------

def _ret_h(close, h):
    """h-bar return. Shape (T, N), zeros for t<h."""
    out = np.zeros_like(close)
    out[h:] = (close[h:] - close[:-h]) / np.where(close[:-h] > 0, close[:-h], 1.0)
    return out


def _intra_bar_pos(arr):
    """1=close at low, 0=close at high. Shape (T, N)."""
    rng = arr["high"] - arr["low"]
    pos = np.where(rng > 0, (arr["high"] - arr["close"]) / np.where(rng > 0, rng, 1.0), 0.5)
    return pos


def _body(arr):
    """+ve for red bars (open above close), normalized by close."""
    c = np.where(arr["close"] > 0, arr["close"], 1.0)
    return (arr["open"] - arr["close"]) / c


def _vol_norm(arr, window=20):
    """volume[t] / mean(volume[t-window+1..t]). Shape (T, N), 1.0 for warmup."""
    v = arr["volume"]
    T, N = v.shape
    out = np.ones_like(v)
    cs = np.cumsum(v, axis=0)
    for t in range(window - 1, T):
        m = (cs[t] - (cs[t - window] if t >= window else 0.0)) / window
        out[t] = np.where(m > 0, v[t] / np.where(m > 0, m, 1.0), 1.0)
    return out


def _atr_norm(arr, period=14):
    """Avg true range / close, shape (T, N)."""
    h, l, c = arr["high"], arr["low"], arr["close"]
    T, N = c.shape
    pc = np.zeros_like(c); pc[1:] = c[:-1]; pc[0] = c[0]
    tr = np.maximum.reduce([h - l, np.abs(h - pc), np.abs(l - pc)])
    atr = np.zeros_like(tr)
    for t in range(period - 1, T):
        atr[t] = tr[max(0, t - period + 1) : t + 1].mean(axis=0)
    return np.where(c > 0, atr / np.where(c > 0, c, 1.0), 0.0)


# Map name -> function(arr)->(T,N)
FEATURES = {
    "ret_1": lambda a: _ret_h(a["close"], 1),
    "ret_2": lambda a: _ret_h(a["close"], 2),
    "ret_3": lambda a: _ret_h(a["close"], 3),
    "ret_5": lambda a: _ret_h(a["close"], 5),
    "ret_20": lambda a: _ret_h(a["close"], 20),
    "intra_pos": _intra_bar_pos,
    "body": _body,
    "vol_norm": _vol_norm,
    "atr_norm": _atr_norm,
}


# ---------- universe-level (T,) gate features ----------

def _xs_dispersion(ret):
    """Std of returns across N symbols at each t. Shape (T,)."""
    return ret.std(axis=1)


def _bench_ret(bench_close, h):
    out = np.zeros(bench_close.shape[0])
    if h < bench_close.shape[0]:
        out[h:] = (bench_close[h:] - bench_close[:-h]) / np.where(bench_close[:-h] > 0, bench_close[:-h], 1.0)
    return out


def _bench_vol(bench_close, window=20):
    """Realized vol of bench 1-bar returns over rolling window."""
    r = np.zeros_like(bench_close)
    r[1:] = (bench_close[1:] - bench_close[:-1]) / np.where(bench_close[:-1] > 0, bench_close[:-1], 1.0)
    out = np.zeros_like(r)
    for t in range(window - 1, len(r)):
        out[t] = r[max(0, t - window + 1) : t + 1].std()
    return out


def build_gate_features(arrays, bench_arrays):
    """Returns (G_dict, names) where each value is shape (T,)."""
    ret_5 = _ret_h(arrays["close"], 5)
    ret_20 = _ret_h(arrays["close"], 20)
    G = {
        "xs_dispersion_5": _xs_dispersion(ret_5),
        "xs_dispersion_20": _xs_dispersion(ret_20),
        "bench_ret_20": _bench_ret(bench_arrays["close"], 20),
        "bench_ret_20_abs": np.abs(_bench_ret(bench_arrays["close"], 20)),
        "bench_vol_20": _bench_vol(bench_arrays["close"], 20),
    }
    names = list(G.keys())
    return G, names


# ---------- assembly ----------

def build_dataset(data_dir: Path, feature_names=None, gate=True, warmup=20):
    """Returns:
        X: (T, N, F) per-symbol feature tensor
        G: (T, F_gate) universe-level gate features
        fwd: (T, N) 1-bar-ahead return  (last row is invalid; mask it via valid_t)
        valid_t: bool (T,) — True where we have full warmup + valid forward.
        feature_names: list of length F
        gate_names: list of length F_gate
        timeline: list[str] of length T
    """
    if feature_names is None:
        feature_names = list(FEATURES.keys())

    arrays, bench_arrays, timeline = load_panel(data_dir)
    T, N = arrays["close"].shape

    feat_list = [FEATURES[name](arrays) for name in feature_names]
    X = np.stack(feat_list, axis=-1)  # (T, N, F)

    # Forward return at t = (close[t+1] - close[t]) / close[t]
    c = arrays["close"]
    fwd = np.zeros((T, N))
    fwd[:-1] = (c[1:] - c[:-1]) / np.where(c[:-1] > 0, c[:-1], 1.0)

    valid_t = np.zeros(T, dtype=bool)
    valid_t[warmup:T - 1] = True  # need warmup history + valid t+1

    if gate:
        G_dict, gate_names = build_gate_features(arrays, bench_arrays)
        G = np.stack([G_dict[n] for n in gate_names], axis=-1)  # (T, F_gate)
    else:
        G, gate_names = None, []

    return {
        "X": X, "G": G, "fwd": fwd, "valid_t": valid_t,
        "feature_names": feature_names, "gate_names": gate_names,
        "timeline": timeline,
    }
