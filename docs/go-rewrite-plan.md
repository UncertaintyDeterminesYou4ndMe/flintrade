# Flintrade Go 重写 + mygo 桌面 App 计划

> 2026-11-09 定稿。目标:整个 flintrade(交易 daemon + 看板)改为 Go,桌面端用 mygo 原生 UI,
> 让「看报表 / 改策略 / 换凭证」都在一个 App 里完成。

## 已定决策

| # | 决策 | 理由 |
|---|---|---|
| D1 | 一个 Go module,两个产物:`flintrade-daemon`(launchd,无 UI)+ `Flintrade.app`(mygo native UI,菜单栏常驻) | daemon 必须 24/7 独立于窗口;App 退出不能停交易。App 不下单。 |
| D2 | daemon 直接改 Go,**不双跑** | 用户决定(paper 账户)。兜底 = D3 的回滚。 |
| D3 | **`flintrade.db` schema 不变**,Go 版读写同一份库;**切换验收通过即删除全部 Python** | 历史连续。回滚靠 git tag `python-final`(签出 + 重建 `.venv` + plist 指回),不靠在仓库里留旧代码。 |
| D4 | Longbridge / `claude -p` / kimi 继续走 CLI 子进程 | 与现状一致,行为可对照;SDK 化留作以后的独立改动。 |
| D5 | 凭证:沿用 maka-agent 的 file-first 方案 —— 单文件、0600、原子写(临时文件+rename)+ 跨进程锁 + schema 版本;切换时移出仓库到 `~/Library/Application Support/Flintrade/credentials.env` | daemon 无人值守:Keychain 在 daemon 重新编译后会弹授权框,launchd 下没人点就卡死。OS 用户账户是安全边界(同 maka SECURITY.md)。移出仓库 = 不会被 git / export_oss / 读仓库的 agent 碰到。 |
| D6 | ~~语义记忆:向量存 `vectors` 表 + Ollama embedding~~ **待定,见下** | 实查后发现:只有已平仓成交(224 条)被向量化;lessons/plans 的召回本来就是 SQL。查询串是「标的 + 分数 + RSI + 量比」这类数字模板,句向量模型不懂数值远近,召回结果 65% 只是同标的的历史成交;738 次调用里模型只在 49 次(6.6%)回复中引用过。倾向改为确定性 SQL(同标的/同方向/同时段的历史成交及胜率),零依赖。 |
| D7 | 回测(`backtest/`)移植到 Go,并作为 App 改风控参数的保存闸门 | `CLAUDE.md` 要求风控数值改动先过回测;App 让改参数变容易,闸门必须在。代码已从 git 恢复(408K);行情数据(gitignored)按 manifest 重下。 |
| D8 | 代码放 flintrade 仓库:`cmd/flintrade-daemon`、`cmd/flintrade-app`、`internal/…` | 与 schema、prompts、config 同仓,单一事实源不分裂。 |

## 架构

```
Flintrade.app (mygo native)                    flintrade-daemon (launchd KeepAlive)
 ├─ 菜单栏:状态/今日 PnL/权益               ├─ executor / reconciler / risk_monitor
 ├─ 通知:熔断、daemon 掉线、token ≤14d       ├─ producers: technical / event / news
 ├─ 总览 / 成交 / 持仓 / 复盘               ├─ reflect / postmortem / invariants
 ├─ 策略编辑(editor + diffview → 校验 → 测试  │
 │    → [风控参数需回测] → 重启 daemon)      │
 └─ 凭证(JWT 过期日 → 原子写 → longbridge    │
      check → 重启 daemon)                  │
          │  只读 + 控制(launchctl kickstart)  │  唯一写者(按 role guard)
          └──────────────► flintrade.db ◄──────────┘
```

App 对 `flintrade.db` **只读**(连接以 `mode=ro` 打开);写的只有配置文件和凭证文件。

## Python → Go 模块映射(按依赖顺序)

| 阶段 | Go 包 | 来源 | 行数 |
|---|---|---|---|
| 1 基础 | `internal/db`(连接池、role guard、schema 迁移) | db.py, schema.sql | 343+206 |
| | `internal/config`(toml 三件套 + strategies 绑定) | config.py, *.toml | 56 |
| | `internal/creds`(D5:读/原子写/锁/JWT 过期) | 新 | — |
| | `internal/lb`(CLI 包装、429 退避、并发抹平) | lb.py | 102 |
| | `internal/session`(四时段、周末/假日、死区) | session.py | 240 |
| 2 数据 | `internal/indicators` | scripts/indicators.py | 384 |
| | `internal/collect`(替代 collect.sh) | scripts/collect.sh | 161 |
| | `internal/quotes` | quotes.py | 123 |
| | `internal/llm`(provider 注册表、tier、调用落盘 llm_calls/) | llm.py | 449 |
| 3 交易核心 | `internal/broker` | broker.py | 293 |
| | `internal/riskgate` | risk_gate.py | 251 |
| | `internal/settle` | settle.py | 93 |
| | `internal/executor` | executor.py | 329 |
| | `internal/reconciler` | reconciler.py | 408 |
| | `internal/riskmonitor` | risk_monitor.py | 362 |
| | `internal/invariants` | invariants.py | 153 |
| 4 生产者/记忆 | `internal/producers/{technical,event,news}` | producers/*.py | 670 |
| | `internal/memory`(D6) | memory_store.py | 214 |
| | `internal/reflect`、`internal/postmortem` | reflect.py, postmortem.py | 1119 |
| 5 编排 | `cmd/flintrade-daemon`(supervisor、watchdog、cadence) | daemon.py, supervisor.py | 435 |
| | `cmd/flintrade`(CLI:status 等) | user_cli.py | 399 |
| 6 回测 | `internal/backtest` + `cmd/flintrade-backtest` | backtest/*.py | ~4k |
| 7 App | `cmd/flintrade-app` | dashboard/server.py, web/ | 961+2100 |

prompts(`agent/prompts/*.md`)与 config(`agent/config/*.toml`)原样复用,路径不变。

## 验证

- Python 测试(1694 行)逐个转 `go test`;临时库 + `FLINTRADE_DRY_RUN=1`,零副作用。
- `docs/hard-lessons.md` 每一条写成一个命名回归测试(`TestHardLesson_…`)。
- indicators / riskgate:用同一份输入跑 Python 与 Go,输出逐字段比对(黄金样本),不是双跑 daemon。
- 切换前:`flintrade-daemon --once`,`FLINTRADE_DRY_RUN=1`,对 `flintrade.db` 的**副本**跑一轮。

## 切换与回滚

删 Python 之前,所有依赖 Python 的验证资料必须已经生成并入库:各阶段的黄金样本
(indicators / riskgate 等,放 `internal/*/testdata/`)、回测基线结果。

1. 选休市时段(周末),无持仓、无在途单。
2. 提交 Python 最终状态,打 tag `python-final`。
3. 凭证迁到 D5 路径;plist 指向 `flintrade-daemon`;`bootout` + `bootstrap`。
4. 验收:`go test ./...` 全绿 → `flintrade-daemon --once`(`FLINTRADE_DRY_RUN=1`,`flintrade.db` 副本)
   → 线上首个完整交易日心跳 / invariants 正常。
5. 验收通过即删除(同一个提交):`agent/*.py`、`agent/schema.sql`、`scripts/*.py`、
   `scripts/collect.sh`、`scripts/agentctl.sh`、`backtest/*.py`、`dashboard/`、`web/`、
   `.venv/`、`flintrade_lance/`;`CLAUDE.md` / `AGENTS.md` / README / `launchd/README.md` /
   `scripts/export_oss.sh` 同步改写为 Go 版。保留:`agent/prompts/`、`agent/config/`。
6. 回滚(仅在删除后出问题时):`git checkout python-final -- agent scripts`,
   `python3 -m venv .venv && .venv/bin/pip install lancedb fastembed`,
   plist 改回 `scripts/agentctl.sh daemon`。凭证文件保持 `export KEY='v'` 格式,
   回滚时 `agentctl.sh` 仍能直接 source。

## 进度

### 阶段 1 基础(2026-11-09 完成)

`internal/{paths,db,config,creds,lb,session}` + `cmd/flintrade check`。`go test ./...` 全绿;
`flintrade check` 对线上环境只读自检通过(凭证/配置/账本/会话时钟)。

与 Python 的有意差异(都更严,行为不变):

- `db.SubmitIntent` 只吞 dedup 冲突;Python 吞一切 IntegrityError。
- `UpdateOrder` / `UpdateRisk` 列名走白名单;Python 把 kwargs 直接拼进 SQL。
- `config` 严格解码,未知 key 报错 —— 由此查出 risk.toml 里 `adopt_*` 两项挂错表
  (已修,值等于默认值,线上行为未变);`ClusterOf` 随文件热加载,Python 是永久缓存。
- `config` 加载即校验:model 不得为空(hard-lesson)、百分比区间、簇/手册标的不重复。
- `lb` 超时杀整个进程组,孙进程不再拖住调用。
- `session.MinutesToClose` 用绝对时间;只在 DST 切换时与 Python 不同,而切换发生在
  周日 02:00,无会话。

验证手段:session 对 Python 黄金样本(一周 2352 个时刻点)逐点一致;db 在线上账本
`VACUUM INTO` 副本上 Init + 全表扫描;creds 与真实 bash `source` 逐值比对(哈希比较,不打印)。

### 阶段 2 数据(2026-11-09 完成)

`internal/{indicators,quotes,collect,llm}` + `flintrade collect` / `flintrade ping`,`flintrade check` 增加模型档位
与 embedding 自检。`go test ./...` 全绿。

验证手段:

- indicators:648 个 Python 黄金样本逐字段**零容差**一致(8 个真实标的 × 多长度/多 lookback +
  合成边界);各指标序列不取整逐点一致。须用 `pySum`(CPython ≥3.12 的 Neumaier 求和)并阻止
  arm64 的 FMA 融合 —— 去掉任一处测试即失败(已实测)。
- collect:真实 `collect.sh` 跑在假 longbridge CLI 上生成黄金样本,Go 输出按键序 + 数值逐项比对;
  变异测试确认能抓住排序稳定性、旧 bar 裁剪。线上对照:16 个标的指标块与 collect.sh **逐字段一致**,
  耗时 **10s vs 105s**。
- quotes:hard-lesson「盘外读子报价不读顶层 last」+ 线上形状校验。
- llm:假 claude/kimi CLI 校验 argv、system prompt 临时文件、环境洗白;httptest 校验两种 HTTP wire
  的重试语义;`flintrade ping` 线上两档位通过。

与 Python 的有意差异:

- **collect.sh 的重试包装是个 bug**:它用 `grep -iE "429|timeout|…"` 匹配整段输出,K 线 JSON 里
  任何含 "429" 的数字都被当成限流,该标的抓 3 次、睡 1+2+3 秒。Go 版走 `internal/lb`,每个标的
  只抓一次 —— 这是 105s→10s 的主要来源。
- collect 报价池取 `trading.toml [universe]`(16 个),collect.sh 硬编码 14 个,漏了 SNDK/MU 的报价。
- collect 单个 CLI 调用失败只让该字段降级为 `[]`,并在 `collect_errors` 里告诉模型;collect.sh
  任一调用失败整个 payload 作废。
- collect 的 K 线并发 4 路(`lb` 仍按 0.35s 间隔起调用)。
- llm:CLI 超时 / 非零退出也落盘(`ok:false` + 新增 `error` 字段)。Python 超时直接抛异常不落盘,
  违反「模型看到的必须留痕」;非零退出时把 stdout(如 `Error: Exceeded USD budget`)当正常回复返回。
- llm:CLI 超时杀整个进程组。
- llm:未知 tier 报错,不再默认成「claude-cli + 空 model」(hard-lesson:model 不得为空)。
- embedding:`fastembed`(进程内 ONNX)在 Go 里无对应,返回不可用。是否还需要 embedding 取决于 D6。
- llm:**交易模型只看到我们的 prompt**(2026-11-09 用户确认)。`claude -p` 原先会顺带加载全局
  `~/.claude/CLAUDE.md`(「用中文交流」等个人工作原则)、仓库 CLAUDE.md(写给编码 agent 的开发说明)、
  自动记忆 MEMORY.md 和约 1.6 万 token 的内置工具定义。现加 `--setting-sources "" --tools ""
  --exclude-dynamic-system-prompt-sections`、`CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`,并在中性目录
  运行。实测输入从 ~2.2 万 token 降到 587,Haiku 单次 $0.045→$0.0036。`--bare` 会禁用 OAuth
  (订阅鉴权),不能用。Python 版保持原样直到切换。

### 阶段 3–5 交易核心 / 生产者 / 编排(2026-11-09 完成)

切换前按用户要求修复的 Python 旧问题:

- 部分成交:以「券商累计成交量 − 账本里同 broker_order_id 已入账股数」为增量逐次结算(这本身
  就是幂等键);挂单中的订单保持 `partial` 继续跟踪,到终态才 `filled` 并收尾 intent。开仓后续
  成交并入首笔成交开出的持仓(加权均价、按止损重算风险);整单平仓的分批成交只缩仓、不置
  `t1_done`;闸门的在途敞口只计未成交余量。Python 把首笔部分成交当整单结算并停止跟踪。
- 舆情 loop 积压:从游标往后按 id 升序取(仍以新→旧呈现),积压逐轮消化;Python 取最新 8 条
  并把游标跳过更早的全部,积压事件永久丢失。
- 舆情 loop 注入 `similar_history`(按催化标的,`{symbol: 战绩}`),与技术面对称;
  `agent/prompts/event_intent.md` 同步加说明。

### 切换(2026-11-09 完成)

- 切换前确认零敞口:0 持仓、0 挂单、0 待裁决意图;Python 最终版打 tag `python-final`。
- `launchctl bootout` 停 Python daemon → 凭据移到 `~/Library/Application Support/Flintrade/credentials.env`
  (0600,哈希校验一致,仓库内不再有 `flintrade.env`)→ plist 改为直接跑 `bin/flintrade-daemon` → bootstrap。
- 验收:8 个 loop 心跳全部来自新进程;首轮 technical 产出 intent #800,executor 以 LIVE 模式挂限价单,
  reconciler 按 15s 跟踪。旧日志归档为 `logs/daemon.python-final.{out,err}`。
- 删除:daemon 相关 Python(`agent/` 下 daemon/executor/reconciler/risk_monitor/risk_gate/broker/
  settle/invariants/user_cli/quotes/producers 与全部 `test_*.py`、`scripts/collect.sh`、
  `scripts/agentctl.sh`、`.venv`、`flintrade_lance`)。保留 dashboard 读路径用到的 `agent/*.py` 与
  `agent/schema.sql`、`scripts/indicators.py`(backtest 用),等 Go backtest 与 Flintrade.app 落地后一起删。
- 切换后收尾:`FLINTRADE_DRY_RUN` 只有明确的 0/false/no 才实盘,空值或拼错一律干跑(Python 把 `""` 当实盘);
  删掉已无用的 embedding(`llm.Embed`、`[embeddings]` 配置、`flintrade check` 的探测)。
- 遗留问题(2026-11-09,与 Fable 5.1 共同决定):
  - `similar_history` 不再计入 `legacy`(v1 Arena 时代:不同编排、不同 prompt、无风控闸门,不是当前策略
    的证据),与 `self_assessment`、做梦、复盘同口径;四处统一用 `db.StrategyRecord`,不再各写一份。
    影响 AAPL/AMZN/GOOGL/NVDA/SLV/TSLA 六个标的的汇总。
  - `vectors` 表删除(线上 0 行):两份 schema 同步删,`Init` 里 `DROP TABLE IF EXISTS`。回滚时
    `git checkout python-final -- agent` 会带回含该表的 schema,Python 自己会重建。
  - `logs/daemon.python-final.{out,err}` gzip 保留,2026-12-09 后删除(对齐 llm_calls 30 天保留期)。
- 回滚:`git checkout python-final -- agent scripts`,把凭据拷回 `flintrade.env`,plist 指回
  `scripts/agentctl.sh daemon`,重建 `.venv`。

### 阶段 7 Flintrade.app v1(2026-11-09)

- `cmd/flintrade-app`(mygo v0.8.3 原生 UI)+ `internal/dash`(只读快照、告警判定、配置/凭证写入、重启 daemon)。
- 菜单栏:今日盈亏 / 「停」/「暂停」;菜单里有状态行、打开、重启 daemon、退出(交易照常)。
- 窗口:总览(模式、daemon、权益/今日/累计/在险、权益曲线、持仓、挂单、待执行意图、进程)、成交、意图、
  复盘(教训/计划/逐笔复盘)、策略(编辑 toml 与 prompt,严格校验 → diff 确认 → 原子写 → 可选重启)、
  凭证(到期日、掩码显示、更换:离线校验 → 用新值调只读 `longbridge assets` → 原子写 0600 → 询问重启)。
- 通知:daemon 掉线/恢复、熔断暂停、凭证问题(token 14 天内到期也算)。只在打包的 .app 里生效。
- 风格:纸墨,唯一强调色红(亏损/告警/主操作),表格选中行用中性灰。
- 未做:`risk.toml` 编辑(等 Go 回测作为闸门,D7);FLINTRADE_DRY_RUN 开关(刻意不做成表单项);
  权益曲线只计策略平仓盈亏,与账面权益有差(开仓佣金、test/outage 归属不计),沿用旧看板口径。
