package reflect

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"flintrade/internal/db"
	"flintrade/internal/llm"
	"flintrade/internal/loop"
	"flintrade/internal/memory"
	"flintrade/internal/producers/pj"
	"flintrade/internal/session"
	"flintrade/internal/testutil"
)

func seed(t *testing.T, s *db.Store, perm db.Perm, q string, args ...any) int64 {
	t.Helper()
	r, err := s.Exec(context.Background(), perm, q, args...)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := r.LastInsertId()
	return id
}

func cannedLoop(d *db.DB, now time.Time, reply string, calls *int) *Loop {
	return &Loop{
		D:      &loop.Deps{DB: d, Now: func() time.Time { return now }},
		LogDir: "",
		Complete: func(_ context.Context, req llm.Request) (string, error) {
			*calls++
			if req.Tag == "postmortem" {
				return "no json", nil
			}
			if req.Tier != "flash" || req.Tag != "reflect:dream" {
				panic("dream must use tier flash, tag reflect:dream")
			}
			return reply, nil
		},
	}
}

func objsOf(t *testing.T, raw json.RawMessage) []pj.Obj {
	t.Helper()
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatal(err)
	}
	out := []pj.Obj{}
	for _, it := range items {
		o, ok := pj.DecodeObj(it)
		if !ok {
			t.Fatalf("not an object: %s", it)
		}
		out = append(out, o)
	}
	return out
}

// Pure helpers and the dream prompt against Python (scratchpad
// gen_c_goldens.py over agent/reflect.py). The prompt is model-visible, so
// it must match byte for byte.
func TestGolden_Pure(t *testing.T) {
	raw, err := os.ReadFile("testdata/pure_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Sessions []struct{ TS, Session string } `json:"sessions"`
		Gaps     []struct {
			Secs int64  `json:"secs"`
			Gap  string `json:"gap"`
		} `json:"gaps"`
		RSI []struct {
			In  *string `json:"in"`
			Out string  `json:"out"`
		} `json:"rsi"`
		Prompts []struct {
			Agg     json.RawMessage `json:"agg"`
			Trades  json.RawMessage `json:"trades"`
			Today   string          `json:"today"`
			Reviews json.RawMessage `json:"reviews"`
			Blocked json.RawMessage `json:"blocked"`
			Prompt  string          `json:"prompt"`
		} `json:"dream_prompts"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	for _, c := range g.Sessions {
		if got := memory.SessionOf(c.TS); got != c.Session {
			t.Errorf("SessionOf(%q) = %s, want %s", c.TS, got, c.Session)
		}
	}
	ref := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	for _, c := range g.Gaps {
		then := ref.Add(-time.Duration(c.Secs) * time.Second)
		if got := HumanGap(&then, ref); got != c.Gap {
			t.Errorf("HumanGap(%ds) = %s, want %s", c.Secs, got, c.Gap)
		}
	}
	for _, c := range g.RSI {
		if got := rsiBucket(c.In); got != c.Out {
			t.Errorf("rsiBucket(%v) = %s, want %s", c.In, got, c.Out)
		}
	}
	for i, p := range g.Prompts {
		got := BuildDreamPrompt(objsOf(t, p.Agg), objsOf(t, p.Trades), p.Today, objsOf(t, p.Reviews), objsOf(t, p.Blocked))
		if got != p.Prompt {
			t.Errorf("dream prompt %d differs:\n--- go\n%s\n--- py\n%s", i, got, p.Prompt)
		}
	}
}

// test_p2 §7: self_assessment counts strategy trades only, and says so.
func TestRecall_SelfAssessment(t *testing.T) {
	ctx := context.Background()
	d := testutil.DB(t)
	m := d.MustAs(db.RoleMigrate)
	for _, r := range []struct {
		pnl  float64
		attr any
	}{{29.6, "strategy"}, {-112.48, "strategy"}, {5.5, nil}, {100, "manual"}, {7, "test"}, {9, "outage-degraded"}} {
		seed(t, m, db.PermTrades, `INSERT INTO trades(ts,symbol,action,qty,fill_price,commission,pnl,source,attribution)
			VALUES('2026-07-10T15:00:00Z','X.US','SELL',1,1,0,?,'technical',?)`, r.pnl, r.attr)
	}
	rec, err := GetRecall(ctx, d.MustAs(db.RoleReader), time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	sa := rec.SelfAssessment
	if sa.N != 3 || *sa.WinRate != 0.667 || sa.NetPnL != pj.F(-77.38) || sa.ConfidenceBuckets != nil {
		t.Fatalf("self_assessment = %+v", sa)
	}
	want := "3-trade sample, win 66.7%, net -77.38 — no proven edge yet; treat your own confidence as uncalibrated"
	if sa.Note != want {
		t.Fatalf("note = %q", sa.Note)
	}
	// Empty ledger: Python printed round(sum([]), 2) as the int 0.
	rec, _ = GetRecall(ctx, testutil.DB(t).MustAs(db.RoleReader), time.Now())
	if rec.SelfAssessment.Note != "0-trade sample, win 0.0%, net 0 — no proven edge yet; treat your own confidence as uncalibrated" {
		t.Fatalf("empty note = %q", rec.SelfAssessment.Note)
	}
}

// test_p2 §8: HARD RULES present, non-strategy trades never reach the dream.
func TestDreamPrompt_Honesty(t *testing.T) {
	ctx := context.Background()
	d := testutil.DB(t)
	m := d.MustAs(db.RoleMigrate)
	seed(t, m, db.PermTrades, `INSERT INTO trades(ts,symbol,action,qty,fill_price,commission,pnl,source,reason,attribution)
		VALUES('2026-07-10T15:00:00Z','OUT.US','SELL',10,10.0,0.1,5.0,'technical','OUTAGE-REASON-XYZ','outage-degraded')`)
	seed(t, m, db.PermTrades, `INSERT INTO trades(ts,symbol,action,qty,fill_price,commission,pnl,source,reason,attribution)
		VALUES('2026-07-10T15:00:00Z','MAN.US','SELL',10,10.0,0.1,5.0,'user','manual close','manual')`)
	tid := seed(t, m, db.PermTrades, `INSERT INTO trades(ts,symbol,action,qty,fill_price,commission,pnl,source,reason,attribution)
		VALUES('2026-07-10T17:00:00Z','ABC.US','SELL',35,203.0,0.7,-112.48,'technical','Stop breach - thesis invalidated','strategy')`)
	seed(t, m, db.PermMemory, `INSERT INTO trade_reviews(trade_id,symbol,thesis_verdict,lesson,reviewed_at) VALUES(?,'ABC.US','wrong','L1','x')`, tid)
	s := d.MustAs(db.RoleReflect)
	trades, _ := recentClosed(ctx, s)
	reviews, _ := recentReviews(ctx, s)
	p := BuildDreamPrompt(nil, trades, "2026-07-14", reviews, nil)
	for _, want := range []string{"HARD RULES", "Stop breach - thesis invalidated", "RECENT TRADE POSTMORTEMS", `"lesson": "L1"`} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	for _, bad := range []string{"OUTAGE-REASON-XYZ", "manual close", "RECENTLY BLOCKED"} {
		if strings.Contains(p, bad) {
			t.Errorf("prompt contains %q", bad)
		}
	}
}

// test_p2 §9: a blocked intent reaches the dream, the dream's plan is stored,
// and recall surfaces it until it expires.
func TestPlansWatchlist(t *testing.T) {
	ctx := context.Background()
	d := testutil.DB(t)
	now := time.Date(2026, 7, 14, 3, 0, 0, 0, time.UTC)
	seed(t, d.MustAs(db.RoleMigrate), db.PermIntentsSubmit, `INSERT INTO intents(source,priority,symbol,side,entry_hint,stop,confidence,reason,status,reject_reason,created_at)
		VALUES('technical',10,'NVDA.US','short',220.0,224.0,66,'BLOCKED-THESIS-MARKER','rejected','no-revenge 冷却中(NVDA.US 还剩 ~41min)',?)`,
		db.FormatTime(now.Add(-time.Hour)))
	s := d.MustAs(db.RoleReflect)
	blocked, err := blockedIntents(ctx, s, now)
	if err != nil || len(blocked) != 1 {
		t.Fatalf("blocked = %v %v", blocked, err)
	}
	p := BuildDreamPrompt(nil, nil, "2026-07-14", nil, blocked)
	if !strings.Contains(p, "RECENTLY BLOCKED INTENTS") || !strings.Contains(p, "BLOCKED-THESIS-MARKER") || !strings.Contains(p, "WATCHLIST") {
		t.Fatal("blocked intents missing from dream prompt")
	}
	if b, _ := blockedIntents(ctx, s, now.Add(4*24*time.Hour)); len(b) != 0 {
		t.Fatal("blocked window must be 3 days")
	}

	calls := 0
	reply := "```json\n{\"lessons\":[{\"text\":\"canned lesson\",\"confidence\":0.7,\"evidence\":[1],\"tags\":{}}]," +
		"\"plans\":[{\"text\":\"re-check NVDA short below 220 - cooldown-blocked\",\"expires_at\":\"2099-01-01\",\"tags\":{\"symbol\":\"NVDA.US\",\"a\":1}}]}\n```"
	l := cannedLoop(d, now, reply, &calls)
	l.LogDir = t.TempDir()
	res, err := l.Synthesize(ctx, s, now)
	if err != nil || !res.Parsed || res.Lessons != 1 || res.Plans != 1 {
		t.Fatalf("synthesize = %+v %v", res, err)
	}
	var exp, tags string
	if err := s.QueryRow(ctx, `SELECT expires_at, tags FROM memory_plans WHERE status='active'`).Scan(&exp, &tags); err != nil {
		t.Fatal(err)
	}
	if exp != "2099-01-01" || tags != `{"symbol": "NVDA.US", "a": 1}` {
		t.Fatalf("plan row: %s %s", exp, tags)
	}
	rec, _ := GetRecall(ctx, s, now)
	if rec.Plans == nil || len(*rec.Plans) != 1 || !strings.Contains((*rec.Plans)[0].Text, "NVDA") {
		t.Fatalf("recall plans = %+v", rec.Plans)
	}
	if _, err := os.Stat(filepath.Join(l.LogDir, "dream-2026-07-13.md")); err != nil {
		t.Fatalf("digest not written: %v", err)
	}
	// Re-dreaming the same plan updates it in place (no duplicate).
	if _, err := l.Synthesize(ctx, s, now); err != nil {
		t.Fatal(err)
	}
	var n int
	s.QueryRow(ctx, `SELECT COUNT(*) FROM memory_plans`).Scan(&n)
	if n != 1 {
		t.Fatalf("plans = %d, want 1 (upsert by text)", n)
	}
}

// A dream with no valid lesson keeps the active set; a valid one replaces it
// atomically (bounded rewrite, not append).
func TestSynthesize_Rewrite(t *testing.T) {
	ctx := context.Background()
	d := testutil.DB(t)
	s := d.MustAs(db.RoleReflect)
	seed(t, s, db.PermMemory, `INSERT INTO memory_lessons(text,evidence,confidence,status,created) VALUES('old',?,0.6,'active','2026-07-01T00:00:00Z')`, "[1, 2]")
	now := time.Date(2026, 7, 14, 3, 0, 0, 0, time.UTC)
	calls := 0
	for _, reply := range []string{"garbage", "```json\n{\"lessons\":[{\"text\":\"  \"}],\"plans\":[]}\n```"} {
		res, err := cannedLoop(d, now, reply, &calls).Synthesize(ctx, s, now)
		if err != nil || res.Lessons != 0 {
			t.Fatalf("%q: %+v %v", reply, res, err)
		}
	}
	rec, _ := GetRecall(ctx, s, now)
	if len(rec.Lessons) != 1 || rec.Lessons[0].Text != "old" || rec.Lessons[0].N != 2 {
		t.Fatalf("old set not kept: %+v", rec.Lessons)
	}
	var many []string
	for i := 0; i < 25; i++ {
		many = append(many, `{"text":"L`+string(rune('a'+i))+`","confidence":"0.9","evidence":[1]}`)
	}
	reply := "```json\n{\"lessons\":[" + strings.Join(many, ",") + "]}\n```"
	res, err := cannedLoop(d, now, reply, &calls).Synthesize(ctx, s, now)
	if err != nil || res.Lessons != MaxLessons {
		t.Fatalf("res = %+v %v", res, err)
	}
	var active, archived int
	s.QueryRow(ctx, `SELECT COUNT(*) FROM memory_lessons WHERE status='active'`).Scan(&active)
	s.QueryRow(ctx, `SELECT COUNT(*) FROM memory_lessons WHERE status='archived'`).Scan(&archived)
	if active != MaxLessons || archived != 1 {
		t.Fatalf("active=%d archived=%d", active, archived)
	}
}

func TestDecay(t *testing.T) {
	ctx := context.Background()
	d := testutil.DB(t)
	s := d.MustAs(db.RoleReflect)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	a := seed(t, s, db.PermMemory, `INSERT INTO memory_lessons(text,confidence,status,created) VALUES('a',0.5,'active','2026-07-05T11:00:00Z')`)
	b := seed(t, s, db.PermMemory, `INSERT INTO memory_lessons(text,confidence,status,created,last_confirmed) VALUES('b',0.21,'active','2026-01-01T00:00:00Z','2026-07-12T00:00:00Z')`)
	seed(t, s, db.PermMemory, `INSERT INTO memory_lessons(text,confidence,status,created) VALUES('c',0.9,'active','2026-07-19T00:00:00Z')`)
	seed(t, s, db.PermMemory, `INSERT INTO memory_plans(text,status,created,expires_at) VALUES('old','active','x','2026-07-19')`)
	seed(t, s, db.PermMemory, `INSERT INTO memory_plans(text,status,created,expires_at) VALUES('today','active','x','2026-07-20')`)
	r, err := Decay(ctx, s, now)
	if err != nil || r != (DecayResult{Decayed: 1, Archived: 1, Expired: 1}) {
		t.Fatalf("decay = %+v %v", r, err)
	}
	var c float64
	var st string
	s.QueryRow(ctx, `SELECT confidence FROM memory_lessons WHERE id=?`, a).Scan(&c)
	if c != 0.405 { // two stale weeks: 0.5 × 0.9²
		t.Fatalf("a = %v", c)
	}
	s.QueryRow(ctx, `SELECT confidence, status FROM memory_lessons WHERE id=?`, b).Scan(&c, &st)
	if st != "archived" || c != 0.189 {
		t.Fatalf("b = %v %s", c, st)
	}
}

// Hard lesson "定时任务不要绑在碰巧存在的标签上" + test_p0 FIX 8: the sleep
// window is computed from the ET clock, never from a session label. Every
// minute of a week: weekdays 04:00–20:00 ET never sleep, everything else
// does, every ET date has a window, and the 03:50–04:00 Overnight-Pre dead
// zone counts as sleep.
func TestHardLesson_DreamWindowNotLabelBased(t *testing.T) {
	start := time.Date(2026, 11, 9, 0, 0, 0, 0, session.ET) // Monday
	perDay := map[string]int{}
	for i := 0; i < 7*24*60; i++ {
		tm := start.Add(time.Duration(i) * time.Minute)
		e := tm.In(session.ET)
		weekend := e.Weekday() == time.Saturday || e.Weekday() == time.Sunday
		day := !weekend && e.Hour() >= 4 && e.Hour() < 20
		if Sleeping(tm) == day {
			t.Fatalf("%s: Sleeping=%v", e, Sleeping(tm))
		}
		if Sleeping(tm) {
			perDay[e.Format(time.DateOnly)]++
		}
	}
	for i := 0; i < 7; i++ {
		date := start.AddDate(0, 0, i).Format(time.DateOnly)
		if perDay[date] < 8*60 {
			t.Errorf("%s has only %d sleep minutes", date, perDay[date])
		}
	}
	// FIX 8: Tuesday 03:55 ET is Overnight-Pre — the label that once starved
	// the dream — and must be inside the window.
	if !Sleeping(time.Date(2026, 11, 10, 3, 55, 0, 0, session.ET)) {
		t.Fatal("Overnight-Pre must be sleep")
	}

	// The loop decides without any session clock: a nil Clock must not be
	// consulted (it would panic), whatever label upstream would report.
	ctx := context.Background()
	d := testutil.DB(t)
	calls := 0
	night := time.Date(2026, 11, 10, 3, 55, 0, 0, session.ET)
	l := cannedLoop(d, night, "```json\n{\"lessons\":[],\"plans\":[]}\n```", &calls)
	lines, err := l.RunOnce(ctx)
	if err != nil || len(lines) != 1 || !strings.HasPrefix(lines[0], "dreamed 2026-11-10:") {
		t.Fatalf("night run = %v %v", lines, err)
	}
	if lines, _ := l.RunOnce(ctx); lines[0] != "not sleeping / already dreamed today" {
		t.Fatalf("second run = %v", lines)
	}
	l.D.Now = func() time.Time { return time.Date(2026, 11, 10, 10, 0, 0, 0, session.ET) }
	l.D.DB.MustAs(db.RoleReflect).KVSet(ctx, "last_dream_date", "2026-11-09")
	if lines, _ := l.RunOnce(ctx); lines[0] != "not sleeping / already dreamed today" {
		t.Fatalf("daytime run = %v", lines)
	}
}

// reflect beats on every round, not only when it dreams (Python beat only
// inside the dream branch, so a healthy idle reflect looked dead all day).
func TestRunOnceBeatsEveryRound(t *testing.T) {
	ctx := context.Background()
	d := testutil.DB(t)
	calls := 0
	l := cannedLoop(d, time.Date(2026, 11, 10, 10, 0, 0, 0, session.ET), "", &calls)
	if _, err := l.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	d.MustAs(db.RoleReader).QueryRow(ctx, `SELECT COUNT(*) FROM heartbeats WHERE process='reflect'`).Scan(&n)
	if n != 1 || calls != 0 {
		t.Fatalf("heartbeats=%d calls=%d", n, calls)
	}
}

// Parity on a VACUUM INTO copy of the live flintrade.db (never the live file):
//
//	FLINTRADE_PARITY_DIR=<scratchpad> go test ./internal/reflect -run LiveCopy
//
// expects c_live.db + c_recall_py.json and c_go_agg.db + c_agg_py.json from
// scratchpad gen_c_live.py. Skipped otherwise.
func TestLiveCopyParity(t *testing.T) {
	dir := os.Getenv("FLINTRADE_PARITY_DIR")
	if dir == "" {
		t.Skip("FLINTRADE_PARITY_DIR not set")
	}
	ctx := context.Background()
	ro, err := db.OpenReadOnly(filepath.Join(dir, "c_live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	now := time.Date(2026, 11, 9, 10, 15, 30, 123456000, session.ET)
	rec, err := GetRecall(ctx, ro.MustAs(db.RoleReader), now)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := pj.Dumps(rec)
	want, _ := os.ReadFile(filepath.Join(dir, "c_recall_py.json"))
	if got != strings.TrimSpace(string(want)) {
		t.Errorf("recall differs:\n go %s\n py %s", got, want)
	}

	rw, err := db.Open(filepath.Join(dir, "c_go_agg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	n, err := Aggregate(ctx, rw.MustAs(db.RoleReflect))
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := rw.MustAs(db.RoleReader).Query(ctx, `SELECT symbol,session,setup,rsi_bucket,trips,wins,losses,pl_ratio FROM agg ORDER BY symbol,session,setup,rsi_bucket`)
	var gotRows [][]any
	for rows.Next() {
		var sym, sess, setup, rb string
		var trips, wins, losses int64
		var pl sql.NullFloat64
		rows.Scan(&sym, &sess, &setup, &rb, &trips, &wins, &losses, &pl)
		var plv any
		if pl.Valid {
			plv = hexFloat(pl.Float64)
		}
		gotRows = append(gotRows, []any{sym, sess, setup, rb, trips, wins, losses, plv})
	}
	rows.Close()
	gotJSON, _ := json.Marshal(map[string]any{"n": n, "rows": gotRows})
	var a, b any
	wantAgg, _ := os.ReadFile(filepath.Join(dir, "c_agg_py.json"))
	json.Unmarshal(gotJSON, &a)
	json.Unmarshal(wantAgg, &b)
	ga, _ := json.Marshal(a)
	gb, _ := json.Marshal(b)
	if !bytes.Equal(ga, gb) {
		t.Errorf("agg differs:\n go %s\n py %s", ga, gb)
	}
}

// hexFloat is Python's float.hex() for normal numbers.
func hexFloat(f float64) string {
	if f == 0 {
		return "0x0.0p+0"
	}
	s := strconv.FormatFloat(f, 'x', 13, 64) // 0x1.xxxxxxxxxxxxxp±dd
	i := strings.LastIndexByte(s, 'p')
	exp, _ := strconv.Atoi(s[i+1:])
	sign := "+"
	if exp < 0 {
		sign, exp = "-", -exp
	}
	return s[:i+1] + sign + strconv.Itoa(exp)
}

// The dream prompt over the same live copy (after Aggregate), against
// c_dream_py.txt. Python's blocked-intent cutoff reads the wall clock, so
// this one uses time.Now() too.
func TestLiveCopyDreamPrompt(t *testing.T) {
	dir := os.Getenv("FLINTRADE_PARITY_DIR")
	if dir == "" {
		t.Skip("FLINTRADE_PARITY_DIR not set")
	}
	ctx := context.Background()
	ro, err := db.OpenReadOnly(filepath.Join(dir, "c_go_agg.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	s := ro.MustAs(db.RoleReader)
	agg, _ := aggSummary(ctx, s)
	trades, _ := recentClosed(ctx, s)
	reviews, _ := recentReviews(ctx, s)
	blocked, _ := blockedIntents(ctx, s, time.Now())
	got := BuildDreamPrompt(agg, trades, "2026-11-09", reviews, blocked)
	want, _ := os.ReadFile(filepath.Join(dir, "c_dream_py.txt"))
	if got != string(want) {
		i := 0
		for i < len(got) && i < len(want) && got[i] == want[i] {
			i++
		}
		lo := max(0, i-150)
		t.Fatalf("dream prompt differs at byte %d (go %d / py %d bytes):\n go …%s\n py …%s", i, len(got), len(want),
			got[lo:min(len(got), i+150)], want[lo:min(len(want), i+150)])
	}
}
