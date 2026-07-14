"""Shared market-session "clock / orientation" module for Flint daemons.

All daemon processes call into this module to agree on the current US market
session. The logic here is a faithful port of the inline session-resolution
code that lives in ``run.sh`` (steps 1 + the OUTSIDE_RTH case statement):

  * the same Pre/Intraday/Post matching against ``longbridge trading session``,
  * the same Overnight 20:00-03:50 ET heuristic (next trading day must be a
    weekday; the 03:50-04:00 transition window maps to 'Overnight-Pre'),
  * the same session -> outside_rth mapping.

Pure Python 3 stdlib only (subprocess, json, datetime, os, time). Longbridge
credentials are read from the inherited environment (LONGBRIDGE_APP_KEY etc.)
which the process launcher exports — nothing is hardcoded here.

Public API:
  current_session()         -> str   (Pre/Intraday/Post/Overnight/Overnight-Pre/Closed)
  outside_rth_for(session)  -> str   (RTH_ONLY/ANY_TIME/OVERNIGHT)
  minutes_to_close(session) -> int | None
  is_tradeable(session)     -> bool
"""

from __future__ import annotations

import json
import subprocess
import time
from datetime import datetime, timedelta
from zoneinfo import ZoneInfo

ET = ZoneInfo("America/New_York")

# ---------------------------------------------------------------------------
# Cached `longbridge trading session` JSON
# ---------------------------------------------------------------------------
# Multiple daemon processes call current_session() frequently. The session
# schedule (Pre/Intraday/Post open/close times) is effectively static within a
# trading day, so we cache the parsed JSON in-module for a short TTL to avoid
# hammering the CLI. The time-of-day check is recomputed on every call; only
# the parsed CLI payload is reused within the TTL.
_CACHE_TTL_SEC = 30.0
_cache_value: list | None = None   # parsed JSON (list of market dicts)
_cache_ts: float = 0.0             # time.monotonic() when cached


def _fetch_session_json() -> list:
    """Run `longbridge trading session --format json` and parse it.

    Returns the parsed list of market dicts, or [] on any error (mirrors
    run.sh's ``|| echo "[]"`` fallback). Credentials flow through the inherited
    process environment — we do not pass env explicitly.
    """
    try:
        from agent import lb
        ok, data, _ = lb.run(["trading", "session", "--format", "json"], timeout=15)
        return data if (ok and isinstance(data, list)) else []
    except Exception:
        return []


def _get_session_json() -> list:
    """Return the cached session JSON, refreshing it if the TTL has expired."""
    global _cache_value, _cache_ts
    now = time.monotonic()
    if _cache_value is not None and (now - _cache_ts) < _CACHE_TTL_SEC:
        return _cache_value
    _cache_value = _fetch_session_json()
    _cache_ts = now
    return _cache_value


def _us_sessions(data: list) -> list:
    """Extract the US market's session list from the parsed JSON."""
    for market in data:
        if isinstance(market, dict) and str(market.get("market", "")).upper() == "US":
            sessions = market.get("sessions", [])
            return sessions if isinstance(sessions, list) else []
    return []


def _norm_hms(raw: str) -> str:
    """Normalise a longbridge time string to zero-padded ``HH:MM:SS``.

    The CLI emits e.g. ``"4:00:00.0"`` / ``"9:30:00.0"``. run.sh strips the
    ``.0`` and zero-pads each colon-separated component so string comparison
    against an ``HH:MM:SS`` ET clock is well-ordered. We reproduce that exactly.
    """
    raw = raw.replace(".0", "")
    parts = raw.split(":")
    return ":".join(p.zfill(2) for p in parts)


# ---------------------------------------------------------------------------
# Public API
# ---------------------------------------------------------------------------
def current_session() -> str:
    """Resolve the current US market session.

    Returns one of: 'Pre', 'Intraday', 'Post', 'Overnight', 'Overnight-Pre',
    'Closed'. Falls back to 'Closed' on any error.

    Mirrors run.sh step 1: match the ET wall clock against the Pre/Intraday/Post
    windows from `longbridge trading session`, then apply the Overnight
    20:00-03:50 heuristic.
    """
    try:
        now = datetime.now(ET)
        et_now = now.strftime("%H:%M:%S")     # HH:MM:SS
        dow = int(now.strftime("%u"))         # 1=Mon..7=Sun
        hhmm = et_now[:5]

        # --- Pre / Intraday / Post: match the live session windows ---
        data = _get_session_json()
        for sess in _us_sessions(data):
            begin = _norm_hms(str(sess.get("open", "")))
            end = _norm_hms(str(sess.get("close", "")))
            if begin and end and begin <= et_now <= end:
                return sess.get("session", "Open")

        # --- Overnight (20:00-03:50) — belongs to the NEXT trading day ---
        # Heuristic (same as run.sh): the holiday-aware path would need the
        # trading-days API; here we only require that the next trading day is a
        # weekday.
        is_overnight_window = (hhmm >= "20:00") or (hhmm < "03:50")
        if is_overnight_window:
            if hhmm >= "20:00":
                next_dow = dow + 1 if dow < 7 else 1
            else:
                next_dow = dow  # early morning belongs to today
            if 1 <= next_dow <= 5:
                if "03:50" <= hhmm < "04:00":
                    return "Overnight-Pre"  # N1: transition window
                return "Overnight"          # N: active overnight

        return "Closed"
    except Exception:
        return "Closed"


def outside_rth_for(session: str) -> str:
    """Map a session to the broker's outside_rth flag.

    Mirrors run.sh's OUTSIDE_RTH case statement. US extended-hours orders MUST
    carry the correct outside_rth flag or the broker rejects them.
    """
    if session == "Intraday":
        return "RTH_ONLY"
    if session in ("Pre", "Post"):
        return "ANY_TIME"
    if session in ("Overnight", "Overnight-Pre"):
        return "OVERNIGHT"
    return "RTH_ONLY"


def minutes_to_close(session: str) -> int | None:
    """Minutes from now (ET) until the current session's close.

    Parsed from the same `longbridge trading session` JSON. Used by the
    session-close-blackout risk rule (no new entries within N minutes of
    close). Returns None if not determinable.

    For Overnight / Overnight-Pre, close is 03:50 ET on the relevant trading day
    (the end of the Overnight window in run.sh's heuristic).
    """
    try:
        now = datetime.now(ET)

        if session in ("Overnight", "Overnight-Pre"):
            # Overnight runs 20:00 -> 03:50 ET. If we're at 20:00-23:59, close
            # is 03:50 tomorrow; if we're at 00:00-03:50, close is 03:50 today.
            close_dt = now.replace(hour=3, minute=50, second=0, microsecond=0)
            if now.strftime("%H:%M") >= "20:00":
                close_dt = close_dt + timedelta(days=1)
            delta = close_dt - now
            return max(0, int(delta.total_seconds() // 60))

        if session in ("Pre", "Intraday", "Post"):
            data = _get_session_json()
            for sess in _us_sessions(data):
                if sess.get("session") == session:
                    end = _norm_hms(str(sess.get("close", "")))
                    if not end:
                        return None
                    try:
                        h, m, s = (int(x) for x in end.split(":"))
                    except ValueError:
                        return None
                    close_dt = now.replace(
                        hour=h, minute=m, second=s, microsecond=0
                    )
                    delta = close_dt - now
                    return max(0, int(delta.total_seconds() // 60))
            return None

        return None
    except Exception:
        return None


def is_tradeable(session: str) -> bool:
    """True for every session except 'Closed' (matches run.sh's mode matrix)."""
    return session != "Closed"


if __name__ == "__main__":
    s = current_session()
    print(f"current_session()       = {s!r}")
    print(f"outside_rth_for({s!r}) = {outside_rth_for(s)!r}")
    print(f"minutes_to_close({s!r}) = {minutes_to_close(s)!r}")
    print(f"is_tradeable({s!r})     = {is_tradeable(s)!r}")
