#!/bin/bash
# Flintrade data collector — fetches all market data before calling Claude
# Outputs a JSON blob to stdout that becomes the USER_PROMPT
# Zero LLM cost — pure bash + longbridge CLI + python indicators

set -euo pipefail

FLINTRADE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STATE_FILE="$FLINTRADE_DIR/state.json"

# longbridge 包装:撞 429/限流就退避重试(最多3次),否则回显 stdout。
# 与 agent/lb.py 同思路,覆盖 collect.sh 这个 bash 重型调用方。
lb() {
  local tries=0 out err
  while [ "$tries" -lt 3 ]; do
    out=$(longbridge "$@" 2>/tmp/lb_err.$$ || true)
    err=$(cat /tmp/lb_err.$$ 2>/dev/null || true); rm -f /tmp/lb_err.$$ 2>/dev/null || true
    # 429 限流 或 网络抖动(空响应/超时/连接错误)→ 退避重试
    if printf '%s%s' "$out" "$err" | grep -qiE "429|request is limited|timeout|timed out|connection|network|temporarily"; then
      tries=$((tries+1)); sleep "$tries"; continue
    fi
    if [ -z "$out" ] && [ "$tries" -lt 2 ]; then    # 空响应也重试一次(可能网络)
      tries=$((tries+1)); sleep "$tries"; continue
    fi
    printf '%s' "$out"; return 0
  done
  printf '%s' "$out"; return 0
}

# Tradeable symbols
SYMBOLS="AAPL.US MSFT.US GOOGL.US AMZN.US NVDA.US META.US TSLA.US GLD.US UGL.US SLV.US AGQ.US USO.US"
# Market regime indicators (always include in quotes + klines)
MARKET_SYMBOLS="QQQ.US SPY.US"

# Time
UTC_NOW=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
ET_NOW=$(TZ=America/New_York date '+%Y-%m-%dT%H:%M:%S %Z')
BJ_NOW=$(TZ=Asia/Shanghai date '+%Y-%m-%dT%H:%M:%S CST')
DOW=$(TZ=America/New_York date '+%u')

# Account data from Longbridge (source of truth)
BALANCE=$(lb assets --format json 2>/dev/null || echo '[]')
POSITIONS=$(lb positions --format json 2>/dev/null || echo '[]')
RECENT_ORDERS=$(lb order executions --format json 2>/dev/null | python3 -c "
import json, sys
try:
    data = json.load(sys.stdin)
    # 'order executions' returns fills directly (newest first); keep last 20
    print(json.dumps(data[:20]))
except:
    print('[]')
" 2>/dev/null || echo '[]')

# Quotes for all symbols + market indices
QUOTES=$(lb quote $SYMBOLS $MARKET_SYMBOLS --format json 2>/dev/null || echo '[]')

# Playbook symbols + h4 confirmation window come from strategies.toml — the single
# source of truth for symbol×strategy bindings. Do NOT hardcode symbol lists here.
read -r PLAYBOOK_SYMS H4_LOOKBACK <<< "$(python3 -c "
import tomllib
try:
    pbs = tomllib.load(open('$FLINTRADE_DIR/agent/config/strategies.toml','rb')).get('playbooks',{})
except FileNotFoundError:
    pbs = {}
syms, lb = [], 12
for pb in pbs.values():
    syms += [s for s in pb.get('symbols',[]) if s not in syms]
    lb = max(lb, int(pb.get('lookback', 12)))
print((','.join(syms) or '-') + ' ' + str(lb))" 2>/dev/null || echo "- 12")"

# Always analyze QQQ + SPY (market regime) + playbook names (binding 手册标的
# 不进 kline 名单就永远无法触发), plus top 5 tradeable.
# Rank by TURNOVER (dollar volume), not share volume — a $1,500 stock trading
# $26B/day would lose a share-count ranking to every $200 mega-cap.
KLINE_SYMBOLS=$(echo "$QUOTES" | python3 -c "
import json, sys
market = ['QQQ.US', 'SPY.US']    # always include
playbook = [s for s in '$PLAYBOOK_SYMS'.split(',') if s and s != '-']  # from strategies.toml
try:
    data = json.load(sys.stdin)
    if isinstance(data, list):
        tradeable = [q for q in data if q.get('symbol','') not in market + playbook]
        def dollar_vol(q):
            t = q.get('turnover')
            if t is not None:
                try: return float(t)
                except (TypeError, ValueError): pass
            try: return float(q.get('volume', 0)) * float(q.get('last', 0))
            except (TypeError, ValueError): return 0.0
        sorted_q = sorted(tradeable, key=dollar_vol, reverse=True)
        top5 = [q.get('symbol', '') for q in sorted_q[:5]]
        print(' '.join(market + playbook + top5))
    else:
        print(' '.join(market + playbook + ['NVDA.US', 'AAPL.US', 'MSFT.US', 'TSLA.US', 'META.US']))
except:
    print(' '.join(market + playbook + ['NVDA.US', 'AAPL.US', 'MSFT.US', 'TSLA.US', 'META.US']))
" 2>/dev/null || echo "QQQ.US SPY.US NVDA.US AAPL.US MSFT.US TSLA.US META.US")

# Klines + indicators for top symbols (1h period — backtested as optimal for 30min execution)
# 手册标的拉 240 根喂 4h 聚合并计算 h4;其余标的维持原 50 根、跳过 h4(--h4-lookback 0)
# —— 策略隔离:非手册标的的指标 payload 与手册引入之前完全等价。
IND_ARRAY="["
FIRST=true
for SYM in $KLINE_SYMBOLS; do
  case ",$PLAYBOOK_SYMS," in
    *",$SYM,"*) COUNT=240; H4_FLAG="${H4_LOOKBACK:-12}" ;;
    *)          COUNT=50;  H4_FLAG=0 ;;
  esac
  KLINE=$(lb kline "$SYM" --period 1h --count "$COUNT" --format json 2>/dev/null || echo '[]')
  if [ "$KLINE" != "[]" ] && [ -n "$KLINE" ]; then
    IND=$(echo "$KLINE" | python3 "$FLINTRADE_DIR/scripts/indicators.py" --symbol "$SYM" --h4-lookback "$H4_FLAG" 2>/dev/null || echo '{"symbol":"'$SYM'","error":"calc failed"}')
    if [ "$FIRST" = true ]; then
      FIRST=false
    else
      IND_ARRAY="$IND_ARRAY,"
    fi
    IND_ARRAY="$IND_ARRAY$IND"
  fi
done
IND_ARRAY="$IND_ARRAY]"

# Trading session
SESSION=$(lb trading session --format json 2>/dev/null || echo '[]')

# Resolved session + outside-RTH flag (passed in by run.sh; defaults keep set -u happy
# when collect.sh is run standalone for debugging).
CURRENT_SESSION="${MARKET_STATUS:-unknown}"
OUTSIDE_RTH_HINT="${OUTSIDE_RTH:-RTH_ONLY}"

# PnL feedback from state.json — win rate, P/L ratio, recent trades
PNL_FEEDBACK=$(python3 -c "
import json, sys
from datetime import datetime

try:
    with open('$STATE_FILE') as f:
        state = json.load(f)
except:
    state = {}

trades = state.get('trades', [])

# Pair BUY/SELL into round trips
round_trips = []
i = 0
while i < len(trades) - 1:
    t = trades[i]
    if t.get('side') == 'BUY' or (t.get('side') == 'SELL' and t.get('pnl') is None):
        if i + 1 < len(trades) and trades[i+1].get('pnl') is not None:
            round_trips.append(trades[i+1])  # the closing trade has pnl
            i += 2
            continue
    i += 1

wins = [t for t in round_trips if t.get('pnl', 0) > 0]
losses = [t for t in round_trips if t.get('pnl', 0) <= 0]

win_count = len(wins)
loss_count = len(losses)
total = win_count + loss_count

win_rate = win_count / total if total > 0 else 0
avg_win = sum(t['pnl'] for t in wins) / win_count if win_count else 0
avg_loss = abs(sum(t['pnl'] for t in losses) / loss_count) if loss_count else 0
pl_ratio = avg_win / avg_loss if avg_loss > 0 else 0

# Invocation count from dispatch log
invocation_count = 0
try:
    with open('$FLINTRADE_DIR/logs/dispatch.log') as f:
        invocation_count = sum(1 for line in f if 'mode=TRADE' in line or 'mode=PLAN' in line)
except:
    pass

# Time since first trade
created = state.get('created', '')
elapsed_minutes = 0
if created:
    try:
        start = datetime.strptime(created, '%Y-%m-%d')
        elapsed_minutes = int((datetime.now() - start).total_seconds() / 60)
    except:
        pass

# Last 10 trades (raw, for context)
last_trades = trades[-10:] if trades else []

feedback = {
    'win_rate': round(win_rate, 3),
    'pl_ratio': round(pl_ratio, 3),
    'total_round_trips': total,
    'wins': win_count,
    'losses': loss_count,
    'avg_win': round(avg_win, 2),
    'avg_loss': round(avg_loss, 2),
    'realized_pnl': state.get('realized_pnl', 0),
    'capital': state.get('capital', 1200),
    'elapsed_minutes': elapsed_minutes,
    'invocation_count': invocation_count,
    'position': state.get('position'),
    'last_trades': last_trades,
}
print(json.dumps(feedback, ensure_ascii=False))
" 2>/dev/null || echo '{}')

# Build the data payload
python3 -c "
import json, sys

data = {
    'time': {
        'utc': '$UTC_NOW',
        'et': '$ET_NOW',
        'bj': '$BJ_NOW',
        'dow': int('$DOW'),
        'dow_name': ['Mon','Tue','Wed','Thu','Fri','Sat','Sun'][int('$DOW')-1]
    },
    'session': json.loads('''$SESSION''') if '''$SESSION''' != '[]' else [],
    'current_session': '$CURRENT_SESSION',
    'outside_rth': '$OUTSIDE_RTH_HINT',
    'account': {
        'balance': json.loads('''$BALANCE'''),
        'positions': json.loads('''$POSITIONS'''),
        'recent_orders': json.loads('''$RECENT_ORDERS''')
    },
    'quotes': json.loads('''$QUOTES''') if '''$QUOTES''' != '[]' else [],
    'indicators': json.loads('''$IND_ARRAY'''),
    'pnl_feedback': json.loads('''$PNL_FEEDBACK'''),
}
print(json.dumps(data, ensure_ascii=False, indent=2))
" 2>/dev/null
