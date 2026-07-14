"""配置加载:risk.toml(风控引擎) + trading.toml(标的/执行/节奏)。
运行时重新调用 load_* 即热加载(Executor 每轮重读,改 toml 立即生效)。"""
from __future__ import annotations

import tomllib
from functools import lru_cache
from pathlib import Path

CONFIG_DIR = Path(__file__).resolve().parent / "config"


def _load(name: str) -> dict:
    with open(CONFIG_DIR / name, "rb") as f:
        return tomllib.load(f)


def load_risk() -> dict:
    """每次读盘,支持热加载。"""
    return _load("risk.toml")


def load_trading() -> dict:
    return _load("trading.toml")


@lru_cache(maxsize=1)
def cluster_of() -> dict[str, str]:
    """symbol → cluster 名(mega_tech/gold/silver/oil)。"""
    clusters = load_risk()["clusters"]
    out = {}
    for name, syms in clusters.items():
        if isinstance(syms, list):
            for s in syms:
                out[s] = name
    return out


if __name__ == "__main__":
    import json
    print(json.dumps({"risk": load_risk(), "trading": load_trading()}, ensure_ascii=False, indent=2))
