#!/usr/bin/env python3
"""
Flintrade Backtesting Engine

Replays 5m bars chronologically, computes indicators on rolling windows
(same logic as indicators.py), simulates entry/exit with commission.

Design:
- One position at a time (long or short)
- Scans all tradeable symbols each bar, picks highest score
- QQQ/SPY market regime filter
- Configurable: score threshold, stop-loss, take-profit, hold limit, sessions
"""

import json
import os
import sys
from dataclasses import dataclass, field
from typing import Optional

# Add parent scripts dir so we can import indicators
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "scripts"))
from indicators import analyze

DATA_DIR = os.path.join(os.path.dirname(__file__), "data")

TRADE_SYMBOLS = [
    "AAPL.US", "MSFT.US", "GOOGL.US", "AMZN.US", "NVDA.US",
    "META.US", "TSLA.US", "GLD.US", "UGL.US", "SLV.US", "AGQ.US", "USO.US",
]
MARKET_SYMBOLS = ["QQQ.US", "SPY.US"]


@dataclass
class StrategyParams:
    """All tunable parameters for the strategy."""
    capital: float = 1200.0
    commission_per_share: float = 0.02

    # Entry
    score_threshold: int = 5           # min score to enter (0-8)
    offhours_score_threshold: int = 6  # min score during pre/post/overnight
    market_regime: bool = True         # require QQQ+SPY bullish

    # Exit
    stop_loss_pct: float = 1.0         # stop loss as % of entry price
    take_profit_pct: float = 0.0       # 0 = any profit (current Flintrade rule)
    trailing_stop_pct: float = 0.0     # 0 = disabled
    max_hold_bars: int = 0             # 0 = unlimited

    # Session filter
    sessions: str = "all"              # "all", "intraday", "intraday+pre", etc.

    # Position sizing
    max_position_pct: float = 1.0      # max % of capital per position

    # Short selling
    allow_short: bool = True

    def label(self):
        parts = [f"sc{self.score_threshold}"]
        if self.offhours_score_threshold != self.score_threshold:
            parts.append(f"oh{self.offhours_score_threshold}")
        if self.stop_loss_pct > 0:
            parts.append(f"sl{self.stop_loss_pct}")
        if self.take_profit_pct > 0:
            parts.append(f"tp{self.take_profit_pct}")
        if self.trailing_stop_pct > 0:
            parts.append(f"ts{self.trailing_stop_pct}")
        if self.max_hold_bars > 0:
            parts.append(f"hb{self.max_hold_bars}")
        if not self.market_regime:
            parts.append("noMR")
        if self.sessions != "all":
            parts.append(self.sessions)
        return "_".join(parts)


@dataclass
class Position:
    symbol: str
    side: str          # "long" or "short"
    quantity: int
    entry_price: float
    entry_bar: int
    entry_time: str
    stop_loss: float
    highest: float = 0.0   # for trailing stop (long)
    lowest: float = 999999.0  # for trailing stop (short)


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
class BacktestResult:
    params: StrategyParams
    trades: list = field(default_factory=list)
    final_capital: float = 0.0
    peak_capital: float = 0.0
    max_drawdown: float = 0.0
    total_bars: int = 0

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
        gross_profit = sum(t.pnl for t in self.trades if t.pnl > 0)
        gross_loss = abs(sum(t.pnl for t in self.trades if t.pnl <= 0))
        return gross_profit / gross_loss if gross_loss > 0 else float('inf')

    @property
    def avg_hold_bars(self):
        return sum(t.hold_bars for t in self.trades) / len(self.trades) if self.trades else 0

    @property
    def return_pct(self):
        return (self.final_capital - self.params.capital) / self.params.capital * 100

    def summary(self):
        return {
            "label": self.params.label(),
            "trades": len(self.trades),
            "wins": self.win_count,
            "losses": self.loss_count,
            "win_rate": round(self.win_rate * 100, 1),
            "total_pnl": round(self.total_pnl, 2),
            "return_pct": round(self.return_pct, 2),
            "avg_win": round(self.avg_win, 2),
            "avg_loss": round(self.avg_loss, 2),
            "profit_factor": round(self.profit_factor, 2) if self.profit_factor != float('inf') else "inf",
            "max_drawdown": round(self.max_drawdown, 2),
            "avg_hold_bars": round(self.avg_hold_bars, 1),
            "final_capital": round(self.final_capital, 2),
        }


def load_data():
    """Load all symbol data, return dict of symbol -> list of bars (oldest first)."""
    data = {}
    for sym in MARKET_SYMBOLS + TRADE_SYMBOLS:
        safe = sym.replace(".", "_")
        path = os.path.join(DATA_DIR, f"{safe}.json")
        if os.path.exists(path):
            with open(path) as f:
                bars = json.load(f)
            bars.sort(key=lambda b: b["time"])
            data[sym] = bars
    return data


def build_timeline(data):
    """
    Build a unified timeline: list of (time, {symbol: bar_index}) sorted chronologically.
    Each timestamp maps to which bar index each symbol is at.
    """
    # Collect all unique timestamps across all symbols
    all_times = set()
    sym_time_index = {}

    for sym, bars in data.items():
        time_to_idx = {}
        for i, bar in enumerate(bars):
            t = bar["time"]
            all_times.add(t)
            time_to_idx[t] = i
        sym_time_index[sym] = time_to_idx

    timeline = []
    for t in sorted(all_times):
        indices = {}
        for sym, time_idx in sym_time_index.items():
            if t in time_idx:
                indices[sym] = time_idx[t]
        timeline.append((t, indices))

    return timeline


def session_type(bar):
    """Get session type from bar data."""
    return bar.get("session", "Intraday")


def is_offhours(bar):
    """Check if bar is pre/post/overnight (not intraday)."""
    s = session_type(bar)
    return s in ("Pre", "Post", "Overnight")


def session_allowed(bar, sessions_filter):
    """Check if this bar's session passes the filter."""
    if sessions_filter == "all":
        return True
    s = session_type(bar)
    if sessions_filter == "intraday":
        return s == "Intraday"
    if sessions_filter == "intraday+pre":
        return s in ("Intraday", "Pre")
    return True


def compute_indicators(bars, window_size=50):
    """Compute indicators on the last `window_size` bars. Returns analyze() result."""
    window = bars[-window_size:] if len(bars) >= window_size else bars
    if len(window) < 30:
        return None
    # indicators.py expects keys: open, high, low, close, volume
    return analyze(window, symbol="")


def check_market_regime(market_indicators):
    """
    Check QQQ + SPY market regime.
    Returns: "bullish", "bearish", or "mixed"
    """
    bullish_count = 0
    bearish_count = 0

    for sym in ["QQQ.US", "SPY.US"]:
        ind = market_indicators.get(sym)
        if not ind or ind.get("error"):
            return "mixed"

        above_vwap = ind["price"] > ind["vwap"]
        macd_bull = ind["macd"]["histogram"] > 0
        if above_vwap and macd_bull:
            bullish_count += 1
        elif not above_vwap and not macd_bull:
            bearish_count += 1

    if bullish_count == 2:
        return "bullish"
    if bearish_count == 2:
        return "bearish"
    return "mixed"


def run_backtest(params: StrategyParams, data: dict = None, timeline: list = None):
    """
    Run a single backtest with given parameters.
    Returns BacktestResult.
    """
    if data is None:
        data = load_data()
    if timeline is None:
        timeline = build_timeline(data)

    capital = params.capital
    peak_capital = capital
    max_drawdown = 0.0
    position: Optional[Position] = None
    trades = []

    # We need rolling windows for each symbol
    # Track how many bars we've seen per symbol
    sym_bars_seen = {sym: [] for sym in data}

    for bar_idx, (time_str, indices) in enumerate(timeline):
        # Update rolling windows
        for sym, idx in indices.items():
            sym_bars_seen[sym].append(data[sym][idx])

        # Get current bar for position symbol (if holding)
        if position:
            sym = position.symbol
            if sym not in indices:
                continue  # no data for held symbol this bar
            current_bar = data[sym][indices[sym]]
            price = current_bar["close"]
            hold_bars = bar_idx - position.entry_bar

            # Update trailing stop tracking
            if position.side == "long":
                position.highest = max(position.highest, current_bar["high"])
            else:
                position.lowest = min(position.lowest, current_bar["low"])

            # Check exit conditions
            exit_reason = None

            if position.side == "long":
                # Stop loss
                if params.stop_loss_pct > 0 and price <= position.stop_loss:
                    exit_reason = "stop_loss"
                # Trailing stop
                elif params.trailing_stop_pct > 0:
                    trail_stop = position.highest * (1 - params.trailing_stop_pct / 100)
                    if price <= trail_stop:
                        exit_reason = "trailing_stop"
                # Take profit
                if not exit_reason:
                    net_pnl_per_share = price - position.entry_price - 2 * params.commission_per_share
                    if params.take_profit_pct > 0:
                        target = position.entry_price * (1 + params.take_profit_pct / 100)
                        if price >= target and net_pnl_per_share > 0:
                            exit_reason = "take_profit"
                    elif net_pnl_per_share > 0:
                        # "毋以利小而不为" — any net profit
                        exit_reason = "any_profit"
            else:
                # Short position
                if params.stop_loss_pct > 0 and price >= position.stop_loss:
                    exit_reason = "stop_loss"
                elif params.trailing_stop_pct > 0:
                    trail_stop = position.lowest * (1 + params.trailing_stop_pct / 100)
                    if price >= trail_stop:
                        exit_reason = "trailing_stop"
                if not exit_reason:
                    net_pnl_per_share = position.entry_price - price - 2 * params.commission_per_share
                    if params.take_profit_pct > 0:
                        target = position.entry_price * (1 - params.take_profit_pct / 100)
                        if price <= target and net_pnl_per_share > 0:
                            exit_reason = "take_profit"
                    elif net_pnl_per_share > 0:
                        exit_reason = "any_profit"

            # Max hold time
            if not exit_reason and params.max_hold_bars > 0 and hold_bars >= params.max_hold_bars:
                exit_reason = "max_hold"

            # Execute exit
            if exit_reason:
                qty = position.quantity
                commission = qty * params.commission_per_share
                if position.side == "long":
                    pnl = (price - position.entry_price) * qty - commission * 2
                    capital += price * qty - commission
                else:
                    pnl = (position.entry_price - price) * qty - commission * 2
                    capital += (2 * position.entry_price - price) * qty - commission

                trades.append(Trade(
                    symbol=position.symbol,
                    side=position.side,
                    quantity=qty,
                    entry_price=position.entry_price,
                    exit_price=price,
                    entry_time=position.entry_time,
                    exit_time=time_str,
                    pnl=pnl,
                    hold_bars=hold_bars,
                    exit_reason=exit_reason,
                ))
                position = None

                # Track drawdown
                peak_capital = max(peak_capital, capital)
                dd = (peak_capital - capital) / peak_capital * 100
                max_drawdown = max(max_drawdown, dd)

            continue  # don't enter new position same bar as exit

        # No position — look for entry
        # Session filter on current bar (use any symbol's bar as reference)
        ref_sym = list(indices.keys())[0] if indices else None
        if not ref_sym:
            continue
        ref_bar = data[ref_sym][indices[ref_sym]]

        if not session_allowed(ref_bar, params.sessions):
            continue

        offhours = is_offhours(ref_bar)
        threshold = params.offhours_score_threshold if offhours else params.score_threshold

        # Market regime check
        if params.market_regime:
            market_ind = {}
            for msym in MARKET_SYMBOLS:
                if len(sym_bars_seen.get(msym, [])) >= 30:
                    market_ind[msym] = compute_indicators(sym_bars_seen[msym])
            regime = check_market_regime(market_ind)
        else:
            regime = "bullish"  # skip check

        # Scan tradeable symbols for best entry
        candidates = []
        for sym in TRADE_SYMBOLS:
            if sym not in indices:
                continue
            bars_so_far = sym_bars_seen.get(sym, [])
            if len(bars_so_far) < 30:
                continue

            ind = compute_indicators(bars_so_far)
            if not ind or ind.get("error"):
                continue

            score = ind["score"]
            signal = ind.get("signal", "WAIT")

            # Long entry: bullish regime + score >= threshold
            if regime == "bullish" and score >= threshold:
                candidates.append(("long", sym, score, ind))

            # Short entry: bearish regime + inverse score
            # For shorts, we want: below VWAP, below EMA20, MACD bearish, etc.
            if params.allow_short and regime == "bearish":
                short_score = score_short(ind)
                if short_score >= threshold:
                    candidates.append(("short", sym, short_score, ind))

        if not candidates:
            continue

        # Pick highest score
        candidates.sort(key=lambda x: x[2], reverse=True)
        side, sym, score, ind = candidates[0]
        price = ind["price"]

        # Position sizing
        max_spend = capital * params.max_position_pct
        if price <= 0:
            continue
        qty = int(max_spend / price)
        if qty <= 0:
            continue

        commission = qty * params.commission_per_share

        # Set stop loss
        if params.stop_loss_pct > 0:
            if side == "long":
                stop = price * (1 - params.stop_loss_pct / 100)
            else:
                stop = price * (1 + params.stop_loss_pct / 100)
        else:
            stop = 0

        # Deduct capital
        if side == "long":
            capital -= price * qty + commission
        else:
            # Short: need margin (simplified: same as long cost)
            capital -= price * qty + commission

        position = Position(
            symbol=sym,
            side=side,
            quantity=qty,
            entry_price=price,
            entry_bar=bar_idx,
            entry_time=time_str,
            stop_loss=stop,
            highest=price,
            lowest=price,
        )

        peak_capital = max(peak_capital, capital)

    # Force close any open position at end
    if position:
        sym = position.symbol
        bars = sym_bars_seen.get(sym, [])
        if bars:
            price = bars[-1]["close"]
            qty = position.quantity
            commission = qty * params.commission_per_share
            if position.side == "long":
                pnl = (price - position.entry_price) * qty - commission * 2
                capital += price * qty - commission
            else:
                pnl = (position.entry_price - price) * qty - commission * 2
                capital += (2 * position.entry_price - price) * qty - commission
            trades.append(Trade(
                symbol=sym,
                side=position.side,
                quantity=qty,
                entry_price=position.entry_price,
                exit_price=price,
                entry_time=position.entry_time,
                exit_time="END",
                pnl=pnl,
                hold_bars=len(timeline) - position.entry_bar,
                exit_reason="end_of_data",
            ))
        position = None

    peak_capital = max(peak_capital, capital)
    dd = (peak_capital - capital) / peak_capital * 100 if peak_capital > 0 else 0
    max_drawdown = max(max_drawdown, dd)

    result = BacktestResult(
        params=params,
        trades=trades,
        final_capital=capital,
        peak_capital=peak_capital,
        max_drawdown=max_drawdown,
        total_bars=len(timeline),
    )
    return result


def score_short(ind):
    """Score short entry: inverted criteria from score_entry."""
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
    # candle and extension checks skipped for shorts (not in data)
    return score


if __name__ == "__main__":
    print("Loading data...")
    data = load_data()
    print(f"Loaded {len(data)} symbols")

    timeline = build_timeline(data)
    print(f"Timeline: {len(timeline)} bars, {timeline[0][0]} to {timeline[-1][0]}")

    # Run with current Flintrade parameters
    params = StrategyParams()
    print(f"\nRunning backtest: {params.label()}")
    result = run_backtest(params, data, timeline)
    print(json.dumps(result.summary(), indent=2))

    if result.trades:
        print(f"\nSample trades:")
        for t in result.trades[:10]:
            print(f"  {t.entry_time} {t.side:5s} {t.symbol:10s} "
                  f"in={t.entry_price:.2f} out={t.exit_price:.2f} "
                  f"pnl={t.pnl:+.2f} ({t.exit_reason}) hold={t.hold_bars}bars")
