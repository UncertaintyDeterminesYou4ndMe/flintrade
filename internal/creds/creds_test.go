package creds

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// bashValues sources path in a clean bash and returns the named variables —
// the ground truth agentctl.sh sees.
func bashValues(t *testing.T, path string, keys ...string) map[string]string {
	t.Helper()
	script := "set -a; source \"$1\"; set +a; shift; for k in \"$@\"; do printf '%s\\0' \"${!k}\"; done"
	out, err := exec.Command("/bin/bash", append([]string{"-c", script, "bash", path}, keys...)...).Output()
	if err != nil {
		t.Fatalf("bash source: %v", err)
	}
	vals := strings.Split(string(out), "\x00")
	m := map[string]string{}
	for i, k := range keys {
		m[k] = vals[i]
	}
	return m
}

func digest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:6]) }

// Go must read the live credential file exactly as bash does. Values are
// compared by hash so a failure never prints a secret.
func TestParseMatchesBashOnLiveFile(t *testing.T) {
	path := "../../flintrade.env"
	if _, err := os.Stat(path); err != nil {
		t.Skip("no live flintrade.env")
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{AppKey, AppSecret, AccessToken, DryRun}
	want := bashValues(t, path, keys...)
	for _, k := range keys {
		if digest(got[k]) != digest(want[k]) {
			t.Errorf("%s differs from bash (go %s, bash %s)", k, digest(got[k]), digest(want[k]))
		}
	}
	if err := CheckPerms(path); err != nil {
		t.Errorf("live file perms: %v", err)
	}
}

func TestParseQuoting(t *testing.T) {
	src := strings.Join([]string{
		"# comment",
		"",
		`export A="dq \"x\" \\ \$"`,
		`export B='sq "x" $HOME'`,
		`C=plain   # trailing comment`,
		`D='it'\''s'`,
		`E=#hash`,
		`  export F=""`,
	}, "\n")
	got, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": `dq "x" \ $`, "B": `sq "x" $HOME`, "C": "plain", "D": "it's", "E": "#hash", "F": ""}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	// Cross-check the fixture against bash itself.
	p := filepath.Join(t.TempDir(), "q.env")
	os.WriteFile(p, []byte(src), 0o600)
	b := bashValues(t, p, "A", "B", "C", "D", "E", "F")
	for k, v := range want {
		if b[k] != v {
			t.Errorf("fixture disagrees with bash for %s: bash %q", k, b[k])
		}
	}
}

func TestParseRejectsExpansion(t *testing.T) {
	for _, src := range []string{`A=$HOME`, `A="x$HOME"`, "A=`id`", `A="unterminated`, `A=x y`} {
		if _, err := Parse(src); err == nil {
			t.Errorf("%q accepted", src)
		}
	}
}

func TestUpdatePreservesAndIsSourceable(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "flintrade.env")
	orig := "# header stays\nexport LONGBRIDGE_APP_KEY=\"old\"\n# note\nexport FLINTRADE_DRY_RUN=\"1\"\n"
	os.WriteFile(p, []byte(orig), 0o644)
	tricky := `new'key "with" $quotes \ and spaces`
	if err := Update(p, map[string]string{AppKey: tricky, AccessToken: "hk_m_tok"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	s := string(b)
	if !strings.HasPrefix(s, "# header stays\n") || !strings.Contains(s, "# note\n") {
		t.Fatalf("comments lost:\n%s", s)
	}
	if strings.Index(s, AppKey) > strings.Index(s, "# note") {
		t.Fatal("existing key moved instead of updated in place")
	}
	if err := CheckPerms(p); err != nil {
		t.Fatalf("perms after update: %v", err)
	}
	got := bashValues(t, p, AppKey, AccessToken, DryRun)
	if got[AppKey] != tricky || got[AccessToken] != "hk_m_tok" || got[DryRun] != "1" {
		t.Fatalf("bash sees %q", got)
	}
	parsed, _ := Load(p)
	if parsed[AppKey] != tricky {
		t.Fatalf("Go re-read %q", parsed[AppKey])
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".flintrade.env.tmp-*")); len(leftovers) > 0 {
		t.Fatalf("temp files left: %v", leftovers)
	}
}

func TestUpdateRejectsBadInput(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.env")
	if err := Update(p, map[string]string{"BAD-KEY": "x"}); err == nil {
		t.Fatal("bad key accepted")
	}
	if err := Update(p, map[string]string{"K": "a\nexport EVIL=1"}); err == nil {
		t.Fatal("newline injection accepted")
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected update created the file")
	}
}

// Concurrent writers (app + CLI) must not lose updates.
func TestUpdateConcurrent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.env")
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := Update(p, map[string]string{fmt.Sprintf("K%02d", i): fmt.Sprint(i)}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 20 {
		t.Fatalf("lost updates: %d of 20 keys survived", len(got))
	}
}

func fakeToken(claims string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return "hk_m_" + enc([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc([]byte(claims)) + ".sig"
}

func TestParseTokenAndCheck(t *testing.T) {
	iat, exp := int64(1791794143), int64(1799570142)
	tok := fakeToken(fmt.Sprintf(`{"exp":%d,"iat":%d,"ak":"AK1","ac":"lb_papertrading","region_home":"HK"}`, exp, iat))
	info, err := ParseToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Paper() || info.Region != "HK" || info.ExpiresAt.Format("2006-01-02") != "2027-01-10" {
		t.Fatalf("info = %+v", info)
	}
	vals := map[string]string{AppKey: "AK1", AppSecret: "s", AccessToken: tok, DryRun: "0"}

	_, probs := Check(vals, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), 14*24*time.Hour)
	if len(probs) != 0 {
		t.Fatalf("healthy set flagged: %+v", probs)
	}
	_, probs = Check(vals, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), 14*24*time.Hour)
	if len(probs) != 1 || probs[0].Fatal || !strings.Contains(probs[0].Message, "expires in 9 days") {
		t.Fatalf("expiry warning: %+v", probs)
	}
	_, probs = Check(vals, time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC), 14*24*time.Hour)
	if len(probs) != 1 || !probs[0].Fatal {
		t.Fatalf("expired: %+v", probs)
	}
	vals[AppKey] = "OTHER"
	_, probs = Check(vals, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), 0)
	if len(probs) != 1 || !strings.Contains(probs[0].Message, "different app key") {
		t.Fatalf("mismatched key: %+v", probs)
	}
	for _, p := range probs {
		if strings.Contains(p.Message, tok) || strings.Contains(p.Message, "OTHER") {
			t.Fatal("problem message leaks a secret")
		}
	}
}

// The live token must parse, belong to the live app key, and be a paper token.
func TestLiveTokenConsistent(t *testing.T) {
	vals, err := Load("../../flintrade.env")
	if err != nil {
		t.Skip("no live flintrade.env")
	}
	info, probs := Check(vals, time.Now(), 0)
	for _, p := range probs {
		if p.Fatal {
			t.Errorf("%s: %s", p.Key, p.Message)
		}
	}
	if !info.Paper() {
		t.Errorf("live token channel %q is not paper", info.Channel)
	}
	t.Logf("live token expires %s (%s left)", info.ExpiresAt.Format(time.DateOnly), info.ExpiresIn(time.Now()).Round(time.Hour))
}
