package event

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flintrade/internal/config"
	"flintrade/internal/db"
	"flintrade/internal/indicators"
	"flintrade/internal/lb"
	"flintrade/internal/llm"
	"flintrade/internal/loop"
	"flintrade/internal/producers/pj"
	"flintrade/internal/producers/technical"
	"flintrade/internal/session"
	"flintrade/internal/testutil"
)

type harness struct {
	l      *Loop
	d      *db.DB
	logDir string
	reply  string
	fix    string
}

// newHarness: collect's fake longbridge + fixtures, a fake `claude` behind
// the real llm.Client, a temp DB.
func newHarness(t *testing.T, now time.Time) *harness {
	t.Helper()
	root, _ := filepath.Abs("../../..")
	cdata := filepath.Join(root, "internal", "collect", "testdata")
	h := &harness{fix: filepath.Join(cdata, "fixtures"), logDir: filepath.Join(t.TempDir(), "llm_calls")}
	t.Setenv("FAKE_LB_DIR", h.fix)
	t.Setenv("FLINTRADE_LLM_DRY", "")
	bin := t.TempDir()
	h.reply = filepath.Join(bin, "reply.txt")
	os.WriteFile(h.reply, []byte(`{"action":"WAIT","reasoning":"stale"}`), 0o644)
	os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/bash\ncat '"+h.reply+"'\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	cfg := config.Loader{Dir: filepath.Join(cdata, "config")}
	h.d = testutil.DB(t)
	testutil.SetEquity(t, h.d, 10000)
	clock := session.New(nil)
	clock.Now = func() time.Time { return now }
	h.l = New(&loop.Deps{Root: root, DB: h.d, Config: cfg, LB: &lb.Runner{Bin: filepath.Join(cdata, "fake", "longbridge"), Retries: 1},
		Clock: clock, LLM: &llm.Client{Config: cfg, LogDir: h.logDir, Now: func() time.Time { return now }},
		Now: func() time.Time { return now }})
	return h
}

func (h *harness) addEvent(t *testing.T, sym, kind, title string) int64 {
	t.Helper()
	id, err := h.d.MustAs(db.RoleNews).AddEvent(context.Background(), db.EventInput{Symbol: &sym, Kind: &kind, Title: &title,
		Payload: pj.O("title", title, "note", "<b> & co")})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestGather_CursorAndSignals(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, time.Now())
	s := h.d.MustAs(db.RoleEvent)
	news := h.d.MustAs(db.RoleNews)
	sym := "AAPL.US"
	for i := 0; i < 6; i++ {
		news.AddSignal(ctx, db.SignalInput{Source: "news_collector", Symbol: &sym, Kind: db.Ptr("news"), Payload: pj.O("i", i)})
	}
	h.addEvent(t, sym, "news_catalyst", "e1")
	h.addEvent(t, sym, "price_alert", "not a catalyst kind")
	last := h.addEvent(t, "META.US", "earnings", "e3")
	cats, err := Gather(ctx, s)
	if err != nil || len(cats) != 2 {
		t.Fatalf("cats = %d %v", len(cats), err)
	}
	if idOf(cats[0].Event) != last || len(cats[0].Signals) != 0 || len(cats[1].Signals) != 5 {
		t.Fatalf("order/signals wrong: %+v", cats)
	}
	if v, _, _ := s.KVGet(ctx, cursorKey); v != fmt.Sprint(last) {
		t.Fatalf("cursor = %s", v)
	}
	if cats, _ := Gather(ctx, s); len(cats) != 0 {
		t.Fatal("an event must be gathered once")
	}
}

// Hard lesson "记忆注入对称": lessons / stats / self_assessment reach the
// event producer exactly as they reach the technical one; only plans (the
// technical watchlist) are withheld.
func TestHardLesson_SymmetricMemoryInjection(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, time.Date(2026, 11, 9, 7, 0, 12, 0, time.UTC))
	m := h.d.MustAs(db.RoleMigrate)
	m.Exec(ctx, db.PermMemory, `INSERT INTO memory_lessons(text,evidence,confidence,status,created) VALUES('precious-metal pulses mean-revert','[1,2]',0.8,'active','x')`)
	m.Exec(ctx, db.PermMemory, `INSERT INTO memory_plans(text,tags,status,created,expires_at) VALUES('PLAN-X','{}','active','x','2099-01-01')`)
	m.Exec(ctx, db.PermAgg, `INSERT INTO agg(symbol,session,setup,rsi_bucket,trips,wins,losses,updated_at) VALUES('UGL.US','Pre','event','na',4,1,3,'x')`)

	ev := h.l.Payload(ctx, []Catalyst{{Event: pj.O("id", int64(1), "symbol", "GLD.US")}})
	tech := technical.New(h.l.D)
	tp, _, err := tech.Payload(ctx, session.Overnight)
	if err != nil {
		t.Fatal(err)
	}
	er, _ := ev.Get("recall")
	tr, _ := tp.Get("recall")
	eb, _ := pj.Dumps(er)
	tb, _ := pj.Dumps(tr)
	if !strings.Contains(eb, "precious-metal pulses mean-revert") || !strings.Contains(eb, `"source": "event", "n": 4`) || !strings.Contains(eb, `"self_assessment"`) {
		t.Fatalf("event recall lacks shared memory: %s", eb)
	}
	if strings.Contains(eb, "PLAN-X") || strings.Contains(eb, `"plans"`) || !strings.Contains(tb, "PLAN-X") {
		t.Fatalf("plans: event %s / technical %s", eb, tb)
	}
	// Everything but plans is the same block, byte for byte.
	var em, tm map[string]json.RawMessage
	json.Unmarshal([]byte(eb), &em)
	json.Unmarshal([]byte(tb), &tm)
	delete(tm, "plans")
	for k := range tm {
		if string(em[k]) != string(tm[k]) {
			t.Errorf("recall.%s differs: event %s / technical %s", k, em[k], tm[k])
		}
	}
	if len(em) != len(tm) {
		t.Errorf("recall keys differ: %d vs %d", len(em), len(tm))
	}
}

func TestHardLesson_WeekendSkip(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, time.Date(2026, 11, 8, 12, 0, 0, 0, session.ET)) // Sunday noon
	h.addEvent(t, "AAPL.US", "news_catalyst", "weekend news")
	h.l.Complete = func(context.Context, llm.Request) (string, error) {
		t.Fatal("model called outside trading sessions")
		return "", nil
	}
	lines, err := h.l.RunOnce(ctx)
	if err != nil || lines[0] != "session=Closed — 非交易时段,跳过" {
		t.Fatalf("lines = %v %v", lines, err)
	}
	// The cursor is untouched: the event waits for the open.
	if _, ok, _ := h.d.MustAs(db.RoleReader).KVGet(ctx, cursorKey); ok {
		t.Fatal("cursor advanced while closed")
	}
}

// A full round: volume_ratio is computed from the symbol's own 1h klines
// (never the model's), the intent binds to its catalyst, and the exact
// prompt is logged.
func TestHardLesson_EventVolumeRatioFromKlines(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, time.Date(2026, 11, 9, 7, 0, 12, 0, time.UTC))
	h.addEvent(t, "META.US", "news_catalyst", "other")
	eid := h.addEvent(t, "AAPL.US", "news_catalyst", "Apple earnings beat")
	os.WriteFile(h.reply, []byte(`{"action":"BUY","symbol":"AAPL.US","entry_hint":200.5,"stop":195,"confidence":66,"volume_ratio":9.9,"reasoning":"beat"}`), 0o644)
	lines, err := h.l.RunOnce(ctx)
	want := fmt.Sprintf("intent #1: long AAPL.US @ 200.5 stop 195.0 conf 66 (event #%d)", eid)
	if err != nil || len(lines) != 1 || lines[0] != want {
		t.Fatalf("lines = %v %v", lines, err)
	}
	raw, _ := os.ReadFile(filepath.Join(h.fix, "kline", "AAPL.US.json"))
	k, _ := indicators.ParseKlines(raw)
	wantVR := indicators.Analyze(k, "AAPL.US", 0).VolumeRatio
	var dedup, feats, src string
	h.d.MustAs(db.RoleReader).QueryRow(ctx, `SELECT source, dedup_key, features FROM intents WHERE id=1`).Scan(&src, &dedup, &feats)
	var f map[string]float64
	json.Unmarshal([]byte(feats), &f)
	if src != "event" || dedup != fmt.Sprintf("event:AAPL.US:%d", eid) || f["volume_ratio"] != wantVR || wantVR == 9.9 {
		t.Fatalf("intent: %s %s %s (want volume_ratio %v)", src, dedup, feats, wantVR)
	}

	files, _ := filepath.Glob(filepath.Join(h.logDir, "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("llm_calls files = %v", files)
	}
	fh, _ := os.Open(files[0])
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(nil, 1<<24)
	sc.Scan()
	var c map[string]any
	json.Unmarshal(sc.Bytes(), &c)
	user, _ := c["user_prompt"].(string)
	p, ok := pj.DecodeObj([]byte(user))
	if c["tag"] != "producer:event" || !ok || len(p) != 4 || p[0].Key != "now_utc" || p[1].Key != "catalysts" || p[2].Key != "quotes" || p[3].Key != "recall" {
		t.Fatalf("logged call: tag=%v prompt=%.200s", c["tag"], user)
	}
	if !strings.Contains(user, `"now_utc": "2026-11-09T07:00:12Z"`) || !strings.Contains(user, "<b> & co") {
		t.Fatalf("prompt: %.400s", user)
	}
	// Same catalyst again → dedup; nothing new → no model call.
	lines, _ = h.l.RunOnce(ctx)
	if lines[0] != "no new catalysts" {
		t.Fatalf("rerun = %v", lines)
	}
}

// A backlog larger than one batch drains over successive rounds, oldest
// first, with every event seen exactly once (Python jumped the cursor past
// everything older than the newest eight).
func TestGather_BacklogNotDropped(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, time.Now())
	s := h.d.MustAs(db.RoleEvent)
	var want []int64
	for i := 0; i < 20; i++ {
		want = append(want, h.addEvent(t, "AAPL.US", "news", fmt.Sprintf("e%d", i)))
	}
	seen := map[int64]int{}
	var rounds []int
	for {
		cats, err := Gather(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		if len(cats) == 0 {
			break
		}
		rounds = append(rounds, len(cats))
		for i, c := range cats {
			seen[idOf(c.Event)]++
			if i > 0 && idOf(c.Event) > idOf(cats[i-1].Event) {
				t.Fatal("a batch must be shown newest first")
			}
		}
	}
	if fmt.Sprint(rounds) != "[8 8 4]" {
		t.Fatalf("rounds %v", rounds)
	}
	for _, id := range want {
		if seen[id] != 1 {
			t.Fatalf("event %d seen %d times", id, seen[id])
		}
	}
}

// Catalyst symbols get their closed-trade record, keyed by symbol.
func TestPayloadSimilarHistoryPerCatalyst(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, time.Now())
	ex := h.d.MustAs(db.RoleMigrate)
	for _, tr := range []db.TradeInput{
		{Symbol: "AAPL.US", Action: "SELL", Qty: 1, FillPrice: 10, PnL: db.Ptr(5.0), Attribution: db.Ptr("strategy")},
		{Symbol: "AAPL.US", Action: "SELL", Qty: 1, FillPrice: 10, PnL: db.Ptr(-2.0), Attribution: db.Ptr("strategy")},
	} {
		if _, err := ex.AppendTrade(ctx, tr); err != nil {
			t.Fatal(err)
		}
	}
	h.addEvent(t, "AAPL.US", "news", "a")
	h.addEvent(t, "META.US", "news", "never traded")
	h.addEvent(t, "AAPL.US", "earnings", "b")
	cats, _ := Gather(ctx, h.d.MustAs(db.RoleEvent))
	p := h.l.Payload(ctx, cats)
	v, ok := p.Get("similar_history")
	if !ok {
		t.Fatal("no similar_history")
	}
	hist := v.(pj.Obj)
	if len(hist) != 1 || hist[0].Key != "AAPL.US" {
		t.Fatalf("keys: %v", hist)
	}
	b, _ := pj.Dumps(p)
	if !strings.Contains(b, `"summary": {"n": 2, "wins": 1`) && !strings.Contains(b, `"summary":{"n":2,"wins":1`) {
		t.Fatalf("summary missing: %.300s", b[strings.Index(b, "similar_history"):])
	}
}
