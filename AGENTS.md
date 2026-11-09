# AGENTS.md — replication contract for coding agents

You are an AI coding agent asked to set up **flintrade** (an autonomous
LLM paper-trading daemon for US equities) on the user's local Mac. This file
is the complete contract: follow it top to bottom and you will not need to
improvise.

## TL;DR

```bash
bash scripts/bootstrap.sh
```

One idempotent command: checks Go (≥ 1.27), installs the `longbridge` CLI
(Homebrew), builds `bin/flintrade` and `bin/flintrade-daemon`, writes a credentials
template outside the repo, runs a dry-run smoke test of all eight loops
against a throwaway database, then the read-only self-check `bin/flintrade check`.
It places **no orders**, spends **no LLM tokens**, and needs **no
credentials**. It exits 0 when everything it could verify passed and prints
the numbered next steps for the user. Before credentials are filled in,
`bin/flintrade check` reports the Longbridge keys as missing — that is expected
and does not fail the bootstrap.

## What this system is (30 seconds)

One Go process (`bin/flintrade-daemon`, source in `cmd/flintrade-daemon`) runs eight
loops as goroutines: three LLM signal producers (technical / news-event /
news collector) submit trade *intents* into SQLite (`flintrade.db`); one
**executor** consumes them through a code-enforced portfolio risk gate and
is the only thing that ever places orders; a **reconciler** audits fills; a
**risk monitor** can halt everything; a **reflect** loop "dreams" nightly,
distilling trades into lessons that feed back into future prompts; an
**invariants** loop checks the ledger read-only. `bin/flintrade` is the
operator CLI. LLM proposes, code disposes.

## Credentials file

The daemon and `bin/flintrade` read one shell-style file (`export KEY='value'`,
no `$` expansion). Lookup order:

1. `$FLINTRADE_CREDENTIALS`, if set;
2. `~/Library/Application Support/Flintrade/credentials.env`, if it exists
   (what bootstrap creates — outside the repo, so git and repo-reading
   tools never see it);
3. `<repo>/flintrade.env` (gitignored).

Keep it `chmod 600` (the daemon and `bin/flintrade check` warn otherwise).
Values in the file **override** the shell environment, `FLINTRADE_DRY_RUN`
included. The daemon loads it itself on every start; the launchd plist
carries no credentials. The template is `flintrade.env.example`.

## After bootstrap: the three user decisions

Do not make these silently for the user — surface them:

1. **Broker credentials** (free paper account): https://open.longbridge.com →
   fill `LONGBRIDGE_APP_KEY` / `LONGBRIDGE_APP_SECRET` /
   `LONGBRIDGE_ACCESS_TOKEN` in the credentials file. Let the user paste
   them into the file themselves; `bin/flintrade check` then validates them
   (token parse, app-key match, expiry, paper vs live) without printing them.
2. **LLM provider**: default is the local `claude` CLI (zero config with
   Claude Code). Subscription plans ride their CLIs: `claude-cli` (Claude)
   and `kimi-cli` (Kimi Code plan, after `kimi login`) — no API key either
   way. A shared Kimi Code plan key works WITHOUT any subscription or CLI:
   `kimi-plan` (Anthropic-compatible endpoint, `KIMI_PLAN_API_KEY`, models
   `kimi-for-coding` / `kimi-for-coding-highspeed` / `k3`). API-key vendors:
   edit `agent/config/trading.toml` `[models]` (providers: `anthropic`,
   `openai`, `deepseek`, `moonshot`, `openrouter`, `ollama`,
   `openai_compatible`) and put the matching key in the credentials file.
   Verify with `bin/flintrade check` (free) and `bin/flintrade ping` (one paid
   round-trip per tier).
3. **Go live on the paper account**: set `FLINTRADE_DRY_RUN=0` in the
   credentials file and restart the daemon. Until then (and whenever
   `FLINTRADE_DRY_RUN` is unset) the daemon simulates fills internally and never
   places orders.

## Build / test / run / deploy / observe

```bash
go build -o bin/ ./cmd/...        # bin/flintrade + bin/flintrade-daemon (bin/ is gitignored)
go test ./...                     # temp DBs, fake broker/CLI, no side effects

# foreground runs (read the credentials file like the daemon does)
bin/flintrade-daemon --once --dry-run # every loop once, simulated fills, then exit
bin/flintrade-daemon --seconds 120    # run 2 minutes, then shut down gracefully
FLINTRADE_LLM_DRY=1 bin/flintrade-daemon --once --dry-run   # also stub every model call (no tokens)

# persistent daemon under launchd (auto-restart, fd limit, watchdog) — details in launchd/README.md
sed -e "s|/path/to/flintrade|$PWD|g" -e "s|/path/to/home|$HOME|g" \
    launchd/com.flintrade.daemon.plist.example > ~/Library/LaunchAgents/com.flintrade.daemon.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.flintrade.daemon.plist

# after rebuilding the binaries or editing the credentials file
launchctl kickstart -k gui/$(id -u)/com.flintrade.daemon

# observe
bin/flintrade                         # status: loops, risk, positions, pending intents, recent trades
bin/flintrade check                   # read-only self-check
python3 dashboard/server.py       # http://localhost:8383 (still Python, read-only)
tail -f logs/daemon.out logs/daemon.err
```

`--once` exits non-zero when any loop fails; before broker credentials exist
the data-dependent loops fail, and that is expected. `FLINTRADE_DB=/tmp/x.db`
points either binary at a different database (useful for smoke tests);
`FLINTRADE_LLM_DRY` in the credentials file would override the shell, so do not
put it there.

Verification that it is actually alive: `sqlite3 flintrade.db "SELECT * FROM
heartbeats"` — every loop beats; `executor`/`reconciler`/`risk_monitor`
should be seconds old. `bin/flintrade check` lists the recent heartbeats too.

## Safety invariants — do not violate, do not "fix"

- The credentials file is the only place holding credentials. **Never
  commit it, never print it, never paste its contents into chat or logs.**
  `flintrade.env.example` and the plist template are the only versioned copies,
  with placeholders.
- `FLINTRADE_DRY_RUN` defaults to dry (`1` in the template, and unset means
  dry). Flipping to `0` sends real orders to a **paper** account (no real
  money) — still, only the user flips it.
- The LLM must never call the broker or write the database directly. If you
  extend the system, producers submit intents; only the executor executes.
  The role guard in `internal/db` enforces this — don't weaken it.
- Model choices in `trading.toml [models]` are pinned on purpose. Never
  leave `model` empty, never add silent cross-model fallback.
- Close every SQLite handle and result set you open (`defer rows.Close()`).
  Leaked connections exhausted the fd limit and silently halted trading for
  5 days once (2026-07-26). The daemon's watchdog now exits (code 70) on a
  stale loop or fd use above 90%, and launchd restarts it — keep it that way.
- One daemon at a time: `.daemon.lock` (flock) enforces it, so two
  executors can never double-consume intents. Don't remove it.
- Hard trading rules live in `internal/riskgate` / `internal/riskmonitor`
  and `agent/config/risk.toml`, never in a prompt.

## Troubleshooting map

| symptom | look at |
|---|---|
| daemon exits right after start | `logs/daemon.err`: credentials file missing/unparsable, or another daemon holds `.daemon.lock` |
| a loop stops beating | `logs/daemon.out`, `logs/daemon.err`; the watchdog restarts the process after max(6×cadence, 30 min) |
| `[loop_technical] error: collect: …` / 401 | Longbridge token expired — regenerate at open.longbridge.com, update the credentials file, `launchctl kickstart -k gui/$(id -u)/com.flintrade.daemon` (`bin/flintrade check` shows the expiry date) |
| LLM parse failures / constant WAIT | `bin/flintrade ping`; check provider status/key; raw calls in `llm_calls/YYYYMMDD.jsonl` |
| orders rejected 603085 | session/outside-RTH flag mismatch — check `internal/session` vs broker session |
| plist change has no effect | `kickstart` does not reread the plist: `launchctl bootout gui/$(id -u)/com.flintrade.daemon`, then `launchctl bootstrap` again |

## Repo map (where to change what)

- Trading behavior / prompts: `agent/prompts/`, `internal/producers/`
- Risk limits (hot-reloadable): `agent/config/risk.toml`
- Universe / cadence / models: `agent/config/trading.toml`
- Strategy playbooks (symbol × strategy bindings): `agent/config/strategies.toml`
- Execution & risk gate: `internal/executor`, `internal/riskgate`,
  `internal/settle`, `internal/broker`
- Memory: `internal/reflect` (dreaming), `internal/postmortem` (per-trade
  review), `internal/memory` (`similar_history`, SQL over closed trades)
- Market data / indicators: `internal/collect`, `internal/indicators`
  (`bin/flintrade collect` prints the payload the model sees)
- Loop wiring / supervision: `cmd/flintrade-daemon`
- Backtests: `backtest/` (Python, pure stdlib, offline) using
  `scripts/indicators.py`; a golden test
  (`internal/indicators/testdata/python_golden.json`) keeps it bit-identical
  with `internal/indicators` — change one, change both.

After any change under `cmd/` or `internal/`: `go test ./...`, rebuild,
then restart the daemon.
