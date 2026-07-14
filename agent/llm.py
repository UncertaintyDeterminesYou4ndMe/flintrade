"""
可插拔 LLM 访问层 —— chat 补全 + embedding,零 Python 依赖。

设计:
  * chat 现在只接 Claude(`claude -p` subprocess,Claude Code 自带鉴权)。
    openai 兼容路径(DeepSeek / OpenAI / 任意兼容端)已实现,改配置即启用。
  * embedding 接本地 Ollama(urllib 调 localhost:11434,GGUF 端侧小模型,
    如 embeddinggemma / qwen3-embedding)。零 API 成本、零 Python 依赖。
    Ollama 没起时优雅降级(返回 None),不拖垮调用方。

档位(tier)→ provider/model 映射在 trading.toml [models];embedding 在 [embeddings]。
后期接 DeepSeek 等,只改 toml,业务代码不动。

干跑:FLINT_LLM_DRY=1 时 complete() 不真调模型,打印将执行的命令并返回占位串
(用于无花费验证,理念同 broker 的 dry-run)。
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import urllib.error
import urllib.request

from agent.config import load_trading

_TRUTHY = {"1", "true", "yes"}


def _dry() -> bool:
    return os.environ.get("FLINT_LLM_DRY", "").strip().lower() in _TRUTHY


def _http_post(url: str, payload: dict, headers: dict, timeout: int = 60) -> dict | None:
    """stdlib urllib POST JSON。失败返回 None(调用方决定降级)。"""
    data = json.dumps(payload).encode()
    req = urllib.request.Request(url, data=data, method="POST",
                                 headers={"Content-Type": "application/json", **headers})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return json.loads(r.read().decode())
    except (urllib.error.URLError, urllib.error.HTTPError, ValueError, OSError) as e:
        print(f"[llm] POST {url} 失败: {e}", file=sys.stderr)
        return None


# ── chat 补全 ──────────────────────────────────────────────────────────────
def _tier_cfg(tier: str) -> dict:
    models = load_trading().get("models", {})
    cfg = models.get(tier)
    if not cfg:  # 缺配置兜底:claude 默认
        return {"provider": "claude", "model": ""}
    return cfg


def _complete_claude(user_prompt: str, system_prompt: str | None, model: str,
                     max_budget_usd: float) -> str:
    args = ["claude", "-p", "--allowedTools", "", "--max-budget-usd", str(max_budget_usd)]
    tmp = None
    if system_prompt:
        tmp = tempfile.NamedTemporaryFile("w", suffix=".md", delete=False)
        tmp.write(system_prompt)
        tmp.close()
        args += ["--system-prompt-file", tmp.name]
    if model:
        args += ["--model", model]
    args.append(user_prompt)
    if _dry():
        print(f"[llm DRY] claude argv: {args[:-1] + ['<prompt %d chars>' % len(user_prompt)]}",
              file=sys.stderr)
        return '{"action":"WAIT","reasoning":"[llm dry-run]"}'
    try:
        res = subprocess.run(args, capture_output=True, text=True, timeout=300)
        return res.stdout
    finally:
        if tmp:
            os.unlink(tmp.name)


def _complete_openai(user_prompt: str, system_prompt: str | None, model: str,
                     cfg: dict) -> str:
    """OpenAI 兼容(DeepSeek/OpenAI/本地兼容端)。现未配置,接 DeepSeek 时启用。"""
    base = cfg.get("base_url", "https://api.deepseek.com")
    key = os.environ.get(cfg.get("api_key_env", "DEEPSEEK_API_KEY"), "")
    msgs = []
    if system_prompt:
        msgs.append({"role": "system", "content": system_prompt})
    msgs.append({"role": "user", "content": user_prompt})
    if _dry():
        print(f"[llm DRY] openai {base} model={model} msgs={len(msgs)}", file=sys.stderr)
        return '{"action":"WAIT","reasoning":"[llm dry-run]"}'
    out = _http_post(f"{base}/chat/completions",
                     {"model": model, "messages": msgs, "stream": False},
                     {"Authorization": f"Bearer {key}"})
    if not out:
        return ""
    try:
        return out["choices"][0]["message"]["content"]
    except (KeyError, IndexError, TypeError):
        return ""


def complete(user_prompt: str, *, system_prompt: str | None = None,
             tier: str = "trader", max_budget_usd: float = 1.5) -> str:
    """按档位补全。tier ∈ {trader, flash, ...}(见 trading.toml [models])。"""
    cfg = _tier_cfg(tier)
    provider = cfg.get("provider", "claude")
    model = cfg.get("model", "")
    if provider == "claude":
        return _complete_claude(user_prompt, system_prompt, model, max_budget_usd)
    if provider in ("deepseek", "openai", "openai_compatible"):
        return _complete_openai(user_prompt, system_prompt, model, cfg)
    raise ValueError(f"未知 chat provider: {provider!r}")


# ── embedding(本地 Ollama 优先)──────────────────────────────────────────
def _embed_cfg() -> dict:
    return load_trading().get("embeddings", {"provider": "ollama",
                                             "model": "embeddinggemma",
                                             "base_url": "http://localhost:11434"})


_FASTEMBED = None  # (model_name, TextEmbedding) 缓存:加载模型昂贵,只做一次


def _fastembed(name: str):
    global _FASTEMBED
    if _FASTEMBED is None or _FASTEMBED[0] != name:
        from fastembed import TextEmbedding  # 仅在 .venv 中可用;缺则上层降级
        _FASTEMBED = (name, TextEmbedding(model_name=name))
    return _FASTEMBED[1]


def embed(texts: list[str], *, model: str | None = None) -> list[list[float]] | None:
    """文本 → 向量。后端不可用时返回 None(语义层据此优雅降级)。"""
    if not texts:
        return []
    cfg = _embed_cfg()
    provider = cfg.get("provider", "fastembed")
    mdl = model or cfg.get("model", "sentence-transformers/all-MiniLM-L6-v2")
    base = cfg.get("base_url", "http://localhost:11434")

    if provider == "fastembed":
        # 进程内 ONNX 小模型(同那个 app)。首次会下载权重。
        try:
            m = _fastembed(mdl)
            return [[float(x) for x in v] for v in m.embed(list(texts))]
        except Exception as e:
            print(f"[llm] fastembed 不可用({e});语义层降级", file=sys.stderr)
            return None

    if provider == "ollama":
        # 新版 /api/embed 支持批量 input;老版 /api/embeddings 单条 prompt。
        out = _http_post(f"{base}/api/embed", {"model": mdl, "input": texts}, {}, timeout=120)
        if out and "embeddings" in out:
            return out["embeddings"]
        vecs = []
        for t in texts:
            o = _http_post(f"{base}/api/embeddings", {"model": mdl, "prompt": t}, {}, timeout=120)
            if not o or "embedding" not in o:
                return None
            vecs.append(o["embedding"])
        return vecs

    if provider in ("openai", "voyage", "openai_compatible"):
        key = os.environ.get(cfg.get("api_key_env", "EMBED_API_KEY"), "")
        out = _http_post(f"{base}/embeddings", {"model": mdl, "input": texts},
                         {"Authorization": f"Bearer {key}"})
        if out and "data" in out:
            return [d["embedding"] for d in out["data"]]
        return None

    print(f"[llm] 未知 embedding provider: {provider!r}", file=sys.stderr)
    return None


def cosine(a: list[float], b: list[float]) -> float:
    """余弦相似度(纯 Python,够小规模暴力检索用)。"""
    if not a or not b or len(a) != len(b):
        return 0.0
    dot = sum(x * y for x, y in zip(a, b))
    na = sum(x * x for x in a) ** 0.5
    nb = sum(y * y for y in b) ** 0.5
    return dot / (na * nb) if na and nb else 0.0


def embeddings_available() -> bool:
    """探活:embedding 后端是否就绪(供语义层决定是否启用)。"""
    return embed(["ping"]) is not None


if __name__ == "__main__":
    print("chat tier=flash dry:", complete("say hi", tier="flash") if _dry() else "(set FLINT_LLM_DRY=1)")
    print("embeddings_available:", embeddings_available())
