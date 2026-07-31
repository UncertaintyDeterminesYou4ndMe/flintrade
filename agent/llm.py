"""
可插拔 LLM 访问层 —— chat 补全 + embedding,零 Python 依赖(stdlib urllib)。

设计(参考 maka-agent 的声明式 provider registry + kimi-code 的通用 OpenAI 协议实现):
  * PROVIDERS 是唯一的注册表:新增一个厂商 = 加一条声明,业务代码不动。
    协议只有三种 wire:"cli"(claude CLI subprocess,吃 Claude Code 订阅鉴权)、
    "openai"(chat/completions,覆盖 OpenAI/DeepSeek/Kimi/Qwen/OpenRouter/
    Ollama/LM Studio/vLLM 等一切兼容端)、"anthropic"(原生 Messages API)。
  * 档位(tier)→ provider/model 映射在 trading.toml [models];embedding 在
    [embeddings]。换厂商只改 toml。
  * 交易系统的确定性优先:**不做跨模型自动 fallback**。调用失败重试后仍失败
    → 返回 ""(调用方解析失败 → WAIT,不交易)。宁可错过,不换脑子交易。
  * API key 一律从环境变量读(flint.env 注入),配置文件里永远不落 key。
    base_url 仅接受 http/https(防止把 key 发到奇怪的地方)。

干跑:FLINT_LLM_DRY=1 时 complete() 不真调模型,打印将执行的调用并返回占位串
(用于无花费验证,理念同 broker 的 dry-run)。

自检:python -m agent.llm check   # 每个 tier 解析到哪个 provider/model、key 是否就位
     python -m agent.llm ping    # 真调一次各 tier(会花一点钱),验证端到端连通
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

from agent.config import load_trading

_TRUTHY = {"1", "true", "yes"}


def _dry() -> bool:
    return os.environ.get("FLINT_LLM_DRY", "").strip().lower() in _TRUTHY


# ── provider 注册表(声明式;新增厂商只加一条) ────────────────────────────────
# wire ∈ {"cli", "openai", "anthropic"}。base_url 是默认值,tier 配置可覆盖;
# openai wire 的约定:base_url 已含版本段(如 …/v1),实际请求 = base_url + "/chat/completions"。
PROVIDERS: dict[str, dict] = {
    "claude-cli": {"wire": "cli", "cli": "claude"},     # Claude Code 订阅,免 API key
    "kimi-cli":   {"wire": "cli", "cli": "kimi"},       # Kimi Code 订阅 plan(kimi login 后免 key)
                                                        # 注意:≠ moonshot(开放平台按量计费)
    "anthropic":  {"wire": "anthropic",
                   "base_url": "https://api.anthropic.com",
                   "api_key_env": "ANTHROPIC_API_KEY"},
    "openai":     {"wire": "openai",
                   "base_url": "https://api.openai.com/v1",
                   "api_key_env": "OPENAI_API_KEY"},
    "deepseek":   {"wire": "openai",
                   "base_url": "https://api.deepseek.com",
                   "api_key_env": "DEEPSEEK_API_KEY"},
    "moonshot":   {"wire": "openai",                    # Kimi 开放平台(按量计费)
                   "base_url": "https://api.moonshot.cn/v1",
                   "api_key_env": "MOONSHOT_API_KEY"},
    "kimi-plan":  {"wire": "anthropic",                 # Kimi Code plan 的 sk-key 直连
                   "base_url": "https://api.kimi.com/coding",  # Anthropic 兼容端
                   "api_key_env": "KIMI_PLAN_API_KEY"},        # 可以用别人共享的 plan key,无需订阅/CLI
    "openrouter": {"wire": "openai",
                   "base_url": "https://openrouter.ai/api/v1",
                   "api_key_env": "OPENROUTER_API_KEY"},
    "ollama":     {"wire": "openai",                    # 本地,无 key
                   "base_url": "http://localhost:11434/v1",
                   "api_key_env": ""},
    # 万能逃生门:任意 OpenAI 兼容端。base_url 必须写在 tier 配置里。
    "openai_compatible": {"wire": "openai", "api_key_env": "LLM_API_KEY"},
}
_ALIASES = {"claude": "claude-cli"}  # 向后兼容旧 trading.toml


class _Fatal(Exception):
    """不可重试的失败(4xx 鉴权/参数错误 —— 重试也不会好)。"""


def _http_post(url: str, payload: dict, headers: dict, timeout: int = 60) -> dict | None:
    """stdlib urllib POST JSON。可重试失败返回 None;不可重试抛 _Fatal。"""
    if not url.startswith(("http://", "https://")):  # scheme 白名单,防 key 泄漏
        raise _Fatal(f"拒绝非 http(s) base_url: {url}")
    data = json.dumps(payload).encode()
    req = urllib.request.Request(url, data=data, method="POST",
                                 headers={"Content-Type": "application/json", **headers})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            out = json.loads(r.read().decode())
            usage = out.get("usage") if isinstance(out, dict) else None
            if usage:  # 成本可观测:按响应记 token 用量(kimi-code 的做法)
                print(f"[llm] usage {payload.get('model')}: {json.dumps(usage)}",
                      file=sys.stderr)
            return out
    except urllib.error.HTTPError as e:
        body = ""
        try:
            body = e.read().decode()[:200]
        except OSError:
            pass
        if e.code == 429 or e.code >= 500:  # 限流/服务端错误:可重试
            print(f"[llm] POST {url} HTTP {e.code}(将重试): {body}", file=sys.stderr)
            return None
        raise _Fatal(f"HTTP {e.code}: {body}") from e  # 401/403/400:重试无意义
    except (urllib.error.URLError, ValueError, OSError) as e:
        print(f"[llm] POST {url} 失败(将重试): {e}", file=sys.stderr)
        return None


def _http_post_retry(url: str, payload: dict, headers: dict, timeout: int) -> dict | None:
    """同一端点最多 3 次(429/5xx/网络错误指数退避;4xx 直接放弃)。不跨模型 fallback。"""
    for delay in (0, 2, 8):
        if delay:
            time.sleep(delay)
        try:
            out = _http_post(url, payload, headers, timeout)
        except _Fatal as e:
            print(f"[llm] POST {url} 不可重试: {e}", file=sys.stderr)
            return None
        if out is not None:
            return out
    return None


# ── chat 补全 ──────────────────────────────────────────────────────────────
def resolve_tier(tier: str) -> dict:
    """tier → 完整配置:PROVIDERS 默认值 + trading.toml [models] 覆盖。"""
    cfg = dict(load_trading().get("models", {}).get(tier) or
               {"provider": "claude-cli", "model": ""})
    name = _ALIASES.get(cfg.get("provider", "claude-cli"), cfg.get("provider", "claude-cli"))
    base = PROVIDERS.get(name)
    if base is None:
        raise ValueError(f"未知 chat provider: {name!r}(可用: {sorted(PROVIDERS)})")
    merged = {**base, **cfg, "provider": name}
    if merged["wire"] != "cli" and not merged.get("model"):
        raise ValueError(f"tier {tier!r} 用 {name} 必须显式指定 model(交易系统不吃默认模型)")
    return merged


def _complete_cli(user_prompt: str, system_prompt: str | None, cfg: dict,
                  max_budget_usd: float) -> str:
    """订阅制 provider 走本机 agent CLI 子进程 —— CLI 自己管 OAuth/plan 计费。

    claude:有 --system-prompt-file 与 --max-budget-usd。
    kimi:  只有 -p/--prompt 与 -m;system prompt 以分隔线并入 prompt(纯分析
           调用,无工具,注意力损失可接受)。
    """
    bin_, model = cfg.get("cli", "claude"), cfg.get("model", "")
    tmp = None
    if bin_ == "kimi":
        text = f"{system_prompt}\n\n---\n\n{user_prompt}" if system_prompt else user_prompt
        args = ["kimi", "-p", text] + (["-m", model] if model else [])
        nargs = len(args)
    else:  # claude
        args = ["claude", "-p", "--allowedTools", "", "--max-budget-usd", str(max_budget_usd)]
        if system_prompt:
            tmp = tempfile.NamedTemporaryFile("w", suffix=".md", delete=False)
            tmp.write(system_prompt)
            tmp.close()
            args += ["--system-prompt-file", tmp.name]
        if model:
            args += ["--model", model]
        args.append(user_prompt)
        nargs = len(args)
    if _dry():
        print(f"[llm DRY] {bin_} argv({nargs}): <prompt {len(user_prompt)} chars> model={model}",
              file=sys.stderr)
        return '{"action":"WAIT","reasoning":"[llm dry-run]"}'
    try:
        res = subprocess.run(args, capture_output=True, text=True,
                             timeout=int(cfg.get("timeout_sec", 300)))
        out = res.stdout
        if bin_ == "kimi":  # 洗掉 kimi -p 的装饰:行首圆点、尾部 resume 提示
            lines = [l for l in out.splitlines()
                     if not l.startswith("To resume this session:")]
            out = "\n".join(l[2:] if l.startswith("• ") else l for l in lines).strip()
        return out
    finally:
        if tmp:
            os.unlink(tmp.name)


def _complete_openai(user_prompt: str, system_prompt: str | None, cfg: dict) -> str:
    """OpenAI chat/completions 协议 —— 事实标准,一份实现覆盖绝大多数厂商。"""
    base = (cfg.get("base_url") or "").rstrip("/")
    if not base:
        print(f"[llm] provider={cfg['provider']} 缺 base_url", file=sys.stderr)
        return ""
    key_env = cfg.get("api_key_env", "")
    key = os.environ.get(key_env, "") if key_env else ""
    headers = {"Authorization": f"Bearer {key}"} if key else {}
    headers.update(cfg.get("extra_headers", {}))
    msgs = ([{"role": "system", "content": system_prompt}] if system_prompt else []) + \
           [{"role": "user", "content": user_prompt}]
    payload = {"model": cfg["model"], "messages": msgs, "stream": False}
    for k in ("temperature", "max_tokens"):
        if k in cfg:
            payload[k] = cfg[k]
    if _dry():
        print(f"[llm DRY] openai-wire {base} model={cfg['model']} msgs={len(msgs)}",
              file=sys.stderr)
        return '{"action":"WAIT","reasoning":"[llm dry-run]"}'
    out = _http_post_retry(f"{base}/chat/completions", payload, headers,
                           int(cfg.get("timeout_sec", 300)))
    try:
        return out["choices"][0]["message"]["content"] or ""
    except (KeyError, IndexError, TypeError):
        return ""


def _complete_anthropic(user_prompt: str, system_prompt: str | None, cfg: dict) -> str:
    """Anthropic 原生 Messages API(直接持 ANTHROPIC_API_KEY 的用户走这条)。"""
    base = (cfg.get("base_url") or "https://api.anthropic.com").rstrip("/")
    key = os.environ.get(cfg.get("api_key_env", "ANTHROPIC_API_KEY"), "")
    payload = {"model": cfg["model"],
               "max_tokens": int(cfg.get("max_tokens", 4096)),
               "messages": [{"role": "user", "content": user_prompt}]}
    if system_prompt:
        payload["system"] = system_prompt
    if "temperature" in cfg:
        payload["temperature"] = cfg["temperature"]
    if _dry():
        print(f"[llm DRY] anthropic-wire {base} model={cfg['model']}", file=sys.stderr)
        return '{"action":"WAIT","reasoning":"[llm dry-run]"}'
    out = _http_post_retry(f"{base}/v1/messages", payload,
                           {"x-api-key": key, "anthropic-version": "2023-06-01"},
                           int(cfg.get("timeout_sec", 300)))
    try:
        return "".join(b.get("text", "") for b in out["content"]
                       if b.get("type") == "text")
    except (KeyError, TypeError):
        return ""


def complete(user_prompt: str, *, system_prompt: str | None = None,
             tier: str = "trader", max_budget_usd: float = 1.5) -> str:
    """按档位补全。tier ∈ {trader, flash, ...}(见 trading.toml [models])。

    失败语义:重试耗尽后返回 ""。调用方把空串当解析失败 → WAIT(不交易)。
    """
    cfg = resolve_tier(tier)
    wire = cfg["wire"]
    if wire == "cli":
        return _complete_cli(user_prompt, system_prompt, cfg, max_budget_usd)
    if wire == "openai":
        return _complete_openai(user_prompt, system_prompt, cfg)
    if wire == "anthropic":
        return _complete_anthropic(user_prompt, system_prompt, cfg)
    raise ValueError(f"未知 wire: {wire!r}")


# ── embedding(本地 Ollama 优先)──────────────────────────────────────────
def _post_quiet(url: str, payload: dict, headers: dict, timeout: int = 60) -> dict | None:
    """embedding 用:任何失败(含不可重试)都归一为 None,由语义层优雅降级。"""
    try:
        return _http_post(url, payload, headers, timeout)
    except _Fatal as e:
        print(f"[llm] POST {url} 失败: {e}", file=sys.stderr)
        return None


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
        out = _post_quiet(f"{base}/api/embed", {"model": mdl, "input": texts}, {}, timeout=120)
        if out and "embeddings" in out:
            return out["embeddings"]
        vecs = []
        for t in texts:
            o = _post_quiet(f"{base}/api/embeddings", {"model": mdl, "prompt": t}, {}, timeout=120)
            if not o or "embedding" not in o:
                return None
            vecs.append(o["embedding"])
        return vecs

    if provider in ("openai", "voyage", "openai_compatible"):
        key = os.environ.get(cfg.get("api_key_env", "EMBED_API_KEY"), "")
        out = _post_quiet(f"{base}/embeddings", {"model": mdl, "input": texts},
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


def _cli_check(live: bool) -> int:
    """自检每个 tier 的解析与就绪度。live=True 时真调一次(花一点钱)。"""
    tiers = sorted(load_trading().get("models", {})) or ["trader", "flash"]
    bad = 0
    for tier in tiers:
        try:
            cfg = resolve_tier(tier)
        except ValueError as e:
            print(f"✗ {tier}: {e}")
            bad += 1
            continue
        wire, prov, model = cfg["wire"], cfg["provider"], cfg.get("model", "(cli default)")
        line = f"{tier}: provider={prov} wire={wire} model={model}"
        if wire == "cli":
            import shutil
            bin_ = cfg.get("cli", "claude")
            ok = shutil.which(bin_) is not None
            line += f" | {bin_} CLI: " + ("找到" if ok else "未安装(装对应 agent CLI,或改用 API provider)")
        else:
            env = cfg.get("api_key_env", "")
            ok = bool(os.environ.get(env)) if env else True
            line += f" | key(${env or '无需'}): " + ("就位" if ok else "缺失")
        bad += 0 if ok else 1
        print(("✓ " if ok else "✗ ") + line)
        if live:
            out = complete("Reply with the single word: pong", tier=tier, max_budget_usd=0.05)
            ok = "pong" in out.lower()
            print(f"  ping → {'✓ ' + out.strip()[:60] if ok else '✗ 响应异常: ' + (out.strip()[:80] or '(空)')}")
            bad += 0 if ok else 1
    print(f"embeddings_available: {embeddings_available()}")
    return 1 if bad else 0


if __name__ == "__main__":
    cmd = sys.argv[1] if len(sys.argv) > 1 else "check"
    if cmd == "check":
        raise SystemExit(_cli_check(live=False))
    if cmd == "ping":
        raise SystemExit(_cli_check(live=True))
    print("用法: python -m agent.llm [check|ping]", file=sys.stderr)
    raise SystemExit(2)
