package broker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flintrade/internal/lb"
)

func fake(t *testing.T, body string) *CLI {
	dir := t.TempDir()
	bin := filepath.Join(dir, "longbridge")
	os.WriteFile(bin, []byte("#!/bin/bash\necho \"$*\" >> "+dir+"/argv\n"+body+"\n"), 0o755)
	return &CLI{LB: &lb.Runner{Bin: bin, Retries: 1}}
}

func TestLiveShapes(t *testing.T) {
	ctx := context.Background()
	b := fake(t, `case "$1 $2" in
  "order buy") echo 'Submitting Buy order: NVDA.US'; echo '{"order_id":"1234"}' ;;
  "order sell") echo 'rejected: insufficient position' >&2; exit 1 ;;
  "order detail") echo '{"status":"Filled","executed_price":"10.5"}' ;;
  "order executions") echo '[{"order_id":"1"},{"order_id":"2"},"x"]' ;;
  "order cancel") echo 'order already filled' >&2; exit 1 ;;
  positions*) echo 'timeout' >&2; exit 1 ;;
esac`)
	r := b.Buy(ctx, "NVDA.US", 6, 200.92, "ANY_TIME", "c1")
	if !r.OK || r.BrokerOrderID != "1234" || r.Status != "Submitted" {
		t.Fatalf("buy: %+v", r)
	}
	argv, _ := os.ReadFile(filepath.Join(filepath.Dir(b.LB.Bin), "argv"))
	if !strings.Contains(string(argv), "order buy NVDA.US 6 --price 200.92 --outside-rth ANY_TIME -y --format json") {
		t.Errorf("argv: %s", argv)
	}
	if r := b.Sell(ctx, "NVDA.US", 6, 199, "RTH_ONLY", ""); r.OK || r.Status != "Rejected" || !strings.Contains(r.Raw, "insufficient position") {
		t.Fatalf("sell must keep the broker's raw rejection: %+v", r)
	}
	if d := b.OrderDetail(ctx, "1234"); d["status"] != "Filled" {
		t.Fatalf("detail: %v", d)
	}
	if f := b.RecentFills(ctx, 1); len(f) != 1 || f[0]["order_id"] != "1" {
		t.Fatalf("fills: %v", f)
	}
	if c := b.Cancel(ctx, "1234"); c.OK {
		t.Fatal("cancel failure reported ok")
	}
	if _, ok := b.Positions(ctx); ok {
		t.Fatal("hard-lesson: a failed positions call must not read as flat")
	}
}

func TestDryRunDefaultAndNoCLI(t *testing.T) {
	os.Unsetenv("FLINTRADE_DRY_RUN")
	if !DryRunFromEnv() {
		t.Fatal("unset FLINTRADE_DRY_RUN must mean dry run")
	}
	for _, v := range []string{"0", "false", " NO "} {
		t.Setenv("FLINTRADE_DRY_RUN", v)
		if DryRunFromEnv() {
			t.Fatalf("%q must mean live", v)
		}
	}
	for _, v := range []string{"", "1", "off", "O"} {
		t.Setenv("FLINTRADE_DRY_RUN", v)
		if !DryRunFromEnv() {
			t.Fatalf("%q must stay dry", v)
		}
	}
	b := &CLI{LB: &lb.Runner{Bin: "/nonexistent"}, Dry: true}
	r := b.Buy(context.Background(), "X.US", 1, 10, "RTH_ONLY", "abc")
	if !r.OK || r.BrokerOrderID != "DRY-abc" || r.Status != "Filled" || *r.FillPrice != 10 {
		t.Fatalf("dry: %+v", r)
	}
	if ps, ok := b.Positions(context.Background()); !ok || len(ps) != 0 {
		t.Fatal("dry positions")
	}
	t.Setenv("LONGBRIDGE_APP_KEY", "")
	if _, err := New(nil, false); err == nil {
		t.Fatal("live without credentials must fail loudly")
	}
}
