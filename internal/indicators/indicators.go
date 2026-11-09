// Package indicators computes Flintrade's technical indicators and entry score.
// Ported from scripts/indicators.py and pinned to it bit-for-bit by a golden
// test over 648 cases (testdata/python_golden.json): 8 real symbols × lengths ×
// h4 lookbacks, sliding windows, and synthetic edge cases.
//
// It is shared by the live collector and the backtest engine, so indicator
// semantics change for both at once — same contract as the Python module.
//
// Two numeric rules make the results identical to CPython, not just close:
//
//   - sum() of floats in Python ≥ 3.12 is Neumaier-compensated; pySum
//     reproduces it. Integer sums (volumes) stay exact int64.
//   - Go may fuse a*b+c into one FMA instruction on arm64 (one rounding
//     instead of two). Every multiply feeding an add is wrapped in an explicit
//     float64() conversion, which the spec says prevents fusion.
//
// None is represented as NaN inside series; use IsNone.
package indicators

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// None marks a missing value in a series (Python's None).
var None = math.NaN()

// IsNone reports whether v is the missing marker.
func IsNone(v float64) bool { return math.IsNaN(v) }

func nones(n int) []float64 {
	s := make([]float64, n)
	for i := range s {
		s[i] = None
	}
	return s
}

// pySum is CPython's float sum(): Neumaier-compensated.
func pySum(xs []float64) float64 {
	f, c := 0.0, 0.0
	for _, x := range xs {
		t := f + x
		if math.Abs(f) >= math.Abs(x) {
			c += (f - t) + x
		} else {
			c += (x - t) + f
		}
		f = t
	}
	if c != 0 && !math.IsInf(c, 0) && !math.IsNaN(c) {
		f += c
	}
	return f
}

// pyRound is Python's round(x, n) for floats: correctly rounded, ties to even
// on the exact binary value — which is what strconv's 'f' formatting does.
func pyRound(x float64, n int) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', n, 64), 64)
	return v
}

// at is Python's s[i] with negative indexing.
func at(s []float64, i int) float64 {
	if i < 0 {
		i += len(s)
	}
	return s[i]
}

// ── series ───────────────────────────────────────────────────────────────

// EMA is the exponential moving average seeded with the first period's SMA.
func EMA(values []float64, period int) []float64 {
	if len(values) < period {
		return nones(len(values))
	}
	k := 2 / float64(period+1)
	out := nones(period - 1)
	prev := pySum(values[:period]) / float64(period)
	out = append(out, prev)
	for i := period; i < len(values); i++ {
		prev = float64(values[i]*k) + float64(prev*(1-k))
		out = append(out, prev)
	}
	return out
}

// SMA is the simple moving average. Like Python's sma(), a series shorter
// than period comes back as period-1 Nones (longer than the input).
func SMA(values []float64, period int) []float64 {
	out := nones(period - 1)
	for i := period - 1; i < len(values); i++ {
		out = append(out, pySum(values[i-period+1:i+1])/float64(period))
	}
	return out
}

// SMAInt is SMA over integers (volumes): the sum is exact, as in Python.
func SMAInt(values []int64, period int) []float64 {
	out := nones(period - 1)
	for i := period - 1; i < len(values); i++ {
		var s int64
		for _, v := range values[i-period+1 : i+1] {
			s += v
		}
		out = append(out, float64(s)/float64(period))
	}
	return out
}

// VWAP is the cumulative volume-weighted average price.
func VWAP(highs, lows, closes []float64, volumes []int64) []float64 {
	cumTPVol := 0.0
	var cumVol int64
	out := make([]float64, 0, len(closes))
	for i := range closes {
		tp := (highs[i] + lows[i] + closes[i]) / 3
		cumTPVol += float64(tp * float64(volumes[i]))
		cumVol += volumes[i]
		if cumVol > 0 {
			out = append(out, cumTPVol/float64(cumVol))
		} else {
			out = append(out, 0)
		}
	}
	return out
}

// RSI is Wilder's relative strength index.
func RSI(closes []float64, period int) []float64 {
	if len(closes) < period+1 {
		return nones(len(closes))
	}
	n := len(closes) - 1
	gains, losses := make([]float64, n), make([]float64, n)
	for i := 1; i < len(closes); i++ {
		d := closes[i] - closes[i-1]
		gains[i-1] = math.Max(d, 0)
		losses[i-1] = math.Abs(math.Min(d, 0))
	}
	rsi := func(g, l float64) float64 {
		if l == 0 {
			return 100.0
		}
		rs := g / l
		return 100 - (100 / (1 + rs))
	}
	out := nones(period)
	avgG := pySum(gains[:period]) / float64(period)
	avgL := pySum(losses[:period]) / float64(period)
	out = append(out, rsi(avgG, avgL))
	p := float64(period)
	for i := period; i < n; i++ {
		avgG = (float64(avgG*(p-1)) + gains[i]) / p
		avgL = (float64(avgL*(p-1)) + losses[i]) / p
		out = append(out, rsi(avgG, avgL))
	}
	return out
}

// MACD returns line, signal and histogram (12/26/9).
func MACD(closes []float64) (line, signal, hist []float64) {
	fast, slow := EMA(closes, 12), EMA(closes, 26)
	line = make([]float64, len(closes))
	var valid []float64
	for i := range closes {
		if IsNone(fast[i]) || IsNone(slow[i]) {
			line[i] = None
			continue
		}
		line[i] = fast[i] - slow[i]
		valid = append(valid, line[i])
	}
	var raw []float64
	if len(valid) >= 9 {
		raw = EMA(valid, 9)
	} else {
		raw = nones(len(valid))
	}
	signal = append(nones(len(line)-len(raw)), raw...)
	hist = make([]float64, len(line))
	for i := range line {
		if IsNone(line[i]) || IsNone(signal[i]) {
			hist[i] = None
		} else {
			hist[i] = line[i] - signal[i]
		}
	}
	return line, signal, hist
}

// WR is Williams %R in the positive CN convention: (HH-C)/(HH-LL)*100,
// >80 oversold, <20 overbought. Period 14.
func WR(highs, lows, closes []float64, period int) []float64 {
	out := nones(len(closes))
	for i := period - 1; i < len(closes); i++ {
		hh, ll := highs[i-period+1], lows[i-period+1]
		for j := i - period + 2; j <= i; j++ {
			hh = math.Max(hh, highs[j])
			ll = math.Min(ll, lows[j])
		}
		if hh == ll {
			out[i] = 50.0
		} else {
			out[i] = (hh - closes[i]) / (hh - ll) * 100.0
		}
	}
	return out
}

// MTM is momentum (close - close[period ago]) and its SMA.
func MTM(closes []float64, period, maPeriod int) (mtm, ma []float64) {
	mtm = nones(min(period, len(closes)))
	for i := period; i < len(closes); i++ {
		mtm = append(mtm, closes[i]-closes[i-period])
	}
	var valid []float64
	for _, v := range mtm {
		if !IsNone(v) {
			valid = append(valid, v)
		}
	}
	var maValid []float64
	if len(valid) >= maPeriod {
		maValid = SMA(valid, maPeriod)
	} else {
		maValid = nones(len(valid))
	}
	ma = append(nones(len(mtm)-len(maValid)), maValid...)
	return mtm, ma
}

// ATR is the average true range (SMA of true ranges).
func ATR(highs, lows, closes []float64, period int) []float64 {
	if len(closes) < 2 {
		return nones(len(closes))
	}
	tr := []float64{highs[0] - lows[0]}
	for i := 1; i < len(closes); i++ {
		tr = append(tr, math.Max(highs[i]-lows[i],
			math.Max(math.Abs(highs[i]-closes[i-1]), math.Abs(lows[i]-closes[i-1]))))
	}
	return SMA(tr, period)
}

// ── klines ───────────────────────────────────────────────────────────────

// Kline is one bar. Epoch is NaN when the bar carries no usable timestamp.
type Kline struct {
	Open, High, Low, Close float64
	Volume                 int64
	Epoch                  float64
}

// ParseKlines decodes the CLI's kline JSON. Prices may be strings
// ("187.310") or numbers; volume must be integral.
func ParseKlines(raw []byte) ([]Kline, error) {
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	out := make([]Kline, len(rows))
	for i, r := range rows {
		var err error
		k := &out[i]
		if k.Open, err = num(r["open"]); err != nil {
			return nil, fmt.Errorf("bar %d open: %w", i, err)
		}
		if k.High, err = num(r["high"]); err != nil {
			return nil, fmt.Errorf("bar %d high: %w", i, err)
		}
		if k.Low, err = num(r["low"]); err != nil {
			return nil, fmt.Errorf("bar %d low: %w", i, err)
		}
		if k.Close, err = num(r["close"]); err != nil {
			return nil, fmt.Errorf("bar %d close: %w", i, err)
		}
		if k.Volume, err = integer(r["volume"]); err != nil {
			return nil, fmt.Errorf("bar %d volume: %w", i, err)
		}
		k.Epoch = epoch(r)
	}
	return out, nil
}

func num(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case string:
		return strconv.ParseFloat(strings.TrimSpace(x), 64)
	}
	return 0, fmt.Errorf("not a number: %T", v)
}

// integer is Python's int(): truncates numbers, parses integral strings only.
func integer(v any) (int64, error) {
	switch x := v.(type) {
	case float64:
		return int64(x), nil
	case string:
		return strconv.ParseInt(strings.TrimSpace(x), 10, 64)
	}
	return 0, fmt.Errorf("not an integer: %T", v)
}

// epoch mirrors _ts_epoch: first truthy of timestamp/ts/time; numbers are
// epoch seconds, strings ISO-8601 (naive strings are local time, as
// datetime.fromisoformat().timestamp() treats them).
func epoch(r map[string]any) float64 {
	var t any
	for _, k := range []string{"timestamp", "ts", "time"} {
		if v, ok := r[k]; ok && truthy(v) {
			t = v
			break
		}
	}
	switch x := t.(type) {
	case float64:
		return x
	case string:
		if ts, err := time.Parse(time.RFC3339Nano, x); err == nil {
			return float64(ts.UnixNano()) / 1e9
		}
		for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999",
			"2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
			if ts, err := time.ParseInLocation(layout, x, time.Local); err == nil {
				return float64(ts.UnixNano()) / 1e9
			}
		}
	}
	return None
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case float64:
		return x != 0
	case bool:
		return x
	}
	return true
}

// Bar4H is an aggregated 4-hour bar.
type Bar4H struct {
	Open   float64 `json:"open"`
	High   float64 `json:"high"`
	Low    float64 `json:"low"`
	Close  float64 `json:"close"`
	Volume int64   `json:"volume"`
}

// Aggregate4H buckets 1h bars by wall-clock 4h (epoch // 14400), falling back
// to fixed groups of four when a bar has no timestamp.
func Aggregate4H(k []Kline) []Bar4H {
	idx := map[int64]int{}
	var out []Bar4H
	for i, b := range k {
		key := int64(i / 4)
		if !IsNone(b.Epoch) {
			key = int64(math.Floor(b.Epoch / 14400))
		}
		j, ok := idx[key]
		if !ok {
			idx[key] = len(out)
			out = append(out, Bar4H{b.Open, b.High, b.Low, b.Close, b.Volume})
			continue
		}
		o := &out[j]
		o.High = math.Max(o.High, b.High)
		o.Low = math.Min(o.Low, b.Low)
		o.Close = b.Close
		o.Volume += b.Volume
	}
	return out
}

// ── 4h playbook snapshot ─────────────────────────────────────────────────

// H4 is the 4h trend + MACD/WR/MTM confluence snapshot the h4_confluence
// playbook consumes. Field order matches the Python dict (it is model-visible).
type H4 struct {
	Bars       int      `json:"bars"`
	Close      *float64 `json:"close"`
	SMA20      *float64 `json:"sma20"`
	TrendOK    bool     `json:"trend_ok"`
	MACD       H4MACD   `json:"macd"`
	WR         H4WR     `json:"wr"`
	MTM        H4MTM    `json:"mtm"`
	Confluence bool     `json:"confluence"`
}

type H4MACD struct {
	Dif        *float64 `json:"dif"`
	Hist       *float64 `json:"hist"`
	HistPrev   *float64 `json:"hist_prev"`
	AboveZero  bool     `json:"above_zero"`
	TurnedBull bool     `json:"turned_bull"`
}

type H4WR struct {
	Value      *float64 `json:"value"`
	RecentMax  *float64 `json:"recent_max"`
	Recovering bool     `json:"recovering"`
}

type H4MTM struct {
	Value       *float64 `json:"value"`
	MA          *float64 `json:"ma"`
	GoldenCross bool     `json:"golden_cross"`
}

// r is the Python lambda r(v, n): rounded, None stays None.
func r(v float64, n int) *float64 {
	if IsNone(v) {
		return nil
	}
	x := pyRound(v, n)
	return &x
}

// crossedUp reports whether series crossed above ref (nil → 0) within the
// last lookback bars.
func crossedUp(series, ref []float64, lookback int) bool {
	for j := 1; j <= lookback; j++ {
		if j+1 > len(series)-1 {
			break
		}
		cur, prev := at(series, -j), at(series, -j-1)
		rc, rp := 0.0, 0.0
		if ref != nil {
			rc, rp = at(ref, -j), at(ref, -j-1)
		}
		if IsNone(cur) || IsNone(prev) || IsNone(rc) || IsNone(rp) {
			continue
		}
		if prev <= rp && cur > rc {
			return true
		}
	}
	return false
}

// H4Snapshot builds the playbook snapshot; nil when there are fewer than 36
// 4h bars (MACD warm-up). Semantics were swept on SNDK/MU 2025-09→2026-08 by
// backtest/playbook_test.py — rerun it before changing them.
func H4Snapshot(k []Kline, lookback int) *H4 {
	bars := Aggregate4H(k)
	if len(bars) < 36 {
		return nil
	}
	highs, lows, closes := make([]float64, len(bars)), make([]float64, len(bars)), make([]float64, len(bars))
	for i, b := range bars {
		highs[i], lows[i], closes[i] = b.High, b.Low, b.Close
	}
	sma20 := SMA(closes, 20)
	line, _, hist := MACD(closes)
	wr := WR(highs, lows, closes, 14)
	mtm, mtmMA := MTM(closes, 12, 6)

	price, ma20 := at(closes, -1), at(sma20, -1)
	ma20Prev := ma20
	if len(sma20) >= 4 && !IsNone(at(sma20, -4)) {
		ma20Prev = at(sma20, -4)
	}
	trendOK := !IsNone(ma20) && price > ma20 && ma20 >= ma20Prev

	macdBull := !IsNone(at(hist, -1)) && at(hist, -1) > 0 && crossedUp(hist, nil, lookback)

	// wr[-(lookback+5):-1] with Python slice clamping.
	lo := max(len(wr)-(lookback+5), 0)
	hi := max(len(wr)-1, 0)
	recentMax := None
	if lo < hi {
		for _, v := range wr[lo:hi] {
			if !IsNone(v) && (IsNone(recentMax) || v > recentMax) {
				recentMax = v
			}
		}
	}
	wrRecovering := !IsNone(at(wr, -1)) && !IsNone(recentMax) && recentMax >= 90 && at(wr, -1) < 80
	mtmGolden := !IsNone(at(mtm, -1)) && !IsNone(at(mtmMA, -1)) && at(mtm, -1) > at(mtmMA, -1) &&
		crossedUp(mtm, mtmMA, lookback)

	var histPrev *float64
	if len(hist) > 1 {
		histPrev = r(at(hist, -2), 6)
	}
	return &H4{
		Bars:    len(bars),
		Close:   r(price, 4),
		SMA20:   r(ma20, 4),
		TrendOK: trendOK,
		MACD: H4MACD{Dif: r(at(line, -1), 6), Hist: r(at(hist, -1), 6), HistPrev: histPrev,
			AboveZero: !IsNone(at(line, -1)) && at(line, -1) > 0, TurnedBull: macdBull},
		WR:         H4WR{Value: r(at(wr, -1), 2), RecentMax: r(recentMax, 2), Recovering: wrRecovering},
		MTM:        H4MTM{Value: r(at(mtm, -1), 4), MA: r(at(mtmMA, -1), 4), GoldenCross: mtmGolden},
		Confluence: trendOK && macdBull && wrRecovering && mtmGolden,
	}
}

// ── entry score ──────────────────────────────────────────────────────────

func scoreEntry(price, vwap, ema20, macdVal, macdSig, macdHist, macdHistPrev, rsi, volRatio, atr,
	body, upperShadow float64) (int, []string) {
	score, reasons := 0, []string{}
	add := func(ok bool, why string) {
		if ok {
			score++
			reasons = append(reasons, why)
		}
	}
	add(price > vwap, "above_vwap")
	add(price > ema20, "above_ema20")
	add(macdVal > 0 || (macdVal > macdSig && macdHist > 0), "macd_bullish")
	add(30 <= rsi && rsi <= 65, "rsi_in_range")
	add(volRatio > 1.5, "high_volume")
	add(macdHist > 0 && macdHist > macdHistPrev, "macd_accelerating")
	add(body > upperShadow, "clean_candle")
	add(!IsNone(atr) && atr > 0 && math.Abs(price-ema20) < 1.5*atr, "not_extended")
	return score, reasons
}

// MACDSummary is the 1h MACD block of a Result.
type MACDSummary struct {
	Value     float64 `json:"value"`
	Signal    float64 `json:"signal"`
	Histogram float64 `json:"histogram"`
	Cross     string  `json:"cross"` // bullish | bearish | none
}

// Result is one symbol's analysis. When Error is set, only Symbol, Error,
// Score (0) and Signal (WAIT) are meaningful and only they are serialised —
// the same shape Python emitted.
type Result struct {
	Symbol      string      `json:"symbol"`
	Price       float64     `json:"price"`
	VWAP        float64     `json:"vwap"`
	EMA20       float64     `json:"ema20"`
	MACD        MACDSummary `json:"macd"`
	RSI         float64     `json:"rsi"`
	VolumeRatio float64     `json:"volume_ratio"`
	ATR         *float64    `json:"atr"`
	Score       int         `json:"score"`
	MaxScore    int         `json:"max_score"`
	Reasons     []string    `json:"reasons"`
	Signal      string      `json:"signal"` // BUY | WAIT
	H4          *H4         `json:"h4"`

	Error string `json:"-"`
}

// MarshalJSON emits the short error shape when Error is set.
func (r Result) MarshalJSON() ([]byte, error) {
	if r.Error != "" {
		return marshalNoEscape(struct {
			Symbol string `json:"symbol"`
			Error  string `json:"error"`
			Score  int    `json:"score"`
			Signal string `json:"signal"`
		}{r.Symbol, r.Error, 0, "WAIT"})
	}
	type plain Result
	return marshalNoEscape(plain(r))
}

// marshalNoEscape matches json.dumps(ensure_ascii=False): no \u003e for '>'.
// This JSON is model-visible; it should read the way Python wrote it.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// ErrNotEnoughBars is wrapped in Result.Error when fewer than 30 bars arrive.
var ErrNotEnoughBars = errors.New("not enough bars")

// Analyze runs the full 1h analysis plus, when h4Lookback > 0, the 4h
// playbook snapshot (fed by the whole history). The 1h indicators always use
// the last 50 bars, however long the input — a 240-bar history exists only to
// feed the 4h aggregation and must not stretch the cumulative VWAP.
func Analyze(k []Kline, symbol string, h4Lookback int) Result {
	if len(k) < 30 {
		return Result{Symbol: symbol, Signal: "WAIT", Error: fmt.Sprintf("Need >=30 bars, got %d", len(k))}
	}
	var h4 *H4
	if h4Lookback != 0 {
		h4 = H4Snapshot(k, h4Lookback)
	}
	if len(k) > 50 {
		k = k[len(k)-50:]
	}
	n := len(k)
	opens, highs, lows, closes := make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n)
	vols := make([]int64, n)
	for i, b := range k {
		opens[i], highs[i], lows[i], closes[i], vols[i] = b.Open, b.High, b.Low, b.Close, b.Volume
	}
	vwaps := VWAP(highs, lows, closes, vols)
	ema20s := EMA(closes, 20)
	line, sig, hist := MACD(closes)
	rsis := RSI(closes, 14)
	atrs := ATR(highs, lows, closes, 14)
	volSMA := SMAInt(vols, 20)

	or := func(v, d float64) float64 {
		if IsNone(v) {
			return d
		}
		return v
	}
	price := closes[n-1]
	vwap := vwaps[n-1]
	ema20 := or(at(ema20s, -1), price)
	macdVal := or(at(line, -1), 0)
	macdSig := or(at(sig, -1), 0)
	macdHist := or(at(hist, -1), 0)
	macdHistPrev := 0.0
	if len(hist) > 1 {
		macdHistPrev = or(at(hist, -2), 0)
	}
	rsi := or(at(rsis, -1), 50)
	atr := at(atrs, -1)
	volAvg := or(at(volSMA, -1), 1)
	volRatio := 1.0
	if volAvg > 0 {
		volRatio = float64(vols[n-1]) / volAvg
	}
	body := math.Abs(closes[n-1] - opens[n-1])
	upper := highs[n-1] - math.Max(opens[n-1], closes[n-1])

	cross := "none"
	if len(line) >= 2 && !IsNone(at(line, -2)) && !IsNone(at(sig, -2)) {
		prevDiff := at(line, -2) - at(sig, -2)
		currDiff := macdVal - macdSig
		if prevDiff <= 0 && currDiff > 0 {
			cross = "bullish"
		} else if prevDiff >= 0 && currDiff < 0 {
			cross = "bearish"
		}
	}

	score, reasons := scoreEntry(price, vwap, ema20, macdVal, macdSig, macdHist, macdHistPrev,
		rsi, volRatio, atr, body, upper)
	signal := "WAIT"
	if score >= 5 {
		signal = "BUY"
	}
	return Result{
		Symbol: symbol,
		Price:  pyRound(price, 4),
		VWAP:   pyRound(vwap, 4),
		EMA20:  pyRound(ema20, 4),
		MACD: MACDSummary{Value: pyRound(macdVal, 6), Signal: pyRound(macdSig, 6),
			Histogram: pyRound(macdHist, 6), Cross: cross},
		RSI:         pyRound(rsi, 2),
		VolumeRatio: pyRound(volRatio, 2),
		ATR:         r(atr, 4),
		Score:       score,
		MaxScore:    8,
		Reasons:     reasons,
		Signal:      signal,
		H4:          h4,
	}
}
