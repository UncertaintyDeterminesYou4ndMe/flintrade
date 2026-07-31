#!/bin/bash
# Flintrade agent launcher —— 加载 flintrade.env 凭据后运行指定模块。
# 用法:
#   ./scripts/agentctl.sh executor --once
#   ./scripts/agentctl.sh producers.loop_technical --once
#   ./scripts/agentctl.sh user_cli status
#   ./scripts/agentctl.sh reflect --force
# 凭据单一来源 = flintrade.env(gitignore)。手动跑任何进程都不用再 export。

FLINTRADE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export PATH="$HOME/.local/bin:/opt/homebrew/bin:$PATH"

if [ ! -f "$FLINTRADE_DIR/flintrade.env" ]; then
  echo "缺 flintrade.env(凭据来源)。" >&2
  exit 1
fi
set -a
source "$FLINTRADE_DIR/flintrade.env"
set +a

if [ -z "$1" ]; then
  echo "用法: ./scripts/agentctl.sh <module-after-agent> [args...]" >&2
  echo "  例: ./scripts/agentctl.sh user_cli status" >&2
  exit 2
fi

MODULE="$1"; shift
cd "$FLINTRADE_DIR" || exit 1
# 优先用 .venv(内含 lancedb/fastembed,语义记忆才活);无则回退系统 python(记忆降级)
PY="$FLINTRADE_DIR/.venv/bin/python"
[ -x "$PY" ] || PY="python3"
exec "$PY" -m "agent.$MODULE" "$@"
