"""
Flint 用户手动控制 + 状态界面(「现在啥情况」)。

设计原则:用户手动交易**不绕过风控**。本 CLI 只往 intents 队列投递意图
(source='user', priority=最高),由 Executor 的 Risk Gate 统一定额/裁决;
状态查询走只读角色。绝不直接调用 broker。纯 stdlib。

角色边界(db.py 的 _WRITE_PERMS):
  * user   —— 仅 {'intents_submit'}。submit_intent 合法,其余写库一律拒绝。
  * reader —— 只读。所有 status 查询走这个角色。
'user' 角色无权写 halt —— halt/resume 委托给 agent.risk_monitor(它持 'halt' 许可)。

用法:
    python3 -m agent.user_cli                       # 默认 = status,「现在啥情况」
    python3 -m agent.user_cli status
    python3 -m agent.user_cli buy NVDA --stop 200 --target 220 --reason "..."
    python3 -m agent.user_cli short TSLA --stop 250
    python3 -m agent.user_cli close NVDA --reason "take profit"
    python3 -m agent.user_cli halt "manual kill"
    python3 -m agent.user_cli resume
"""
from __future__ import annotations

import argparse
import json
import subprocess
import sys

from agent.config import load_risk, load_trading
from agent.db import DB, now as _now


def _fmt_gap(prev_iso: str | None) -> str:
    """距上次互动多久 —— 给 agent 时空纵深感。"""
    if not prev_iso:
        return "首次互动"
    from datetime import datetime, timezone
    try:
        prev = datetime.strptime(prev_iso, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
    except (ValueError, TypeError):
        return "未知"
    secs = (datetime.now(timezone.utc) - prev).total_seconds()
    if secs < 90:
        return f"{int(secs)}秒前"
    if secs < 5400:
        return f"{int(secs/60)}分钟前"
    if secs < 172800:
        return f"{secs/3600:.1f}小时前"
    return f"{secs/86400:.1f}天前"


# ── 实时报价(--entry 省略时用作 entry_hint;Risk Gate 需要它做风险定额)──────
def _current_price(symbol: str) -> float | None:
    """取最新价。容忍多种 longbridge quote JSON 形态;失败返回 None。"""
    try:
        from agent import lb
        ok, data, _ = lb.run(["quote", symbol, "--format", "json"], timeout=15)
        if not ok or data is None:
            return None
        rows = data if isinstance(data, list) else [data]
        for q in rows:
            if not isinstance(q, dict):
                continue
            for key in ("last_done", "last", "price", "last_price", "close"):
                v = q.get(key)
                if v not in (None, "", 0, "0"):
                    return float(v)
    except (ValueError, TypeError, subprocess.SubprocessError):
        return None
    return None


# ── 标的规范化 ────────────────────────────────────────────────────────────
def _universe() -> list[str]:
    """tradeable 标的(.US 形式)。market regime 标的不可交易,排除在外。"""
    return list(load_trading()["universe"]["symbols"])


def normalize_symbol(raw: str) -> str:
    """
    'NVDA' 或 'nvda' 或 'NVDA.US' → 'NVDA.US'(校验在 universe 内)。
    不在 universe → ValueError(带可交易清单提示)。
    """
    s = raw.strip().upper()
    if not s:
        raise ValueError("空标的")
    if "." not in s:
        s = f"{s}.US"
    uni = _universe()
    if s not in uni:
        bare = ", ".join(x.removesuffix(".US") for x in uni)
        raise ValueError(
            f"{raw!r} 不在可交易 universe(规范化为 {s!r})。\n"
            f"  可交易标的: {bare}"
        )
    return s


def _user_priority() -> int:
    """用户意图优先级 —— 最高,赢冲突仲裁。"""
    return int(load_risk()["priority"]["user"])


# ── 意图提交(open / close)──────────────────────────────────────────────
def _submit_open(args, side: str) -> int:
    """提交 long/short 开仓意图。--stop 必填(风险定额的分母)。"""
    if args.stop is None:
        print(
            f"错误:{side} 需要 --stop(它是 Risk Gate 风险定额的分母)。\n"
            f"  例: python3 -m agent.user_cli {side if side=='long' else 'short'} "
            f"{args.symbol} --stop <价位> [--target T] [--entry E]",
            file=sys.stderr,
        )
        return 2

    try:
        symbol = normalize_symbol(args.symbol)
    except ValueError as e:
        print(f"错误:{e}", file=sys.stderr)
        return 2

    # --entry 省略 → 取实时报价。Risk Gate 用 entry_hint 做风险定额,不能为空。
    entry = args.entry
    if entry is None:
        entry = _current_price(symbol)
        if entry is None:
            print(
                f"错误:取不到 {symbol} 实时报价,无法定 entry。\n"
                f"  请显式给 --entry <价位>(Risk Gate 需要 entry 做风险定额)。",
                file=sys.stderr,
            )
            return 2
        print(f"  (--entry 省略,用实时报价 {entry:.2f} 作 entry_hint)")

    prio = _user_priority()
    with DB(role="user") as db:
        iid = db.submit_intent(
            source="user",
            priority=prio,
            symbol=symbol,
            side=side,
            entry_hint=entry,
            stop=args.stop,
            target=args.target,
            confidence=args.confidence,
            reason=args.reason,
        )
    if iid is None:
        print("意图未创建(dedup_key 冲突?)。", file=sys.stderr)
        return 1
    print(f"已提交 {side} 意图 #{iid}  {symbol}  stop={args.stop}"
          + (f" target={args.target}" if args.target is not None else "")
          + (f" entry={args.entry}" if args.entry is not None else "")
          + f"  (source=user, priority={prio} = 最高)")
    print("注意:意图进入待裁决队列。Executor 的 Risk Gate 会定额并批准/拒绝 —— "
          "手动单和自动单走**同一道风控闸门**。")
    return 0


def _submit_close(args) -> int:
    """提交 close 意图。无对应持仓只告警,仍提交(Gate 会拒真空仓)。"""
    try:
        symbol = normalize_symbol(args.symbol)
    except ValueError as e:
        print(f"错误:{e}", file=sys.stderr)
        return 2

    prio = _user_priority()
    with DB(role="reader") as rdb:
        pos = rdb.position_for(symbol)
    if pos is None:
        print(f"告警:当前无 {symbol} 的未平持仓。仍提交 close 意图 —— "
              f"若确无持仓,Risk Gate 会拒绝。", file=sys.stderr)

    with DB(role="user") as db:
        iid = db.submit_intent(
            source="user",
            priority=prio,
            symbol=symbol,
            side="close",
            reason=args.reason,
        )
    if iid is None:
        print("意图未创建(dedup_key 冲突?)。", file=sys.stderr)
        return 1
    print(f"已提交 close 意图 #{iid}  {symbol}  (source=user, priority={prio})")
    print("注意:Executor 会执行平仓;Risk Gate 对退出永远放行。")
    return 0


# ── halt / resume(委托 risk_monitor;user 角色无 halt 权)────────────────
def _do_halt(reason: str) -> int:
    try:
        from agent.risk_monitor import RiskMonitor
    except Exception as e:
        print(f"无法导入 risk_monitor({e})。\n"
              f"  请手动运行: python3 -m agent.risk_monitor --halt \"{reason}\"",
              file=sys.stderr)
        return 1
    try:
        rm = RiskMonitor()
        for line in rm.halt(reason):
            print(line)
        return 0
    except Exception as e:
        print(f"halt 失败({e})。\n"
              f"  请手动运行: python3 -m agent.risk_monitor --halt \"{reason}\"",
              file=sys.stderr)
        return 1


def _do_resume(reason: str) -> int:
    try:
        from agent.risk_monitor import RiskMonitor
    except Exception as e:
        print(f"无法导入 risk_monitor({e})。\n"
              f"  请手动运行: python3 -m agent.risk_monitor --resume \"{reason}\"",
              file=sys.stderr)
        return 1
    try:
        rm = RiskMonitor()
        for line in rm.resume(reason):
            print(line)
        return 0
    except Exception as e:
        print(f"resume 失败({e})。\n"
              f"  请手动运行: python3 -m agent.risk_monitor --resume \"{reason}\"",
              file=sys.stderr)
        return 1


# ── status(「现在啥情况」)────────────────────────────────────────────────
def _money(v, sign: bool = False) -> str:
    if v is None:
        return "n/a"
    return f"${v:+,.2f}" if sign else f"${v:,.2f}"


def _status(prev_seen: str | None = None) -> int:
    with DB(role="reader") as db:
        risk = db.get_risk()
        last_dream = db.kv_get("last_dream_date")
        inception_date = db.kv_get("inception_date")
        inception_eq = db.kv_get("inception_equity")
        positions = db.open_positions()
        trades = db.recent_trades(limit=8)
        pending = db.conn.execute(
            "SELECT * FROM intents WHERE status='pending' "
            "ORDER BY priority DESC, created_at"
        ).fetchall()

    line = "─" * 64
    print(line)
    print("  FLINT · 现在啥情况")
    print(f"  时空锚点  距上次互动 {_fmt_gap(prev_seen)}"
          + (f"   上次做梦 {last_dream}" if last_dream else "   尚未做梦"))
    print(line)

    # ── RISK ──
    if risk is None:
        print("  RISK    (risk_state 行缺失;请先 init_db)")
    else:
        halted = bool(risk["halt"])
        flag = "🛑 HALTED" if halted else "✅ ok"
        equity = risk["equity"]
        ds = risk["day_start_equity"]
        day_pnl = None
        if equity is not None and ds is not None:
            day_pnl = float(equity) - float(ds)
        print(f"  RISK    {flag}   equity={_money(equity)}   "
              f"day_pnl={_money(day_pnl, sign=True)}   "
              f"open_risk={_money(risk['open_risk'])}")
        if halted and risk["halt_reason"]:
            print(f"          reason: {risk['halt_reason']}")
        # 自 inception 的收益(本金 sleeve 基准)
        if inception_eq and equity is not None:
            try:
                base = float(inception_eq)
                ret = float(equity) - base
                pct = ret / base * 100 if base else 0.0
                print(f"  收益    自 {inception_date or '?'} 起  "
                      f"{_money(ret, sign=True)} ({pct:+.2f}%)   本金 sleeve {_money(base)}")
            except (ValueError, TypeError):
                pass

    # ── POSITIONS ──
    print(f"  POSITIONS ({len(positions)})")
    if not positions:
        print("    (无持仓)")
    else:
        print(f"    {'symbol':<10} {'side':<5} {'qty':>5} {'entry':>9} "
              f"{'stop':>9} {'target':>9} {'risk':>9}")
        for p in positions:
            stop = "n/a" if p["stop"] is None else f"{p['stop']:.2f}"
            tgt = "n/a" if p["target"] is None else f"{p['target']:.2f}"
            rk = "n/a" if p["risk_amt"] is None else f"${p['risk_amt']:.2f}"
            print(f"    {p['symbol']:<10} {p['side']:<5} {p['qty']:>5} "
                  f"{p['entry_price']:>9.2f} {stop:>9} {tgt:>9} {rk:>9}")

    # ── PENDING INTENTS ──
    print(f"  PENDING INTENTS ({len(pending)})")
    if not pending:
        print("    (无待裁决意图)")
    else:
        print(f"    {'#id':>5} {'source':<12} {'prio':>4} {'side':<6} "
              f"{'symbol':<10} {'stop':>8} reason")
        for it in pending:
            stop = "" if it["stop"] is None else f"{it['stop']:.2f}"
            reason = (it["reason"] or "")[:28]
            print(f"    {it['id']:>5} {it['source']:<12} {it['priority']:>4} "
                  f"{it['side']:<6} {it['symbol']:<10} {stop:>8} {reason}")

    # ── RECENT TRADES ──
    print("  RECENT TRADES")
    if not trades:
        print("    (无成交)")
    else:
        print(f"    {'ts':<21} {'action':<6} {'qty':>5} {'symbol':<10} "
              f"{'fill':>9} {'pnl':>10} src")
        for t in trades:
            pnl = "" if t["pnl"] is None else f"{t['pnl']:+,.2f}"
            print(f"    {t['ts']:<21} {t['action']:<6} {t['qty']:>5} "
                  f"{t['symbol']:<10} {t['fill_price']:>9.2f} {pnl:>10} "
                  f"{t['source'] or ''}")
    print(line)
    return 0


# ── argparse ──────────────────────────────────────────────────────────────
def _build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        prog="agent.user_cli",
        description="Flint 用户手动控制 + 状态。手动单走同一道风控闸门(不绕过风控)。",
    )
    sub = p.add_subparsers(dest="cmd")

    sub.add_parser("status", help="交易全景:持仓 / 风险 / 待裁决意图 / 近期成交")

    def _add_open(name, help_):
        sp = sub.add_parser(name, help=help_)
        sp.add_argument("symbol", help="标的(NVDA 或 NVDA.US)")
        sp.add_argument("--stop", type=float, required=False,
                        help="止损位(必填;风险定额分母)")
        sp.add_argument("--target", type=float, help="目标位")
        sp.add_argument("--entry", type=float, help="期望入场价(限价参考)")
        sp.add_argument("--confidence", type=int, help="信心 0-100")
        sp.add_argument("--reason", help="备注 / 理由")
        return sp

    _add_open("buy", "提交 long 开仓意图(需 --stop)")
    _add_open("short", "提交 short 开仓意图(需 --stop)")

    cl = sub.add_parser("close", help="提交 close 意图(平掉已有持仓)")
    cl.add_argument("symbol", help="标的(NVDA 或 NVDA.US)")
    cl.add_argument("--reason", help="备注 / 理由")

    h = sub.add_parser("halt", help="拍急停(委托 risk_monitor)")
    h.add_argument("reason", nargs="?", default="manual halt (user_cli)",
                   help="原因")

    r = sub.add_parser("resume", help="解除急停(委托 risk_monitor)")
    r.add_argument("reason", nargs="?", default="manual resume (user_cli)",
                   help="原因")

    return p


def main(argv=None) -> int:
    argv = sys.argv[1:] if argv is None else argv
    parser = _build_parser()
    args = parser.parse_args(argv)

    cmd = args.cmd or "status"

    # 时空锚点:记录本次互动时刻,并取出上次的(供 status 显示「距上次互动多久」)。
    # 这也写进 last_user_seen,reflect.recall() 会读它,让 agent 醒来时有时间纵深感。
    with DB(role="user") as _db:
        prev_seen = _db.kv_get("last_user_seen")
        _db.kv_set("last_user_seen", _now())

    if cmd == "status":
        return _status(prev_seen)
    if cmd == "buy":
        return _submit_open(args, side="long")
    if cmd == "short":
        return _submit_open(args, side="short")
    if cmd == "close":
        return _submit_close(args)
    if cmd == "halt":
        return _do_halt(args.reason)
    if cmd == "resume":
        return _do_resume(args.reason)

    parser.print_help()
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
