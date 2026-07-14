# memory_prior — agent realized-outcome prior for the ML factor/gate

`memory_prior.py` bridges Flint's **Dreaming** layer (the agent's own realized
trade outcomes, rolled up by `agent/reflect.py::aggregate()` into the `agg`
table) into the offline backtest ML framework (`features.py` / `model.py` /
`train.py`). It is the "two learning loops converge" piece: the factor model
learns from historical KLINE; this gives it a **calibration prior** from the
agent's ACTUAL realized win-rates per setup.

It is pure stdlib + sqlite3 (no torch), read-only, and never trades.

## What it exposes

| Function | Returns |
|---|---|
| `load_agg(db_path=None)` | nested `symbol → session → rsi_bucket → {trips, win_rate, pl_ratio}` |
| `win_rate_prior(symbol, session=None, rsi_bucket=None, *, min_trips=5, default=0.5)` | realized win-rate of the most specific bucket clearing `min_trips`, else `default` |
| `pl_ratio_prior(...)` | companion: realized `avg_win/avg_loss`, same back-off |
| `as_feature_row(symbol, session, rsi_bucket)` | `{realized_win_rate, realized_pl_ratio, sample_size, has_prior}` |
| `export_json(path, db_path=None)` | dumps the whole prior to JSON for offline (no-db) training |

Default db path = repo `flint.db`, overridable via `FLINT_DB` env. Reuses
`agent.db.DB(role="reader")` if importable, else opens sqlite directly — so it
works even without the `agent` package on the path.

Symbols are normalized: `agg` stores dot form (`NVDA.US`), the ML `UNIVERSE`
uses underscore form (`NVDA_US`); lookups accept either.

## The sample-size back-off (the whole point)

A 2-sample bucket is noise. `win_rate_prior` walks from most-specific to
coarsest and returns the FIRST level whose pooled `trips >= min_trips`:

```
symbol+session+rsi  →  symbol+session  →  symbol  →  global  →  default (0.5)
```

With the current `flint.db` (32 migrated trades, all `rsi_bucket='na'`, max 3
trips per bucket), every fine-grained bucket is too thin, so `win_rate_prior`
backs off all the way to the global pool (16 closed trips, 0.5 win-rate). That
is the correct, conservative behavior: **never trust a < min_trips bucket.**

`has_prior=True` means *some* level cleared `min_trips` (including the global
pool). When the agent has accumulated enough per-symbol/session history, the
back-off naturally surfaces the finer buckets without any code change here.

## How to opt in (documentation-only — features.py is NOT modified)

### Option A — add realized win-rate as a per-symbol factor feature

`features.py` builds the per-symbol feature tensor in `build_dataset()`: each
named entry in the module-level **`FEATURES`** dict maps to a function returning
a `(T, N)` array, stacked into `X` of shape `(T, N, F)`. The factor in
`model.py::FactorModel.forward` is a plain dot product `X @ weights`, so any new
`(T, N)` plane becomes a learnable factor.

The prior is per-`(symbol[,session,rsi])`, not per-bar, so broadcast it across
the time axis. After `load_panel` gives you `UNIVERSE` order, build one constant
column per symbol and register it:

```python
# in features.py, near the FEATURES dict:
from memory_prior import win_rate_prior   # pure stdlib, no torch

def _realized_winrate_plane(arrays, *, session=None):
    # broadcast the agent's realized win-rate (centered at 0) across all T bars
    T, N = arrays["close"].shape
    col = np.array([win_rate_prior(sym, session) - 0.5 for sym in UNIVERSE])  # (N,)
    return np.tile(col, (T, 1))                                              # (T, N)

# then register behind a flag (don't change default feature set):
# FEATURES["realized_winrate"] = _realized_winrate_plane
```

Center at `0.5` so a no-information prior contributes 0; the model learns its
weight (and `train.py`'s per-feature standardization handles scaling). Keep it
behind a flag / explicit `feature_names=[...]` so the baseline is unaffected.

### Option B — multiply the gate's raw score by a prior-derived confidence

`model.py::GateModel.forward` / `evaluate.py` compute a per-`t` gate
probability. Because the chosen symbol is known only after `argmax` in
`evaluate.py` (`top1_idx`), the cleanest hook is in **`evaluate.py::evaluate`**,
where `top1_idx` selects the traded symbol per bar. Scale the gate decision by a
confidence factor derived from `win_rate_prior` of the chosen symbol:

```python
# in evaluate.py, after `top1_idx = scores.argmax(axis=1)`:
from memory_prior import win_rate_prior
conf = np.array([2.0 * win_rate_prior(UNIVERSE[i]) for i in top1_idx])  # 1.0 at wr=0.5
gate_open = gate_open & (conf >= conf_threshold)   # e.g. only trade setups the agent wins
```

`2 * win_rate` maps a 0.5 prior to 1.0 (neutral), >0.5 to >1.0 (boost), <0.5 to
<1.0 (damp). Use it as a multiplicative confidence on the gate or as an extra
veto. As above, gate this behind a flag so the trained baseline is reproducible.

### Offline (no live db) path

Run `python3 backtest/ml/memory_prior.py` to write `memory_prior.json`, then in
training load that file instead of touching `flint.db`:

```python
prior = json.load(open("backtest/ml/memory_prior.json"))
wr = prior["by_symbol"].get("NVDA.US", {}).get("win_rate")  # None if no data
```

## Regenerating the prior

```bash
python3 -m agent.reflect --force      # repopulate agg from closed trades (pure code, no broker)
python3 backtest/ml/memory_prior.py   # print summary + rewrite memory_prior.json
```
