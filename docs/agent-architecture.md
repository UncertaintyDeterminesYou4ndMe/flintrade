---
name: Flintrade Agent Architecture (Daemon Plan)
description: Flintrade 从 cron 单次决策升级为常驻多进程交易 Agent 的设计。多个异步信号生产者(技术面/舆情/资讯/用户)只提交 intent,单一执行权威(Executor)做组合风控 + 下单 + 单一写者,Reconciler 持续对账,Dreaming 在睡眠期合成记忆。去掉一仓制与固定本金,以组合风控纪律取代。
type: project
supersedes: arena-architecture.md
---

# Flintrade Agent Architecture (Daemon)

> **演进说明(2026-08-21):** v1 Arena 路径(`run.sh` / `prompt.md` / `state.json`)已**删除**。
> 下文出现的 v1↔v2 对照表是历史参照,不是现状;§6/§7 里"Executor 导出 state.json 投影"
> 这条兼容措施已随之取消 —— dashboard 与 backtest 直接读 `flintrade.db`。
> v1 账本已迁入 db(`trades.attribution='legacy'`),`migrate.py` 完成使命后一并移除。

> **演进说明(2026-06):** 下文按"多进程"描述各 loop。实现上已**合并为单进程**
> `agent/daemon.py`:同样的 loop,改为一个进程里的多线程,各按 cadence 调 `run_once()`。
> 解耦不变(只通过 SQLite 队列对话),单一写者由 db role guard 按 handle 角色强制(与进程无关),
> 崩溃隔离改为每 loop try/except + launchd 对单 job 的 KeepAlive。
> 语义记忆后端 = **LanceDB + fastembed**(进程内 ONNX 小模型),装在 `.venv`;
> LLM 走 `agent/llm.py` 可插拔 provider(chat 现 Claude,embed 现 fastembed)。
> 部署 = 一个 `com.flintrade.daemon` plist,跑 `.venv/bin/python -m agent.daemon`。

## 0. 这次升级的本质

| | cron 时代(现状) | daemon 时代(本设计) |
|---|---|---|
| 触发 | launchd 定时跑一次 `run.sh`,跑完即死 | 多个常驻进程,事件/节奏驱动 |
| 信号来源 | 单一(技术面 LLM) | 多源并发:技术面 / 舆情 / 资讯 / 用户主动 |
| 仓位 | 一仓制,$1200 固定 | **多仓组合,动态权益,组合风控纪律** |
| state 写者 | 串行天然单写者(run.sh) | **显式单写者:Executor 进程** |
| 下单 | run.sh step5/6 | Executor(唯一对接 broker) |
| 对账 | run.sh 内一步 | 独立 Reconciler,持续运行 |
| 记忆 | 无(只有机械 pnl_feedback) | Dreaming 睡眠期合成 lessons/plans |

**保留的不变量(来自 Arena):** 代码拥有 I/O 与执行;LLM 只做分析、只输出结构化 intent,**永远不碰 broker、不写 state**。这条不变,只是从"bash 单线程"分布成"多生产者 → 单一权威"。

**被替换的规则:** `一仓制 + $1200 固定本金`(prompt.md §1)删除。其背后的真实目的——风控纪律——上移为 Executor 里的**组合风控引擎**(见 §3)。

---

## 1. 拓扑:感知 → 仲裁 → 执行 → 对账 → 做梦

```
  信号生产者(并发、异步、各自带 LLM 分析)        单一权威(串行)              睡眠期
┌──────────────────────────────┐
│ loop_technical  技术面(Arena)  │──intent─┐
│ loop_event      舆情/catalyst  │──intent─┤      ┌─────────────────┐    ┌──────────┐
│ news_collector  资讯抓取        │──signal─┤─────▶│    Executor      │    │ reflect  │
│ user_cli        用户主动交易     │──intent─┘      │ 风控Gate+下单+写state │───▶│ 做梦合成  │
└──────────────────────────────┘                 └────────┬────────┘    │ Haiku/Opus│
                                                          │ 唯一调 longbridge └──────────┘
        ┌──────────────┐   broker fills(含盘外/止损/部分)   │
        │ risk_monitor │◀──────────────┐                  ▼
        │ 熔断/急停HALT │               │           ┌─────────────┐
        └──────┬───────┘        ┌──────┴──────┐    │  flintrade.db    │ 单一真相
               │ FLATTEN intent  │ reconciler  │───▶│ (SQLite/WAL) │
               └────────────────▶│ 持续对账     │    └─────────────┘
                                 └─────────────┘
              supervisor(launchd KeepAlive + 心跳看门狗)守护以上所有进程
```

核心规则一句话:**生产者只写 `intents` 表;只有 Executor 写 `positions`/`trades`/`capital`、只有 Executor 调 `longbridge buy/sell`。** 任何策略想交易,都得排队过风控。

---

## 2. 共享底座:flintrade.db(SQLite,WAL 模式,零依赖)

为什么从 JSON 升级到 SQLite:不是因为数据大(8MB/年都算不上),而是 (a) 多进程并发读写需要原子事务,JSON 整文件重写会撕裂;(b) 高频后要频繁 `GROUP BY symbol,session,setup` 做聚合。stdlib `sqlite3` 即可,仍然零外部依赖,符合 CLAUDE.md 约束。

WAL 模式:多读者并发 + 单写者串行,天然吻合我们的写者模型。

表设计:

- **`positions`** — 当前持仓(可多条)。单一真相。`symbol, side, qty, entry_price, stop, target, risk_amt, opened_at, source, intent_id`
- **`trades`** — append-only 账本。每笔带 `source`(technical/event/user)、`intent_id`、`pnl`、当时的 `features` 快照(RSI/VWAP/session/score…用于做梦聚合)
- **`intents`** — **消息总线/队列**。生产者写,Executor 消费。`id, source, priority, symbol, side, hypothesis_qty, stop, target, confidence, dedup_key, created_at, status(pending/approved/rejected/filled/expired), reason`
- **`orders`** — broker 订单镜像。`client_order_id, broker_order_id, status, filled_qty, avg_price` — Reconciler 维护
- **`signals`** — 原始信号(尤其舆情),溯源用。`source, symbol, kind, payload, ts`
- **`events`** — 被追踪的 catalyst(财报/CPI/FOMC),带 `fires_at`,供 prompt 上下文与 event loop
- **`risk_state`** — 单行:`equity, day_start_equity, day_realized_pnl, open_risk, halt(bool), halt_reason, updated_at`
- **`memory_lessons`** / **`memory_plans`** — Dreaming 产物(见 §6)
- **`agg`** — 滚动聚合:`(symbol × session × setup × rsi_bucket) → trips, win_rate, pl_ratio, sample`
- **`heartbeats`** — `process, last_beat` 进程存活

`state.json` 去向:由 Executor 在每次写后导出一份投影(兼容现有 dashboard / backtest 读取),逐步迁移到直接读 db。

---

## 3. 组合风控引擎(替代一仓制 —— 这是"风控纪律"的真身)

Executor 收到每个 intent,**串行**过一道 Risk Gate。配置集中在 `config/risk.toml`,运行时可改、改了热加载:

```toml
[portfolio]
max_concurrent_positions = 4          # 取代"一仓制"
max_gross_exposure_pct   = 100        # 总持仓市值 / 权益;>100 需融资,默认不融
max_open_risk_pct        = 6          # 所有持仓 (entry-stop)*qty 之和 / 权益
max_per_symbol_pct       = 40         # 单票市值上限

[per_trade]
max_risk_pct = 3                      # 单笔最大风险(沿用现规则 §7)
volume_ratio_floor = 0.3              # 沿用现规则 §8
session_close_blackout_min = 30       # 沿用现规则 §9
revenge_cooldown_min = 60             # 沿用现规则 §7 no-revenge,量化成冷却

# 相关性簇:取代一仓制提供的"伪分散"。同簇高度相关,合并限额。
[clusters]
mega_tech = ["AAPL.US","MSFT.US","GOOGL.US","AMZN.US","NVDA.US","META.US","TSLA.US"]
gold      = ["GLD.US","UGL.US"]
silver    = ["SLV.US","AGQ.US"]
oil       = ["USO.US"]
max_per_cluster_pct = 60              # 防止"4 个仓全是 mega tech"的假分散

[circuit_breaker]
daily_loss_limit_pct = 5              # 当日权益回撤 >5% → HALT 全局停手
flatten_on_halt = false              # 是否自动平仓(默认仅停新单,留给人/risk_monitor 决定)
```

Risk Gate 对每个 intent 的裁决流水(全部硬编码、不经 LLM):
1. **HALT 检查** — `risk_state.halt=true` → 一律拒(除 FLATTEN/平仓类)。
2. **去重** — `dedup_key` 已存在 pending/filled → 拒(舆情同一新闻多次触发的防线)。
3. **冷却** — 该 symbol 在 `revenge_cooldown_min` 内刚平过亏损单 → 拒(no-revenge)。
4. **过滤器** — volume_ratio < floor、距收盘 < blackout → 拒。
5. **仓位定额** — 由 `max_risk_pct × equity / (entry-stop)` 反算 qty(风险定额,取代"按 confidence 半/全仓"的拍脑袋)。
6. **组合约束** — 加上这笔后是否突破 concurrent / gross / open_risk / per_symbol / per_cluster?突破则缩量或拒。
7. **冲突仲裁** — 同 symbol 已有反向 intent/持仓:按 priority(用户 > 舆情 > 技术面,可配)裁决,低优先级让路。
8. 通过 → 计算最终 qty、生成 client_order_id、下单、验单、写 positions/trades/capital、回写 intent.status。

**关键:风控从"prompt 里的英文规则、LLM 自觉遵守"变成"代码 Gate、不可绕过"。** LLM 仍负责"要不要交易、方向、止损位"(这是它的 edge),但"能不能交易、多大量"由风控引擎说了算。

---

## 4. 信号生产者(各自一个进程,各自带 LLM)

所有生产者**只产出 intent/signal,绝不下单**。这保住 Arena 不变量。

- **`loop_technical.py`** — 现有 Arena 决策搬过来。`collect.sh` 采数据 → LLM 分析 → 输出 intent(而非直接下单)。**频次是配置项**(现状~每 30min,可调到分钟级——这是你要的"高频"旋钮)。LLM 的输出 schema 从"action+order"改成"hypothesis intent(方向/止损/目标/置信)"。
- **`loop_event.py`(舆情)** — 消费 `news_collector` 写入的 `events`/`signals`,LLM 判断 catalyst 的方向与强度 → 高优先级 intent。带强 dedup(同一事件只触发一次)。
- **`news_collector.py`(资讯)** — 轮询新闻源(起点用 longbridge `market-data:news`;若 opencli 是具体工具,接它),写 `signals`/`events`。本身不交易,既喂 event loop 也喂技术面 prompt 的上下文。
- **`user_cli.py`(用户主动)** — 你打字下单 → 也变成 intent,**走同一道风控 Gate**。手动单不能绕过风控、不能直接写 state。这也是"随时问它现在啥情况"的聊天入口的落点。

---

## 5. 自治系统:Reconciler / Risk Monitor / Supervisor

- **`reconciler.py`** — 持续对账。**这里 WebSocket 终于值得上了**(cron 时代不值,daemon 时代必须):有挂着的止损/止盈单、盘外成交、部分成交、被拒单,这些不在任何 loop 的节奏里发生。优先用 longbridge 推送;无推送则轮询 `order executions`。把真实成交灌回 `orders`/`positions`,与 Executor 记录漂移时报警。
- **`risk_monitor.py`** — 独立于 Executor 的熔断器(prefrontal cortex)。实时算权益回撤,触发 `daily_loss_limit` → 置 `halt=true`,必要时以最高优先级提交 FLATTEN intent。**独立进程**:即使 Executor 卡死,急停仍能生效。你也能手动拍 HALT。
- **`supervisor`** — launchd `KeepAlive` 守每个进程 + 一个 `heartbeats` 看门狗:任一进程心跳过期就告警(reconciler 死掉 = 带仓裸奔,必须尖叫)。

---

## 6. Dreaming:睡眠期记忆合成(接前几轮讨论)

挂在**市场关闭窗口**(原 `MODE=SKIP`,即 Flintrade 的"睡眠期"),用日期戳防重复做梦。分级:

- **平仓即时(纯代码,0 LLM)** — 每次平仓回写对应 lesson 的 confidence(印证+/打脸-)。在线时间衰减。
- **每日(Haiku)** — 当天成交滚进 `agg` 聚合表,生成当日简报。
- **每周(Opus)** — 重写整个 `memory_lessons` 集:合并同类、淘汰跌破阈值的、识别 regime 切换。

记忆两类(对应人脑):
- **`memory_plans`(情景,会过期)** — "盯 NVDA 财报 6/12"。带 `expires_at`,唤醒时按今天日期决定还活不活。
- **`memory_lessons`(语义,沉淀)** — "收盘前30min追单 n=47 胜率31% vs 全天54%"。带 confidence,不绑日期。

**唤醒回忆**:`collect.sh`/技术面 prompt 在决策前注入 `recall` 块——**以当前 ET 时间为基准**过滤过期 plans,lessons 带样本量与 confidence。即"先看今天几号几点,再调取昨天的总结与今天的计划"。

**多源后的新红利**:trades 带 `source`,Dreaming 能算出"舆情单胜率 vs 技术面单胜率",反过来调 Executor 里各 source 的 priority,甚至喂 `backtest/ml/` 的 gate 当先验。两条学习回路(历史 K 线因子进化 / agent 自身行为反思)在此合流。

---

## 7. 分阶段落地(每阶段独立可验、不破坏现有 cron)

**Phase 0 — 地基(零行为变更):**
- `flintrade.db` schema + `migrate.py`(state.json → db)。
- `db.py` 访问层(WAL、单写者断言)。
- Executor 在写后导出 state.json 投影,保证 dashboard/backtest 不断。

**Phase 1 — 执行权威(核心):**
- `executor.py` + Risk Gate(§3)+ `config/risk.toml`。
- `intents` 队列。
- 把 `loop_technical.py` 从"自己下单"改成"提交 intent"。验证:技术面单全程走队列,组合风控生效,state 不被并发撕裂。

**Phase 2 — 自治:**
- `reconciler.py`(先轮询,后接 WebSocket)。
- `risk_monitor.py` + HALT。
- `supervisor` + 心跳。

**Phase 3 — 多源:**
- `news_collector.py` + `loop_event.py`(舆情)。
- `user_cli.py`(用户主动 + 聊天问状态)。

**Phase 4 — 做梦:**
- `reflect.py`(§6)+ memory 表 + recall 注入。
- (单独验证)技术面 prompt 接入 recall,过 `backtest/rule_test.py`。

**Phase 5 — 合流:**
- dashboard 升级读 db(intent 流 + 溯源 + 风控状态 + 记忆)。
- Dreaming 聚合矩阵接 `backtest/ml/` gate。

---

## 8. 待定的旋钮(需人拍板,非阻塞)

- `risk.toml` 各阈值的具体数值(上面是默认草案)。
- 技术面 loop 的频次(高频程度)。
- 是否允许融资(gross > 100%)、是否允许做空多仓。
- `flatten_on_halt`:熔断时自动平仓还是仅停手。
- source 优先级次序(默认 用户 > 舆情 > 技术面)。
- opencli 具体是什么工具(决定 news_collector 接哪个源)。
