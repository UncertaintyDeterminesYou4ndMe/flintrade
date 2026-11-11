// Package riskgate is the portfolio risk engine, ported from agent/risk_gate.py.
//
// The executor runs every pending intent through Evaluate, serially. All of it
// is hard-coded, none of it goes through a model:
//
//  1. HALT        — while halted only exits pass
//  2. playbooks   — binding playbooks need their h4 confirmation
//  3. no-revenge  — cool-down after a losing close on the same symbol
//  4. filters     — volume_ratio floor, session-close blackout, invalid stops
//  5. risk sizing — qty = max_risk_pct·equity / |entry − stop|
//  6. portfolio   — concurrency, gross, per-symbol, per-cluster, open risk
//
// Evaluate never orders and never writes; it returns a Verdict.
//
// Arithmetic mirrors Python bit for bit: CPython's compensated sum(), its
// float floor division, and float64(a*b) around every product so Go cannot
// fuse a multiply-add into an FMA (it may, even across statements).
package riskgate

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"flintrade/internal/config"
	"flintrade/internal/db"
	"flintrade/internal/pyfloat"
	"flintrade/internal/reasoncode"
)

// Intent is what the gate reads from an intent row.
type Intent struct {
	Symbol        string
	Side          string // long | short | close | flatten
	Source        string
	HypothesisQty *int
	EntryHint     *float64
	Stop          *float64
	Confidence    *int
	Features      map[string]any // parsed intents.features; nil when absent or not an object
}

// FromDB converts an intent row, parsing the features JSON. A features value
// that is not a JSON object reads as nil (Python: "not a dict").
func FromDB(it db.Intent) Intent {
	var feats map[string]any
	if len(it.Features) > 0 {
		if err := json.Unmarshal(it.Features, &feats); err != nil {
			feats = nil
		}
	}
	return Intent{Symbol: it.Symbol, Side: it.Side, Source: it.Source, HypothesisQty: it.HypothesisQty,
		EntryHint: it.EntryHint, Stop: it.Stop, Confidence: it.Confidence, Features: feats}
}

// IsExit reports whether side closes a position.
func IsExit(side string) bool { return side == "flatten" || side == "close" }

// Verdict is the gate's ruling.
type Verdict struct {
	Approved bool
	Qty      int             // final order size after sizing and caps
	Reason   string          // rejection cause / approval summary
	Code     reasoncode.Code // a rejection's cause, for Flintrade.app to say in its language
	Notes    map[string]any
}

// Gate is a snapshot: config re-read by the caller (hot reload) plus the
// current portfolio.
type Gate struct {
	Risk           *config.Risk
	Strategies     *config.Strategies // nil = no playbooks
	Equity         float64
	Halted         bool
	Positions      []db.Position
	RecentTrades   []db.Trade // newest first
	Session        string
	MinutesToClose *int // nil = unknown / not applicable
	VolumeRatio    *float64
	Now            time.Time
	// OpenOrders are submitted/partial orders. Entry orders (BUY/SHORT with a
	// price) count as exposure: while a limit order rests it is real risk
	// (5 stacked AMZN orders were $19.4K notional on a $10K sleeve while the
	// gate believed the book was flat).
	OpenOrders []db.Order
}

type inflight struct {
	symbol   string
	notional float64
}

func (g *Gate) inflight() []inflight {
	var out []inflight
	for _, o := range g.OpenOrders {
		// Only the unfilled remainder rests: a partially filled entry's
		// filled shares are already a position and counted there.
		// The order still takes a concurrency slot even with nothing left
		// (or no qty recorded), exactly as Python counted it.
		if (o.Side == "BUY" || o.Side == "SHORT") && o.Price != nil {
			rest := max(o.Qty-o.FilledQty, 0)
			out = append(out, inflight{o.Symbol, float64(float64(rest) * *o.Price)})
		}
	}
	return out
}

func posNotional(p db.Position) float64 {
	return math.Abs(float64(float64(p.Qty) * p.EntryPrice))
}

func (g *Gate) notional(match func(symbol string) bool) float64 {
	var held, fly []float64
	for _, p := range g.Positions {
		if match(p.Symbol) {
			held = append(held, posNotional(p))
		}
	}
	for _, x := range g.inflight() {
		if match(x.symbol) {
			fly = append(fly, math.Abs(x.notional))
		}
	}
	return pyfloat.Sum(held) + pyfloat.Sum(fly)
}

func (g *Gate) openRisk() float64 {
	var xs []float64
	for _, p := range g.Positions {
		if p.RiskAmt != nil {
			xs = append(xs, *p.RiskAmt)
		} else {
			xs = append(xs, 0)
		}
	}
	return pyfloat.Sum(xs)
}

// ConfidenceScale maps the producer's self-reported confidence to a size
// multiplier in (0, 1]: it only ever shrinks. Over-reporting buys nothing
// beyond the nominal cap; honest low scores really cut exposure. A missing
// confidence (manual orders) scales 1.0.
func ConfidenceScale(cs config.ConfidenceScale, conf *int) float64 {
	if conf == nil {
		return 1.0
	}
	c := float64(*conf)
	if c >= cs.FullAt {
		return 1.0
	}
	if c <= cs.FloorAt || cs.FullAt <= cs.FloorAt {
		return cs.FloorScale
	}
	return cs.FloorScale + float64(float64((1.0-cs.FloorScale)*(c-cs.FloorAt))/(cs.FullAt-cs.FloorAt))
}

func reject(code reasoncode.Code, format string, a ...any) Verdict {
	return Verdict{Reason: fmt.Sprintf(format, a...), Code: code}
}

// Evaluate rules on one intent.
func (g *Gate) Evaluate(in Intent) Verdict {
	symbol, side := in.Symbol, in.Side
	isExit := IsExit(side)

	// 1. HALT — exits always pass (the book must be reducible while halted).
	if g.Halted && !isExit {
		return reject(reasoncode.New("halted"), "HALT: 全局熔断,仅允许平仓")
	}
	if isExit {
		// Exits skip sizing and caps, but there must be something to close.
		var pos *db.Position
		for i := range g.Positions {
			if g.Positions[i].Symbol == symbol {
				pos = &g.Positions[i]
				break
			}
		}
		if pos == nil {
			return reject(reasoncode.New("noPositionToClose", "symbol", symbol), "无 %s 持仓可平", symbol)
		}
		qty := pos.Qty
		// Partial close (scale-out): hypothesis_qty below the held qty.
		if hq := in.HypothesisQty; hq != nil && 0 < *hq && *hq < qty {
			qty = *hq
		}
		return Verdict{Approved: true, Qty: qty, Reason: "平仓放行", Notes: map[string]any{"exit": true}}
	}

	r := g.Risk
	if side == "short" && !r.Direction.AllowShort {
		return reject(reasoncode.New("shortingDisabled"), "配置禁止做空")
	}

	// Binding playbooks: an automated entry must carry the playbook's
	// confirmation, taken from the indicator snapshot, never from the model.
	// Manual (user) orders are exempt; stops and the breaker still apply.
	if g.Strategies != nil {
		if pb := g.Strategies.PlaybookFor(symbol); pb != nil && pb.Binding && in.Source != "user" {
			if pb.LongOnly && side != "long" {
				return reject(reasoncode.New("playbookLongOnly", "playbook", pb.Name), "playbook[%s]: 仅允许多头入场", pb.Name)
			}
			if in.Features == nil || !truthy(in.Features["h4_confluence"]) {
				return reject(reasoncode.New("playbookNeedsH4", "playbook", pb.Name), "playbook[%s]: 缺少 h4 共振确认(binding)", pb.Name)
			}
		}
	}

	// 3. no-revenge: the symbol's latest close was a loss inside the window.
	cd := r.PerTrade.RevengeCooldownMin
	for _, t := range g.RecentTrades {
		if t.Symbol != symbol || t.PnL == nil {
			continue // other symbols and opening legs
		}
		if *t.PnL <= 0 {
			if ts, err := db.ParseTime(t.TS); err == nil {
				// Python works in microseconds (datetime / timedelta).
				window := int64(math.RoundToEven(float64(cd * 60e6)))
				elapsed := g.Now.Truncate(time.Microsecond).Sub(ts).Microseconds()
				if elapsed < window {
					mins := int(float64(window-elapsed) / 1e6 / 60)
					return reject(reasoncode.New("revengeCooldown", "symbol", symbol, "minutes", strconv.Itoa(mins)),
						"no-revenge 冷却中(%s 还剩 ~%dmin)", symbol, mins)
				}
			}
		}
		break // only the symbol's most recent close counts
	}

	// 4. Filters. A missing volume_ratio is fail-closed: it only goes missing
	// when the collection pipeline broke (2026-08: three entries passed with
	// no indicator source at all). Manual orders are exempt, as for playbooks.
	floor := r.PerTrade.VolumeRatioFloor
	if g.VolumeRatio == nil {
		if in.Source != "user" {
			return reject(reasoncode.New("missingVolumeRatio"), "缺 volume_ratio(指标数据缺失),fail-closed 拒开仓")
		}
	} else if *g.VolumeRatio < floor {
		return reject(reasoncode.New("volumeRatioLow", "ratio", fmt.Sprintf("%.2f", *g.VolumeRatio), "floor", PyRepr(floor)),
			"volume_ratio %.2f < %s", *g.VolumeRatio, PyRepr(floor))
	}

	blackout := r.PerTrade.SessionCloseBlackoutMin
	if m := g.MinutesToClose; m != nil && 0 <= *m && float64(*m) < blackout {
		return reject(reasoncode.New("closeBlackout", "minutesLeft", strconv.Itoa(*m), "blackout", PyRepr(blackout)),
			"距收盘 %dmin < %smin 黑窗,不开新仓", *m, PyRepr(blackout))
	}

	if in.EntryHint == nil || *in.EntryHint == 0 || in.Stop == nil || *in.Stop == 0 || *in.EntryHint <= 0 {
		return reject(reasoncode.New("missingEntryOrStop"), "缺 entry_hint/stop,无法风险定额")
	}
	entry, stop := *in.EntryHint, *in.Stop
	stopDist := math.Abs(entry - stop)
	minDist := float64(entry*r.PerTrade.MinStopDistancePct) / 100.0
	// stopDist == 0 only when min_stop_distance_pct is 0; Python then divided
	// by zero and crashed the executor iteration. Reject instead.
	if stopDist < minDist || stopDist == 0 {
		return reject(reasoncode.New("stopTooClose", "distance", fmt.Sprintf("%.4f", stopDist), "minimum", fmt.Sprintf("%.4f", minDist)),
			"止损距离 %.4f 过近(<%.4f),无效止损", stopDist, minDist)
	}

	// 5. Risk sizing.
	maxRiskAmt := float64(r.PerTrade.MaxRiskPct / 100.0 * g.Equity)
	qty := pyInt(floorDiv(maxRiskAmt, stopDist))
	if qty < 1 {
		return reject(reasoncode.New("budgetBelowOneShare", "budget", fmt.Sprintf("%.2f", maxRiskAmt), "perShare", fmt.Sprintf("%.2f", stopDist)),
			"风险预算 $%.2f 不足 1 股(每股风险 $%.2f)", maxRiskAmt, stopDist)
	}

	// 6a. Concurrency: a new symbol takes a slot; in-flight entries do too.
	port := r.Portfolio
	held := map[string]bool{}
	for _, p := range g.Positions {
		held[p.Symbol] = true
	}
	for _, x := range g.inflight() {
		held[x.symbol] = true
	}
	if !held[symbol] && len(held) >= port.MaxConcurrentPositions {
		return reject(reasoncode.New("maxPositions", "max", strconv.Itoa(port.MaxConcurrentPositions)),
			"已达并发仓上限 %d", port.MaxConcurrentPositions)
	}

	type limit struct {
		remaining float64
		label     string
	}
	all := func(string) bool { return true }
	caps := []limit{
		{float64(port.MaxGrossExposurePct/100.0*g.Equity) - g.notional(all), "gross"},
		{float64(port.MaxPerSymbolPct/100.0*g.Equity) - g.notional(func(s string) bool { return s == symbol }), "per_symbol"},
	}
	clusterOf := r.ClusterOf()
	if cl := clusterOf[symbol]; cl != "" {
		caps = append(caps, limit{float64(r.Clusters.MaxPerClusterPct/100.0*g.Equity) -
			g.notional(func(s string) bool { return clusterOf[s] == cl }), "cluster:" + cl})
	}
	for _, c := range caps {
		allowed := pyInt(floorDiv(c.remaining, entry))
		if allowed < qty {
			if allowed < 1 {
				return reject(reasoncode.New("capFull", "cap", c.label), "%s 限额已满,无可用敞口", c.label)
			}
			qty = allowed // shrink to what the cap allows
		}
	}

	// 6c. Confidence scaling — after the notional caps, which almost always
	// bind before the 2% risk budget; scaling the budget would be a no-op.
	scale := ConfidenceScale(r.PerTrade.ConfidenceScale, in.Confidence)
	if scale < 1.0 {
		scaled := int(float64(float64(qty) * scale))
		if scaled < 1 {
			return reject(reasoncode.New("confidenceBelowOneShare", "confidence", strconv.Itoa(*in.Confidence), "scale", fmt.Sprintf("%.2f", scale)),
				"confidence %d 缩放(%.2f)后不足 1 股", *in.Confidence, scale)
		}
		qty = scaled
	}

	// 6b. Total open risk.
	newRisk := float64(float64(qty) * stopDist)
	maxOpenRisk := float64(port.MaxOpenRiskPct / 100.0 * g.Equity)
	room := maxOpenRisk - g.openRisk()
	if newRisk > room {
		qty = pyInt(floorDiv(room, stopDist))
		if qty < 1 {
			return reject(reasoncode.New("openRiskCap", "pct", TomlNum(port.MaxOpenRiskPct)), "组合总风险已达上限 %s%%", TomlNum(port.MaxOpenRiskPct))
		}
	}

	note := ""
	if scale < 1.0 {
		note = fmt.Sprintf(", conf %d→×%.2f", *in.Confidence, scale)
	}
	riskAmt := float64(float64(qty) * stopDist)
	return Verdict{
		Approved: true, Qty: qty,
		Reason: fmt.Sprintf("批准 %d 股(风险定额 $%.2f / 预算 $%.2f%s)", qty, riskAmt, maxRiskAmt, note),
		Notes: map[string]any{"risk_amt": pyfloat.Round(riskAmt, 4), "stop_dist": pyfloat.Round(stopDist, 4),
			"confidence_scale": pyfloat.Round(scale, 4)},
	}
}

// truthy is Python truthiness for a decoded JSON value.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// floorDiv is CPython's float // (float_floor_div): not math.Floor(a/b),
// which differs when a/b rounds up to an integer (1 // 0.1 == 9.0).
func floorDiv(vx, wx float64) float64 {
	mod := math.Mod(vx, wx)
	div := (vx - mod) / wx
	if mod != 0 {
		if (wx < 0) != (mod < 0) {
			div -= 1.0
		}
	}
	if div != 0 {
		fd := math.Floor(div)
		if div-fd > 0.5 {
			fd += 1.0
		}
		return fd
	}
	return math.Copysign(0, vx/wx)
}

// pyInt is int(x) for a float: truncation toward zero.
func pyInt(x float64) int { return int(math.Trunc(x)) }

// PyRepr is Python's repr() of a float (30.0 → "30.0", 0.3 → "0.3").
func PyRepr(f float64) string {
	if math.IsInf(f, 0) {
		if f > 0 {
			return "inf"
		}
		return "-inf"
	}
	if math.IsNaN(f) {
		return "nan"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64)
	exp, _ := strconv.Atoi(e[strings.IndexByte(e, 'e')+1:])
	if exp < -4 || exp >= 16 {
		mant, ex, _ := strings.Cut(e, "e")
		sign := ex[:1]
		digits := strings.TrimLeft(ex[1:], "0")
		if len(digits) < 2 {
			digits = strings.Repeat("0", 2-len(digits)) + digits
		}
		return mant + "e" + sign + digits
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".") {
		s += ".0"
	}
	return s
}

// TomlNum prints a value the way Python printed the raw toml number: an
// integral value was (by convention in risk.toml) written as an int.
func TomlNum(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return PyRepr(f)
}
