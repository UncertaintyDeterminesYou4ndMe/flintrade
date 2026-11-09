// Package technical is the technical-signal producer, ported from
// agent/producers/loop_technical.py: collect market data → pure-analysis
// model call → at most one intent on the queue. It never sizes and never
// orders; the executor and risk gate own that.
package technical

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"flintrade/internal/collect"
	"flintrade/internal/config"
	"flintrade/internal/db"
	"flintrade/internal/llm"
	"flintrade/internal/loop"
	"flintrade/internal/memory"
	"flintrade/internal/producers/pj"
	"flintrade/internal/reflect"
	"flintrade/internal/session"
)

// Complete is the model call; tests inject canned replies.
type Complete func(ctx context.Context, req llm.Request) (string, error)

// ── decision parsing (shared with the event producer) ───────────────────

var fenced = regexp.MustCompile("(?s)```json\\s*(\\{.*?\\})\\s*```")

// ParseIntent extracts the decision JSON: a ```json fence, else the first
// line that starts with "{" and mentions "action".
func ParseIntent(text string) map[string]any {
	raw := ""
	if m := fenced.FindStringSubmatch(text); m != nil {
		raw = m[1]
	} else {
		for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "{") && strings.Contains(line, "action") {
				raw = line
				break
			}
		}
	}
	if raw == "" {
		return nil
	}
	return decodeNum(raw)
}

// decodeNum decodes a JSON object keeping numbers as json.Number, so ints
// and floats stay distinct as in Python's json.loads.
func decodeNum(raw string) map[string]any {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var out map[string]any
	if dec.Decode(&out) != nil {
		return nil
	}
	return out
}

var sides = map[string]string{"BUY": "long", "SHORT": "short", "CLOSE": "close", "SELL": "close"}

// Intent is a decision mapped onto submit_intent arguments.
type Intent struct {
	Symbol     string
	Side       string
	EntryHint  *float64
	Stop       *float64
	Target     *float64
	Confidence *int
	Reason     string
	Features   pj.Obj // nil when empty
}

// num reads a model-supplied price; numeric strings count (SQLite's REAL
// affinity stored them as numbers in Python).
func num(v any) *float64 {
	switch x := v.(type) {
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return &f
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
			return &f
		}
	}
	return nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// ToIntent maps a decision to an intent; WAIT / HOLD / unknown → nil.
//
// volume_ratio is deliberately NOT taken from the decision: it is the risk
// gate's hard floor, and letting the constrained party supply it handed the
// model the key (2026-08: three entries passed on self-reported values, one
// on a symbol with no indicators at all). AttachGateEvidence backfills it.
func ToIntent(d map[string]any) *Intent {
	action := strings.ToUpper(str(d["action"]))
	side, ok := sides[action]
	sym := str(d["symbol"])
	if !ok || sym == "" {
		return nil
	}
	in := &Intent{Symbol: sym, Side: side, EntryHint: num(d["entry_hint"]), Stop: num(d["stop"]),
		Target: num(d["target"]), Reason: pj.Truncate(str(d["reasoning"]), 1000)}
	if c := num(d["confidence"]); c != nil {
		v := int(math.RoundToEven(*c))
		in.Confidence = &v
	}
	if t2, ok := d["target2"].(json.Number); ok {
		in.Features.Set("target2", pj.PyNum(t2)) // second take-profit: model judgement, not a gate input
	}
	return in
}

// AttachGateEvidence backfills the risk gate's mechanical inputs from the
// collected indicator snapshot, never from the model: volume_ratio (hard
// floor, fail-closed when absent) and, for playbook symbols, h4 confluence.
// No indicator block for the symbol → nothing attached → the gate rejects,
// which is right: a broken data path is no time to trade.
func AttachGateEvidence(in *Intent, indicators []json.RawMessage) {
	for _, raw := range indicators {
		ind := decodeNum(string(raw))
		if ind == nil || str(ind["symbol"]) != in.Symbol {
			continue
		}
		if vr, ok := ind["volume_ratio"].(json.Number); ok {
			in.Features.Set("volume_ratio", pj.PyNum(vr))
		}
		if h4, ok := ind["h4"].(map[string]any); ok {
			in.Features.Set("h4_confluence", truthy(h4["confluence"]))
			in.Features.Set("h4_trend_ok", truthy(h4["trend_ok"]))
		}
		return
	}
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case json.Number:
		f, _ := x.Float64()
		return f != 0
	case int64:
		return x != 0
	case pj.F:
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

// Submit queues the intent; inserted=false means the dedup key existed.
func Submit(ctx context.Context, s *db.Store, source string, priority int, dedup string, in *Intent) (int64, bool, error) {
	var feats any
	if len(in.Features) > 0 {
		feats = in.Features
	}
	return s.SubmitIntent(ctx, db.IntentInput{Source: source, Priority: priority, Symbol: in.Symbol, Side: in.Side,
		EntryHint: in.EntryHint, Stop: in.Stop, Target: in.Target, Confidence: in.Confidence,
		DedupKey: &dedup, Reason: &in.Reason, Features: feats})
}

func pyOpt(f *float64) string {
	if f == nil {
		return "None"
	}
	return pj.Repr(*f)
}

func pyInt(i *int) string {
	if i == nil {
		return "None"
	}
	return strconv.Itoa(*i)
}

// Describe is the log line for a queued intent.
func Describe(id int64, in *Intent) string {
	return fmt.Sprintf("intent #%d: %s %s @ %s stop %s conf %s", id, in.Side, in.Symbol, pyOpt(in.EntryHint), pyOpt(in.Stop), pyInt(in.Confidence))
}

// ── payload ─────────────────────────────────────────────────────────────

// Playbooks renders strategies.toml's playbooks as the model sees them.
func Playbooks(s *config.Strategies) pj.Obj {
	o := pj.Obj{}
	for _, pb := range s.Playbooks {
		o.Set(pb.Name, pj.O("symbols", pb.Symbols, "binding", pb.Binding, "long_only", pb.LongOnly,
			"lookback", pb.Lookback, "stop_pct", pb.StopPct, "t1_pct", pb.T1Pct, "t2_pct", pb.T2Pct))
	}
	return o
}

// FlintradePositions is every open position row, columns in table order.
func FlintradePositions(ctx context.Context, s *db.Store) ([]pj.Obj, error) {
	rows, err := s.Query(ctx, `SELECT * FROM positions WHERE status='open'`)
	if err != nil {
		return nil, err
	}
	return pj.Rows(rows)
}

// FlintradeRisk is the ledger's equity / open risk / halt, typed as Python
// read them ({} when the row is missing).
func FlintradeRisk(ctx context.Context, s *db.Store) (pj.Obj, error) {
	rows, err := s.Query(ctx, `SELECT equity, open_risk, halt FROM risk_state WHERE id=1`)
	if err != nil {
		return nil, err
	}
	rs, err := pj.Rows(rows)
	if err != nil || len(rs) == 0 {
		return pj.Obj{}, err
	}
	r := rs[0]
	eq, _ := r.Get("equity")
	or, _ := r.Get("open_risk")
	h, _ := r.Get("halt")
	return pj.O("equity", eq, "open_risk", or, "halt", truthy(h)), nil
}

// bestCandidate is the highest-scoring indicator block (first on ties).
func bestCandidate(indicators []json.RawMessage) (string, bool) {
	best, bestScore, found := "", 0.0, false
	for _, raw := range indicators {
		ind := decodeNum(string(raw))
		if ind == nil {
			continue
		}
		score, _ := pj.Float(ind["score"])
		if !found || score > bestScore {
			best, bestScore, found = str(ind["symbol"]), score, true
		}
	}
	return best, found && best != ""
}

// ── loop ────────────────────────────────────────────────────────────────

// Loop is "loop_technical".
type Loop struct {
	D         *loop.Deps
	Collector collect.Collector
	Complete  Complete
	Prompt    string // system prompt path
}

// New builds the loop on the shared deps.
func New(d *loop.Deps) *Loop {
	return &Loop{
		D:         d,
		Collector: collect.Collector{LB: d.LB, Config: d.Config, Now: d.Now},
		Complete:  func(ctx context.Context, req llm.Request) (string, error) { return d.LLM.Complete(ctx, req) },
		Prompt:    filepath.Join(d.Root, "agent", "prompts", "technical_intent.md"),
	}
}

func (l *Loop) Name() string { return "loop_technical" }

// Payload is what the model sees: the collector's fields, then playbooks,
// flintrade_positions, flintrade_risk, recall and (when Flintrade has history on the
// best candidate) similar_history — Python's key order.
func (l *Loop) Payload(ctx context.Context, sess string) (pj.Obj, []json.RawMessage, error) {
	p, err := l.Collector.Collect(ctx, sess, session.OutsideRTHFor(sess, false))
	if err != nil {
		return nil, nil, err
	}
	raw, err := pj.Compact(p)
	if err != nil {
		return nil, nil, err
	}
	data, _ := pj.DecodeObj(raw)
	strategies, err := l.D.Config.Strategies()
	if err != nil {
		return nil, nil, err
	}
	data.Set("playbooks", Playbooks(strategies))
	rd := l.D.DB.MustAs(db.RoleReader)
	pos, err := FlintradePositions(ctx, rd)
	if err != nil {
		return nil, nil, err
	}
	data.Set("flintrade_positions", pos)
	risk, err := FlintradeRisk(ctx, rd)
	if err != nil {
		return nil, nil, err
	}
	data.Set("flintrade_risk", risk)
	if rec, err := reflect.GetRecall(ctx, rd, l.D.Time()); err != nil {
		data.Set("recall", pj.O("error", "recall unavailable: "+err.Error()))
	} else {
		data.Set("recall", rec)
	}
	if sym, ok := bestCandidate(p.Indicators); ok {
		if sh, err := memory.SimilarHistory(ctx, rd, sym, sess); err != nil {
			fmt.Fprintf(os.Stderr, "[loop_technical] similar_history skipped: %v\n", err)
		} else if sh != nil {
			data.Set("similar_history", sh)
		}
	}
	return data, p.Indicators, nil
}

// RunOnce is one producer round.
func (l *Loop) RunOnce(ctx context.Context) ([]string, error) {
	s := l.D.DB.MustAs(db.RoleTechnical)
	if err := s.Beat(ctx, "loop_technical", nil); err != nil {
		return nil, err
	}
	if halted, err := s.IsHalted(ctx); err != nil {
		return nil, err
	} else if halted {
		return []string{"halted — 跳过技术面信号产出"}, nil
	}
	// Outside trading sessions every indicator is stale; an intent now would
	// rest over the weekend (2026-08-01 Saturday, flintrade-210).
	sess := l.D.Clock.Current(ctx)
	if !session.IsTradeable(sess) {
		return []string{fmt.Sprintf("session=%s — 非交易时段,跳过", sess)}, nil
	}
	data, indicators, err := l.Payload(ctx, sess)
	if err != nil {
		return nil, fmt.Errorf("collect: %w", err)
	}
	user, err := pj.Dumps(data)
	if err != nil {
		return nil, err
	}
	system, err := os.ReadFile(l.Prompt)
	if err != nil {
		return nil, err
	}
	text, cerr := l.Complete(ctx, llm.Request{User: user, System: string(system), Tier: "trader", Tag: "producer:technical"})
	decision := ParseIntent(text)
	if decision == nil {
		line := fmt.Sprintf("无法解析 LLM 输出(前80字: %q)", pj.Truncate(text, 80))
		if cerr != nil {
			line += " — " + cerr.Error()
		}
		return []string{line}, nil
	}
	in := ToIntent(decision)
	if in == nil {
		return []string{"WAIT — " + pj.Truncate(str(decision["reasoning"]), 80)}, nil
	}
	AttachGateEvidence(in, indicators)
	risk, err := l.D.Config.Risk()
	if err != nil {
		return nil, err
	}
	// One intent per symbol per minute-cycle (the executor polls faster).
	dedup := fmt.Sprintf("technical:%s:%s", in.Symbol, l.D.Time().UTC().Format("200601021504"))
	id, inserted, err := Submit(ctx, s, "technical", risk.Priority["technical"], dedup, in)
	if err != nil {
		return nil, err
	}
	if !inserted {
		return []string{fmt.Sprintf("重复 intent 已去重(%s)", in.Symbol)}, nil
	}
	return []string{Describe(id, in)}, nil
}
