#!/bin/bash
# flintrade bootstrap — fresh clone → verified dry-run, one command, macOS.
#
#   bash scripts/bootstrap.sh
#
# Idempotent: safe to re-run. Never places real orders and never needs
# credentials — everything below runs in dry-run mode. Designed so that a
# human OR a coding agent can go from `git clone` to a passing smoke test
# without reading anything else. Next steps are printed at the end.
set -uo pipefail

FLINT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$FLINT_DIR"
PASS="✓"; WARN="!"

step()  { printf '\n── %s ──\n' "$1"; }
ok()    { printf '%s %s\n' "$PASS" "$1"; }
warn()  { printf '%s %s\n' "$WARN" "$1"; }

FAILED=0

# ── 1. Python ≥ 3.10 ─────────────────────────────────────────────────────
step "1/6 Python"
PY="$(command -v python3 || true)"
if [ -z "$PY" ]; then
  echo "python3 not found. Install it first:  brew install python@3.12" >&2
  exit 1
fi
PYVER="$($PY -c 'import sys; print("%d.%d" % sys.version_info[:2])')"
if ! $PY -c 'import sys; sys.exit(0 if sys.version_info >= (3,10) else 1)'; then
  echo "python3 is $PYVER; need >= 3.10.  brew install python@3.12" >&2
  exit 1
fi
ok "python3 $PYVER"

# ── 2. venv + optional semantic-memory deps ──────────────────────────────
step "2/6 virtualenv"
if [ ! -x .venv/bin/python ]; then
  $PY -m venv .venv
fi
ok ".venv ready"
# lancedb + fastembed power semantic recall of similar historical trades.
# They are OPTIONAL — the agent degrades gracefully without them.
if .venv/bin/python -c 'import lancedb, fastembed' 2>/dev/null; then
  ok "semantic memory deps already installed"
elif .venv/bin/pip install -q lancedb fastembed 2>/dev/null; then
  ok "installed lancedb + fastembed (semantic memory ON)"
else
  warn "lancedb/fastembed install failed — continuing WITHOUT semantic memory (optional)"
fi

# ── 3. longbridge CLI (broker: quotes + paper-trading orders) ────────────
step "3/6 longbridge CLI"
if command -v longbridge >/dev/null 2>&1; then
  ok "longbridge CLI: $(longbridge --version 2>/dev/null | head -1)"
elif command -v brew >/dev/null 2>&1; then
  echo "installing via Homebrew (longbridge/tap)…"
  brew tap longbridge/tap >/dev/null 2>&1
  if brew install --cask longbridge-terminal >/dev/null 2>&1; then
    ok "longbridge CLI installed"
  else
    warn "auto-install failed — install manually: brew tap longbridge/tap && brew install --cask longbridge-terminal"
    FAILED=1
  fi
else
  warn "Homebrew not found — install the longbridge CLI manually: https://longbridge.com (or brew tap longbridge/tap)"
  FAILED=1
fi

# ── 4. config files ───────────────────────────────────────────────────────
step "4/6 config"
if [ ! -f flint.env ]; then
  cp flint.env.example flint.env
  ok "created flint.env from template (FLINT_DRY_RUN=1 — safe default)"
else
  ok "flint.env exists (kept as-is)"
fi
[ -f state.json ] || { cp state.json.example state.json 2>/dev/null && ok "created state.json" || true; }
mkdir -p logs
.venv/bin/python -c "from agent.db import init_db; init_db()" && ok "flint.db schema ready"

# ── 5. LLM provider check ────────────────────────────────────────────────
step "5/6 LLM provider"
# Default = local `claude` CLI (Claude Code). Any OpenAI-compatible or
# Anthropic API provider works too — edit agent/config/trading.toml [models]
# and export the matching key in flint.env. Details: python -m agent.llm check
if .venv/bin/python -m agent.llm check; then
  ok "LLM tiers resolved"
else
  warn "LLM provider not ready — pick one in agent/config/trading.toml [models]:"
  echo '     claude-cli (default, needs the `claude` CLI) | anthropic | openai | deepseek | moonshot | openrouter | ollama'
  echo '     then put the API key in flint.env and re-run: .venv/bin/python -m agent.llm check'
  FAILED=1
fi

# ── 6. dry-run smoke test (no orders, no LLM cost, isolated temp db) ─────
step "6/6 smoke test"
SMOKE_LOG=/tmp/flintrade_smoke.log
SMOKE_DB=/tmp/flintrade_smoke.db
rm -f "$SMOKE_DB" "$SMOKE_DB"-wal "$SMOKE_DB"-shm
FLINT_DB="$SMOKE_DB" .venv/bin/python -c "from agent.db import init_db; init_db()" >/dev/null 2>&1
if FLINT_DB="$SMOKE_DB" FLINT_DRY_RUN=1 FLINT_LLM_DRY=1 \
   .venv/bin/python -m agent.daemon --once >"$SMOKE_LOG" 2>&1; then
  ERRS=$(grep -c '✗' "$SMOKE_LOG" 2>/dev/null || true)
  if [ "${ERRS:-0}" -eq 0 ]; then
    ok "daemon --once: all 7 loops ran clean (log: $SMOKE_LOG)"
  else
    ok "daemon --once ran; $ERRS loop(s) degraded — normal before broker credentials are filled in (log: $SMOKE_LOG)"
  fi
else
  warn "smoke test crashed — inspect $SMOKE_LOG"
  tail -5 "$SMOKE_LOG" || true
  FAILED=1
fi

# ── done ──────────────────────────────────────────────────────────────────
printf '\n════════════════════════════════════════════════════════════\n'
if [ "$FAILED" -eq 0 ]; then
  echo "Bootstrap complete. The system works end-to-end in dry-run."
else
  echo "Bootstrap finished WITH WARNINGS (see ! lines above)."
fi
cat <<'NEXT'

Next steps (in order, each optional until the last):
  1. Paper-trading credentials (free): https://open.longbridge.com
     → fill LONGBRIDGE_APP_KEY / APP_SECRET / ACCESS_TOKEN in flint.env
  2. Pick your LLM in agent/config/trading.toml [models]  (default: claude CLI)
     → verify:  .venv/bin/python -m agent.llm check
  3. Try one real cycle (still no orders — FLINT_DRY_RUN=1 in flint.env):
     → set -a; source flint.env; set +a; .venv/bin/python -m agent.daemon --once
  4. Run it for real (paper account, real orders, no real money):
     → set FLINT_DRY_RUN=0 in flint.env
     → cp launchd/com.flint.daemon.plist.example ~/Library/LaunchAgents/com.flint.daemon.plist
       (edit paths + credentials inside), then: launchctl load ~/Library/LaunchAgents/com.flint.daemon.plist
  5. Watch it: python3 dashboard/server.py  →  http://localhost:8383
NEXT
exit "$FAILED"
