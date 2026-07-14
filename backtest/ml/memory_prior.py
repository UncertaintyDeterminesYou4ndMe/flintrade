"""
memory_prior — the bridge between Flint's "Dreaming" aggregate stats (the
agent's OWN realized trade outcomes) and the offline backtest ML factor/gate
framework.

This is the "two learning loops converge" piece. The offline factor model in
`model.py`/`train.py` learns purely from historical KLINE. This module exposes
the agent's ACTUAL realized win-rates per setup (from the `agg` table that
`agent/reflect.py::aggregate()` populates from closed trades) as a calibration
PRIOR that the factor features and/or the gate can opt into.

Design principles:
  - Pure stdlib + sqlite3. NO torch on the export path, so `export_json()` runs
    anywhere (and training can load the prior offline, with no live db).
  - Read-only. Never writes to flint.db. Never trades.
  - Sample-size back-off is the whole point: a 2-sample bucket is noise, so we
    back off (symbol+session+rsi → symbol+session → symbol → global) to the
    most specific bucket that clears `min_trips`, and fall to `default` if even
    the global pool is too thin.
  - Symbol normalization: `agg` stores dot form (NVDA.US); the ML UNIVERSE uses
    underscore form (NVDA_US). Lookups accept either and normalize internally.

CLI:
    python3 backtest/ml/memory_prior.py
        -> prints a readable summary of current priors and writes
           backtest/ml/memory_prior.json
"""

from __future__ import annotations

import json
import os
import sqlite3
from pathlib import Path

# Repo root = .../flint ; this file is .../flint/backtest/ml/memory_prior.py
_HERE = Path(__file__).resolve().parent
_REPO_ROOT = _HERE.parent.parent
_DEFAULT_JSON = _HERE / "memory_prior.json"

# A "global" pseudo-key for the coarsest back-off level.
_GLOBAL = "__global__"


# ─────────────────────────────────────────────────────────────────────────
# db path resolution — reuse agent.db.DB if importable, else open sqlite raw
# ─────────────────────────────────────────────────────────────────────────
def _resolve_db_path(db_path: str | Path | None) -> Path:
    """FLINT_DB env > explicit arg > agent.db.DB_PATH (if importable) > repo flint.db."""
    if db_path is not None:
        return Path(db_path)
    env = os.environ.get("FLINT_DB")
    if env:
        return Path(env)
    try:
        from agent.db import DB_PATH  # type: ignore
        return Path(DB_PATH)
    except Exception:
        return _REPO_ROOT / "flint.db"


def _connect(db_path: str | Path | None) -> sqlite3.Connection:
    """Read-only-ish connection. Prefer agent.db.DB(role='reader') for the same
    PRAGMAs/role guard; fall back to a plain sqlite3 connection so this works
    even without the agent package on the path."""
    path = _resolve_db_path(db_path)
    try:
        from agent.db import DB  # type: ignore
        return DB(role="reader", path=path).conn
    except Exception:
        conn = sqlite3.connect(str(path), timeout=10.0)
        conn.row_factory = sqlite3.Row
        return conn


def _norm_symbol(symbol: str) -> str:
    """Normalize to the dot form used in the `agg` table (NVDA_US -> NVDA.US).

    Accepts either form. Only the LAST underscore is treated as the market
    separator, so multi-part tickers survive (BRK_B_US -> BRK_B.US)."""
    if symbol is None:
        return symbol
    s = symbol.strip()
    if "." in s:
        return s
    if "_" in s:
        head, _, tail = s.rpartition("_")
        return f"{head}.{tail}"
    return s


# ─────────────────────────────────────────────────────────────────────────
# load_agg — read the agg table into a nested structure
# ─────────────────────────────────────────────────────────────────────────
def load_agg(db_path: str | Path | None = None) -> dict:
    """Read the `agg` table, return a nested structure:

        { symbol(dot form): { session: { rsi_bucket: {trips, win_rate, pl_ratio} } } }

    win_rate = wins / trips (0.0..1.0); None when trips == 0 (shouldn't happen).
    pl_ratio is passed through from agg (may be None when there were no losses).
    Sessions / rsi_buckets that are 'na' (e.g. migrated trades lacking features)
    are preserved verbatim — callers handle them gracefully via back-off.
    """
    conn = _connect(db_path)
    try:
        rows = conn.execute(
            "SELECT symbol, session, setup, rsi_bucket, trips, wins, losses, pl_ratio FROM agg"
        ).fetchall()
    except sqlite3.OperationalError:
        # No agg table yet (db never reflected). Return empty; priors back off to default.
        return {}

    out: dict = {}
    for r in rows:
        symbol = _norm_symbol(r["symbol"])
        session = r["session"]
        rsi = r["rsi_bucket"]
        trips = int(r["trips"] or 0)
        wins = int(r["wins"] or 0)
        # NOTE: the agg PK is (symbol, session, setup, rsi_bucket). We collapse
        # over `setup` here (sum trips/wins) so the prior is setup-agnostic; the
        # ML loop has no notion of technical/event/user. If two setups share a
        # (symbol,session,rsi) cell we accumulate them.
        cell = out.setdefault(symbol, {}).setdefault(session, {}).setdefault(
            rsi, {"trips": 0, "wins": 0, "pl_sum": 0.0, "pl_n": 0}
        )
        cell["trips"] += trips
        cell["wins"] += wins
        if r["pl_ratio"] is not None:
            cell["pl_sum"] += float(r["pl_ratio"]) * trips  # trip-weighted
            cell["pl_n"] += trips

    # Finalize derived fields.
    for sym, sess_map in out.items():
        for sess, rsi_map in sess_map.items():
            for rsi, cell in rsi_map.items():
                trips = cell["trips"]
                cell["win_rate"] = (cell["wins"] / trips) if trips else None
                cell["pl_ratio"] = (cell["pl_sum"] / cell["pl_n"]) if cell["pl_n"] else None
                del cell["pl_sum"]
                del cell["pl_n"]
    return out


# ─────────────────────────────────────────────────────────────────────────
# back-off aggregation helpers (operate over the agg table directly so we can
# sum at each back-off level without losing sample mass)
# ─────────────────────────────────────────────────────────────────────────
def _agg_rows(db_path: str | Path | None) -> list[dict]:
    conn = _connect(db_path)
    try:
        rows = conn.execute(
            "SELECT symbol, session, rsi_bucket, trips, wins, losses, pl_ratio FROM agg"
        ).fetchall()
    except sqlite3.OperationalError:
        return []
    out = []
    for r in rows:
        out.append({
            "symbol": _norm_symbol(r["symbol"]),
            "session": r["session"],
            "rsi_bucket": r["rsi_bucket"],
            "trips": int(r["trips"] or 0),
            "wins": int(r["wins"] or 0),
            "pl_ratio": r["pl_ratio"],
        })
    return out


def _pool(rows: list[dict], *, symbol=None, session=None, rsi_bucket=None) -> dict:
    """Sum trips/wins (and trip-weighted pl_ratio) over rows matching the given
    non-None filters. None filter = wildcard (don't constrain that dimension)."""
    sym = _norm_symbol(symbol) if symbol is not None else None
    trips = wins = 0
    pl_sum = 0.0
    pl_n = 0
    for r in rows:
        if sym is not None and r["symbol"] != sym:
            continue
        if session is not None and r["session"] != session:
            continue
        if rsi_bucket is not None and r["rsi_bucket"] != rsi_bucket:
            continue
        trips += r["trips"]
        wins += r["wins"]
        if r["pl_ratio"] is not None:
            pl_sum += float(r["pl_ratio"]) * r["trips"]
            pl_n += r["trips"]
    return {
        "trips": trips,
        "wins": wins,
        "win_rate": (wins / trips) if trips else None,
        "pl_ratio": (pl_sum / pl_n) if pl_n else None,
    }


def _backoff_chain(symbol, session, rsi_bucket) -> list[dict]:
    """Most-specific → coarsest filter dicts. Levels with a None dimension are
    skipped (no point querying 'symbol+None+None' twice as 'symbol')."""
    chain = []
    if symbol is not None and session is not None and rsi_bucket is not None:
        chain.append({"symbol": symbol, "session": session, "rsi_bucket": rsi_bucket, "level": "symbol+session+rsi"})
    if symbol is not None and session is not None:
        chain.append({"symbol": symbol, "session": session, "level": "symbol+session"})
    if symbol is not None:
        chain.append({"symbol": symbol, "level": "symbol"})
    chain.append({"level": "global"})  # everything
    return chain


# ─────────────────────────────────────────────────────────────────────────
# win_rate_prior — the headline API
# ─────────────────────────────────────────────────────────────────────────
def win_rate_prior(
    symbol: str,
    session: str | None = None,
    rsi_bucket: str | None = None,
    *,
    min_trips: int = 5,
    default: float = 0.5,
    db_path: str | Path | None = None,
    _rows: list[dict] | None = None,
) -> float:
    """Agent's realized win-rate for the MOST SPECIFIC bucket clearing min_trips.

    Backs off: symbol+session+rsi → symbol+session → symbol → global → default.
    Never trusts a thin (< min_trips) bucket: that's the entire point of the
    bridge. Returns `default` (0.5 = coin-flip, no information) when even the
    global pool is too thin.
    """
    rows = _rows if _rows is not None else _agg_rows(db_path)
    for level in _backoff_chain(symbol, session, rsi_bucket):
        flt = {k: v for k, v in level.items() if k != "level"}
        pool = _pool(rows, **flt)
        if pool["trips"] >= min_trips and pool["win_rate"] is not None:
            return float(pool["win_rate"])
    return float(default)


def pl_ratio_prior(
    symbol: str,
    session: str | None = None,
    rsi_bucket: str | None = None,
    *,
    min_trips: int = 5,
    default: float | None = None,
    db_path: str | Path | None = None,
    _rows: list[dict] | None = None,
) -> float | None:
    """Companion to win_rate_prior: realized avg_win/avg_loss for the most
    specific bucket clearing min_trips. Same back-off chain."""
    rows = _rows if _rows is not None else _agg_rows(db_path)
    for level in _backoff_chain(symbol, session, rsi_bucket):
        flt = {k: v for k, v in level.items() if k != "level"}
        pool = _pool(rows, **flt)
        if pool["trips"] >= min_trips and pool["pl_ratio"] is not None:
            return float(pool["pl_ratio"])
    return default


# ─────────────────────────────────────────────────────────────────────────
# as_feature_row — small dict to merge into features.py's feature dict
# ─────────────────────────────────────────────────────────────────────────
def as_feature_row(
    symbol: str,
    session: str | None = None,
    rsi_bucket: str | None = None,
    *,
    min_trips: int = 5,
    db_path: str | Path | None = None,
    _rows: list[dict] | None = None,
) -> dict:
    """Derived features for one (symbol[,session,rsi_bucket]), suitable to merge
    into the per-symbol feature dict in `features.py`.

    Keys:
      realized_win_rate  — backed-off win rate (0..1); 0.5 when no usable prior.
      realized_pl_ratio  — backed-off avg_win/avg_loss; 1.0 fallback when none.
      sample_size        — trips in the most specific bucket that *cleared*
                           min_trips (the level the win_rate came from); 0 if none.
      has_prior          — bool: True iff some level cleared min_trips.
    """
    rows = _rows if _rows is not None else _agg_rows(db_path)
    sample_size = 0
    has_prior = False
    win_rate = 0.5
    for level in _backoff_chain(symbol, session, rsi_bucket):
        flt = {k: v for k, v in level.items() if k != "level"}
        pool = _pool(rows, **flt)
        if pool["trips"] >= min_trips and pool["win_rate"] is not None:
            win_rate = float(pool["win_rate"])
            sample_size = int(pool["trips"])
            has_prior = True
            break
    pl = pl_ratio_prior(symbol, session, rsi_bucket,
                        min_trips=min_trips, default=None, _rows=rows)
    return {
        "realized_win_rate": win_rate,
        "realized_pl_ratio": pl if pl is not None else 1.0,
        "sample_size": sample_size,
        "has_prior": has_prior,
    }


# ─────────────────────────────────────────────────────────────────────────
# export_json — dump the whole prior for offline (no-db) training
# ─────────────────────────────────────────────────────────────────────────
def export_json(path: str | Path, db_path: str | Path | None = None) -> dict:
    """Dump the full prior to JSON. Shape:

        {
          "meta": {min_trips_hint, n_symbols, total_trips, global_win_rate},
          "by_symbol": { "NVDA.US": {trips, win_rate, pl_ratio,
                                     "by_session": {sess: {...}}}, ... },
          "nested": <load_agg() output>   # full symbol→session→rsi grid
        }

    Returns the dict it wrote. Training can json.load this and feed it to the
    pure-data versions of win_rate_prior / as_feature_row (pass nested rows),
    so no live db connection is needed at train time.
    """
    rows = _agg_rows(db_path)
    nested = load_agg(db_path)
    glob = _pool(rows)

    by_symbol = {}
    for sym in sorted({r["symbol"] for r in rows}):
        sp = _pool(rows, symbol=sym)
        sessions = {}
        for sess in sorted({r["session"] for r in rows if r["symbol"] == sym}):
            ssp = _pool(rows, symbol=sym, session=sess)
            sessions[sess] = {
                "trips": ssp["trips"],
                "win_rate": ssp["win_rate"],
                "pl_ratio": ssp["pl_ratio"],
            }
        by_symbol[sym] = {
            "trips": sp["trips"],
            "win_rate": sp["win_rate"],
            "pl_ratio": sp["pl_ratio"],
            "by_session": sessions,
        }

    payload = {
        "meta": {
            "min_trips_hint": 5,
            "n_symbols": len(by_symbol),
            "total_trips": glob["trips"],
            "global_win_rate": glob["win_rate"],
        },
        "by_symbol": by_symbol,
        "nested": nested,
    }
    p = Path(path)
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text(json.dumps(payload, ensure_ascii=False, indent=2))
    return payload


# ─────────────────────────────────────────────────────────────────────────
# CLI — print a readable summary + write export
# ─────────────────────────────────────────────────────────────────────────
def _print_summary(db_path: str | Path | None = None) -> None:
    rows = _agg_rows(db_path)
    glob = _pool(rows)
    path = _resolve_db_path(db_path)
    print(f"memory_prior — agent realized-outcome priors")
    print(f"  db: {path}")
    if not rows:
        print("  (agg table empty or missing — run `python3 -m agent.reflect --force` first)")
        print(f"  win_rate_prior() will back off to default for all buckets.")
        return

    print(f"  global: trips={glob['trips']}  win_rate="
          f"{glob['win_rate']:.3f}" if glob['win_rate'] is not None else "  global: (none)")
    print()
    print(f"  {'symbol':10s} {'session':10s} {'rsi':6s} {'trips':>5s} {'win%':>6s} {'pl_ratio':>9s} {'prior@5':>8s}")
    print(f"  {'-'*10} {'-'*10} {'-'*6} {'-'*5} {'-'*6} {'-'*9} {'-'*8}")
    nested = load_agg(db_path)
    for sym in sorted(nested):
        for sess in sorted(nested[sym]):
            for rsi in sorted(nested[sym][sess]):
                c = nested[sym][sess][rsi]
                wr = f"{c['win_rate']*100:.0f}" if c["win_rate"] is not None else "  -"
                plr = f"{c['pl_ratio']:.2f}" if c["pl_ratio"] is not None else "    -"
                # what win_rate_prior would actually return for this exact bucket:
                prior = win_rate_prior(sym, sess, rsi, _rows=rows)
                print(f"  {sym:10s} {sess:10s} {rsi:6s} {c['trips']:>5d} "
                      f"{wr:>6s} {plr:>9s} {prior:>8.3f}")
    print()
    print("  prior@5 = win_rate_prior(min_trips=5) with sample-size back-off")
    print("  (thin buckets back off to symbol → global → 0.5; never trust < 5 samples)")


if __name__ == "__main__":
    _print_summary()
    payload = export_json(_DEFAULT_JSON)
    print()
    print(f"wrote {_DEFAULT_JSON} "
          f"({payload['meta']['n_symbols']} symbols, "
          f"{payload['meta']['total_trips']} total trips)")
