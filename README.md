*[中文](README.zh-CN.md)*

# flintrade

An autonomous paper-trading agent that dreams, remembers, and reviews its own trades.

## What it is

flintrade is a single-process, multi-loop daemon that paper-trades US equities. (Naming: **flintrade** is the repo; the agent calls itself **Flint** — hence `flint.env`, `flint.db`, `FLINT_*` env vars, and the `com.flint.daemon` launchd label.) Internally it runs as **seven concurrent loops implemented as threads**: three signal producers (technical/Arena, event/catalyst, news) write trade *intents* into a SQLite queue; an **executor** thread consumes that queue through a hard-coded portfolio risk gate before it ever touches a broker; a **reconciler** keeps the book honest against real fills; a **risk monitor** watches for drawdown and can halt trading; and a **dreaming** loop synthesizes memory during market-closed windows.

The core philosophy, carried over from the project's original ["Arena"](docs/agent-architecture.md) design and never relaxed: **the LLM proposes, deterministic code disposes.** The model reasons about direction, sizing rationale, and thesis. It never calls the broker and never writes the books — only the executor does that, and only after every proposal clears a risk gate that is plain Python, not a prompt.

## Architecture

```
 signal producers (async, each with its own LLM)         single authority (serial)         sleep window
┌───────────────────────────────┐
│ loop_technical   (Arena)       │──intent──┐
│ loop_event       (catalyst)    │──intent──┤     ┌──────────────┐      ┌───────────┐
│ news_collector   (news)        │──signal──┤────▶│   Executor    │      │  reflect  │
│ user_cli         (manual)      │──intent──┘     │ risk gate +   │─────▶│ dreaming  │
└───────────────────────────────┘                │ order + write │      │ (flash    │
                                                  └──────┬────────┘      │  tier LLM)│
       ┌──────────────┐   broker fills (after-hours/     │ only caller   └───────────┘
       │ risk_monitor │◀── partial/stop/etc.)            │ of `longbridge`
       │ circuit       │                                 ▼
       │ breaker       │                          ┌──────────────┐
       └──────┬────────┘                          │  flint.db    │  single source
              │ FLATTEN intent      ┌───────────┐  │ (SQLite/WAL) │  of truth
              └────────────────────▶│reconciler │─▶└──────────────┘
                                    └───────────┘
       supervisor: launchd KeepAlive + heartbeat watchdog over all loops
```

One rule underlies everything: **producers only ever write to the `intents` table; only the executor writes `positions`, `trades`, and `capital`, and only the executor calls `longbridge buy/sell`.** Every proposal, whether it comes from a technical signal, a news event, or you typing a command, queues through the same risk gate.

## Why it's interesting

**Arena invariant & single-writer.** `agent/db.py` is a role-guarded access layer over a WAL-mode SQLite database — each connection is opened with a role (`technical`, `executor`, `reflect`, `reader`, ...) and the layer raises if that role attempts a write it isn't permitted. Only the executor role can write positions/trades/capital. The `intents` table is the message bus between producers and the executor; nothing talks to anything else directly.

**Portfolio risk gate.** `agent/risk_gate.py` (config in `agent/config/risk.toml`) evaluates every intent through a fixed pipeline before anything is sized: halt check, dedup, no-revenge cooldown, session/volume filters, then risk-based position sizing (`max_risk_pct × equity / stop distance` — not a confidence-scaled guess), correlation-cluster exposure caps (mega-tech / gold / silver / oil buckets so four correlated names don't masquerade as diversification), in-flight order exposure counting (working orders count against limits before they fill), and a circuit breaker that halts all new entries on a daily drawdown threshold. None of this is LLM-mediated — it's plain, hot-reloadable config and code.

**Dreaming memory.** `agent/reflect.py` runs during closed-market windows. It maintains a bounded set of at most 20 lessons (atomic rewrite each cycle, confidence decay over time, archived below a floor) and dated plans that expire on their own. Recall is time-anchored — the agent knows how long it's been since its last dream or last trade before it reasons about anything — and includes semantic recall of similar historical setups via a local vector store (LanceDB + fastembed, on-device ONNX embeddings, no embedding API cost).

**Self-postmortem & calibration.** `agent/postmortem.py` reviews every closed strategy trade with a structured, flash-tier-model verdict: was the thesis right, how were entry/exit timing, how much did it slip against the stop. Hard honesty rules are enforced in the synthesis prompt — a statistic with n<10 can never be phrased as a rule, only as preliminary. The agent's actual track record (sample size, win rate, net P&L) is computed in plain code and injected into every decision as `self_assessment`, so the model can't quietly forget how it's really doing.

**Pluggable LLM tiers.** `agent/llm.py` is a declarative provider registry over three wire protocols (Claude CLI subprocess, OpenAI chat/completions, Anthropic Messages API) — Claude, OpenAI, DeepSeek, Kimi, OpenRouter, local Ollama, or any OpenAI-compatible endpoint, all selected per tier in `agent/config/trading.toml` with keys read from env only. A trader tier (frontier model, the real edge) is separated from a flash tier (cheap model for dreaming and postmortems). Failures retry with backoff but never fall back to a different model — a trading system shouldn't swap brains mid-flight. Embeddings default to local fastembed with no external dependency at inference time.

## Quickstart — one command

```bash
git clone https://github.com/UncertaintyDeterminesYou4ndMe/flintrade && cd flintrade && bash scripts/bootstrap.sh
```

That's it. On a fresh Mac, `bootstrap.sh` sets up the venv, installs the `longbridge` CLI via Homebrew, initializes the database, checks your LLM provider, and runs a full dry-run smoke test (all seven loops, zero orders, zero LLM cost). It's idempotent and ends by printing exactly what to do next — get free [paper-trading credentials](https://open.longbridge.com), pick an LLM, watch a dry run, then flip `FLINT_DRY_RUN=0`.

**Using an AI coding agent?** Paste this into Claude Code / any coding agent:

> Clone https://github.com/UncertaintyDeterminesYou4ndMe/flintrade, run `bash scripts/bootstrap.sh`, then follow AGENTS.md.

`AGENTS.md` gives an agent the full replication contract: setup, verification commands, config knobs, and the safety invariants it must not violate.

### Choosing your LLM

No hard dependency on any one vendor. The default is the local `claude` CLI (no API key needed if you have Claude Code); otherwise pick any provider in `agent/config/trading.toml [models]`:

| provider | wire | key env |
|---|---|---|
| `claude-cli` (default) | Claude Code CLI — rides your Claude subscription | — |
| `kimi-cli` | kimi CLI — rides your Kimi Code plan (`kimi login`) | — |
| `kimi-plan` | Kimi Code plan key, direct API (Anthropic-compatible) — works with a key someone shares with you, no subscription or CLI needed | `KIMI_PLAN_API_KEY` |
| `anthropic` | Anthropic Messages API | `ANTHROPIC_API_KEY` |
| `openai` / `deepseek` / `moonshot` (Kimi open platform, pay-per-token) / `openrouter` | OpenAI chat/completions | `OPENAI_API_KEY` / `DEEPSEEK_API_KEY` / … |
| `ollama` | local, no key | — |
| `openai_compatible` | any compatible endpoint (`base_url` in config) | configurable |

Subscription plans (Claude Code, Kimi Code) are wired through their own CLIs in headless mode — the CLI owns auth and plan billing, flintrade just runs it as a subprocess. Note `kimi-cli` (subscription) and `moonshot` (open-platform API key) are different providers on purpose.

```bash
.venv/bin/python -m agent.llm check   # verify resolution + keys, no cost
.venv/bin/python -m agent.llm ping    # one real round-trip per tier
```

Two tiers: `trader` (decisions — use a frontier model) and `flash` (dreaming/postmortems — use the cheapest thing you trust). Deliberate design choice: **no automatic cross-model fallback** — if the configured model fails after retries, the agent WAITs instead of trading with a different brain.

`FLINT_DRY_RUN=1` is the default posture in `flint.env.example` — the daemon runs its full decision loop and simulates fills without ever calling the broker. Only flip it to `0` once you've watched it run.

For a persistent deployment, see `launchd/com.flint.daemon.plist.example` as a template for running the daemon under launchd (macOS) with auto-restart.

Two operational views:

```bash
bash scripts/agentctl.sh user_cli status   # CLI status view (positions, risk state, recent intents)
python3 dashboard/server.py                # web dashboard at http://localhost:8383
```

## Repo map

| Path | What's there |
|---|---|
| `agent/` | The daemon — producers, executor, risk gate, reconciler, risk monitor, dreaming/postmortem, DB layer, LLM access layer |
| `scripts/` | Data collectors and operational CLI (`agentctl.sh`) |
| `backtest/` | Backtest engines, factor experiments, ML gate research |
| `dashboard/` | Read-only web UI over the trade log and database |
| `docs/` | Architecture notes |
| `launchd/` | macOS launchd deployment template |

## Safety & disclaimer

flintrade is paper-trading only by design, and dry-run (`FLINT_DRY_RUN=1`) is the default in the example config — no order reaches a broker unless you deliberately flip that switch, and even then it targets a paper-trading account, not real capital. This is an educational and research project exploring how far a constrained LLM-proposes/code-disposes architecture can go, not a trading product.

**This is not financial advice.** Nothing here is a recommendation to buy, sell, or hold any security. Use it entirely at your own risk.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
