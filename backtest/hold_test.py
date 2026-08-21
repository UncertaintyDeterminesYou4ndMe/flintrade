#!/usr/bin/env python3
"""
持仓时长规则回测:时间止损(max_hold) vs 亏损重审(reeval_losing)。

动机:live 库 73 笔复盘按时长分桶,12-24h 桶 +$523(最佳),>24h 桶净 -$394 但
内含 +167/+127/+110 三笔大赢单 —— 硬时间止损的反事实无法从表内推算,按
CLAUDE.md 契约送回测。reeval_losing 是旧 prompt.md「亏损持仓 >4h 强制重审」
规则的悲观代理(一律平仓;真实重审会选择性持有),给出该规则价值的下界。

两个时期:
  A: 2026(data/ 5m 撮合 + data_1h 信号)—— 与 rule_test.py 同口径
  B: 2025(data_1h_2025 信号+撮合,1h 粒度)—— 体制外验证
"""
import os
import sys

sys.path.insert(0, os.path.dirname(__file__))
from rule_test import load_data, build_time_lookup, run_backtest  # noqa: E402

BASE_DIR = os.path.dirname(__file__)

# 与 rule_test.py 相同的基准参数(此前 sweep 的最优)+ 已上线的两条规则
BASE = dict(tp_pct=0.75, sl_pct=1.0, score_threshold=4,
            min_volume_ratio=0.3, no_entry_before_close=30)

VARIANTS = [
    ("baseline (vol0.3+close30)", {}),
    ("reeval losing @4h",   {"reeval_losing_min": 240}),
    ("reeval losing @8h",   {"reeval_losing_min": 480}),
    ("max hold 12h",        {"max_hold_min": 720}),
    ("max hold 24h",        {"max_hold_min": 1440}),
    ("max hold 48h",        {"max_hold_min": 2880}),
    ("24h + reeval4h",      {"max_hold_min": 1440, "reeval_losing_min": 240}),
]


def run_period(name, exec_data, sig_data, sig_lookup, bar_minutes, entry_every):
    print(f"\n================ Period {name} ================")
    rows = []
    for label, over in VARIANTS:
        r = run_backtest(exec_data, sig_data, sig_lookup,
                         bar_minutes=bar_minutes, entry_every_bars=entry_every,
                         label=label, **{**BASE, **over})
        exits = {}
        for t in r["all_trades"]:
            exits[t["exit_reason"]] = exits.get(t["exit_reason"], 0) + 1
        rows.append((label, r, exits))
    hdr = f"{'variant':<28}{'n':>4}{'win%':>7}{'pnl':>10}{'pf':>6}{'maxDD':>7}{'avg_hold':>9}  exits"
    print(hdr)
    print("-" * len(hdr))
    for label, r, exits in rows:
        ex = " ".join(f"{k}:{v}" for k, v in sorted(exits.items()))
        print(f"{label:<28}{r['trades']:>4}{r['win_rate']:>7}{r['total_pnl']:>10}"
              f"{r['pf']:>6}{r['max_dd']:>7}{r['avg_hold_min']:>9.0f}  {ex}")
    return rows


def main():
    print("Loading 2026 data (5m exec + 1h signal)...")
    exec_5m = load_data(os.path.join(BASE_DIR, "data"))
    sig_1h = load_data(os.path.join(BASE_DIR, "data_1h"))
    run_period("A: 2026 (5m exec)", exec_5m, sig_1h, build_time_lookup(sig_1h),
               bar_minutes=5, entry_every=6)

    print("\nLoading 2025 data (1h exec+signal)...")
    d_2025 = load_data(os.path.join(BASE_DIR, "data_1h_2025"))
    if d_2025:
        run_period("B: 2025 (1h exec)", d_2025, d_2025, build_time_lookup(d_2025),
                   bar_minutes=60, entry_every=1)
    else:
        print("  data_1h_2025 缺失,跳过")


if __name__ == "__main__":
    main()
