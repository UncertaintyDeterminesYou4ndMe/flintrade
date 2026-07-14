"""
longbridge CLI 速率治理器 —— 所有 Python 侧的 longbridge 调用唯一入口。

为什么:daemon 是单进程多线程,多个 loop 并发打 longbridge 会撞限流(429002)
和超时。这里用一个**进程内**(跨线程)的最小间隔限速 + 429/超时**指数退避重试**,
把并发突发抹平、把瞬时限流自愈。collect.sh(bash 子进程)另在脚本里加 429 重试。

用法:
    from agent import lb
    ok, data, raw = lb.run(["quote", "NVDA.US", "--format", "json"])   # data=已解析 JSON 或 None
    ok, data, raw = lb.run(["order","buy","NVDA.US","3","--price","200","--outside-rth","ANY_TIME","-y","--format","json"], timeout=20)

可调(env):FLINT_LB_MIN_INTERVAL(默认 0.35s 全局最小间隔)、FLINT_LB_RETRIES(默认 4)。
"""
from __future__ import annotations

import json
import os
import subprocess
import threading
import time

_lock = threading.Lock()
_last_start = 0.0
_MIN_INTERVAL = float(os.environ.get("FLINT_LB_MIN_INTERVAL", "0.35"))
_RETRIES = int(os.environ.get("FLINT_LB_RETRIES", "4"))


def _rate_limited(s: str) -> bool:
    s = s.lower()
    return "429" in s or "request is limited" in s or "rate limit" in s


def _pace():
    """跨线程:让相邻两次调用的起点至少间隔 _MIN_INTERVAL,抹平突发。"""
    global _last_start
    with _lock:
        wait = _MIN_INTERVAL - (time.monotonic() - _last_start)
        if wait > 0:
            time.sleep(wait)
        _last_start = time.monotonic()


def run(args: list[str], *, timeout: int = 20, retries: int | None = None) -> tuple[bool, object | None, str]:
    """治理后的 longbridge 调用。返回 (ok, 解析后的JSON或None, 原始stdout/错误串)。

    429 或超时 → 指数退避重试(2,4,6,8s)。FileNotFoundError → 立即失败。
    """
    n = retries if retries is not None else _RETRIES
    last_err = "retries exhausted"
    for attempt in range(n):
        _pace()
        try:
            p = subprocess.run(["longbridge", *args], capture_output=True, text=True,
                               timeout=timeout, env=os.environ.copy())
        except FileNotFoundError:
            return False, None, "longbridge CLI not found on PATH"
        except subprocess.TimeoutExpired:
            last_err = f"timeout after {timeout}s"
            time.sleep(2 * (attempt + 1))
            continue
        except Exception as e:  # 防御:任何 subprocess 异常都退避重试
            last_err = repr(e)
            time.sleep(2 * (attempt + 1))
            continue

        combined = (p.stdout or "") + "\n" + (p.stderr or "")
        if p.returncode != 0:
            last_err = (p.stderr or "").strip() or f"exit {p.returncode}"
            if _rate_limited(combined) and attempt < n - 1:
                time.sleep(2 * (attempt + 1))  # 限流退避
                continue
            return False, None, last_err

        out = (p.stdout or "").strip()
        if not out:
            return True, None, ""
        try:
            return True, json.loads(out), out
        except (ValueError, TypeError):
            # 容忍前导非 JSON 行 —— `order buy/sell` 会先打一行
            # "Submitting Buy order: ..." 再跟 JSON 体({"order_id":...})。
            # 严格 json.loads 整段会失败;这里抽出内嵌的 JSON 对象/数组。
            import re
            m = re.search(r"(\{.*\}|\[.*\])", out, re.DOTALL)
            if m:
                try:
                    return True, json.loads(m.group(1)), out
                except (ValueError, TypeError):
                    pass
            return True, None, out  # 确实无 JSON

    return False, None, last_err


if __name__ == "__main__":
    import sys
    ok, data, raw = run(sys.argv[1:] or ["trading", "session", "--format", "json"])
    print("ok:", ok)
    print("data:", json.dumps(data, ensure_ascii=False)[:300] if data is not None else None)
    if not ok:
        print("err:", raw)
