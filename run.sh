#!/bin/bash
# Flintrade Trading Bot — Arena-style architecture
# bash collects data → Claude decides → bash executes
#
# Flow: collect.sh → claude -p (no tools) → parse decision → longbridge buy/sell
# Account truth from longbridge, not self-calculated

# Note: intentionally NOT using set -euo pipefail
# Claude's output contains special chars that break shell pipelines.
# Each step handles its own errors.

export PATH="$HOME/.local/bin:/opt/homebrew/bin:$PATH"

FLINTRADE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Credentials — single source: flintrade.env (gitignored, never committed).
# (Same source the daemon path uses via scripts/agentctl.sh.)
if [ -f "$FLINTRADE_DIR/flintrade.env" ]; then
  set -a
  source "$FLINTRADE_DIR/flintrade.env"
  set +a
else
  echo "FATAL: missing $FLINTRADE_DIR/flintrade.env (credential source). Copy flintrade.env.example and fill it in." >&2
  exit 1
fi

STATE_FILE="$FLINTRADE_DIR/state.json"
LOG_DIR="$FLINTRADE_DIR/logs"
mkdir -p "$LOG_DIR"

TIMESTAMP=$(date +%Y%m%d_%H%M%S)
LOG_FILE="$LOG_DIR/${TIMESTAMP}.json"

TIME_BJ=$(TZ=Asia/Shanghai date '+%H:%M CST')
TIME_ET=$(TZ=America/New_York date '+%H:%M %Z')
DOW=$(TZ=America/New_York date '+%u')

# =========================================================================
# Step 1: Pre-check market status (zero cost)
# =========================================================================
SESSION_JSON=$(longbridge trading session --format json 2>/dev/null || echo "[]")
ET_NOW=$(TZ=America/New_York date '+%H:%M:%S')

# Check trading session including Overnight (20:00-03:50)
# Overnight requires checking if NEXT trading day is valid (timestamp+4h)
MARKET_STATUS=$(python3 -c "
import json, sys, subprocess, os
from datetime import datetime, timedelta, timezone

et_now = '${ET_NOW}'  # HH:MM:SS
dow = int('${DOW}')   # 1=Mon..7=Sun
hhmm = et_now[:5]

# Parse session JSON for Pre/Intraday/Post
try:
    data = json.loads('''${SESSION_JSON}''')
    for market in data:
        if market.get('market', '').upper() == 'US':
            for sess in market.get('sessions', []):
                raw_open = sess.get('open', '').replace('.0','')
                raw_close = sess.get('close', '').replace('.0','')
                parts_o = raw_open.split(':')
                parts_c = raw_close.split(':')
                begin = ':'.join(p.zfill(2) for p in parts_o)
                end = ':'.join(p.zfill(2) for p in parts_c)
                if begin and end and begin <= et_now <= end:
                    print(sess.get('session', 'Open'))
                    sys.exit(0)
except SystemExit:
    raise
except Exception:
    pass

# Not in Pre/Intraday/Post — check Overnight (20:00-03:50)
# Overnight belongs to the NEXT trading day
# Simple heuristic: if 20:00-23:59 or 00:00-03:50, and next day is a weekday, treat as Overnight
# (Full accuracy would need the longbridge trading-days API for holidays)
is_overnight_window = (hhmm >= '20:00') or (hhmm < '03:50')
if is_overnight_window:
    # Next trading day check: if current is Fri 20:00+, next is Mon; if Sun, next is Mon
    # dow: 1=Mon..5=Fri, 6=Sat, 7=Sun
    if hhmm >= '20:00':
        next_dow = dow + 1 if dow < 7 else 1
    else:
        next_dow = dow  # early morning belongs to today
    # Weekday = 1-5
    if 1 <= next_dow <= 5:
        if hhmm >= '03:50' and hhmm < '04:00':
            print('Overnight-Pre')  # N1: transition
        else:
            print('Overnight')  # N: active overnight
        sys.exit(0)

print('Closed')
" 2>/dev/null || echo "Closed")

HAS_POSITION=$(python3 -c "
import json
try:
    with open('$STATE_FILE') as f:
        d = json.load(f)
    p = d.get('position')
    print('true' if p and p.get('symbol') else 'false')
except:
    print('false')
" 2>/dev/null || echo "false")

IS_WEEKEND="false"
[ "$DOW" -ge 6 ] && IS_WEEKEND="true"

# Decision matrix — Overnight/Pre/Intraday/Post are all tradeable
if [ "$MARKET_STATUS" != "Closed" ]; then
  MODE="TRADE"
elif [ "$HAS_POSITION" = "true" ] && [ "$IS_WEEKEND" = "false" ]; then
  MODE="PLAN"
elif [ "$HAS_POSITION" = "true" ] && [ "$IS_WEEKEND" = "true" ]; then
  MODE="MONITOR"
else
  MODE="SKIP"
fi

echo "[${TIMESTAMP}] ET=${TIME_ET} BJ=${TIME_BJ} session=${MARKET_STATUS} position=${HAS_POSITION} mode=${MODE}" >> "$LOG_DIR/dispatch.log"

# =========================================================================
# SKIP: nothing to do
# =========================================================================
if [ "$MODE" = "SKIP" ]; then
  echo '{"action":"SKIP","chain_of_thought":"market closed, no position","time_et":"'${TIME_ET}'","time_bj":"'${TIME_BJ}'"}' > "$LOG_FILE"
  exit 0
fi

# =========================================================================
# MONITOR: weekend + position — bash only, read from longbridge
# =========================================================================
if [ "$MODE" = "MONITOR" ]; then
  POS_JSON=$(longbridge positions --format json 2>/dev/null || echo '[]')
  BALANCE_JSON=$(longbridge assets --format json 2>/dev/null || echo '[]')
  echo "{\"action\":\"MONITOR\",\"chain_of_thought\":\"weekend, market closed\",\"time_et\":\"${TIME_ET}\",\"time_bj\":\"${TIME_BJ}\",\"positions\":${POS_JSON},\"balance\":${BALANCE_JSON}}" > "$LOG_FILE"
  exit 0
fi

# =========================================================================
# TRADE / PLAN: Collect data → Claude executes → Harness reconciles
# =========================================================================

# Step 2: Collect all market data (zero LLM cost)
# Pass resolved session + outside-RTH flag to collect.sh (single source of truth).
# US extended-hours orders MUST carry outside_rth or the broker rejects them:
#   Intraday -> RTH_ONLY, Pre/Post -> ANY_TIME, Overnight -> OVERNIGHT
case "$MARKET_STATUS" in
  Intraday)              OUTSIDE_RTH="RTH_ONLY" ;;
  Pre|Post)              OUTSIDE_RTH="ANY_TIME" ;;
  Overnight|Overnight-Pre) OUTSIDE_RTH="OVERNIGHT" ;;
  *)                     OUTSIDE_RTH="RTH_ONLY" ;;
esac
export MARKET_STATUS OUTSIDE_RTH
DATA=$("$FLINTRADE_DIR/scripts/collect.sh" 2>/dev/null || echo '{"error":"collect failed"}')

# Step 3: Claude — full agent with Read, Write, Bash(longbridge)
DECISION=$(claude -p \
  --system-prompt-file "$FLINTRADE_DIR/prompt.md" \
  --allowedTools "Read Write Bash(longbridge *) Bash(date *) Bash(TZ=*)" \
  --max-budget-usd 3.00 \
  "$DATA" 2>/dev/null || echo '{"action":"ERROR","chain_of_thought":"claude call failed"}')

# Step 4: Parse Claude's JSON output for logging
DECISION_JSON=$(echo "$DECISION" | python3 -c "
import json, sys, re
text = sys.stdin.read()
m = re.search(r'\x60\x60\x60json\s*(\{.*?\})\s*\x60\x60\x60', text, re.DOTALL)
if m:
    print(m.group(1))
else:
    for line in text.strip().split('\n'):
        line = line.strip()
        if line.startswith('{') and 'action' in line:
            print(line)
            sys.exit(0)
    # Fallback: build from state.json + raw text
    action = 'UNKNOWN'
    for a in ['BUY','SELL','SHORT','HOLD','WAIT','MONITOR']:
        if a in text.upper():
            action = a
            break
    print(json.dumps({'action': action, 'chain_of_thought': text[:500].replace(chr(10),' ')}))
" 2>/dev/null || echo '{"action":"ERROR"}')

# Step 5: Harness — reconcile state.json vs broker
python3 -c "
import json, subprocess, os

state_file = '$STATE_FILE'
env = dict(os.environ)
env['LONGBRIDGE_APP_KEY'] = '$LONGBRIDGE_APP_KEY'
env['LONGBRIDGE_APP_SECRET'] = '$LONGBRIDGE_APP_SECRET'
env['LONGBRIDGE_ACCESS_TOKEN'] = '$LONGBRIDGE_ACCESS_TOKEN'

with open(state_file) as f:
    state = json.load(f)

# Verify our recorded position against the broker by order_id.
# 'longbridge order detail <id>' works across days (today's order-list endpoint does not).
pos = state.get('position')
if pos and pos.get('order_id'):
    try:
        r = subprocess.run(['longbridge', 'order', 'detail', str(pos['order_id']), '--format', 'json'], capture_output=True, text=True, timeout=10, env=env)
        detail = json.loads(r.stdout) if r.returncode == 0 else {}
    except:
        detail = {}
    status = detail.get('status', '')
    if status != 'Filled':
        print(f'RECONCILE_WARN: state says position {pos[\"symbol\"]} (order {pos[\"order_id\"]}) but broker status={status or \"unknown\"}')

print('RECONCILE_OK')
" 2>/dev/null || echo "RECONCILE_SKIP"

# Claude handles order execution + state.json update via Bash + Write tools

# Step 6: Save log — write Claude output to temp file first (avoids shell quoting issues)
DECISION_TMP=$(mktemp)
echo "$DECISION" > "$DECISION_TMP"

python3 -c "
import json, sys, re

state_file = '$STATE_FILE'
with open('$DECISION_TMP') as f:
    raw_decision = f.read()
timestamp = '$TIMESTAMP'
mode = '$MODE'
market = '$MARKET_STATUS'
time_et = '$TIME_ET'
time_bj = '$TIME_BJ'

# Read current state.json (Claude may have updated it)
try:
    with open(state_file) as f:
        state = json.load(f)
except:
    state = {}

# Try to extract JSON from Claude output
entry = None
m = re.search(r'\x60\x60\x60json\s*(\{.*?\})\s*\x60\x60\x60', raw_decision, re.DOTALL)
if m:
    try:
        entry = json.loads(m.group(1))
    except:
        pass
if not entry:
    for line in raw_decision.strip().split('\n'):
        line = line.strip()
        if line.startswith('{') and 'action' in line:
            try:
                entry = json.loads(line)
                break
            except:
                pass

# If no structured JSON from Claude, build from state.json
if not entry:
    pos = state.get('position')
    last_trade = state.get('trades', [])[-1] if state.get('trades') else None
    if pos:
        action = last_trade.get('action', 'BUY') if last_trade and last_trade.get('timestamp') == state.get('last_scan') else 'HOLD'
    else:
        action = 'WAIT'
    # Detect action from raw text
    raw_lower = raw_decision.lower()
    for a in ['BUY', 'SELL', 'SHORT', 'HOLD', 'WAIT', 'MONITOR']:
        if a.lower() in raw_lower:
            action = a
            break
    entry = {
        'action': action,
        'symbol': pos.get('symbol') if pos else None,
        'chain_of_thought': raw_decision[:500].replace('\n', ' ').strip(),
    }

# Merge with state snapshot
entry['_timestamp'] = timestamp
entry['_mode'] = mode
entry['_market_status'] = market
entry['time_et'] = entry.get('time_et', time_et)
entry['time_bj'] = entry.get('time_bj', time_bj)
entry['state_snapshot'] = {
    'capital': state.get('capital'),
    'position': state.get('position'),
    'realized_pnl': state.get('realized_pnl'),
    'trade_count': state.get('trade_count'),
}

print(json.dumps(entry, ensure_ascii=False, indent=2))
" > "$LOG_FILE" 2>/dev/null

# Step 7: Harness updates last_scan (Claude's Write may fail on WAIT/HOLD)
python3 -c "
import json
from datetime import datetime, timezone
state_file = '$STATE_FILE'
try:
    with open(state_file) as f:
        state = json.load(f)
    state['last_scan'] = datetime.now(timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
    with open(state_file, 'w') as f:
        json.dump(state, f, ensure_ascii=False, indent=2)
except Exception as e:
    print(f'HARNESS_WARN: last_scan update failed: {e}')
" 2>/dev/null

# Cleanup
rm -f "$DECISION_TMP" 2>/dev/null
find "$LOG_DIR" -name "*.json" -mtime +30 -delete 2>/dev/null
find "$LOG_DIR" -name "*.log" -mtime +30 -delete 2>/dev/null
