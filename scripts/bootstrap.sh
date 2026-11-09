#!/bin/bash
# flintrade bootstrap — fresh clone → verified dry-run, one command, macOS.
#
#   bash scripts/bootstrap.sh
#
# Idempotent: safe to re-run. Never places orders, never calls a model, and
# never needs credentials: the smoke test runs every loop once in dry-run
# mode with model calls stubbed, against a throwaway root (temp database,
# temp logs, empty credentials) so it cannot touch an existing install.
# Designed so that a human OR a coding agent can go from `git clone` to a
# passing smoke test without reading anything else. Next steps are printed
# at the end.
set -uo pipefail

FLINTRADE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$FLINTRADE_DIR"
PASS="✓"; WARN="!"

step()  { printf '\n── %s ──\n' "$1"; }
ok()    { printf '%s %s\n' "$PASS" "$1"; }
warn()  { printf '%s %s\n' "$WARN" "$1"; }

FAILED=0

# ── 1. Go (version pinned by go.mod) ─────────────────────────────────────
step "1/6 Go"
GOMIN="$(awk '/^go /{print $2; exit}' go.mod)"
if ! command -v go >/dev/null 2>&1; then
  echo "go not found. Install it first:  brew install go   (need >= $GOMIN)" >&2
  exit 1
fi
# Run inside the module: with GOTOOLCHAIN=auto an older go fetches the
# go.mod toolchain itself; with GOTOOLCHAIN=local it fails here, clearly.
if ! GOVER="$(go env GOVERSION 2>/dev/null)"; then
  go env GOVERSION >/dev/null   # repeat, letting the error through
  echo "go is too old for go.mod (needs >= $GOMIN):  brew upgrade go" >&2
  exit 1
fi
if ! printf '%s\n%s\n' "go$GOMIN" "$GOVER" | sort -V -C; then
  echo "go is $GOVER; need >= $GOMIN.  brew upgrade go" >&2
  exit 1
fi
ok "$GOVER"

# ── 2. longbridge CLI (broker: quotes + paper-trading orders) ────────────
step "2/6 longbridge CLI"
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

# ── 3. build ──────────────────────────────────────────────────────────────
step "3/6 build"
if go build -o bin/ ./cmd/... && [ -x bin/flintrade ] && [ -x bin/flintrade-daemon ]; then
  ok "bin/flintrade + bin/flintrade-daemon built"
else
  echo "go build failed — nothing below can run without the binaries" >&2
  exit 1
fi
mkdir -p logs

# ── 4. credentials file (template only — never real values) ──────────────
# Same lookup order as internal/creds: $FLINTRADE_CREDENTIALS, else the
# Application Support file if it exists, else <repo>/flintrade.env. A fresh
# install gets the Application Support file: outside the repo, so git,
# exports and repo-reading agents never see it. An existing file is never
# overwritten — creating one would shadow it with blank values.
step "4/6 credentials file"
APPSUPPORT_CRED="$HOME/Library/Application Support/Flintrade/credentials.env"
if [ -n "${FLINTRADE_CREDENTIALS:-}" ]; then
  CRED="$FLINTRADE_CREDENTIALS"
elif [ -f "$APPSUPPORT_CRED" ]; then
  CRED="$APPSUPPORT_CRED"
elif [ -f flintrade.env ]; then
  CRED="$FLINTRADE_DIR/flintrade.env"
else
  CRED="$APPSUPPORT_CRED"
fi
if [ -f "$CRED" ]; then
  ok "kept existing $CRED"
else
  CRED_DIR="$(dirname "$CRED")"
  [ -d "$CRED_DIR" ] || mkdir -p -m 700 "$CRED_DIR"
  install -m 600 flintrade.env.example "$CRED"
  ok "created $CRED from flintrade.env.example (blank keys, FLINTRADE_DRY_RUN=1)"
fi
PERM="$(stat -f '%Lp' "$CRED")"
case "$PERM" in
  600|400) ok "permissions $PERM" ;;
  *) chmod 600 "$CRED" && ok "permissions were $PERM — tightened to 600" ;;
esac

# ── 5. dry-run smoke test (no orders, no LLM calls, isolated root) ───────
step "5/6 smoke test"
SMOKE_LOG="$FLINTRADE_DIR/logs/bootstrap-smoke.log"
SMOKE_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/flintrade-smoke.XXXXXX")"
trap 'rm -rf "$SMOKE_ROOT"' EXIT
# The daemon finds its root via FLINTRADE_DIR (the dir holding agent/config);
# config and prompts are linked in read-only, everything it writes lands in
# the temp root. An empty credentials file keeps any real one out of it.
mkdir -p "$SMOKE_ROOT/agent"
ln -s "$FLINTRADE_DIR/agent/config"  "$SMOKE_ROOT/agent/config"
ln -s "$FLINTRADE_DIR/agent/prompts" "$SMOKE_ROOT/agent/prompts"
install -m 600 /dev/null "$SMOKE_ROOT/credentials.env"
FLINTRADE_DIR="$SMOKE_ROOT" \
FLINTRADE_DB="$SMOKE_ROOT/flintrade.db" \
FLINTRADE_CREDENTIALS="$SMOKE_ROOT/credentials.env" \
FLINTRADE_LLM_LOG_DIR="$SMOKE_ROOT/llm_calls" \
FLINTRADE_LLM_DRY=1 \
  bin/flintrade-daemon --once --dry-run >"$SMOKE_LOG" 2>&1
SMOKE_RC=$?
RAN=$(grep -c 'ran once$' "$SMOKE_LOG" 2>/dev/null || true)
BAD=$(grep '\[daemon\] ✗' "$SMOKE_LOG" 2>/dev/null | sed -E 's/.*✗ ([a-z_]+):.*/\1/' | tr '\n' ' ')
if [ "$SMOKE_RC" -eq 0 ]; then
  ok "flintrade-daemon --once --dry-run: all $RAN loops ran clean (log: $SMOKE_LOG)"
elif grep -q '\[daemon\] --once' "$SMOKE_LOG" 2>/dev/null; then
  ok "flintrade-daemon --once --dry-run: $RAN loops clean; degraded: ${BAD}— normal before broker credentials are filled in (log: $SMOKE_LOG)"
else
  warn "smoke test crashed before running the loops — inspect $SMOKE_LOG"
  tail -5 "$SMOKE_LOG" || true
  FAILED=1
fi

# ── 6. self-check (read-only) ────────────────────────────────────────────
step "6/6 bin/flintrade check"
# A fresh clone has no ledger yet (the daemon creates flintrade.db on its first
# real start), so the ledger section reads the smoke-test database instead.
CHECK_ENV=(FLINTRADE_CREDENTIALS="$CRED")
if [ ! -f "${FLINTRADE_DB:-$FLINTRADE_DIR/flintrade.db}" ]; then
  CHECK_ENV+=(FLINTRADE_DB="$SMOKE_ROOT/flintrade.db")
fi
CHECK_OUT="$(env "${CHECK_ENV[@]}" bin/flintrade check 2>&1)"
echo "$CHECK_OUT"
# Missing/blank Longbridge keys are expected until step 1 below; anything
# else marked ✗ (config, models, ledger) is a real problem.
CRED_FAILS=$(printf '%s\n' "$CHECK_OUT" | grep '✗' | grep -c 'LONGBRIDGE_' || true)
OTHER_FAILS=$(printf '%s\n' "$CHECK_OUT" | grep '✗' | grep -vc 'LONGBRIDGE_' || true)
if [ "${OTHER_FAILS:-0}" -gt 0 ]; then
  warn "$OTHER_FAILS check(s) failed above — for models: pick a provider in agent/config/trading.toml [models]"
  echo '     claude-cli (default, needs the `claude` CLI) | kimi-cli | kimi-plan | anthropic | openai | deepseek | moonshot | openrouter | ollama | openai_compatible'
  echo "     put any API key in $CRED and re-run: bin/flintrade check"
  FAILED=1
elif [ "${CRED_FAILS:-0}" -gt 0 ]; then
  ok "config and models OK; Longbridge credentials not filled in yet (step 1 below)"
else
  ok "self-check passed"
fi

# ── done ──────────────────────────────────────────────────────────────────
printf '\n════════════════════════════════════════════════════════════\n'
if [ "$FAILED" -eq 0 ]; then
  echo "Bootstrap complete. The system works end-to-end in dry-run."
else
  echo "Bootstrap finished WITH WARNINGS (see ! lines above)."
fi
PLIST_SRC="launchd/com.flintrade.daemon.plist.example"
[ -f "$PLIST_SRC" ] || PLIST_SRC="launchd/com.flintrade.daemon.plist"
cat <<NEXT

Next steps (in order, each optional until the last):
  1. Paper-trading credentials (free): https://open.longbridge.com
     → fill LONGBRIDGE_APP_KEY / APP_SECRET / ACCESS_TOKEN in:
       $CRED
     → verify:  bin/flintrade check
  2. Pick your LLM in agent/config/trading.toml [models]  (default: claude CLI)
     → API keys go in the same credentials file
     → verify:  bin/flintrade check  (free),  bin/flintrade ping  (one paid call per tier)
  3. Try one real cycle — real model calls, still no orders; creates flintrade.db:
     → bin/flintrade-daemon --once --dry-run
  4. Run it persistently under launchd (dry run until you say otherwise):
     → sed -e "s|/path/to/flintrade|\$PWD|g" -e "s|/path/to/home|\$HOME|g" \\
           $PLIST_SRC > ~/Library/LaunchAgents/com.flintrade.daemon.plist
     → launchctl bootstrap gui/\$(id -u) ~/Library/LaunchAgents/com.flintrade.daemon.plist
     Then, to place real orders on the paper account (no real money):
     → set FLINTRADE_DRY_RUN=0 in the credentials file
     → launchctl kickstart -k gui/\$(id -u)/com.flintrade.daemon
  5. Watch it: bin/flintrade   ·   python3 dashboard/server.py  →  http://localhost:8383
NEXT
exit "$FAILED"
