"""
Flint 进程监管层(Phase 2 watchdog + 健康 CLI)。

职责:只观察,不写库。以 reader 角色读取 heartbeats / risk_state / positions /
intents / trades,判断每个长驻进程是否还在心跳。STALE/MISSING 时把告警 **打印** 到
stdout/stderr(reader 无写权限,刻意不落库)。

用法:
    python3 -m agent.supervisor            # 默认:run_forever 看门狗循环
    python3 -m agent.supervisor --status   # 一次性健康仪表盘后退出

不变量:reconciler 死了意味着持仓可能脱离跟踪地漂移 —— 这条告警最要命。
纯 stdlib。
"""
from __future__ import annotations

import sys
import time
from datetime import datetime, timezone

from agent.db import DB, now
from agent.config import load_trading

# 应当持续心跳的长驻进程。
EXPECTED = ["executor", "reconciler", "risk_monitor", "loop_technical"]

_ISO = "%Y-%m-%dT%H:%M:%SZ"


def _parse_utc(ts: str) -> datetime:
    """把库里的 'YYYY-MM-DDTHH:MM:SSZ' 解析成带 UTC tzinfo 的 datetime。"""
    return datetime.strptime(ts, _ISO).replace(tzinfo=timezone.utc)


def _stale_threshold() -> int:
    try:
        return int(load_trading()["cadence"]["heartbeat_stale_sec"])
    except Exception:
        return 120


def check(db: DB | None = None, stale_sec: int | None = None) -> list[dict]:
    """
    逐个 EXPECTED 进程读心跳,计算 staleness = now - last_beat。
      up      —— staleness <= stale_sec
      STALE   —— staleness >  stale_sec
      MISSING —— heartbeats 里没有该进程的行
    返回结构化结果(每项: process / status / age_sec / pid / last_beat / note)。
    """
    own = db is None
    if own:
        db = DB(role="reader")
    if stale_sec is None:
        stale_sec = _stale_threshold()
    try:
        beats = {r["process"]: r for r in db.heartbeats()}
        ref = _parse_utc(now())
        results: list[dict] = []
        for proc in EXPECTED:
            row = beats.get(proc)
            if row is None:
                results.append({
                    "process": proc, "status": "MISSING", "age_sec": None,
                    "pid": None, "last_beat": None, "note": None,
                })
                continue
            try:
                age = int((ref - _parse_utc(row["last_beat"])).total_seconds())
            except (ValueError, TypeError):
                age = None
            status = "up" if (age is not None and age <= stale_sec) else "STALE"
            results.append({
                "process": proc, "status": status, "age_sec": age,
                "pid": row["pid"], "last_beat": row["last_beat"], "note": row["note"],
            })
        return results
    finally:
        if own:
            db.close()


def _fmt_age(age: int | None) -> str:
    if age is None:
        return "  --  "
    if age < 120:
        return f"{age:4d}s "
    if age < 7200:
        return f"{age // 60:4d}m "
    return f"{age // 3600:4d}h "


def _pending_intents(db: DB) -> int:
    row = db.conn.execute(
        "SELECT COUNT(*) AS n FROM intents WHERE status='pending'"
    ).fetchone()
    return int(row["n"]) if row else 0


def status() -> None:
    """打印一屏紧凑的健康仪表盘。"""
    stale_sec = _stale_threshold()
    with DB(role="reader") as db:
        results = check(db, stale_sec)
        risk = db.get_risk()
        positions = db.open_positions()
        pending = _pending_intents(db)
        trades = db.recent_trades(limit=1)

    icon = {"up": "✅", "STALE": "⚠️ ", "MISSING": "❌"}
    print("=" * 60)
    print(f"  FLINT SUPERVISOR  ·  {now()}  ·  stale>{stale_sec}s")
    print("=" * 60)
    print("  PROCESSES")
    for r in results:
        pid = f"pid {r['pid']}" if r["pid"] else "no pid"
        line = (f"    {icon[r['status']]} {r['process']:<14} "
                f"{r['status']:<7} {_fmt_age(r['age_sec'])} {pid}")
        if r["note"]:
            line += f"  ({r['note']})"
        print(line)

    print("  RISK")
    if risk is None:
        print("    (no risk_state row)")
    else:
        halt = "HALTED" if risk["halt"] else "ok"
        equity = risk["equity"]
        eq_s = f"${equity:,.2f}" if equity is not None else "n/a"
        open_risk = risk["open_risk"]
        or_s = f"${open_risk:,.2f}" if open_risk is not None else "n/a"
        halt_icon = "🛑" if risk["halt"] else "  "
        print(f"    {halt_icon} halt={halt}  equity={eq_s}  open_risk={or_s}")
        if risk["halt"] and risk["halt_reason"]:
            print(f"       reason: {risk['halt_reason']}")

    print("  STATE")
    print(f"    open positions : {len(positions)}")
    for p in positions:
        print(f"      - {p['symbol']:<10} {p['side']:<5} qty={p['qty']} "
              f"entry={p['entry_price']}")
    print(f"    pending intents: {pending}")
    if trades:
        t = trades[0]
        pnl = t["pnl"]
        pnl_s = f" pnl={pnl:+.2f}" if pnl is not None else ""
        print(f"    last trade     : {t['ts']}  {t['action']} {t['qty']} "
              f"{t['symbol']} @ {t['fill_price']}{pnl_s}")
    else:
        print("    last trade     : (none)")
    print("=" * 60)


def run_forever() -> None:
    """看门狗循环:每 ~stale/2 秒检查一次,对 STALE/MISSING 进程打印醒目告警。"""
    stale_sec = _stale_threshold()
    interval = max(5, stale_sec // 2)
    print(f"[supervisor] watchdog started · interval={interval}s · "
          f"stale>{stale_sec}s · watching {', '.join(EXPECTED)}", flush=True)
    with DB(role="reader") as db:
        while True:
            try:
                results = check(db, stale_sec)
            except Exception as e:  # 库瞬时锁等不该弄死看门狗
                print(f"[supervisor] check error: {e}", file=sys.stderr, flush=True)
                time.sleep(interval)
                continue

            down = [r for r in results if r["status"] != "up"]
            ts = now()
            if down:
                for r in down:
                    age = f"{r['age_sec']}s" if r["age_sec"] is not None else "no beat"
                    msg = f"⚠️  PROCESS DOWN: {r['process']} {r['status'].lower()} {age}"
                    if r["process"] == "reconciler":
                        msg += "  (positions may drift untracked!)"
                    print(f"[{ts}] {msg}", file=sys.stderr, flush=True)
            else:
                print(f"[{ts}] all {len(results)} processes up", flush=True)
            time.sleep(interval)


def main(argv: list[str] | None = None) -> int:
    argv = sys.argv[1:] if argv is None else argv
    if "--status" in argv or "-s" in argv:
        status()
        return 0
    if "--help" in argv or "-h" in argv:
        print(__doc__)
        return 0
    run_forever()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
