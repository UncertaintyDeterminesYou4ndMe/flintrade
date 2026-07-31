"""
Flint 单进程总管 —— 把所有 loop 合到一个程序里。

各模块仍只通过 SQLite 队列/状态解耦(谁也不直接调谁),但不再是 N 个 OS 进程,
而是这一个进程里的 N 个线程,各按自己的 cadence 调对应 run_once()。

为什么单进程够:所有 loop 都是 I/O 密集(等 longbridge CLI / claude / HTTP / sleep),
Python 线程在 I/O 上释放 GIL,并发足够;没有 CPU 并行需求。
不变量不丢:单一写者由 db.py 的 role guard 按 DB handle 角色强制(与进程无关);
意图队列仍是 SQLite;每个 loop 套 try/except 做崩溃隔离,硬崩则 launchd 重启本进程。

关键:每个 loop 的 DB 连接/实例在**自己线程内**构造(SQLite 连接默认不跨线程)。

跑:  python3 -m agent.daemon            # 整个交易系统,一个进程
     python3 -m agent.daemon --once     # 每个 loop 跑一次即退出(自检用)
"""
from __future__ import annotations

import fcntl
import os
import resource
import signal
import sys
import threading
import time

from agent.config import load_trading
from agent.db import DB, FLINT_DIR

_stop = threading.Event()
_LOCK_FD = None  # 持有到进程退出
_LAST_OK: dict[str, float] = {}  # loop name → 最近一次成功 run_once 的 monotonic 时刻(看门狗用)


def _acquire_singleton() -> bool:
    """单实例锁:防止两个 daemon 并存(=两个 executor 重复消费/下单)。"""
    global _LOCK_FD
    _LOCK_FD = open(FLINT_DIR / ".daemon.lock", "w")
    try:
        fcntl.flock(_LOCK_FD, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError:
        return False
    _LOCK_FD.write(str(os.getpid()))
    _LOCK_FD.flush()
    return True


def _cadence() -> dict:
    return load_trading()["cadence"]


# ── 每个 loop 的"线程内工厂":返回每轮要调的 callable ────────────────────────
# 工厂在 worker 线程内执行一次,确保实例/连接归属该线程。
def _mk_executor():
    from agent.executor import Executor
    return Executor().process_once

def _mk_reconciler():
    from agent import reconciler
    return reconciler.run_once

def _mk_risk_monitor():
    from agent.risk_monitor import RiskMonitor
    return RiskMonitor().run_once

def _mk_reflect():
    from agent import reflect
    return reflect.run_once

def _mk_loop_technical():
    from agent.producers import loop_technical
    return loop_technical.run_once

def _mk_loop_event():
    from agent.producers import loop_event
    return loop_event.run_once

def _mk_news_collector():
    from agent.producers import news_collector
    return news_collector.run_once


def _loops() -> list[tuple]:
    """(name, factory, cadence_sec)。cadence 现读配置,可热调。"""
    c = _cadence()
    return [
        ("executor",       _mk_executor,       c.get("executor_poll_sec", 10)),
        ("reconciler",     _mk_reconciler,     c.get("reconciler_poll_sec", 15)),
        ("risk_monitor",   _mk_risk_monitor,   c.get("risk_monitor_sec", 10)),
        ("loop_technical", _mk_loop_technical, c.get("technical_loop_sec", 1800)),
        ("loop_event",     _mk_loop_event,     c.get("event_loop_sec", 60)),
        ("news_collector", _mk_news_collector, c.get("news_poll_sec", 300)),
        ("reflect",        _mk_reflect,        c.get("reflect_check_sec", 600)),
    ]


def _run_thunk(name: str, thunk) -> None:
    """调一次 run_once,统一打印输出(兼容返回 str / list[str] / None)。"""
    out = thunk()
    if not out:
        return
    lines = out if isinstance(out, list) else [out]
    for line in lines:
        if line:
            print(f"[{name}] {line}", flush=True)


def _worker(name: str, factory, cadence: float) -> None:
    """一个 loop 的线程主体:线程内构造 → 循环调用 → 可中断 sleep。"""
    try:
        thunk = factory()          # 实例/DB 连接在本线程内创建
    except Exception as e:
        print(f"[{name}] 启动失败,该 loop 退出: {e!r}", file=sys.stderr, flush=True)
        return
    print(f"[daemon] ▶ {name} 每 {cadence}s", flush=True)
    _LAST_OK[name] = time.monotonic()
    while not _stop.is_set():
        try:
            _run_thunk(name, thunk)
            _LAST_OK[name] = time.monotonic()  # 只在成功迭代后打点
        except Exception as e:       # 单 loop 出错不拖垮其余
            print(f"[{name}] error: {e!r}", file=sys.stderr, flush=True)
        _stop.wait(cadence)          # 可被关停信号立即唤醒
    print(f"[daemon] ■ {name} 已停", flush=True)


def _fd_usage() -> tuple[int, int]:
    """(当前打开 fd 数, soft limit)。读不到时 used=-1。"""
    soft, _hard = resource.getrlimit(resource.RLIMIT_NOFILE)
    try:
        used = len(os.listdir("/dev/fd"))
    except OSError:
        used = -1
    return used, soft


def _watchdog_check(loops: list[tuple]) -> str | None:
    """自愈看门狗:返回致命原因字符串,健康则 None。

    背景(2026-07-26 事故):sqlite 连接泄漏耗尽 fd 后,各 loop 每轮报错但进程
    不死,launchd 的 KeepAlive 帮不上忙 —— 系统带病运行 5 天,没有任何交易。
    对策:daemon 主线程盯两件事,任一触发就 os._exit 交给 launchd 重启:
      1. 关键 loop 连续 max(6×cadence, 30min) 没有一次成功迭代(卡死/持续报错);
      2. fd 用量超过 soft limit 的 90%(泄漏早期就重启,而不是等 EMFILE)。
    """
    now_m = time.monotonic()
    for name, _factory, cadence in loops:
        last = _LAST_OK.get(name)
        if last is None:
            continue  # 线程启动失败已单独打日志;不在这里判死刑
        limit = max(6 * cadence, 1800)
        if now_m - last > limit:
            return f"loop '{name}' 已 {int(now_m - last)}s 无成功迭代(阈值 {int(limit)}s)"
    used, soft = _fd_usage()
    if used >= 0 and soft > 0 and used > soft * 0.9:
        return f"fd 用量 {used}/{soft} 超过 90%(疑似泄漏)"
    return None


def _install_signals() -> None:
    def handler(signum, _frame):
        print(f"\n[daemon] 收到信号 {signum},优雅关停…", flush=True)
        _stop.set()
    signal.signal(signal.SIGINT, handler)
    signal.signal(signal.SIGTERM, handler)


def run_once_all() -> None:
    """每个 loop 各跑一次(自检),不起线程、不循环。"""
    for name, factory, _ in _loops():
        try:
            _run_thunk(name, factory())
            print(f"[daemon] ✓ {name} ran once", flush=True)
        except Exception as e:
            print(f"[daemon] ✗ {name}: {e!r}", file=sys.stderr, flush=True)


def main() -> int:
    if "--once" in sys.argv:
        run_once_all()
        return 0

    if not _acquire_singleton():
        print("[daemon] 已有 daemon 在运行(.daemon.lock 被占),退出。", file=sys.stderr)
        return 1

    _install_signals()
    with DB(role="reader") as db:
        db.beat(process="daemon")  # 总管自身心跳一记
    loops = _loops()
    print(f"[daemon] Flint 单进程总管启动 · {len(loops)} loops", flush=True)
    threads = []
    for name, factory, cadence in loops:
        t = threading.Thread(target=_worker, args=(name, factory, cadence),
                             name=name, daemon=True)
        t.start()
        threads.append(t)

    # 限时运行(冒烟/测试用):--seconds N 跑满即优雅关停
    secs = None
    if "--seconds" in sys.argv:
        try:
            secs = float(sys.argv[sys.argv.index("--seconds") + 1])
        except (IndexError, ValueError):
            secs = None

    if secs:
        _stop.wait(secs)
        _stop.set()
    else:
        last_wd = time.monotonic()
        while not _stop.is_set():  # 主线程守着关停信号 + 每 30s 一次看门狗巡检
            _stop.wait(1.0)
            if time.monotonic() - last_wd < 30:
                continue
            last_wd = time.monotonic()
            reason = _watchdog_check(loops)
            if reason:
                print(f"[daemon] WATCHDOG: {reason} — 主动退出,交给 launchd 重启",
                      file=sys.stderr, flush=True)
                try:
                    with DB(role="reader") as db:
                        db.beat(process="watchdog", note=f"restart: {reason}"[:200])
                except Exception:
                    pass
                os._exit(70)  # 不走优雅关停:卡死线程可能让 join 永远等下去
    # 给各 loop 一点时间收尾(它们在 _stop.wait 处会立即醒)
    for t in threads:
        t.join(timeout=10)
    print("[daemon] 全部停止。", flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
