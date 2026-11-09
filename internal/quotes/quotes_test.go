package quotes

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"flintrade/internal/lb"
	"flintrade/internal/session"
)

// Observed payload shape (2026-08-05 overnight NVDA): the top-level last is
// the previous regular close; the live price sits in the overnight sub-quote.
var realShape = Quote{
	"symbol": "NVDA.US", "last": "211.940", "prev_close": "206.640",
	"overnight":   map[string]any{"last": "217.140", "prev_close": "211.940"},
	"pre_market":  map[string]any{"last": "211.650", "prev_close": "206.640"},
	"post_market": map[string]any{"last": "216.500", "prev_close": "211.940"},
}

// Ported from test_stop_guard.py section A2 (AAPL 2026-07-31).
func TestHardLesson_SessionPriceNotStaleTopLevel(t *testing.T) {
	for _, c := range []struct {
		sess string
		want float64
	}{
		{session.Overnight, 217.14}, {session.OvernightPre, 217.14},
		{session.Pre, 211.65}, {session.Post, 216.50}, {session.Intraday, 211.94}, {session.Closed, 211.94},
	} {
		if got, ok := LastPrice(realShape, c.sess); !ok || got != c.want {
			t.Errorf("%s: %v %v, want %v", c.sess, got, ok, c.want)
		}
	}
	if got, _ := LastPrice(Quote{"last": "100.5"}, session.Overnight); got != 100.5 {
		t.Errorf("missing sub-quote must fall back to top level, got %v", got)
	}
	for _, q := range []Quote{{"last": "0"}, {}, {"last": "-1"}, {"last": "abc"}, {"last": true}} {
		if _, ok := LastPrice(q, session.Intraday); ok {
			t.Errorf("%v: zero/missing/garbage must read as no quote", q)
		}
	}
	if got, _ := LastPrice(Quote{"last_done": 42.0}, session.Intraday); got != 42 {
		t.Errorf("alias key last_done: %v", got)
	}
}

func fake(t *testing.T, body string) Source {
	dir := t.TempDir()
	bin := filepath.Join(dir, "longbridge")
	os.WriteFile(bin, []byte("#!/bin/bash\n"+body+"\n"), 0o755)
	return Source{LB: &lb.Runner{Bin: bin, Retries: 1}}
}

func TestQuotesShapes(t *testing.T) {
	ctx := context.Background()
	s := fake(t, `echo '[{"symbol":"A.US","last":"1.5"},{"symbol":"B.US","last":"2"},"junk"]'`)
	got := s.LastPrices(ctx, []string{"A.US", "B.US", "A.US", ""}, session.Intraday)
	if len(got) != 2 || got["A.US"] != 1.5 || got["B.US"] != 2 {
		t.Fatalf("list shape: %v", got)
	}
	s = fake(t, `echo '{"data":[{"code":"C.US","last_price":"3"}]}'`)
	if got := s.LastPrices(ctx, []string{"C.US"}, session.Intraday); got["C.US"] != 3 {
		t.Fatalf("wrapped shape: %v", got)
	}
	s = fake(t, `echo '{"last":"4"}'`)
	if got := s.LastPrices(ctx, []string{"D.US"}, session.Intraday); got["D.US"] != 4 {
		t.Fatalf("single row without symbol: %v", got)
	}
	s = fake(t, `echo boom >&2; exit 1`)
	if got := s.Quotes(ctx, []string{"E.US"}); len(got) != 0 {
		t.Fatalf("failure must be empty, got %v", got)
	}
	if got := s.Quotes(ctx, nil); len(got) != 0 {
		t.Fatal("no symbols → no call, empty map")
	}
}

// Shape check against the live CLI (read-only market data).
func TestLiveQuoteShape(t *testing.T) {
	if os.Getenv("LONGBRIDGE_APP_KEY") == "" {
		t.Skip("no Longbridge credentials in env")
	}
	got := Source{LB: lb.New()}.Quotes(context.Background(), []string{"NVDA.US", "QQQ.US"})
	if len(got) != 2 {
		t.Fatalf("live quotes: %d", len(got))
	}
	for sym, q := range got {
		if _, ok := LastPrice(q, session.Intraday); !ok {
			t.Errorf("%s: no top-level price in live shape", sym)
		}
	}
}
