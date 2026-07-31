# AGENTS.md — replication contract for coding agents

You are an AI coding agent asked to set up **flintrade** (an autonomous
LLM paper-trading daemon for US equities) on the user's local Mac. This file
is the complete contract: follow it top to bottom and you will not need to
improvise.

## TL;DR

```bash
bash scripts/bootstrap.sh
```

One idempotent command: venv, optional semantic-memory deps, `longbridge`
CLI (Homebrew), config templates, database schema, LLM provider check, and a
full dry-run smoke test of all seven loops. It places **no orders**, spends
**no LLM tokens**, and needs **no credentials**. It exits 0 when the system
is verified end-to-end and prints the numbered next steps for the user.

## What this system is (30 seconds)

**Naming**: the repo/distribution is **flintrade**; the agent's internal
name is **Flint**. Expect `flint.env`, `flint.db`, `FLINT_*` env vars, and
the launchd label `com.flint.daemon` — that's intentional, don't rename.

A single Python process (`agent/daemon.py`) runs seven loops as threads:
three LLM signal producers (technical / news-event / news collector) submit
trade *intents* into SQLite (`flint.db`); one **executor** consumes them
through a code-enforced portfolio risk gate and is the only thing that ever
calls the broker; a **reconciler** audits fills; a **risk monitor** can halt
everything; a **reflect** loop "dreams" nightly, distilling trades into
lessons that feed back into future prompts. LLM proposes, code disposes.

## After bootstrap: the three user decisions

Do not make these silently for the user — surface them:

1. **Broker credentials** (free paper account): https://open.longbridge.com →
   fill `LONGBRIDGE_APP_KEY` / `APP_SECRET` / `ACCESS_TOKEN` in `flint.env`.
2. **LLM provider**: default is the local `claude` CLI (zero config with
   Claude Code). Subscription plans ride their CLIs: `claude-cli` (Claude)
   and `kimi-cli` (Kimi Code plan, after `kimi login`) — no API key either
   way. A shared Kimi Code plan key works WITHOUT any subscription or CLI:
   `kimi-plan` (Anthropic-compatible endpoint, `KIMI_PLAN_API_KEY`, models
   `kimi-for-coding` / `kimi-for-coding-highspeed` / `k3`). API-key vendors:
   edit `agent/config/trading.toml` `[models]` (providers: `anthropic`,
   `openai`, `deepseek`, `moonshot`, `openrouter`, `ollama`,
   `openai_compatible`) and export the matching key in `flint.env`. Verify
   with `.venv/bin/python -m agent.llm check` (free) and
   `.venv/bin/python -m agent.llm ping` (one paid round-trip).
3. **Go live on the paper account**: set `FLINT_DRY_RUN=0` in `flint.env`
   (and in the launchd plist if installing it). Until then the daemon
   simulates fills internally and never calls the broker.

## Run / deploy / observe

```bash
# one full cycle in the foreground (respects flint.env)
set -a; source flint.env; set +a
.venv/bin/python -m agent.daemon --once

# persistent daemon under launchd (auto-restart, fd limits, watchdog)
cp launchd/com.flint.daemon.plist.example ~/Library/LaunchAgents/com.flint.daemon.plist
$EDITOR ~/Library/LaunchAgents/com.flint.daemon.plist   # fix paths + credentials
launchctl load ~/Library/LaunchAgents/com.flint.daemon.plist

# observe
python3 dashboard/server.py            # http://localhost:8383
bash scripts/agentctl.sh user_cli status
tail -f logs/daemon.out
```

Verification that it is actually alive: `sqlite3 flint.db "SELECT * FROM
heartbeats"` — every loop beats; `executor`/`reconciler`/`risk_monitor`
should be seconds old.

## Safety invariants — do not violate, do not "fix"

- `flint.env` and the filled-in plist contain credentials. **Never commit
  them, never paste their contents into chat or logs.** Templates
  (`flint.env.example`, `*.plist.example`) are the only versioned copies.
- `FLINT_DRY_RUN` defaults to `1`. Flipping to `0` sends real orders to a
  **paper** account (no real money) — still, only the user flips it.
- The LLM must never call the broker or write the database directly. If you
  extend the system, producers submit intents; only the executor executes.
  The role guard in `agent/db.py` enforces this — don't weaken it.
- Model choices in `trading.toml [models]` are pinned on purpose. Never
  leave `model` empty, never add silent cross-model fallback.
- SQLite connections must be closed explicitly (`with DB(...) as db:`).
  Python ≥3.13 does not close them on GC promptly; leaked connections
  exhausted the fd limit and silently halted trading for 5 days once
  (2026-07-26). The daemon watchdog now self-restarts on stale loops or fd
  pressure — keep it that way.

## Troubleshooting map

| symptom | look at |
|---|---|
| a loop stops beating | `logs/daemon.out`, `logs/daemon.err`; watchdog restarts within ~30 min |
| `collect failed` / 401 | Longbridge token expired — regenerate at open.longbridge.com, update `flint.env` **and** plist, then `launchctl unload && load` (kickstart does NOT reread plist env) |
| LLM parse failures / constant WAIT | `.venv/bin/python -m agent.llm ping`; check provider status/key |
| orders rejected 603085 | session/outside-RTH flag mismatch — check `agent/session.py` vs broker session |
| semantic memory "skipped" | optional; `pip install lancedb fastembed` in `.venv` to enable |

## Repo map (where to change what)

- Trading behavior / prompts: `agent/prompts/`, `agent/producers/`
- Risk limits (hot-reloadable): `agent/config/risk.toml`
- Universe / cadence / models: `agent/config/trading.toml`
- Execution & risk gate: `agent/executor.py`, `agent/risk_gate.py`
- Memory: `agent/reflect.py` (dreaming), `agent/postmortem.py` (per-trade
  review), `agent/memory_store.py` (semantic recall)
- Backtests: `backtest/` (pure stdlib, offline)
