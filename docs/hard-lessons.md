# 硬教训(Hard Lessons)

格式借鉴 deepseek-harness 的 `defensive-patterns.md`:**每一条 = 一类真实翻过车的
bug + 防止复发的规则**。不收假设性的最佳实践——没翻过车的不配进来。

约定:修复一起新事故后,必须在这里登记一条(规则 + 事故经过 + 防线位置)。
改编排、结算、券商交互、风控代码之前,先把对应小节读一遍。

---

## 编排 / 进程

### 不要把 LLM 的 stdout 喂进严格模式的管道(历史条目)
v1 的 `run.sh` 曾因 `set -euo pipefail` 静默整轮死亡:Claude 的 stdout 含有会破坏
shell 管道的字符。当年的对策是每步各自 fallback(`|| echo '[]'`),不开全局严格模式。
`run.sh` 已于 2026-08-21 随 v1 退役删除(commit `a8e8b8b` 存有原始上下文);
规则本身对任何"把模型输出接进 shell 管道"的新代码仍然成立。

### 带病存活比死亡更危险
2026-07-26:sqlite 连接泄漏耗尽 fd 后,各 loop 每轮报错但**进程不死**,
launchd 的 KeepAlive 帮不上忙——系统带病运行 5 天,零交易。
规则:进程要么健康要么死。daemon 主线程看门狗盯两件事,任一触发就 `os._exit`
交给 launchd 重启:关键 loop 连续 `max(6×cadence, 30min)` 无成功迭代;
fd 用量超 soft limit 90%(泄漏早期就重启,不等 EMFILE)。
→ `agent/daemon.py:_watchdog_check`

### SQLite 连接不跨线程
每个 loop 的实例与 DB 连接必须在**自己线程内**构造(工厂模式),
主线程建好传进去会炸。
→ `agent/daemon.py` 各 `_mk_*` 工厂

### 定时任务不要绑在"碰巧存在"的标签上
做梦窗口曾只认 session=='Closed',而工作日唯一的 'Closed' 是被误标的
03:50–04:00 死区——10 分钟窗口 × 600 秒轮询,能不能做梦纯靠对齐。
2026-08-05/06 连续两晚没做梦。睡眠/调度判定要用时间区间自己算,
不要依赖上游恰好返回某个标签。
→ `agent/reflect.py`(睡眠期判定)

## 账本 / 结算

### 多步写必须原子
建仓/平仓 = 持仓 + 成交 + 风险状态三步写,任何一步之间崩溃都会留下撕裂的账
(有持仓没扣风险、有成交没记盈亏)。全部包进 `BEGIN IMMEDIATE` 事务。
→ `agent/settle.py`、`agent/db.py:transaction`

### 结算幂等键 = broker_order_id
executor 的即时结算与 reconciler 的延迟结算存在竞态。同一 `broker_order_id`
已有成交记录 = 这单已结算过,绝不重复记账。
→ `agent/reconciler.py:_settle_pending`

### 订单到终态必须回写 intent
2026-08 复盘:十几条 intent 永远停在 `approved`(NVDA #230、AAPL #198/199/200…),
intent 状态统计全失真,止损守卫的在途判断险些被永久锁死。
限价单的收尾发生在 reconciler,所以 reconciler 必须有 `intents_decide` 权限;
只动 `approved` 是幂等防线。
→ `agent/db.py:_WRITE_PERMS` 注释、`agent/reconciler.py:_finish_intent`

### 账本要有独立守夜人
`settle_close` 对 `open_risk` 用 `max(0,·)` 钳位——漂移会被悄悄吞掉而不是报错。
对账断言必须由独立的只读 loop 定期跑,而不是指望写路径自己发现自己错了。
注意合法瞬态:`update_order(filled)` 与随后的 settle 是两条独立提交的语句,
断言要留结算宽限,否则把正常竞态误报成撕裂。
→ `agent/invariants.py`(2026-08-14 起)

## 券商交互

### API 失败 ≠ 空仓
`broker.positions()` 返回 None(超时/抖动)时必须跳过本轮漂移检测——
当成空仓处理会把每个本地持仓都判成漂移。31,606 条假信号就是这么刷出来的。
同类:报警只报**变化**,同一个未变化的漂移不许每轮重复上报。
→ `agent/reconciler.py:_detect_drift`

### 落库券商的原始报错,不只是 status
2026-07-31 三张被拒的单只存了 "Rejected",复盘时无法诊断被拒原因。
→ `agent/executor.py`(下单失败路径)

### 在途挂单也是真实敞口
5 笔 16 股 AMZN 堆挂单 = $19.4K 名义敞口,而 gate 只看已结算仓位,
一直以为是空仓。组合约束必须把 submitted/partial 的开仓单计进
gross / per_symbol / per_cluster / 并发仓。
→ `agent/risk_gate.py`(inflight 敞口)

### 平仓先清场:券商用挂单锁持仓
挂着的卖单会锁住持仓,第二张同向卖单以「超卖」被拒——2026-07-31 flintrade-198
挂着,flintrade-199 直接被拒,行情继续下跌。平仓路径必须先撤在途单再下新单,
否则改价追单永远追不上。
且撤单与成交是赛跑:撤完必须 `order_detail` 复核,**绝不能凭撤单返回值把
本地订单标 cancelled**——那一刻它可能刚成交。
→ `agent/executor.py:_cancel_inflight`

### 开仓限价单要有 TTL,平仓单不要
论点在下单那一刻定价;挂得越久,成交越可能发生在价格不利穿过限价的时刻
(逆向选择)。flintrade-210 (NVDA) 2026-08-01 周六下单躺过周末,周一开盘在自己
止损位上方 4 美分成交,还连带让在途防线挡掉 6 笔新 intent。
平仓单相反:超时撤掉 = 让持仓裸奔,由 risk_monitor 重触发。
→ `agent/executor.py:_expire_stale_entries`

### 权益校准尊重 sleeve
`equity mode=fixed` 时不许用券商账户净值覆盖 `risk_state.equity`,
否则 $10K sleeve 会被刷成账户真实净值 $126K,所有风险定额瞬间放大 12 倍。
→ `agent/reconciler.py:_sync_equity`

## 风控

### 等模型想明白的止损不是止损
2026-07-31 AAPL 跳空:30 分钟才检查一次、还要等 LLM 推理的止损拦不住跳空。
止损守卫硬编码在 risk_monitor(10 秒节奏、纯规则、不经 LLM)。
→ `agent/risk_monitor.py`

## 时间 / 会话

### CLI 时刻表不含日期,周末要自己挡
`longbridge trading-session` 返回不带日期的静态时刻表,周六中午照样"匹配"到
Intraday 窗口。2026-08-01(周六)technical producer 拿着周五的陈旧数据下单,
挂单在周一开盘成交于自己止损位上方 4 美分。Pre/Intraday/Post 永远不许出现
在周末;只有 Overnight 分支(周日 ≥20:00 属于周一)可以。
→ `agent/session.py:current_session`

### 会话边界的死区会杀掉退出单
03:50–04:00 曾是标签死区:哪个窗口都不匹配 → 'Closed' → `outside_rth=RTH_ONLY`
→ 券商拒单。2026-07-31 AAPL 退出因此三次失败,滑点 $45。
会话区间必须完整覆盖 24 小时中所有可交易时刻,边界值要有测试钉死。
→ `agent/session.py`(Overnight 边界)、`agent/test_stop_guard.py`

## LLM

### 不做跨模型自动 fallback
调用失败重试耗尽 → 返回空串 → 调用方解析失败 → WAIT,不交易。
宁可错过,不换脑子交易——不同模型的风格差异会污染绩效归因。
→ `agent/llm.py`

### model 永不留空
留空 = 跟随 claude CLI 的默认模型,用户改自己的默认配置,交易系统就跟着换模型
(曾意外烧过最贵的 Fable)。tier 配置必须显式钉死 model。
→ `agent/config/trading.toml [models]`、`agent/llm.py:resolve_tier`

### Model-visible means logged
凡真实到达模型的输入输出必须逐字可重建,否则坏交易无法复盘
(intents.reason 只是摘要,不算)。dry-run 没有真实调用,不落盘。
→ `agent/llm.py:_log_call` → `llm_calls/YYYYMMDD.jsonl`(2026-08-14 起)

### 子进程不给无关凭据
claude/kimi CLI 跑的是外部模型,其输出/日志都可能回显环境变量。
spawn 时剔除 `*KEY*/*SECRET*/*TOKEN*/*PASSWORD*`,只放行该 CLI 自身鉴权前缀。
需要凭据的子进程(longbridge CLI、collect.sh)不在此列。
→ `agent/llm.py:_cli_env`

### 记忆注入必须对所有决策 producer 对称
lessons/stats 从全部成交蒸馏(含 event 源),但 recall 曾只注入技术面 loop。
2026-08-21 UGL:技术面依「贵金属同步脉冲易均值回归」教训放弃的簇,舆情 loop
两天后开仓 —— 不是推翻教训,是根本没看见。新增决策 producer 时,注入共享记忆
(可裁剪与其职责无关的部分,如 watchlist),不注入要写明理由。
→ `agent/producers/loop_event.py:_build_payload`、`agent/prompts/event_intent.md`

## 运维 / 导出

### openrsync 无视 `protect .git`
macOS 自带的 rsync 是 openrsync,filter 规则 `protect .git` 不生效。
2026-08-06 它把导出目标仓的 `.git` 整个删了(靠 GitHub remote 才恢复)。
导出前先把 `DEST/.git` 挪到一边,rsync 完再挪回来。
→ `scripts/export_oss.sh`,commit `54aa943`
