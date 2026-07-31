# Running the daemon under launchd (macOS)

One job, all loops: `com.flintrade.daemon` runs `.venv/bin/python -m agent.daemon`
with auto-restart (`KeepAlive`), a raised fd limit, and the daemon's own
watchdog (self-exits on stale loops or fd pressure; launchd restarts it).

## Install

```bash
cp launchd/com.flintrade.daemon.plist.example ~/Library/LaunchAgents/com.flintrade.daemon.plist
$EDITOR ~/Library/LaunchAgents/com.flintrade.daemon.plist
#   → fix /path/to/flintrade (WorkingDirectory, ProgramArguments, log paths)
#   → fill LONGBRIDGE_* credentials
#   → keep FLINTRADE_DRY_RUN=1 until you've watched it run
launchctl load -w ~/Library/LaunchAgents/com.flintrade.daemon.plist
```

## Verify

```bash
launchctl list | grep com.flintrade.daemon        # pid, exit code 0
tail -f logs/daemon.out                       # all loops announce themselves
sqlite3 flintrade.db "SELECT * FROM heartbeats"   # executor/reconciler/risk_monitor beat every few seconds
```

## Change config / rotate credentials

Always reload the job — a plist edit is only picked up by unload+load:

```bash
launchctl unload ~/Library/LaunchAgents/com.flintrade.daemon.plist
launchctl load -w ~/Library/LaunchAgents/com.flintrade.daemon.plist
```

> Do NOT use `launchctl kickstart -k` after editing the plist: it restarts the
> process but does **not** re-read `EnvironmentVariables`, so the daemon keeps
> running with stale credentials (this exact failure cost us two silent days).

> The filled-in plist contains credentials. Never commit it — the versioned
> copy is `com.flintrade.daemon.plist.example` only.
