# Running the daemon under launchd (macOS)

One job, all loops: `com.flintrade.daemon` runs `bin/flintrade-daemon` with
auto-restart (`KeepAlive`), a raised fd limit, and the daemon's own watchdog
(it exits with code 70 when a loop has not succeeded for max(6×cadence,
30 min) or fd use passes 90%; launchd restarts it).

The plist holds **no credentials**. The daemon reads the credentials file
itself on every start (`$FLINTRADE_CREDENTIALS`, else
`~/Library/Application Support/Flintrade/credentials.env`, else
`<repo>/flintrade.env`); see `flintrade.env.example`.

## Install

```bash
go build -o bin/ ./cmd/...                    # if you haven't run scripts/bootstrap.sh
sed -e "s|/path/to/flintrade|$PWD|g" -e "s|/path/to/home|$HOME|g" \
    launchd/com.flintrade.daemon.plist.example > ~/Library/LaunchAgents/com.flintrade.daemon.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.flintrade.daemon.plist
```

`PATH` in the plist must reach the `longbridge` CLI and, for the default
`claude-cli` provider, the `claude` CLI (`~/.local/bin`). Keep
`FLINTRADE_DRY_RUN=1` in the credentials file until you've watched it run.

## Verify

```bash
launchctl list | grep com.flintrade.daemon        # pid, last exit code
tail -f logs/daemon.out                       # every loop announces itself
bin/flintrade                                     # loops, risk, positions, pending intents
sqlite3 flintrade.db "SELECT * FROM heartbeats"   # executor/reconciler/risk_monitor beat every few seconds
```

## Rotate credentials / go live / rebuild

Edit the credentials file (or rebuild `bin/`), then restart the job:

```bash
launchctl kickstart -k gui/$(id -u)/com.flintrade.daemon
```

## Change the plist itself

`kickstart` restarts the process but does **not** re-read the plist. After
editing it:

```bash
launchctl bootout gui/$(id -u)/com.flintrade.daemon
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.flintrade.daemon.plist
```
