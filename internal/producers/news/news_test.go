package news

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"flintrade/internal/db"
	"flintrade/internal/lb"
	"flintrade/internal/loop"
	"flintrade/internal/producers/pj"
	"flintrade/internal/testutil"
)

// Keyword matching, normalisation and dedup keys against news_collector.py
// (scratchpad gen_c_goldens.py).
func TestGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/news_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Keywords []struct {
			Title string   `json:"title"`
			Kws   []string `json:"kws"`
		} `json:"keywords"`
		Items []struct {
			In  json.RawMessage `json:"in"`
			Out json.RawMessage `json:"out"`
			Key *string         `json:"key"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	for _, c := range g.Keywords {
		if got := MatchedKeywords(c.Title); strings.Join(got, ",") != strings.Join(c.Kws, ",") {
			t.Errorf("MatchedKeywords(%q) = %v, want %v", c.Title, got, c.Kws)
		}
	}
	for _, c := range g.Items {
		in, _ := pj.DecodeObj(c.In)
		it := Normalize(in)
		got, _ := pj.Dumps(it.Obj)
		wo, _ := pj.DecodeObj(c.Out)
		want, _ := pj.Dumps(wo)
		if got != want {
			t.Errorf("Normalize(%s)\n got %s\nwant %s", c.In, got, want)
		}
		key, ok := it.Key()
		if ok != (c.Key != nil) || (ok && key != *c.Key) {
			t.Errorf("Key(%s) = %q,%v want %v", c.In, key, ok, c.Key)
		}
	}
}

type harness struct {
	l      *Loop
	d      *db.DB
	dir    string // FAKE_NEWS_DIR
	sleeps int
}

func newHarness(t *testing.T, symbols ...string) *harness {
	t.Helper()
	h := &harness{dir: t.TempDir()}
	t.Setenv("FAKE_NEWS_DIR", h.dir)
	bin, _ := filepath.Abs("testdata/fake/longbridge")
	quoted := make([]string, len(symbols))
	for i, s := range symbols {
		quoted[i] = `"` + s + `"`
	}
	trading := regexp.MustCompile(`(?s)(\[universe\]\s*symbols\s*=\s*)\[.*?\]`).
		ReplaceAllString(testutil.ConfigFile(t, "trading.toml"), "${1}["+strings.Join(quoted, ",")+"]")
	h.d = testutil.DB(t)
	h.l = New(&loop.Deps{DB: h.d, Config: testutil.Config(t, map[string]string{"trading.toml": trading}),
		LB: &lb.Runner{Bin: bin, Retries: 1}})
	h.l.Sleep = func(context.Context, time.Duration) { h.sleeps++ }
	return h
}

func (h *harness) serve(t *testing.T, sym, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.dir, sym+".json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, d *db.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := d.MustAs(db.RoleReader).QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRunOnce(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, "AAA.US", "BAD.US", "CCC.US", "DDD.US")
	h.serve(t, "AAA.US", `[{"id":1,"title":"AAA Q3 earnings beat","published_at":"2026-11-09T12:00:00Z","url":"u"},
		{"id":2,"title":"AAA opens a store"},{"title":"","foo":1}]`)
	h.serve(t, "CCC.US", `{"items":[{"news_id":"c1","headline":"CCC downgrade <x> & y"}]}`)
	h.serve(t, "DDD.US", `{"id":"d1","title":"single object"}`)
	lines, err := h.l.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := "AAA.US: +2 news (1 catalyst)|CCC.US: +1 news (1 catalyst)|DDD.US: +1 news"
	if strings.Join(lines, "|") != want || h.sleeps != 3 {
		t.Fatalf("lines = %v sleeps=%d", lines, h.sleeps)
	}
	if n := count(t, h.d, `SELECT COUNT(*) FROM signals WHERE source='news_collector' AND kind='news'`); n != 4 {
		t.Fatalf("signals = %d", n)
	}
	var title, fires, payload string
	d := h.d.MustAs(db.RoleReader)
	d.QueryRow(ctx, `SELECT title, fires_at, payload FROM events WHERE symbol='AAA.US' AND kind='news_catalyst'`).Scan(&title, &fires, &payload)
	if title != "AAA Q3 earnings beat" || fires != "2026-11-09T12:00:00Z" ||
		!strings.Contains(payload, `"_id":"1","_title":"AAA Q3 earnings beat","_published_at":"2026-11-09T12:00:00Z","_url":"u","catalyst_keywords":["earnings"]`) {
		t.Fatalf("event: %s %s %s", title, fires, payload)
	}
	d.QueryRow(ctx, `SELECT payload FROM events WHERE symbol='CCC.US'`).Scan(&payload)
	if !strings.Contains(payload, `"catalyst_keywords":["downgrade"]`) || !strings.Contains(payload, "<x> & y") {
		t.Fatalf("CCC payload: %s", payload)
	}
	if v, _, _ := d.KVGet(ctx, "news_seen_AAA.US"); v != `["1", "2"]` {
		t.Fatalf("seen = %s", v)
	}

	// Second round: nothing new; the failing symbol stays isolated.
	lines, _ = h.l.RunOnce(ctx)
	if strings.Join(lines, "|") != "no new news" {
		t.Fatalf("rerun = %v", lines)
	}
}

func TestSeenCap(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, "AAA.US")
	var old []string
	for i := 0; i < seenCap; i++ {
		old = append(old, fmt.Sprint("old", i))
	}
	b, _ := json.Marshal(old)
	h.d.MustAs(db.RoleNews).KVSet(ctx, "news_seen_AAA.US", string(b))
	h.serve(t, "AAA.US", `[{"id":"new1","title":"t"},{"id":"old7","title":"seen"}]`)
	n, c, err := h.l.CollectSymbol(ctx, h.d.MustAs(db.RoleNews), "AAA.US")
	if err != nil || n != 1 || c != 0 {
		t.Fatalf("n=%d c=%d err=%v", n, c, err)
	}
	v, _, _ := h.d.MustAs(db.RoleReader).KVGet(ctx, "news_seen_AAA.US")
	var seen []string
	json.Unmarshal([]byte(v), &seen)
	if len(seen) != seenCap || seen[0] != "old1" || seen[seenCap-1] != "new1" {
		t.Fatalf("seen len=%d first=%s last=%s", len(seen), seen[0], seen[len(seen)-1])
	}
}
