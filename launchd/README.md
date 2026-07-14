# Flint launchd job

**单进程总管。** 整个交易系统现在是**一个 launchd job** —— `com.flint.daemon`,
跑 `agent.daemon`,内部把所有 loop(executor / reconciler / risk_monitor /
producers / reflect)作为线程,各按自己的 cadence 调 `run_once()`。
`RunAtLoad` + `KeepAlive`(开机自起、崩溃自重启)。

> 早期的每进程 plist(`com.flint.executor` 等 4 个)已删除——它们被这一个 daemon
> 取代。各 loop 仍只通过 `flint.db` 的 SQLite 队列/状态解耦,只是从多进程变成
> 单进程多线程(单一写者由 db role guard 按 handle 角色强制,与进程无关)。

| plist | 入口 | 说明 |
|---|---|---|
| `com.flint.daemon.plist.example` | `.venv/bin/python -m agent.daemon` | 全部 loop 合一 |

`EnvironmentVariables`:`PATH`(让 `longbridge` CLI 在 launchd 下可解析)、paper 账户
`LONGBRIDGE_*` 凭据、以及 **`FLINT_DRY_RUN=1`**。

> **`FLINT_DRY_RUN=1` 是故意的。** 加载即以 dry-run 启动,**不会下真单**。翻成 live 见下。
> python 指向 **`.venv/bin/python`**(内含 lancedb + fastembed,语义记忆才活)。

## 首次部署

1. 建库 + venv(一次性):

   ```bash
   cd /path/to/flintrade
   python3 -m agent.migrate                       # 建 flint.db + schema
   python3 -m venv .venv && .venv/bin/pip install lancedb fastembed
   ```

2. 软链 plist 进 LaunchAgents(软链便于改后 reload 生效):

   ```bash
   ln -sf /path/to/flintrade/launchd/com.flint.daemon.plist.example ~/Library/LaunchAgents/
   ```

3. 加载(`-w` 跨重启保持启用):

   ```bash
   launchctl load -w ~/Library/LaunchAgents/com.flint.daemon.plist.example
   ```

也可不走 launchd,直接前台跑(调试):`bash scripts/agentctl.sh daemon`
或 `.venv/bin/python -m agent.daemon`(`--once` 每个 loop 跑一次即退出)。

## 看健康 / 状态

```bash
bash scripts/agentctl.sh supervisor --status   # 进程线程心跳 + 风控 + 时空锚点
bash scripts/agentctl.sh user_cli  status      # 交易全景 + 自 6/9 收益
```

日志:`logs/daemon.out` / `logs/daemon.err`。

## 翻成 LIVE(真实下单)

编辑 `com.flint.daemon.plist.example`,把:

```xml
<key>FLINT_DRY_RUN</key>
<string>1</string>     <!-- 改成 0 才会下真单 -->
```

(同步改 `flint.env` 的 `FLINT_DRY_RUN`,以便手动跑也一致),然后 reload:

```bash
launchctl unload ~/Library/LaunchAgents/com.flint.daemon.plist.example
launchctl load -w ~/Library/LaunchAgents/com.flint.daemon.plist.example
```

**建议先 dry-run 跑几天**,在 dashboard 看 intent 流 / 拒单原因 / 做梦产出,确认风控行为
符合预期,再翻 live。

## 管理

```bash
launchctl list | grep com.flint
launchctl kickstart -k gui/$(id -u)/com.flint.daemon     # 强制重启
launchctl unload ~/Library/LaunchAgents/com.flint.daemon.plist.example
```

> plist 内是 **paper 账户**凭据。若换 live token,**勿提交** plist。
