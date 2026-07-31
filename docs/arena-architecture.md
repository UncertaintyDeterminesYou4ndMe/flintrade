---
name: Flintrade Arena Architecture (Branch Plan)
description: 未实施的 Flintrade 重构方案 — 采用 AlphaZero Arena 模式，bash 预采集数据打包成富 prompt，LLM 只做分析决策不调工具，下单回到 bash。省 token、更快、更可控。
type: project
---
## Arena 模式 vs 当前 Flintrade

| | Flintrade 当前 | Arena 模式（未实施） |
|---|---|---|
| 数据采集 | LLM 自己调 longbridge CLI（多轮工具调用） | bash 预采集 quote/kline/news，打包成 structured prompt |
| LLM 职责 | 采数据 + 分析 + 决策 + 下单 | **只做分析和决策**（纯文本 in/out） |
| 下单 | LLM 调 longbridge buy/sell | bash 解析 LLM 输出的 JSON，bash 执行下单 |
| token 消耗 | 高（每次 $2-5，多轮工具调用） | 低（一轮 prompt，无工具调用） |
| allowedTools | 需要 Bash/Read/Edit/Write | 不需要，--allowedTools 可以为空 |

## Arena Prompt 结构（参考 AlphaZero）

```
USER_PROMPT:
  0. Context Snapshot (Risk Regime) — 市场总体环境
  1. Raw Data Dashboard — 每只票的技术指标、K线、VWAP、funding
  2. Narrative vs Reality Check — 资讯叙事 vs 实际表现
  3. FOMO Map & Catalyst Horizon — 即将到来的事件（CPI/FOMC 等）
  4. Alpha Setups: Menu of Hypotheses — 每只票的多空假设
  5. Edge Quality Matrix — 高置信/战术/无边缘 分类
  6. Current Positions + Capital + Trade Limits

OUTPUT:
  CHAIN_OF_THOUGHT → TRADING_DECISIONS (JSON with signal/quantity/stop_loss/take_profit/confidence/invalidation)
```

## 实施要点

1. `run.sh` 里用 bash 调 `longbridge quote/kline/news` 预采集所有标的数据
2. 用 Python 脚本将原始数据格式化成 Arena 风格的 prompt
3. `claude -p --system-prompt` 只传纯文本 prompt，无工具权限
4. Claude 输出 JSON 决策
5. bash 解析 JSON，调 `longbridge buy/sell` 执行

**Why:** 省 token（从 $5/次降到 $1/次），更快（一轮调用 vs 5-10 轮），更可控（下单逻辑在 bash 里，不怕 LLM 幻觉）。

**How to apply:** 当需要降低 API 成本或提高执行可靠性时，切换到此架构。
