import json
with open('state.json') as f:
    s = json.load(f)
s['capital'] = round(s['capital'] - (209.84 * 3 + 3 * 0.02), 4)
s['position'] = {
    "symbol": "NVDA.US", "side": "BUY", "quantity": 3, "entry_price": 209.84,
    "order_id": "1248944683363614720", "entry_time": "2026-06-09T10:20:16Z",
    "stop": 207.15, "target": 215.20,
}
s['trade_count'] = 33
s['trades'].append({
    "trade_id": 33, "symbol": "NVDA.US", "side": "BUY", "quantity": 3,
    "fill_price": 209.84, "commission": 0.06,
    "order_id": "1248944683363614720", "time": "2026-06-09T10:20:16Z",
    "reason": "NVDA score 5/8 Pre-market BUY. Only clean non-revenge BUY (TSLA also 5/8 but skipped per no-revenge rule after two consecutive TSLA losses, trades 4 and 30). NVDA green +1.73% leading the tape while mega-caps red (AAPL -1.89, MSFT -1.18, GOOGL -1.36, META -1.28). MACD bullish accelerating (histogram +0.175), RSI 41.86 (huge room), clean candle, not extended. Reclaiming EMA20 210.55, below VWAP 216.52 (capped conviction). Best pre-market liquidity in universe (394K shares, 82M turnover). volume_ratio 0.49 (above 0.3 floor but thin). Stop 207.15 (~1 ATR, ATR 2.68), target 215.20 (~2 ATR). Confidence 60, half position 3 shares (629 dollars). Filled at 209.84 (price improvement +0.76 over 210.60 limit). Capital to 585.66.",
})
with open('state.json', 'w') as f:
    json.dump(s, f, indent=2)
print("capital", s['capital'])
