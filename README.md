*[中文](README.zh-CN.md)*

# flintrade

An autonomous paper-trading agent that dreams, remembers, and reviews its own trades.

## What it is

flintrade is a single-process, multi-loop daemon that paper-trades US equities. Internally it runs as **seven concurrent loops implemented as threads**: three signal producers (technical/Arena, event/catalyst, news) write trade *intents* into a SQLite queue; an **executor** thread consumes that queue through a hard-coded portfolio risk gate before it ever touches a broker; a **reconciler** keeps the book honest against real fills; a **risk monitor** watches for drawdown and can halt trading; and a **dreaming** loop synthesizes memory during market-closed windows.

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

**Pluggable LLM tiers.** `agent/llm.py` separates a trader tier (frontier model, the real edge) from a flash tier (cheap model for dreaming and postmortems), configured in `agent/config/trading.toml` — swapping providers (Claude today, OpenAI-compatible endpoints supported) touches only config. Embeddings default to local fastembed with no external dependency at inference time.

## Quickstart

Prerequisites:
- Python 3.12+
- [Longbridge OpenAPI](https://open.longbridge.com) paper-trading credentials, plus the `longbridge` CLI
- The `claude` CLI (used as the LLM backend)

```bash
# 1. Credentials — copy the template and fill in your paper-trading keys
cp flint.env.example flint.env
$EDITOR flint.env

# 2. Memory subsystem deps (local vector store + embeddings)
python3 -m venv .venv
.venv/bin/pip install lancedb fastembed

# 3. Initialize the database
.venv/bin/python -m agent.db

# 4. Dry run — simulates fills internally, places NO real orders
FLINT_DRY_RUN=1 .venv/bin/python -m agent.daemon

# 5. When you're ready to send real paper orders, flip the switch
FLINT_DRY_RUN=0 .venv/bin/python -m agent.daemon
```

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
