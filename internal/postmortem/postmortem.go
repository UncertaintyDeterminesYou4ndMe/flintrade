// Package postmortem reviews each closed strategy trade on its own, ported
// from agent/postmortem.py: pair the closing fill with its position and entry
// intent, compute objective metrics in code (exit kind, slippage vs stop),
// then ask the cheap tier for an honest hindsight verdict. Rows go to
// trade_reviews — a dreaming product, not the ledger — under the reflect role.
package postmortem

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"flintrade/internal/db"
	"flintrade/internal/llm"
	"flintrade/internal/producers/pj"
	"flintrade/internal/pyfloat"
)

// Complete is the model call; tests inject canned responses here.
type Complete func(ctx context.Context, req llm.Request) (string, error)

// Context is one closed round trip. Nullable SQLite values stay `any`
// (int64, float64, string or nil) so the prompt prints them as Python did.
type Context struct {
	TradeID     int64
	PositionID  int64
	Symbol      string
	Attribution any
	Side        string
	EntryPrice  any
	ExitPrice   any
	Qty         any
	PnL         any
	Stop        any
	Target      any
	HoldMinutes any // int64 or nil
	Confidence  any
	Thesis      any
	ExitReason  any
}

// closing is the subset of a trades row the review needs.
type closing struct {
	ID          int64
	TS          string
	Symbol      string
	FillPrice   any
	Qty         any
	PnL         any
	PositionID  *int64
	Attribution any
	Reason      any
}

func parseTS(ts string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(ts))
	return t, err == nil
}

// pair returns nil when the position is missing (old migrated fills).
func pair(ctx context.Context, s *db.Store, t closing) (*Context, error) {
	if t.PositionID == nil {
		return nil, nil
	}
	var (
		posID                    int64
		side, openedAt           string
		entry, stop, target, iid any
	)
	err := s.QueryRow(ctx, `SELECT id, side, entry_price, stop, target, intent_id, opened_at FROM positions WHERE id=?`,
		*t.PositionID).Scan(&posID, &side, &entry, &stop, &target, &iid, &openedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c := &Context{TradeID: t.ID, PositionID: posID, Symbol: t.Symbol, Attribution: t.Attribution, Side: side,
		EntryPrice: entry, ExitPrice: t.FillPrice, Qty: t.Qty, PnL: t.PnL, Stop: stop, Target: target,
		ExitReason: t.Reason}
	if iid != nil {
		var conf, reason any
		err := s.QueryRow(ctx, `SELECT confidence, reason FROM intents WHERE id=?`, iid).Scan(&conf, &reason)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		c.Confidence, c.Thesis = conf, reason
	}
	if o, ok1 := parseTS(openedAt); ok1 {
		if x, ok2 := parseTS(t.TS); ok2 {
			c.HoldMinutes = int64(math.RoundToEven(x.Sub(o).Seconds() / 60))
		}
	}
	return c, nil
}

// Metrics are derived in code, never by the model.
type Metrics struct {
	ExitKind       string // target | stop | discretionary
	SlippageVsStop *float64
}

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	}
	return 0, false
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// CodeMetrics derives exit kind from the exit reason text and, for stops,
// slippage vs the stop (positive = worse than the stop).
func CodeMetrics(c Context) Metrics {
	reason := strings.ToLower(str(c.ExitReason))
	m := Metrics{ExitKind: "discretionary"}
	if strings.Contains(reason, "target") {
		m.ExitKind = "target"
	} else if strings.Contains(reason, "stop") {
		m.ExitKind = "stop"
	}
	stop, ok1 := num(c.Stop)
	exit, ok2 := num(c.ExitPrice)
	if m.ExitKind == "stop" && ok1 && ok2 {
		v := stop - exit
		if c.Side != "long" {
			v = exit - stop
		}
		v = pyfloat.Round(v, 4)
		m.SlippageVsStop = &v
	}
	return m
}

// py prints a value the way a Python f-string did.
func py(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case *float64:
		if x == nil {
			return "None"
		}
		return pj.Repr(*x)
	}
	return pj.Str(v)
}

func orNone(v any) string {
	if s := str(v); s != "" {
		return s
	}
	return "(none recorded)"
}

func buildPrompt(c Context, m Metrics) string {
	return fmt.Sprintf(`You are the trade-postmortem ("cognitive iteration") layer for an autonomous short-term US-equities paper-trading agent. A trade has just closed. Review it honestly and critically — you are not the trader, you are its skeptical supervisor.

Symbol: %s
Side: %s
Entry price: %s
Stop: %s
Target: %s
Exit price: %s
Realized PnL: %s
Hold time: %s minutes
Exit kind (code-derived from exit reason text): %s
Slippage vs stop (code-derived; positive = worse than stop, only set when exit_kind=stop): %s

Entry thesis / reasoning, in the trader's own words at entry time:
%s

Entry confidence stated at entry time (0-100): %s

Exit reason, in the trader's own words at exit time:
%s

Judge, in hindsight, with the full outcome known:
- thesis_verdict: was the entry thesis actually correct? correct | partial | wrong | unknowable
- entry_grade: was the entry itself well-timed/well-structured? good | ok | bad
- exit_grade: was the exit timing right? early | right | late
- confidence_justified: true/false — was the stated entry confidence warranted given the setup quality and outcome?
- lesson: ONE specific, reusable lesson (<=140 chars) — durable and actionable, not just a restatement of the pnl.

Respond with ONE fenced `+"```json"+` block, exactly this shape, nothing else:
`+"```json"+`
{"thesis_verdict":"correct","entry_grade":"ok","exit_grade":"right","confidence_justified":false,"lesson":"..."}
`+"```", c.Symbol, c.Side, py(c.EntryPrice), py(c.Stop), py(c.Target), py(c.ExitPrice), py(c.PnL),
		py(c.HoldMinutes), m.ExitKind, py(m.SlippageVsStop), orNone(c.Thesis), py(c.Confidence), orNone(c.ExitReason))
}

var fenced = regexp.MustCompile("(?s)```json\\s*(\\{.*?\\})\\s*```")

// ExtractJSON finds the reply's JSON text: a ```json fence first, else the
// outermost {...}. Shared by the dream parser.
func ExtractJSON(text string) string {
	if m := fenced.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	if i, j := strings.Index(text, "{"), strings.LastIndex(text, "}"); i != -1 && j > i {
		return text[i : j+1]
	}
	return ""
}

// ParseJSONObject is ExtractJSON decoded; nil unless it is a JSON object.
func ParseJSONObject(text string) map[string]any {
	raw := ExtractJSON(text)
	if raw == "" {
		return nil
	}
	var out map[string]any
	if json.Unmarshal([]byte(raw), &out) != nil {
		return nil
	}
	return out
}

type verdict struct {
	ThesisVerdict, EntryGrade, ExitGrade, Lesson *string
	ConfidenceJustified                          *int
}

var (
	validVerdict    = map[string]bool{"correct": true, "partial": true, "wrong": true, "unknowable": true}
	validEntryGrade = map[string]bool{"good": true, "ok": true, "bad": true}
	validExitGrade  = map[string]bool{"early": true, "right": true, "late": true}
)

// review asks the model; anything unusable leaves that field NULL. Never fails.
func review(ctx context.Context, complete Complete, c Context, m Metrics) verdict {
	var v verdict
	text, _ := complete(ctx, llm.Request{User: buildPrompt(c, m), Tier: "flash", Tag: "postmortem", MaxBudgetUSD: 0.30})
	p := ParseJSONObject(text)
	if p == nil {
		return v
	}
	pick := func(key string, ok map[string]bool) *string {
		if s, isStr := p[key].(string); isStr && ok[s] {
			return &s
		}
		return nil
	}
	v.ThesisVerdict = pick("thesis_verdict", validVerdict)
	v.EntryGrade = pick("entry_grade", validEntryGrade)
	v.ExitGrade = pick("exit_grade", validExitGrade)
	if b, ok := p["confidence_justified"].(bool); ok {
		n := 0
		if b {
			n = 1
		}
		v.ConfidenceJustified = &n
	}
	if s, ok := p["lesson"].(string); ok && strings.TrimSpace(s) != "" {
		s = pj.Truncate(strings.TrimSpace(s), 140)
		v.Lesson = &s
	}
	return v
}

// ReviewNew reviews up to limit closed strategy trades without a review,
// oldest first. manual / test / outage-degraded trades are never reviewed so
// they cannot pollute performance conclusions. s must hold the reflect role.
func ReviewNew(ctx context.Context, s *db.Store, complete Complete, limit int) ([]string, error) {
	rows, err := s.Query(ctx, `SELECT t.id, t.ts, t.symbol, t.fill_price, t.qty, t.pnl, t.position_id, t.attribution, t.reason
		FROM trades t LEFT JOIN trade_reviews r ON r.trade_id = t.id
		WHERE t.pnl IS NOT NULL AND `+db.StrategyRecord("t.")+` AND r.id IS NULL
		ORDER BY t.id ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	var todo []closing
	for rows.Next() {
		var t closing
		if err := rows.Scan(&t.ID, &t.TS, &t.Symbol, &t.FillPrice, &t.Qty, &t.PnL, &t.PositionID, &t.Attribution, &t.Reason); err != nil {
			rows.Close()
			return nil, err
		}
		todo = append(todo, t)
	}
	rows.Close()

	var lines []string
	for _, t := range todo {
		c, err := pair(ctx, s, t)
		if err != nil {
			return lines, err
		}
		if c == nil {
			// A stub row is still required: otherwise an unpairable trade
			// (pre-ledger migration) blocks the oldest-first queue forever.
			if _, err := s.Exec(ctx, db.PermMemory, `INSERT OR IGNORE INTO trade_reviews(trade_id, symbol, attribution, pnl,
				exit_kind, reviewed_at) VALUES(?,?,?,?, 'unpairable', ?)`, t.ID, t.Symbol, t.Attribution, t.PnL, db.Now()); err != nil {
				return lines, err
			}
			lines = append(lines, fmt.Sprintf("trade %d (%s): no matching position → stub(unpairable)", t.ID, t.Symbol))
			continue
		}
		m := CodeMetrics(*c)
		v := review(ctx, complete, *c, m)
		if _, err := s.Exec(ctx, db.PermMemory, `INSERT OR IGNORE INTO trade_reviews(
				trade_id, position_id, symbol, attribution, entry_price, exit_price,
				qty, pnl, stop, target, hold_minutes, exit_kind, slippage_vs_stop,
				confidence, thesis, exit_reason, thesis_verdict, entry_grade, exit_grade,
				confidence_justified, lesson, reviewed_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			c.TradeID, c.PositionID, c.Symbol, c.Attribution, c.EntryPrice, c.ExitPrice, c.Qty, c.PnL, c.Stop,
			c.Target, c.HoldMinutes, m.ExitKind, m.SlippageVsStop, c.Confidence, c.Thesis, c.ExitReason,
			v.ThesisVerdict, v.EntryGrade, v.ExitGrade, v.ConfidenceJustified, v.Lesson, db.Now()); err != nil {
			return lines, err
		}
		verdictText := "None"
		if v.ThesisVerdict != nil {
			verdictText = *v.ThesisVerdict
		}
		lines = append(lines, fmt.Sprintf("trade %d (%s): exit_kind=%s pnl=%s verdict=%s", c.TradeID, c.Symbol, m.ExitKind, py(c.PnL), verdictText))
	}
	return lines, nil
}

// Bucket is one confidence bucket's record.
type Bucket struct {
	N       int   `json:"n"`
	WinRate *pj.F `json:"win_rate"`
}

// Buckets is <60 / 60-70 / >70, in that key order.
type Buckets struct {
	Low  Bucket `json:"<60"`
	Mid  Bucket `json:"60-70"`
	High Bucket `json:">70"`
}

// StatsResult summarises trade_reviews joined to the authoritative trades.pnl.
type StatsResult struct {
	N                 int            `json:"n"`
	WinRate           *pj.F          `json:"win_rate"`
	NetPnL            float64        `json:"net_pnl"`
	AvgSlippageVsStop *float64       `json:"avg_slippage_vs_stop"`
	ExitKindCounts    map[string]int `json:"exit_kind_counts"`
	ConfidenceBuckets Buckets        `json:"confidence_buckets"`
}

func rate(wins, n int) *pj.F {
	if n == 0 {
		return nil
	}
	v := pj.F(pyfloat.Round(float64(wins)/float64(n), 3))
	return &v
}

// Stats is postmortem.stats(): pure code over trade_reviews.
func Stats(ctx context.Context, s *db.Store) (*StatsResult, error) {
	rows, err := s.Query(ctx, `SELECT r.exit_kind, r.slippage_vs_stop, r.confidence, t.pnl
		FROM trade_reviews r JOIN trades t ON t.id = r.trade_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &StatsResult{ExitKindCounts: map[string]int{}}
	var pnls, slips []float64
	var b [3]struct{ n, wins int }
	wins := 0
	for rows.Next() {
		var kind *string
		var slip *float64
		var conf, pnl any
		if err := rows.Scan(&kind, &slip, &conf, &pnl); err != nil {
			return nil, err
		}
		p, _ := num(pnl)
		pnls = append(pnls, p)
		if p > 0 {
			wins++
		}
		if slip != nil {
			slips = append(slips, *slip)
		}
		k := "unknown"
		if kind != nil && *kind != "" {
			k = *kind
		}
		out.ExitKindCounts[k]++
		if c, ok := num(conf); ok {
			i := 2
			if c < 60 {
				i = 0
			} else if c <= 70 {
				i = 1
			}
			b[i].n++
			if p > 0 {
				b[i].wins++
			}
		}
	}
	out.N = len(pnls)
	out.WinRate = rate(wins, out.N)
	out.NetPnL = pyfloat.Round(pyfloat.Sum(pnls), 2)
	if len(slips) > 0 {
		v := pyfloat.Round(pyfloat.Sum(slips)/float64(len(slips)), 4)
		out.AvgSlippageVsStop = &v
	}
	out.ConfidenceBuckets = Buckets{
		Low:  Bucket{b[0].n, rate(b[0].wins, b[0].n)},
		Mid:  Bucket{b[1].n, rate(b[1].wins, b[1].n)},
		High: Bucket{b[2].n, rate(b[2].wins, b[2].n)},
	}
	return out, rows.Err()
}
