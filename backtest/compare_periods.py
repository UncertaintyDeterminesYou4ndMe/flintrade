#!/usr/bin/env python3
"""
Compare trading performance across kline periods (1m, 5m, 1h)
under 30-minute decision frequency.

Key insight: Flint runs every 30 min. The question is which kline period
produces better *signals* at that decision frequency.

- 1m bars: indicators computed on 50 × 1m = 50 min lookback. Decide every 30th bar.
- 5m bars: indicators computed on 50 × 5m = 250 min lookback. Decide every 6th bar.
- 1h bars: indicators computed on 50 × 1h = 50h lookback. Decide every bar (~60 min).

Usage:
    python3 compare_periods.py
"""

import json
import os
import sys
import time
from dataclasses import dataclass, field
from typing import Optional

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "scripts"))
from indicators import analyze

BASE_DIR = os.path.dirname(__file__)

TRADE_SYMBOLS = [
    "AAPL.US", "MSFT.US", "GOOGL.US", "AMZN.US", "NVDA.US",
    "META.US", "TSLA.US", "GLD.US", "UGL.US", "SLV.US", "AGQ.US", "USO.US",
]
MARKET_SYMBOLS = ["QQQ.US", "SPY.US"]


@dataclass
class Trade:
    symbol: str
    side: str
    quantity: int
    entry_price: float
    exit_price: float
    entry_time: str
    exit_time: str
    pnl: float
    hold_bars: int
    exit_reason: str


@dataclass
class Result:
    period: str
    trades: list = field(default_factory=list)
    final_capital: float = 0.0
    peak_capital: float = 0.0
    max_drawdown: float = 0.0
    total_decisions: int = 0

    @property
    def total_pnl(self):
        return sum(t.pnl for t in self.trades)

    @property
    def win_count(self):
        return sum(1 for t in self.trades if t.pnl > 0)

    @property
    def loss_count(self):
        return sum(1 for t in self.trades if t.pnl <= 0)

    @property
    def win_rate(self):
        return self.win_count / len(self.trades) if self.trades else 0

    @property
    def avg_win(self):
        wins = [t.pnl for t in self.trades if t.pnl > 0]
        return sum(wins) / len(wins) if wins else 0

    @property
    def avg_loss(self):
        losses = [t.pnl for t in self.trades if t.pnl <= 0]
        return sum(losses) / len(losses) if losses else 0

    @property
    def profit_factor(self):
        gp = sum(t.pnl for t in self.trades if t.pnl > 0)
        gl = abs(sum(t.pnl for t in self.trades if t.pnl <= 0))
        return gp / gl if gl > 0 else float('inf')

    @property
    def return_pct(self):
        return (self.final_capital - 1200) / 1200 * 100


def load_data(data_dir):
    """Load all symbol data from a directory."""
    data = {}
    for sym in MARKET_SYMBOLS + TRADE_SYMBOLS:
        safe = sym.replace(".", "_")
        path = os.path.join(data_dir, f"{safe}.json")
        if os.path.exists(path):
            with open(path) as f:
                data[sym] = json.load(f)
    return data


def build_timeline(data):
    """Build unified timeline: [(time_str, {sym: bar_idx}), ...]"""
    all_times = set()
    sym_time_idx = {}
    for sym, bars in data.items():
        mapping = {}
        for i, bar in enumerate(bars):
            t = bar["time"]
            all_times.add(t)
            mapping[t] = i
        sym_time_idx[sym] = mapping

    timeline = []
    for t in sorted(all_times):
        indices = {}
        for sym, mapping in sym_time_idx.items():
            if t in mapping:
                indices[sym] = mapping[t]
        timeline.append((t, indices))
    return timeline


def check_market_regime(market_indicators):
    bullish = 0
    bearish = 0
    for sym in ["QQQ.US", "SPY.US"]:
        ind = market_indicators.get(sym)
        if not ind or ind.get("error"):
            return "mixed"
        above_vwap = ind["price"] > ind["vwap"]
        macd_bull = ind["macd"]["histogram"] > 0
        if above_vwap and macd_bull:
            bullish += 1
        elif not above_vwap and not macd_bull:
            bearish += 1
    if bullish == 2:
        return "bullish"
    if bearish == 2:
        return "bearish"
    return "mixed"


def score_short(ind):
    score = 0
    if ind["price"] < ind["vwap"]:
        score += 1
    if ind["price"] < ind["ema20"]:
        score += 1
    macd = ind["macd"]
    if macd["value"] < 0 or macd["histogram"] < 0:
        score += 1
    if 35 <= ind["rsi"] <= 70:
        score += 1
    if ind["volume_ratio"] > 1.5:
        score += 1
    if macd["histogram"] < 0 and macd.get("cross") == "bearish":
        score += 1
    return score


def run_backtest(period_label, data, decision_interval, indicator_window=50,
                 tp_pct=0.75, sl_pct=3.0, score_threshold=5,
                 offhours_threshold=6):
    """
    Run backtest with 30-min decision frequency simulation.

    decision_interval: how many bars between decisions
        1m → 30, 5m → 6, 1h → 1
    """
    timeline = build_timeline(data)
    capital = 1200.0
    peak_capital = capital
    max_drawdown = 0.0
    position = None  # {symbol, side, qty, entry_price, entry_bar, entry_time, stop_loss, take_profit, highest, lowest}
    trades = []
    total_decisions = 0

    sym_bars = {sym: [] for sym in data}
    commission = 0.02

    for bar_idx, (time_str, indices) in enumerate(timeline):
        # Update rolling bars
        for sym, idx in indices.items():
            sym_bars[sym].append(data[sym][idx])

        # Check exit on EVERY bar (stop loss / take profit can trigger anytime)
        if position:
            sym = position["symbol"]
            if sym not in indices:
                continue
            price = data[sym][indices[sym]]["close"]

            # Update trailing high/low
            bar = data[sym][indices[sym]]
            if position["side"] == "long":
                position["highest"] = max(position["highest"], bar["high"])
            else:
                position["lowest"] = min(position["lowest"], bar["low"])

            exit_reason = None

            if position["side"] == "long":
                if price <= position["stop_loss"]:
                    exit_reason = "stop_loss"
                elif price >= position["take_profit"]:
                    net = (price - position["entry_price"]) * position["qty"] - position["qty"] * commission * 2
                    if net > 0:
                        exit_reason = "take_profit"
            else:
                if price >= position["stop_loss"]:
                    exit_reason = "stop_loss"
                elif price <= position["take_profit"]:
                    net = (position["entry_price"] - price) * position["qty"] - position["qty"] * commission * 2
                    if net > 0:
                        exit_reason = "take_profit"

            if exit_reason:
                qty = position["qty"]
                if position["side"] == "long":
                    pnl = (price - position["entry_price"]) * qty - qty * commission * 2
                    capital += price * qty - qty * commission
                else:
                    pnl = (position["entry_price"] - price) * qty - qty * commission * 2
                    capital += (2 * position["entry_price"] - price) * qty - qty * commission

                trades.append(Trade(
                    symbol=sym, side=position["side"], quantity=qty,
                    entry_price=position["entry_price"], exit_price=price,
                    entry_time=position["entry_time"], exit_time=time_str,
                    pnl=pnl, hold_bars=bar_idx - position["entry_bar"],
                    exit_reason=exit_reason,
                ))
                position = None
                peak_capital = max(peak_capital, capital)
                dd = (peak_capital - capital) / peak_capital * 100
                max_drawdown = max(max_drawdown, dd)
                continue

        # Entry decisions only at decision_interval
        if bar_idx % decision_interval != 0:
            continue
        if position:
            continue  # already holding

        total_decisions += 1

        # Check session (offhours needs higher score)
        ref_sym = list(indices.keys())[0] if indices else None
        if not ref_sym:
            continue
        ref_bar = data[ref_sym][indices[ref_sym]]
        offhours = ref_bar.get("session", "Intraday") in ("Pre", "Post", "Overnight")
        threshold = offhours_threshold if offhours else score_threshold

        # Market regime
        market_ind = {}
        for msym in MARKET_SYMBOLS:
            if len(sym_bars.get(msym, [])) >= 30:
                window = sym_bars[msym][-indicator_window:]
                if len(window) >= 30:
                    market_ind[msym] = analyze(window, msym)
        regime = check_market_regime(market_ind)

        # Scan symbols
        candidates = []
        for sym in TRADE_SYMBOLS:
            if sym not in indices:
                continue
            bars = sym_bars.get(sym, [])
            if len(bars) < 30:
                continue
            window = bars[-indicator_window:]
            if len(window) < 30:
                continue
            ind = analyze(window, sym)
            if ind.get("error"):
                continue

            score = ind["score"]
            if regime == "bullish" and score >= threshold:
                candidates.append(("long", sym, score, ind))
            elif regime == "bearish":
                ss = score_short(ind)
                if ss >= threshold:
                    candidates.append(("short", sym, ss, ind))

        if not candidates:
            continue

        candidates.sort(key=lambda x: x[2], reverse=True)
        side, sym, score, ind = candidates[0]
        price = ind["price"]
        if price <= 0:
            continue

        # Position sizing: score 5-6 = half, 7-8 = full
        max_spend = capital
        if score <= 6:
            max_spend *= 0.5
        qty = int(max_spend / price)
        if qty <= 0:
            continue

        if side == "long":
            sl = price * (1 - sl_pct / 100)
            tp = price * (1 + tp_pct / 100)
        else:
            sl = price * (1 + sl_pct / 100)
            tp = price * (1 - tp_pct / 100)

        capital -= price * qty + qty * commission

        position = {
            "symbol": sym, "side": side, "qty": qty,
            "entry_price": price, "entry_bar": bar_idx, "entry_time": time_str,
            "stop_loss": sl, "take_profit": tp,
            "highest": price, "lowest": price,
        }

    # Force close at end
    if position:
        sym = position["symbol"]
        bars = sym_bars.get(sym, [])
        if bars:
            price = bars[-1]["close"]
            qty = position["qty"]
            if position["side"] == "long":
                pnl = (price - position["entry_price"]) * qty - qty * commission * 2
                capital += price * qty - qty * commission
            else:
                pnl = (position["entry_price"] - price) * qty - qty * commission * 2
                capital += (2 * position["entry_price"] - price) * qty - qty * commission
            trades.append(Trade(
                symbol=sym, side=position["side"], quantity=qty,
                entry_price=position["entry_price"], exit_price=price,
                entry_time=position["entry_time"], exit_time="END",
                pnl=pnl, hold_bars=len(timeline) - position["entry_bar"],
                exit_reason="end_of_data",
            ))

    peak_capital = max(peak_capital, capital)
    dd = (peak_capital - capital) / peak_capital * 100 if peak_capital > 0 else 0
    max_drawdown = max(max_drawdown, dd)

    result = Result(
        period=period_label, trades=trades,
        final_capital=capital, peak_capital=peak_capital,
        max_drawdown=max_drawdown, total_decisions=total_decisions,
    )
    return result


def print_comparison(results):
    print(f"\n{'='*100}")
    print(f"PERIOD COMPARISON — 30-min decision frequency, TP 0.75%, SL 3.0%, Score >= 5")
    print(f"{'='*100}")
    print(f"{'Period':<8} {'Bars':>7} {'Lookback':>10} {'Decisions':>10} {'Trades':>7} "
          f"{'WinR%':>6} {'PnL$':>9} {'Ret%':>7} {'AvgWin':>8} {'AvgLoss':>9} "
          f"{'PF':>6} {'MaxDD%':>7}")
    print(f"{'-'*100}")

    for r in results:
        pf = f"{r.profit_factor:.2f}" if r.profit_factor != float('inf') else "inf"
        print(f"{r.period:<8} "
              f"{len(r.trades):>7} "
              f"{'':>10} "
              f"{r.total_decisions:>10} "
              f"{len(r.trades):>7} "
              f"{r.win_rate*100:>5.1f}% "
              f"{r.total_pnl:>+9.2f} "
              f"{r.return_pct:>6.2f}% "
              f"{r.avg_win:>+8.2f} "
              f"{r.avg_loss:>+9.2f} "
              f"{pf:>6} "
              f"{r.max_drawdown:>6.2f}%")

    print(f"{'='*100}")

    # Per-symbol breakdown for each period
    for r in results:
        print(f"\n--- {r.period} per-symbol breakdown ---")
        syms = {}
        for t in r.trades:
            s = t.symbol
            if s not in syms:
                syms[s] = {"count": 0, "pnl": 0, "wins": 0}
            syms[s]["count"] += 1
            syms[s]["pnl"] += t.pnl
            if t.pnl > 0:
                syms[s]["wins"] += 1
        for s, d in sorted(syms.items(), key=lambda x: -x[1]["pnl"]):
            wr = d["wins"] / d["count"] * 100 if d["count"] > 0 else 0
            print(f"  {s:10s}: {d['count']:3d} trades, WR {wr:5.1f}%, PnL ${d['pnl']:+.2f}")

    # Exit reason distribution
    for r in results:
        print(f"\n--- {r.period} exit reasons ---")
        reasons = {}
        for t in r.trades:
            reasons[t.exit_reason] = reasons.get(t.exit_reason, {"n": 0, "pnl": 0})
            reasons[t.exit_reason]["n"] += 1
            reasons[t.exit_reason]["pnl"] += t.pnl
        for reason, d in sorted(reasons.items(), key=lambda x: -x[1]["n"]):
            print(f"  {reason:15s}: {d['n']:3d} trades, PnL ${d['pnl']:+.2f}")


def main():
    results = []

    # 5m (existing data)
    print("Loading 5m data...")
    data_5m = load_data(os.path.join(BASE_DIR, "data"))
    if data_5m:
        tl = build_timeline(data_5m)
        print(f"  {len(data_5m)} symbols, {len(tl)} bars")
        t0 = time.time()
        r = run_backtest("5m", data_5m, decision_interval=6)
        print(f"  Backtest: {time.time()-t0:.1f}s, {len(r.trades)} trades")
        results.append(r)

    # 1h
    print("Loading 1h data...")
    data_1h = load_data(os.path.join(BASE_DIR, "data_1h"))
    if data_1h:
        tl = build_timeline(data_1h)
        print(f"  {len(data_1h)} symbols, {len(tl)} bars")
        t0 = time.time()
        # 1h bars: 1 bar = 60 min. Decision every bar (closest to 30 min we can get).
        r = run_backtest("1h", data_1h, decision_interval=1)
        print(f"  Backtest: {time.time()-t0:.1f}s, {len(r.trades)} trades")
        results.append(r)

    # 1m
    print("Loading 1m data...")
    data_1m = load_data(os.path.join(BASE_DIR, "data_1m"))
    if data_1m:
        tl = build_timeline(data_1m)
        print(f"  {len(data_1m)} symbols, {len(tl)} bars")
        t0 = time.time()
        r = run_backtest("1m", data_1m, decision_interval=30)
        print(f"  Backtest: {time.time()-t0:.1f}s, {len(r.trades)} trades")
        results.append(r)
    else:
        print("  1m data not ready yet, skipping")

    if results:
        print_comparison(results)

        # Save
        out = []
        for r in results:
            out.append({
                "period": r.period,
                "trades": len(r.trades),
                "wins": r.win_count, "losses": r.loss_count,
                "win_rate": round(r.win_rate * 100, 1),
                "total_pnl": round(r.total_pnl, 2),
                "return_pct": round(r.return_pct, 2),
                "avg_win": round(r.avg_win, 2),
                "avg_loss": round(r.avg_loss, 2),
                "profit_factor": round(r.profit_factor, 2) if r.profit_factor != float('inf') else "inf",
                "max_drawdown": round(r.max_drawdown, 2),
                "total_decisions": r.total_decisions,
                "final_capital": round(r.final_capital, 2),
            })
        os.makedirs(os.path.join(BASE_DIR, "results"), exist_ok=True)
        with open(os.path.join(BASE_DIR, "results", "period_comparison.json"), "w") as f:
            json.dump(out, f, indent=2)
        print(f"\nSaved to results/period_comparison.json")


if __name__ == "__main__":
    main()
