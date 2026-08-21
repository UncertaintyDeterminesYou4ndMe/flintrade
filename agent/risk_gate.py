"""
组合风控引擎(Risk Gate)—— 取代旧的「一仓制」。

Executor 对每个 pending intent 串行调用 evaluate()。全部硬编码、不经 LLM:
  1. HALT 检查      —— 熔断时除平仓类一律拒
  2. 去重           —— dedup_key 已 pending/filled
  3. 冷却 no-revenge —— 同票刚平亏损单
  4. 过滤器         —— volume_ratio / session 收盘黑窗 / 无效止损
  5. 风险定额       —— qty = max_risk_pct*equity / |entry-stop|(取代拍脑袋半/全仓)
  6. 组合约束       —— concurrent / gross / open_risk / per_symbol / per_cluster
  7. 冲突仲裁       —— 同票反向持仓:按 priority 让路(在 executor 层结合此结果处理)

evaluate() 不下单、不写库,只返回裁决 Verdict。执行交给 Executor。
"""
from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone

from agent.config import cluster_of, load_risk, playbook_for


@dataclass
class Verdict:
    approved: bool
    qty: int = 0                     # 风险定额后的最终下单量
    reason: str = ""                 # 拒绝原因 / 批准说明
    notes: dict = field(default_factory=dict)


def _parse_ts(ts: str | None):
    if not ts:
        return None
    try:
        return datetime.strptime(ts, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
    except (ValueError, TypeError):
        return None


class RiskGate:
    """无状态:每次 evaluate 现读 risk.toml(热加载)+ 当前持仓快照。"""

    def __init__(self, *, equity: float, halted: bool, open_positions: list,
                 recent_trades: list, session: str, minutes_to_close: int | None,
                 volume_ratio: float | None = None, now: datetime | None = None,
                 open_orders: list | None = None):
        self.cfg = load_risk()
        self.equity = equity
        self.halted = halted
        self.positions = open_positions          # list of dict-likes: symbol/side/qty/entry_price/risk_amt
        self.recent_trades = recent_trades       # newest-first dict-likes: symbol/action/pnl/ts
        self.session = session
        self.mins_to_close = minutes_to_close
        self.volume_ratio = volume_ratio
        self.now = now or datetime.now(timezone.utc)
        self._cluster = cluster_of()
        # 在途(submitted/partial)开仓单:结算延迟期间也是真实敞口,组合约束必须把它们
        # 计进去,否则限价单堆挂单时,gate 只看已结算仓位会严重低估敞口(实例:5 笔
        # 16 股 AMZN 堆单 = $19.4K 名义敞口,而 gate 一直觉得是空仓)。
        self.inflight = [
            {"symbol": o.get("symbol"), "notional": (o.get("qty") or 0) * o["price"]}
            for o in (open_orders or [])
            if o.get("side") in ("BUY", "SHORT") and o.get("price") is not None
        ]

    # ── 组合快照辅助 ──────────────────────────────────────────────────────
    def _gross_notional(self) -> float:
        held = sum(abs(p["qty"] * p["entry_price"]) for p in self.positions)
        inflight = sum(abs(x["notional"]) for x in self.inflight)
        return held + inflight

    def _symbol_notional(self, symbol: str) -> float:
        held = sum(abs(p["qty"] * p["entry_price"]) for p in self.positions if p["symbol"] == symbol)
        inflight = sum(abs(x["notional"]) for x in self.inflight if x["symbol"] == symbol)
        return held + inflight

    def _cluster_notional(self, cluster: str) -> float:
        held = sum(abs(p["qty"] * p["entry_price"]) for p in self.positions
                   if self._cluster.get(p["symbol"]) == cluster)
        inflight = sum(abs(x["notional"]) for x in self.inflight
                       if self._cluster.get(x["symbol"]) == cluster)
        return held + inflight

    def _open_risk(self) -> float:
        return sum((p.get("risk_amt") or 0.0) for p in self.positions)

    def _pct(self, key_section: str, key: str) -> float:
        return float(self.cfg[key_section][key])

    def _confidence_scale(self, conf) -> float:
        """自报 confidence → 批准量缩放系数(只减不增,上限 1.0)。

        兑现两份 producer prompt 里「The Executor scales risk by it」的契约
        (在此之前该承诺从未被代码兑现,producer 诚实报低分没有任何后果)。
        Goodhart 属性:虚报高分最多拿到名义上限内的满额 —— 即今天的现状,
        没有额外好处;报低分则真实缩减敞口。刻意用自报分数的线性映射而非
        历史校准 —— confidence_buckets 的桶样本还小(总 n=75),等样本够了
        再考虑校准化缩放。conf 缺失(用户手动单等)按 1.0 向后兼容。
        """
        try:
            cs = self.cfg["per_trade"]["confidence_scale"]
        except (KeyError, TypeError):
            return 1.0  # 配置未定义 → 不缩放(旧 risk.toml 兼容)
        if conf is None:
            return 1.0
        full_at = float(cs.get("full_at", 75))
        floor_at = float(cs.get("floor_at", 50))
        floor_scale = float(cs.get("floor_scale", 0.5))
        if conf >= full_at:
            return 1.0
        if conf <= floor_at or full_at <= floor_at:
            return floor_scale
        return floor_scale + (1.0 - floor_scale) * (conf - floor_at) / (full_at - floor_at)

    # ── 主裁决 ────────────────────────────────────────────────────────────
    def evaluate(self, intent: dict) -> Verdict:
        side = intent["side"]
        symbol = intent["symbol"]
        is_exit = side in ("flatten", "close")

        # 1. HALT —— 平仓类永远放行(熔断时要能减仓),开仓类拒
        if self.halted and not is_exit:
            return Verdict(False, reason="HALT: 全局熔断,仅允许平仓")

        if is_exit:
            # 平仓不受组合约束/定额限制,但必须确有持仓
            pos = next((p for p in self.positions if p["symbol"] == symbol), None)
            if not pos:
                return Verdict(False, reason=f"无 {symbol} 持仓可平")
            qty = int(pos["qty"])
            # 部分平仓(分批止盈):意图带 hypothesis_qty 且小于持仓量时只平该部分
            hq = intent.get("hypothesis_qty")
            if hq is not None and 0 < int(hq) < qty:
                qty = int(hq)
            return Verdict(True, qty=qty, reason="平仓放行", notes={"exit": True})

        # 方向许可
        if side == "short" and not self.cfg["direction"]["allow_short"]:
            return Verdict(False, reason="配置禁止做空")

        # 播放手册硬门槛(strategies.toml,binding=true):自动开仓必须携带手册的
        # 共振确认(producer 从指标快照如实附上,不采信 LLM 自述)。prompt 里的
        # 承诺升级为闸门的硬规则 —— 与 volume_ratio 过滤同级。人工(user)单豁免:
        # 人可以 override 策略,止损/熔断等其余防线照常适用。
        pb = playbook_for(symbol)
        if pb and pb.get("binding") and intent.get("source") != "user":
            if pb.get("long_only") and side != "long":
                return Verdict(False, reason=f"playbook[{pb['name']}]: 仅允许多头入场")
            feats = intent.get("features")
            if not (isinstance(feats, dict) and feats.get("h4_confluence")):
                return Verdict(False, reason=f"playbook[{pb['name']}]: 缺少 h4 共振确认(binding)")

        # 3. 冷却 no-revenge:同票最近一笔平仓为亏损且在冷却窗内
        cd = self._pct("per_trade", "revenge_cooldown_min")
        for t in self.recent_trades:
            if t["symbol"] != symbol:
                continue
            if t.get("pnl") is not None:  # 平仓腿
                if t["pnl"] <= 0:
                    ts = _parse_ts(t.get("ts"))
                    if ts and self.now - ts < timedelta(minutes=cd):
                        mins = int((timedelta(minutes=cd) - (self.now - ts)).total_seconds() / 60)
                        return Verdict(False, reason=f"no-revenge 冷却中({symbol} 还剩 ~{mins}min)")
                break  # 只看该票最近一次平仓

        # 4. 过滤器
        floor = self._pct("per_trade", "volume_ratio_floor")
        if self.volume_ratio is not None and self.volume_ratio < floor:
            return Verdict(False, reason=f"volume_ratio {self.volume_ratio:.2f} < {floor}")

        blackout = self._pct("per_trade", "session_close_blackout_min")
        if self.mins_to_close is not None and 0 <= self.mins_to_close < blackout:
            return Verdict(False, reason=f"距收盘 {self.mins_to_close}min < {blackout}min 黑窗,不开新仓")

        entry = intent.get("entry_hint")
        stop = intent.get("stop")
        if not entry or not stop or entry <= 0:
            return Verdict(False, reason="缺 entry_hint/stop,无法风险定额")
        stop_dist = abs(entry - stop)
        min_dist = entry * self._pct("per_trade", "min_stop_distance_pct") / 100.0
        if stop_dist < min_dist:
            return Verdict(False, reason=f"止损距离 {stop_dist:.4f} 过近(<{min_dist:.4f}),无效止损")

        # 5. 风险定额:qty = (max_risk_pct% * equity) / 每股风险
        max_risk_amt = self._pct("per_trade", "max_risk_pct") / 100.0 * self.equity
        qty = int(max_risk_amt // stop_dist)
        if qty < 1:
            return Verdict(False, reason=f"风险预算 ${max_risk_amt:.2f} 不足 1 股(每股风险 ${stop_dist:.2f})")

        # 6. 组合约束(加上这笔后是否突破;突破则缩量,缩到 0 则拒)
        port = self.cfg["portfolio"]
        # 6a. 并发仓数(新票才占用名额;加仓同票不增计数;在途开仓单同样占名额)
        held_syms = {p["symbol"] for p in self.positions} | {x["symbol"] for x in self.inflight}
        if symbol not in held_syms and len(held_syms) >= int(port["max_concurrent_positions"]):
            return Verdict(False, reason=f"已达并发仓上限 {port['max_concurrent_positions']}")

        notional_per_share = entry
        caps = []  # (剩余可用名义额, 标签)
        caps.append((port["max_gross_exposure_pct"] / 100.0 * self.equity - self._gross_notional(), "gross"))
        caps.append((port["max_per_symbol_pct"] / 100.0 * self.equity - self._symbol_notional(symbol), "per_symbol"))
        cl = self._cluster.get(symbol)
        if cl:
            caps.append((self.cfg["clusters"]["max_per_cluster_pct"] / 100.0 * self.equity
                         - self._cluster_notional(cl), f"cluster:{cl}"))

        for remaining, label in caps:
            allowed_qty = int(remaining // notional_per_share)
            if allowed_qty < qty:
                if allowed_qty < 1:
                    return Verdict(False, reason=f"{label} 限额已满,无可用敞口")
                qty = allowed_qty  # 缩量到约束允许的最大

        # 6c. confidence 缩放 —— 必须在名义上限之后:实测名义上限(per_symbol 40%/
        # cluster 60%)几乎总是先于 2% 风险定额卡住批准量(#71 定额 42 股被压到 12,
        # #70 定额 78 股被压到 6),缩放若作用在定额上会被上限吞掉,变成 no-op。
        conf = intent.get("confidence")
        scale = self._confidence_scale(conf)
        if scale < 1.0:
            scaled = int(qty * scale)
            if scaled < 1:
                return Verdict(False, reason=f"confidence {conf} 缩放({scale:.2f})后不足 1 股")
            qty = scaled

        # 6b. 组合总风险约束
        new_risk = qty * stop_dist
        max_open_risk = port["max_open_risk_pct"] / 100.0 * self.equity
        room = max_open_risk - self._open_risk()
        if new_risk > room:
            qty = int(room // stop_dist)
            if qty < 1:
                return Verdict(False, reason=f"组合总风险已达上限 {port['max_open_risk_pct']}%")

        scale_note = f", conf {conf}→×{scale:.2f}" if scale < 1.0 else ""
        return Verdict(
            True, qty=qty,
            reason=f"批准 {qty} 股(风险定额 ${qty*stop_dist:.2f} / 预算 ${max_risk_amt:.2f}{scale_note})",
            notes={"risk_amt": round(qty * stop_dist, 4), "stop_dist": round(stop_dist, 4),
                   "confidence_scale": round(scale, 4)},
        )
