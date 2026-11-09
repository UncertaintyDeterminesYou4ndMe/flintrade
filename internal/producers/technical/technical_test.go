package technical

import (
	"bufio"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flintrade/internal/config"
	"flintrade/internal/db"
	"flintrade/internal/lb"
	"flintrade/internal/llm"
	"flintrade/internal/loop"
	"flintrade/internal/producers/pj"
	"flintrade/internal/session"
	"flintrade/internal/testutil"
)

// Python's parse_intent → to_intent → attach_gate_evidence over the same
// inputs (scratchpad gen_c_goldens.py). Prices Python passed through raw are
// compared numerically; confidence is an int here (rounded half-even).
func TestGolden_Intent(t *testing.T) {
	raw, err := os.ReadFile("testdata/intent_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Indicators []json.RawMessage `json:"indicators"`
		Cases      []struct {
			Text   string          `json:"text"`
			Parsed bool            `json:"parsed"`
			Intent json.RawMessage `json:"intent"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	for i, c := range g.Cases {
		d := ParseIntent(c.Text)
		if (d != nil) != c.Parsed {
			t.Errorf("case %d parsed=%v want %v", i, d != nil, c.Parsed)
			continue
		}
		var in *Intent
		if d != nil {
			in = ToIntent(d)
		}
		if string(c.Intent) == "null" {
			if in != nil {
				t.Errorf("case %d: got %+v, want nil", i, in)
			}
			continue
		}
		if in == nil {
			t.Errorf("case %d: got nil, want %s", i, c.Intent)
			continue
		}
		AttachGateEvidence(in, g.Indicators)
		want := decodeNum(string(c.Intent))
		fnum := func(v any) *float64 {
			if s, ok := v.(string); ok {
				v = json.Number(s)
			}
			if n, ok := v.(json.Number); ok {
				f, _ := n.Float64()
				return &f
			}
			return nil
		}
		eq := func(a, b *float64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
		if in.Symbol != want["symbol"] || in.Side != want["side"] || in.Reason != str(want["reason"]) ||
			!eq(in.EntryHint, fnum(want["entry_hint"])) || !eq(in.Stop, fnum(want["stop"])) || !eq(in.Target, fnum(want["target"])) {
			t.Errorf("case %d: got %+v, want %s", i, in, c.Intent)
		}
		if wc := fnum(want["confidence"]); (wc == nil) != (in.Confidence == nil) || (wc != nil && *in.Confidence != int(math.RoundToEven(*wc))) {
			t.Errorf("case %d confidence: got %v want %v", i, in.Confidence, wc)
		}
		gotF, _ := pj.Dumps(in.Features)
		if len(in.Features) == 0 {
			gotF = "null"
		}
		wo, _ := pj.DecodeObj(c.Intent)
		wf, _ := wo.Get("features")
		wantF, _ := pj.Dumps(wf)
		if gotF != wantF {
			t.Errorf("case %d features: got %s want %s", i, gotF, wantF)
		}
	}
}

// Hard lesson "机械闸门的判据不能由被它约束的一方提供": volume_ratio and h4
// come from the indicator snapshot, never from the model's own claim.
func TestHardLesson_GateEvidenceFromIndicators(t *testing.T) {
	d := ParseIntent(`{"action":"BUY","symbol":"AAA.US","volume_ratio":9.9,"h4_confluence":true,"stop":1}`)
	in := ToIntent(d)
	if _, ok := in.Features.Get("volume_ratio"); ok {
		t.Fatal("to_intent copied the model's volume_ratio")
	}
	AttachGateEvidence(in, []json.RawMessage{json.RawMessage(`{"symbol":"AAA.US","volume_ratio":0.42,"h4":{"confluence":false,"trend_ok":1}}`)})
	if b, _ := pj.Dumps(in.Features); b != `{"volume_ratio": 0.42, "h4_confluence": false, "h4_trend_ok": true}` {
		t.Fatalf("features = %s", b)
	}
	// No indicator block for the symbol → no evidence → the gate fails closed.
	in = ToIntent(d)
	AttachGateEvidence(in, []json.RawMessage{json.RawMessage(`{"symbol":"BBB.US","volume_ratio":3}`)})
	if len(in.Features) != 0 {
		t.Fatalf("features = %v", in.Features)
	}
}

// ── loop harness ────────────────────────────────────────────────────────

type harness struct {
	l      *Loop
	d      *db.DB
	logDir string
	reply  string // file the fake claude prints
}

// newHarness wires the loop to the collect package's fake longbridge and
// fixtures, a fake `claude` CLI on PATH behind the real llm.Client (so every
// call goes through the real logging path), and a temp DB.
func newHarness(t *testing.T, now time.Time) *harness {
	t.Helper()
	root, _ := filepath.Abs("../../..")
	cdata := filepath.Join(root, "internal", "collect", "testdata")
	t.Setenv("FAKE_LB_DIR", filepath.Join(cdata, "fixtures"))
	t.Setenv("FLINTRADE_LLM_DRY", "")
	bin := t.TempDir()
	h := &harness{reply: filepath.Join(bin, "reply.txt"), logDir: filepath.Join(t.TempDir(), "llm_calls")}
	os.WriteFile(h.reply, []byte(`{"action":"WAIT","reasoning":"default"}`), 0o644)
	os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/bash\ncat '"+h.reply+"'\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	cfg := config.Loader{Dir: filepath.Join(cdata, "config")}
	h.d = testutil.DB(t)
	testutil.SetEquity(t, h.d, 10000)
	clock := session.New(nil)
	clock.Now = func() time.Time { return now }
	deps := &loop.Deps{Root: root, DB: h.d, Config: cfg, LB: &lb.Runner{Bin: filepath.Join(cdata, "fake", "longbridge"), Retries: 1},
		Clock: clock, LLM: &llm.Client{Config: cfg, LogDir: h.logDir, Now: func() time.Time { return now }},
		Now: func() time.Time { return now }}
	h.l = New(deps)
	return h
}

func (h *harness) calls(t *testing.T) []map[string]any {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(h.logDir, "*.jsonl"))
	var out []map[string]any
	for _, f := range files {
		fh, _ := os.Open(f)
		sc := bufio.NewScanner(fh)
		sc.Buffer(nil, 1<<24)
		for sc.Scan() {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				t.Fatal(err)
			}
			out = append(out, m)
		}
		fh.Close()
	}
	return out
}

// Hard lesson "非交易时段不产出": Saturday noon is Closed — no collect, no
// model call, no intent; the heartbeat still lands.
func TestHardLesson_WeekendSkip(t *testing.T) {
	sat := time.Date(2026, 11, 7, 12, 0, 0, 0, session.ET)
	h := newHarness(t, sat)
	h.l.Complete = func(context.Context, llm.Request) (string, error) {
		t.Fatal("model called outside trading sessions")
		return "", nil
	}
	h.l.D.LB = &lb.Runner{Bin: "/nonexistent"}
	lines, err := h.l.RunOnce(context.Background())
	if err != nil || len(lines) != 1 || lines[0] != "session=Closed — 非交易时段,跳过" {
		t.Fatalf("lines = %v %v", lines, err)
	}
	var beats, intents int
	r := h.d.MustAs(db.RoleReader)
	r.QueryRow(context.Background(), `SELECT COUNT(*) FROM heartbeats WHERE process='loop_technical'`).Scan(&beats)
	r.QueryRow(context.Background(), `SELECT COUNT(*) FROM intents`).Scan(&intents)
	if beats != 1 || intents != 0 {
		t.Fatalf("beats=%d intents=%d", beats, intents)
	}
}

// One full round: payload in Python key order with recall + similar_history,
// the model's volume_ratio ignored in favour of the indicator's, and — hard
// lesson "模型可见即留痕" — the exact prompt the model saw is in llm_calls.
func TestHardLesson_ModelVisibleIsLogged(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 9, 7, 0, 12, 0, time.UTC) // Monday 02:00 ET, Overnight
	h := newHarness(t, now)
	sess := h.l.D.Clock.Current(ctx)
	if sess != session.Overnight {
		t.Fatalf("session = %s", sess)
	}
	_, inds, err := h.l.Payload(ctx, sess)
	if err != nil {
		t.Fatal(err)
	}
	best, ok := bestCandidate(inds)
	if !ok {
		t.Fatal("fixtures produced no indicators")
	}
	var wantVR any
	for _, raw := range inds {
		m := decodeNum(string(raw))
		if str(m["symbol"]) == best {
			wantVR = pj.PyNum(m["volume_ratio"].(json.Number))
		}
	}
	m := h.d.MustAs(db.RoleMigrate)
	m.Exec(ctx, db.PermTrades, `INSERT INTO trades(ts,symbol,action,qty,fill_price,commission,pnl,source,reason,attribution)
		VALUES('2026-11-06T07:00:00Z',?,'SELL',1,1,0,12.5,'technical','target hit','strategy')`, best)
	m.Exec(ctx, db.PermMemory, `INSERT INTO memory_lessons(text,evidence,confidence,status,created) VALUES('LESSON-X','[1]',0.8,'active','2026-11-08T00:00:00Z')`)
	m.Exec(ctx, db.PermMemory, `INSERT INTO memory_plans(text,tags,status,created,expires_at) VALUES('PLAN-X','{}','active','x','2099-01-01')`)

	os.WriteFile(h.reply, []byte("```json\n{\"action\":\"BUY\",\"symbol\":\""+best+"\",\"entry_hint\":100.5,\"stop\":98,\"target\":104,\"target2\":106.0,\"confidence\":70,\"volume_ratio\":9.9,\"reasoning\":\"r\"}\n```"), 0o644)
	lines, err := h.l.RunOnce(ctx)
	if err != nil || len(lines) != 1 || lines[0] != "intent #1: long "+best+" @ 100.5 stop 98.0 conf 70" {
		t.Fatalf("lines = %v %v", lines, err)
	}

	var src, dedup, feats string
	var prio int
	if err := h.d.MustAs(db.RoleReader).QueryRow(ctx, `SELECT source, priority, dedup_key, features FROM intents WHERE id=1`).Scan(&src, &prio, &dedup, &feats); err != nil {
		t.Fatal(err)
	}
	risk, _ := h.l.D.Config.Risk()
	if src != "technical" || prio != risk.Priority["technical"] || dedup != "technical:"+best+":202611090700" {
		t.Fatalf("intent row: %s %d %s", src, prio, dedup)
	}
	fo := decodeNum(feats)
	if pj.PyNum(fo["volume_ratio"].(json.Number)) != wantVR || pj.PyNum(fo["target2"].(json.Number)) != pj.F(106) {
		t.Fatalf("features = %s, want volume_ratio %v from indicators (not 9.9)", feats, wantVR)
	}

	calls := h.calls(t)
	if len(calls) != 1 {
		t.Fatalf("llm_calls lines = %d", len(calls))
	}
	c := calls[0]
	sys, _ := os.ReadFile(h.l.Prompt)
	if c["tag"] != "producer:technical" || c["tier"] != "trader" || c["system_prompt"] != string(sys) {
		t.Fatalf("log meta: tag=%v tier=%v", c["tag"], c["tier"])
	}
	user := c["user_prompt"].(string)
	payload, ok := pj.DecodeObj([]byte(user))
	if !ok {
		t.Fatal("logged prompt is not the JSON payload")
	}
	var keys []string
	for _, kv := range payload {
		keys = append(keys, kv.Key)
	}
	got := strings.Join(keys, ",")
	if !strings.HasSuffix(got, "indicators,playbooks,flintrade_positions,flintrade_risk,recall,similar_history") &&
		!strings.HasSuffix(got, "collect_errors,playbooks,flintrade_positions,flintrade_risk,recall,similar_history") {
		t.Fatalf("payload keys = %s", got)
	}
	for _, want := range []string{`"text": "LESSON-X"`, `"text": "PLAN-X"`, `"symbol": "` + best + `", "summary": {"n": 1`, `"flintrade_risk": {"equity": 10000.0, "open_risk": 0.0, "halt": false}`} {
		if !strings.Contains(user, want) {
			t.Errorf("logged prompt lacks %s", want)
		}
	}
	// Python's json.dumps(ensure_ascii=False) prints <, > and & as is (the
	// fixture account note is "模拟账户 <paper> & test").
	if esc := `\` + "u00"; strings.Contains(user, esc) || !strings.Contains(user, "<paper> & test") {
		t.Error("payload carries Go's HTML escaping")
	}
	// A rerun in the same minute dedups.
	lines, _ = h.l.RunOnce(ctx)
	if lines[0] != "重复 intent 已去重("+best+")" {
		t.Fatalf("rerun = %v", lines)
	}
}

// A model reply that is unparsable or WAIT queues nothing.
func TestRunOnce_WaitAndGarbage(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, time.Date(2026, 11, 9, 7, 0, 12, 0, time.UTC))
	lines, err := h.l.RunOnce(ctx)
	if err != nil || lines[0] != "WAIT — default" {
		t.Fatalf("wait: %v %v", lines, err)
	}
	os.WriteFile(h.reply, []byte("I think we should buy"), 0o644)
	lines, _ = h.l.RunOnce(ctx)
	if lines[0] != `无法解析 LLM 输出(前80字: "I think we should buy")` {
		t.Fatalf("garbage: %v", lines)
	}
	var n int
	h.d.MustAs(db.RoleReader).QueryRow(ctx, `SELECT COUNT(*) FROM intents`).Scan(&n)
	if n != 0 {
		t.Fatalf("intents = %d", n)
	}
}
