// Package reflect is Flintrade's sleep ("dreaming"), ported from agent/reflect.py.
// In the sleep window it distils trade history into bounded, reusable memory,
// and GetRecall injects that memory — with a sense of elapsed time — into
// every decision producer.
//
//	Aggregate   code: closed trades → agg (symbol × session × setup × RSI bucket)
//	Synthesize  flash-tier "dream": rewrite ≤20 lessons + expiring plans
//	Decay       code: confidence decay, plan expiry
//	GetRecall   wake-up block for producer payloads
//
// Never trades, never orders. The old semantic-vector indexing step is gone
// (see internal/memory).
package reflect

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"flintrade/internal/db"
	"flintrade/internal/llm"
	"flintrade/internal/loop"
	"flintrade/internal/memory"
	"flintrade/internal/postmortem"
	"flintrade/internal/producers/pj"
	"flintrade/internal/pyfloat"
	"flintrade/internal/session"
)

const (
	MaxLessons     = 20 // bounded rewrite: compression, not accumulation
	decayStaleDays = 7.0
	decayFactor    = 0.9
	archiveBelow   = 0.2
	dreamBudgetUSD = 0.50
)

// ── aggregate ───────────────────────────────────────────────────────────

func rsiBucket(features *string) string {
	if features == nil || *features == "" {
		return "na"
	}
	var f map[string]any
	if json.Unmarshal([]byte(*features), &f) != nil {
		return "na"
	}
	var r float64
	switch x := f["rsi"].(type) {
	case float64:
		r = x
	case string:
		v, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return "na"
		}
		r = v
	default:
		return "na"
	}
	switch {
	case r < 30:
		return "<30"
	case r < 50:
		return "30-50"
	case r < 70:
		return "50-70"
	}
	return ">70"
}

// Aggregate recomputes every agg cell from all closed trades (idempotent).
func Aggregate(ctx context.Context, s *db.Store) (int, error) {
	rows, err := s.Query(ctx, `SELECT symbol, ts, source, features, pnl FROM trades WHERE pnl IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	type key struct{ symbol, session, setup, rsi string }
	type cell struct {
		trips, wins, losses int
		winSum, lossSum     float64
	}
	cells := map[key]*cell{}
	var order []key
	for rows.Next() {
		var sym, ts string
		var src, feats *string
		var pnl float64
		if err := rows.Scan(&sym, &ts, &src, &feats, &pnl); err != nil {
			rows.Close()
			return 0, err
		}
		setup := "unknown"
		if src != nil && *src != "" {
			setup = *src
		}
		k := key{sym, memory.SessionOf(ts), setup, rsiBucket(feats)}
		c, ok := cells[k]
		if !ok {
			c = &cell{}
			cells[k] = c
			order = append(order, k)
		}
		c.trips++
		if pnl > 0 {
			c.wins++
			c.winSum += pnl
		} else if pnl < 0 {
			c.losses++
			c.lossSum += math.Abs(pnl)
		}
	}
	rows.Close()
	ts := db.Now()
	for _, k := range order {
		c := cells[k]
		avgWin, avgLoss := 0.0, 0.0
		if c.wins > 0 {
			avgWin = c.winSum / float64(c.wins)
		}
		if c.losses > 0 {
			avgLoss = c.lossSum / float64(c.losses)
		}
		var pl *float64
		if avgLoss > 0 {
			v := avgWin / avgLoss
			pl = &v
		}
		if _, err := s.Exec(ctx, db.PermAgg, `INSERT INTO agg(symbol,session,setup,rsi_bucket,trips,wins,losses,pl_ratio,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?)
			ON CONFLICT(symbol,session,setup,rsi_bucket) DO UPDATE SET
				trips=excluded.trips, wins=excluded.wins, losses=excluded.losses,
				pl_ratio=excluded.pl_ratio, updated_at=excluded.updated_at`,
			k.symbol, k.session, k.setup, k.rsi, c.trips, c.wins, c.losses, pl, ts); err != nil {
			return 0, err
		}
	}
	return len(order), nil
}

// ── dream prompt inputs ─────────────────────────────────────────────────

func objs(ctx context.Context, s *db.Store, q string, args ...any) ([]pj.Obj, error) {
	rows, err := s.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return pj.Rows(rows)
}

func aggSummary(ctx context.Context, s *db.Store) ([]pj.Obj, error) {
	rows, err := objs(ctx, s, `SELECT symbol,session,setup,rsi_bucket,trips,wins,losses,pl_ratio FROM agg ORDER BY trips DESC LIMIT 60`)
	if err != nil {
		return nil, err
	}
	out := []pj.Obj{}
	for _, r := range rows {
		trips, _ := r.Get("trips")
		wins, _ := r.Get("wins")
		pl, _ := r.Get("pl_ratio")
		t, _ := trips.(int64)
		w, _ := wins.(int64)
		var plv any
		if f, ok := pj.Float(pl); ok {
			plv = pj.F(pyfloat.Round(f, 2))
		}
		sym, _ := r.Get("symbol")
		sess, _ := r.Get("session")
		setup, _ := r.Get("setup")
		rb, _ := r.Get("rsi_bucket")
		out = append(out, pj.O("symbol", sym, "session", sess, "setup", setup, "rsi_bucket", rb, "n", t, "win_rate", round3(w, t), "pl_ratio", plv))
	}
	return out, nil
}

// recentClosed feeds only strategy-attributed trades: manual / test /
// outage-degraded would contaminate "how does my strategy perform".
func recentClosed(ctx context.Context, s *db.Store) ([]pj.Obj, error) {
	rows, err := objs(ctx, s, `SELECT id,ts,symbol,action,qty,fill_price,pnl,source,reason FROM trades
		WHERE pnl IS NOT NULL AND `+db.StrategyRecord("")+` ORDER BY id DESC LIMIT 40`)
	if err != nil {
		return nil, err
	}
	out := []pj.Obj{}
	for _, r := range rows {
		get := func(k string) any { v, _ := r.Get(k); return v }
		pnl, _ := pj.Float(get("pnl"))
		reason, _ := get("reason").(string)
		out = append(out, pj.O("id", get("id"), "ts", get("ts"), "symbol", get("symbol"), "action", get("action"), "qty", get("qty"), "pnl", pj.F(pyfloat.Round(pnl, 2)), "source", get("source"), "reason", pj.Truncate(reason, 400)))
	}
	return out, nil
}

// blockedIntents: entries rejected or expired in the last 3 days. A thesis
// blocked for a temporal reason was not invalidated — natural plan material.
func blockedIntents(ctx context.Context, s *db.Store, now time.Time) ([]pj.Obj, error) {
	cutoff := db.FormatTime(now.Add(-3 * 24 * time.Hour))
	rows, err := objs(ctx, s, `SELECT symbol, side, entry_hint, stop, target, confidence, reject_reason, reason, created_at
		FROM intents WHERE status IN ('rejected', 'expired') AND side IN ('long', 'short') AND created_at >= ?
		ORDER BY id DESC LIMIT 15`, cutoff)
	if err != nil {
		return nil, err
	}
	out := []pj.Obj{}
	for _, r := range rows {
		get := func(k string) any { v, _ := r.Get(k); return v }
		reason, _ := get("reason").(string)
		out = append(out, pj.O("symbol", get("symbol"), "side", get("side"), "entry_hint", get("entry_hint"), "stop", get("stop"), "target", get("target"), "confidence", get("confidence"), "blocked_by", get("reject_reason"), "at", get("created_at"), "thesis", pj.Truncate(reason, 200)))
	}
	return out, nil
}

func recentReviews(ctx context.Context, s *db.Store) ([]pj.Obj, error) {
	return objs(ctx, s, `SELECT symbol, thesis_verdict, entry_grade, exit_grade, lesson FROM trade_reviews ORDER BY id DESC LIMIT 15`)
}

// HardRules close every dream prompt.
const HardRules = `HARD RULES: (1) Any statistic with n<10 must be phrased as a preliminary observation "(n=X, low sample)" and must NEVER be stated as a rule, a win-rate verdict, or an avoid-list entry. (2) Never derive symbol-avoidance rules from n<10. (3) Trades tagged test/outage-degraded/manual are excluded from performance conclusions.`

// BuildDreamPrompt asks for a complete rewrite (not an append) of memory.
func BuildDreamPrompt(aggRows, trades []pj.Obj, todayET string, reviews, blocked []pj.Obj) string {
	dump := func(v any) string { s, _ := pj.Dumps(v); return s }
	reviewsBlock, blockedBlock := "", ""
	if len(reviews) > 0 {
		reviewsBlock = "\n\n3. RECENT TRADE POSTMORTEMS — per-trade hindsight verdicts from the postmortem layer (symbol, thesis_verdict, entry_grade, exit_grade, one-line lesson). These are already-distilled per-trade judgments; use them to corroborate or challenge patterns you see in the raw trades above.\n" + dump(reviews)
	}
	if len(blocked) > 0 {
		blockedBlock = "\n\n4. RECENTLY BLOCKED INTENTS — trade hypotheses from the last few days that the risk gate rejected or that expired unfilled, with the blocking reason. A thesis blocked for a *temporal* reason (cooldown, session blackout, duplicate in-flight order, exposure cap full) was NOT invalidated — it may still be a live setup worth revisiting. A thesis blocked on quality grounds (volume floor, invalid stop) usually is not.\n" + dump(blocked)
	}
	f := "```"
	return `You are the memory-consolidation ("dreaming") layer of an autonomous short-term US-equities paper-trading agent. The market is CLOSED; this is the agent's sleep. Your job is to DISTILL raw trade history into a small, durable, reusable memory — like a human consolidating the day into long-term memory.

Today (ET) is ` + todayET + `.

You are given two inputs:

1. AGGREGATE STATS — rolled-up performance by (symbol, session, setup, rsi_bucket). win_rate is fraction; pl_ratio = avg_win/avg_loss. n is sample size. Treat small-n rows skeptically.
` + dump(nonNil(aggRows)) + `

2. RECENT CLOSED TRADES with the trader's own reasoning text (these reasons are rich — they already contain the trader's reflections). Use them to find patterns, mistakes, and recurring setups.
` + dump(nonNil(trades)) + reviewsBlock + blockedBlock + `

Produce a COMPLETE REWRITE of the agent's memory (NOT an append). This is compression, not accumulation. Output AT MOST ` + strconv.Itoa(MaxLessons) + ` durable lessons. Each lesson must be:
  - durable & semantic (NOT date-bound) — a reusable rule, not "today I did X";
  - grounded in evidence (cite trade ids when possible);
  - quantified where the stats support it (cite n, win_rate, pl_ratio);
  - assigned a confidence 0.0-1.0 reflecting sample size and consistency.

Separately, produce a small WATCHLIST of plans — episodic, expiring notes about concrete setups worth revisiting on waking, NOT durable rules. Source them from: (a) BLOCKED INTENTS whose thesis was stopped by a temporal factor and may still be live; (b) setups the trader mentioned in reasoning but did not take; (c) any explicitly dated catalyst that appears in the inputs. Each plan states the condition to re-check (e.g. "re-check NVDA short below 220 — blocked by cooldown, thesis not invalidated") plus tags with the symbol, and an expires_at date (YYYY-MM-DD, typically 1-3 trading days out — a stale watchlist is noise; it is auto-dropped after that date).

Respond with ONE fenced ` + f + `json block, exactly this shape, nothing else:
` + f + `json
{"lessons":[{"text":"...","confidence":0.0,"evidence":[1,2],"tags":{"symbol":"NVDA.US","session":"Intraday","setup":"technical"}}],"plans":[{"text":"re-check NVDA short below 220 — cooldown-blocked, thesis not invalidated","expires_at":"2026-06-13","tags":{"symbol":"NVDA.US"}}]}
` + f + `
If there is no evidence for plans, return an empty plans list. Do not exceed ` + strconv.Itoa(MaxLessons) + ` lessons.

` + HardRules
}

func nonNil(x []pj.Obj) []pj.Obj {
	if x == nil {
		return []pj.Obj{}
	}
	return x
}

// ── synthesize ──────────────────────────────────────────────────────────

// SynthResult reports one dream.
type SynthResult struct {
	Lessons, Plans int
	Parsed         bool
}

// dream is the parsed reply, items kept raw so tags keep their key order.
type dream struct {
	Lessons []json.RawMessage
	Plans   []json.RawMessage
}

func parseDream(text string) (*dream, bool) {
	raw := postmortem.ExtractJSON(text)
	var top map[string]json.RawMessage
	if raw == "" || json.Unmarshal([]byte(raw), &top) != nil || top == nil {
		return nil, false
	}
	d := &dream{}
	_ = json.Unmarshal(top["lessons"], &d.Lessons) // not a list → none
	_ = json.Unmarshal(top["plans"], &d.Plans)
	return d, true
}

func confOf(v any) float64 {
	c := 0.5
	switch x := v.(type) {
	case float64:
		c = x
	case bool:
		if x {
			c = 1
		} else {
			c = 0
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
			c = f
		}
	}
	return math.Max(0, math.Min(1, c))
}

type prepared struct {
	text, evidence, tags string
	conf                 float64
}

func dumpOr(raw json.RawMessage, wantArray bool) string {
	trim := strings.TrimSpace(string(raw))
	ok := (wantArray && strings.HasPrefix(trim, "[")) || (!wantArray && strings.HasPrefix(trim, "{"))
	if !ok || !json.Valid(raw) {
		if wantArray {
			return "[]"
		}
		return "{}"
	}
	return string(pj.PySeparators(compactRaw(raw)))
}

func compactRaw(raw json.RawMessage) []byte {
	b, err := pj.Compact(raw)
	if err != nil {
		return raw
	}
	return b
}

// Synthesize dreams once. Lessons are replaced atomically (archive old +
// insert new in one transaction) and never wiped when the dream yields none;
// plans are upserted by text.
func (l *Loop) Synthesize(ctx context.Context, s *db.Store, now time.Time) (SynthResult, error) {
	aggRows, err := aggSummary(ctx, s)
	if err != nil {
		return SynthResult{}, err
	}
	trades, err := recentClosed(ctx, s)
	if err != nil {
		return SynthResult{}, err
	}
	reviews, err := recentReviews(ctx, s)
	if err != nil {
		return SynthResult{}, err
	}
	blocked, err := blockedIntents(ctx, s, now)
	if err != nil {
		return SynthResult{}, err
	}
	today := now.In(session.ET).Format(time.DateOnly)
	text, cerr := l.Complete(ctx, llm.Request{User: BuildDreamPrompt(aggRows, trades, today, reviews, blocked),
		Tier: "flash", Tag: "reflect:dream", MaxBudgetUSD: dreamBudgetUSD})
	if cerr != nil {
		fmt.Fprintf(os.Stderr, "[reflect] dream call failed: %v\n", cerr)
	}
	d, ok := parseDream(text)
	if !ok {
		return SynthResult{}, nil
	}
	rawLessons := d.Lessons
	if len(rawLessons) > MaxLessons {
		rawLessons = rawLessons[:MaxLessons]
	}
	var prep []prepared
	for _, raw := range rawLessons {
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil || m == nil {
			continue
		}
		var text string
		_ = json.Unmarshal(m["text"], &text)
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		var c any = 0.5
		if r, ok := m["confidence"]; ok {
			c = nil
			_ = json.Unmarshal(r, &c)
		}
		prep = append(prep, prepared{text: text, conf: confOf(c), evidence: dumpOr(m["evidence"], true), tags: dumpOr(m["tags"], false)})
	}
	ts := db.Now()
	res := SynthResult{Parsed: true}
	if len(prep) > 0 {
		err := s.Tx(ctx, func(tx *db.Store) error {
			if _, err := tx.Exec(ctx, db.PermMemory, `UPDATE memory_lessons SET status='archived' WHERE status='active'`); err != nil {
				return err
			}
			for _, p := range prep {
				if _, err := tx.Exec(ctx, db.PermMemory, `INSERT INTO memory_lessons(text,evidence,confidence,tags,status,created,last_confirmed)
					VALUES(?,?,?,?, 'active', ?, ?)`, p.text, p.evidence, p.conf, p.tags, ts, ts); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return res, err
		}
		res.Lessons = len(prep)
	} else {
		fmt.Fprintln(os.Stderr, "[reflect] dream produced no valid lesson; keeping the active set")
	}
	for _, raw := range d.Plans {
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil || m == nil {
			continue
		}
		var text string
		_ = json.Unmarshal(m["text"], &text)
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		var exp any
		_ = json.Unmarshal(m["expires_at"], &exp)
		if f, ok := exp.(float64); ok {
			exp = pj.Repr(f)
		}
		tags := dumpOr(m["tags"], false)
		var id int64
		err := s.QueryRow(ctx, `SELECT id FROM memory_plans WHERE text=? AND status='active'`, text).Scan(&id)
		if err == nil {
			_, err = s.Exec(ctx, db.PermMemory, `UPDATE memory_plans SET expires_at=?, tags=? WHERE id=?`, exp, tags, id)
		} else {
			_, err = s.Exec(ctx, db.PermMemory, `INSERT INTO memory_plans(text,tags,status,created,expires_at) VALUES(?,?, 'active', ?, ?)`,
				text, tags, ts, exp)
		}
		if err != nil {
			return res, err
		}
		res.Plans++
	}
	l.writeDigest(today, rawLessons, d.Plans)
	return res, nil
}

// writeDigest writes logs/dream-<date>.md; best effort, decoration only.
func (l *Loop) writeDigest(today string, lessons, plans []json.RawMessage) {
	if l.LogDir == "" {
		return
	}
	field := func(raw json.RawMessage, key, dflt string) string {
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			return dflt
		}
		v, ok := m[key]
		if !ok {
			return dflt
		}
		return pj.Str(v)
	}
	lines := []string{"# Dream digest — " + today, "", fmt.Sprintf("## Lessons (%d)", len(lessons)), ""}
	for _, ls := range lessons {
		lines = append(lines, fmt.Sprintf("- (%s) %s", field(ls, "confidence", "?"), field(ls, "text", "")))
	}
	lines = append(lines, "", fmt.Sprintf("## Plans (%d)", len(plans)), "")
	for _, p := range plans {
		lines = append(lines, fmt.Sprintf("- [exp %s] %s", field(p, "expires_at", "?"), field(p, "text", "")))
	}
	_ = os.MkdirAll(l.LogDir, 0o755)
	_ = os.WriteFile(filepath.Join(l.LogDir, "dream-"+today+".md"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

// ── decay ───────────────────────────────────────────────────────────────

// DecayResult reports one decay pass.
type DecayResult struct{ Decayed, Archived, Expired int }

// Decay ages lesson confidence (×0.9 per stale week since last confirmed,
// archive below 0.2) and expires plans whose date has passed (ET).
func Decay(ctx context.Context, s *db.Store, now time.Time) (DecayResult, error) {
	var r DecayResult
	today := now.In(session.ET).Format(time.DateOnly)
	rows, err := s.Query(ctx, `SELECT id, confidence, created, last_confirmed FROM memory_lessons WHERE status='active'`)
	if err != nil {
		return r, err
	}
	type lesson struct {
		id               int64
		conf             float64
		created, confirm *string
	}
	var ls []lesson
	for rows.Next() {
		var x lesson
		if err := rows.Scan(&x.id, &x.conf, &x.created, &x.confirm); err != nil {
			rows.Close()
			return r, err
		}
		ls = append(ls, x)
	}
	rows.Close()
	for _, x := range ls {
		var base *time.Time
		if x.confirm != nil {
			base = parseUTC(*x.confirm)
		}
		if base == nil && x.created != nil {
			base = parseUTC(*x.created)
		}
		if base == nil {
			continue
		}
		weeks := int(math.Floor(now.Sub(*base).Seconds() / 86400 / decayStaleDays))
		if weeks <= 0 {
			continue
		}
		c := pyfloat.Round(x.conf*math.Pow(decayFactor, float64(weeks)), 4)
		if x.conf*math.Pow(decayFactor, float64(weeks)) < archiveBelow {
			_, err = s.Exec(ctx, db.PermMemory, `UPDATE memory_lessons SET confidence=?, status='archived' WHERE id=?`, c, x.id)
			r.Archived++
		} else {
			_, err = s.Exec(ctx, db.PermMemory, `UPDATE memory_lessons SET confidence=? WHERE id=?`, c, x.id)
			r.Decayed++
		}
		if err != nil {
			return r, err
		}
	}
	prow, err := s.Query(ctx, `SELECT id, expires_at FROM memory_plans WHERE status='active'`)
	if err != nil {
		return r, err
	}
	var expired []int64
	for prow.Next() {
		var id int64
		var exp *string
		if err := prow.Scan(&id, &exp); err != nil {
			prow.Close()
			return r, err
		}
		if exp != nil {
			if e := strings.TrimSpace(*exp); e != "" && prefix10(e) < today {
				expired = append(expired, id)
			}
		}
	}
	prow.Close()
	for _, id := range expired {
		if _, err := s.Exec(ctx, db.PermMemory, `UPDATE memory_plans SET status='expired' WHERE id=?`, id); err != nil {
			return r, err
		}
		r.Expired++
	}
	return r, nil
}

// ── schedule + loop ─────────────────────────────────────────────────────

// Sleeping reports whether t falls in the sleep window, computed from the ET
// clock alone: weekends all day, weekdays 20:00–04:00. It deliberately reads
// no session label — 2026-08-06 the window listed the labels it accepted, a
// relabel (Overnight-Pre) removed the only weekday match, and Flintrade stopped
// dreaming. For regular days this is the same window Python accepted
// (Closed / Overnight / Overnight-Pre).
func Sleeping(t time.Time) bool {
	e := t.In(session.ET)
	if wd := e.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return true
	}
	return e.Hour() >= 20 || e.Hour() < 4
}

// Complete is the model call; tests inject canned replies.
type Complete = postmortem.Complete

// Loop is the "reflect" loop.
type Loop struct {
	D        *loop.Deps
	Complete Complete
	LogDir   string // dream digests; "" disables
}

// New builds the loop with the real model client.
func New(d *loop.Deps) *Loop {
	l := &Loop{D: d, LogDir: filepath.Join(d.Root, "logs")}
	l.Complete = func(ctx context.Context, req llm.Request) (string, error) { return d.LLM.Complete(ctx, req) }
	return l
}

func (l *Loop) Name() string { return "reflect" }

// ShouldDream: in the sleep window and not yet dreamed this ET date.
func (l *Loop) ShouldDream(ctx context.Context, s *db.Store, now time.Time) bool {
	if !Sleeping(now) {
		return false
	}
	last, _, _ := s.KVGet(ctx, "last_dream_date")
	return last != now.In(session.ET).Format(time.DateOnly)
}

// RunOnce checks the window and dreams at most once per ET date.
func (l *Loop) RunOnce(ctx context.Context) ([]string, error) {
	s := l.D.DB.MustAs(db.RoleReflect)
	if err := s.Beat(ctx, "reflect", nil); err != nil {
		return nil, err
	}
	now := l.D.Time()
	if !l.ShouldDream(ctx, s, now) {
		return []string{"not sleeping / already dreamed today"}, nil
	}
	return l.Dream(ctx, now)
}

// Dream runs a full sleep cycle regardless of the window (CLI --force).
func (l *Loop) Dream(ctx context.Context, now time.Time) ([]string, error) {
	s := l.D.DB.MustAs(db.RoleReflect)
	nAgg, err := Aggregate(ctx, s)
	if err != nil {
		return nil, fmt.Errorf("aggregate: %w", err)
	}
	dec, err := Decay(ctx, s, now)
	if err != nil {
		return nil, fmt.Errorf("decay: %w", err)
	}
	var out []string
	// Postmortems first, so the dream sees fresh trade_reviews. A failing
	// review layer must not stop the dream.
	reviews, err := postmortem.ReviewNew(ctx, s, l.Complete, 10)
	for _, line := range reviews {
		out = append(out, "postmortem: "+line)
	}
	if err != nil {
		out = append(out, "postmortem error: "+err.Error())
	}
	syn, err := l.Synthesize(ctx, s, now)
	if err != nil {
		return out, fmt.Errorf("synthesize: %w", err)
	}
	today := now.In(session.ET).Format(time.DateOnly)
	if err := s.KVSet(ctx, "last_dream_date", today); err != nil {
		return out, err
	}
	return append(out, fmt.Sprintf("dreamed %s: agg=%d rows, decay(decayed=%d,archived=%d,expired=%d), reviews=%d, lessons=%d plans=%d parsed=%s",
		today, nAgg, dec.Decayed, dec.Archived, dec.Expired, len(reviews), syn.Lessons, syn.Plans, pyBool(syn.Parsed))), nil
}

func pyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}
