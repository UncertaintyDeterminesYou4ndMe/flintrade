package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"flintrade/internal/config"
)

// setup writes a trading.toml whose [models] block is models, and returns a
// client logging into a temp dir.
func setup(t *testing.T, models string) *Client {
	t.Helper()
	src, err := os.ReadFile("../../agent/config/trading.toml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	cut := func(s, section, repl string) string {
		i := strings.Index(s, "\n["+section+"]")
		j := strings.Index(s[i+1:], "\n[")
		return s[:i+1] + repl + s[i+1+j:]
	}
	s = cut(s, "models", "[models]\n"+models+"\n")
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "trading.toml"), []byte(s), 0o644)
	t.Setenv("FLINTRADE_LLM_DRY", "")
	return &Client{Config: config.Loader{Dir: dir}, LogDir: filepath.Join(dir, "llm_calls"),
		retryDelays: []time.Duration{0, 0, 0}}
}

func readLog(t *testing.T, c *Client) []map[string]any {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(c.LogDir, "*.jsonl"))
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

const cliModels = `trader = { provider = "claude", model = "claude-sonnet-5" }
flash  = { provider = "kimi-cli", model = "kimi-k3" }`

func TestResolveTier(t *testing.T) {
	c := setup(t, cliModels+`
deep = { provider = "deepseek", model = "deepseek-chat", base_url = "https://proxy.example/v1", timeout_sec = 12.5 }
bad  = { provider = "nope", model = "x" }`)
	tr, err := c.Config.Trading()
	if err != nil {
		t.Fatal(err)
	}
	r, err := ResolveTier(tr, "trader")
	if err != nil || r.Provider != "claude-cli" || r.Wire != "cli" || r.CLI != "claude" || r.Timeout != 300*time.Second {
		t.Fatalf("alias: %+v %v", r, err)
	}
	r, _ = ResolveTier(tr, "deep")
	if r.BaseURL != "https://proxy.example/v1" || r.APIKeyEnv != "DEEPSEEK_API_KEY" || r.Timeout != 12500*time.Millisecond {
		t.Fatalf("override: %+v", r)
	}
	if _, err := ResolveTier(tr, "bad"); err == nil || !strings.Contains(err.Error(), "unknown chat provider") {
		t.Fatalf("unknown provider: %v", err)
	}
	if _, err := ResolveTier(tr, "missing"); err == nil {
		t.Fatal("missing tier must error, not default to an unnamed model")
	}
}

// Ported from test_invariants.py §7.
func TestHardLesson_CLIEnvScrubbed(t *testing.T) {
	env := []string{"HOME=/h", "PATH=/p", "LONGBRIDGE_APP_KEY=x", "LONGBRIDGE_APP_SECRET=x",
		"LONGBRIDGE_ACCESS_TOKEN=x", "DEEPSEEK_API_KEY=x", "CLAUDE_CODE_OAUTH_TOKEN=x",
		"ANTHROPIC_API_KEY=x", "KIMI_API_KEY=x", "db_password=x"}
	names := func(kvs []string) []string {
		var out []string
		for _, kv := range kvs {
			k, _, _ := strings.Cut(kv, "=")
			out = append(out, k)
		}
		return out
	}
	if got := names(CLIEnv("claude", env)); !slices.Equal(got, []string{"HOME", "PATH", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"}) {
		t.Errorf("claude env: %v", got)
	}
	if got := names(CLIEnv("kimi", env)); !slices.Equal(got, []string{"HOME", "PATH", "KIMI_API_KEY"}) {
		t.Errorf("kimi env: %v", got)
	}
}

// fakeCLI installs claude and kimi stubs on PATH. Each records its argv (one
// per line), the system prompt file's content and its env into dir/<bin>.*.
func fakeCLI(t *testing.T, claudeBody, kimiBody string) string {
	t.Helper()
	dir := t.TempDir()
	rec := `printf '%s\n' "$@" > "` + dir + `/$(basename $0).argv"; env > "` + dir + `/$(basename $0).env"
prev=""; for a in "$@"; do [ "$prev" = "--system-prompt-file" ] && cp "$a" "` + dir + `/sysfile"; prev="$a"; done
`
	os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/bash\n"+rec+claudeBody+"\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "kimi"), []byte("#!/bin/bash\n"+rec+kimiBody+"\n"), 0o755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return dir
}

func TestClaudeCLIWire(t *testing.T) {
	c := setup(t, cliModels)
	dir := fakeCLI(t, `echo '{"action":"WAIT"}'`, "")
	t.Setenv("LONGBRIDGE_ACCESS_TOKEN", "must-not-leak")
	out, err := c.Complete(context.Background(), Request{User: "数据 <x>", System: "SYS prompt", Tier: "trader", Tag: "producer:technical", MaxBudgetUSD: 0.3})
	if err != nil || out != "{\"action\":\"WAIT\"}\n" {
		t.Fatalf("out %q err %v", out, err)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "claude.argv"))
	lines := strings.Split(strings.TrimSuffix(string(argv), "\n"), "\n")
	want := []string{"-p", "--setting-sources", "", "--exclude-dynamic-system-prompt-sections", "--tools", "", "--allowedTools", "", "--max-budget-usd", "0.3", "--system-prompt-file", "?", "--model", "claude-sonnet-5", "数据 <x>"}
	if len(lines) != len(want) {
		t.Fatalf("argv %q", lines)
	}
	for i := range want {
		if want[i] != "?" && lines[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, lines[i], want[i])
		}
	}
	if sys, _ := os.ReadFile(filepath.Join(dir, "sysfile")); string(sys) != "SYS prompt" {
		t.Errorf("system prompt file held %q", sys)
	}
	if _, err := os.Stat(lines[11]); !os.IsNotExist(err) {
		t.Errorf("system prompt temp file left behind: %s", lines[11])
	}
	env, _ := os.ReadFile(filepath.Join(dir, "claude.env"))
	if strings.Contains(string(env), "must-not-leak") {
		t.Error("broker token reached the model CLI")
	}
	// Repo/global CLAUDE.md and auto-memory must not reach the trading model.
	if !strings.Contains(string(env), "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1") {
		t.Error("auto-memory not disabled")
	}
	if wd, _ := os.Getwd(); strings.Contains(string(env), "PWD="+wd+"\n") {
		t.Error("CLI ran inside the repo, where it discovers CLAUDE.md")
	}
	// The call is logged verbatim, Python's field order, no HTML escaping.
	raw, _ := os.ReadFile(filepath.Join(c.LogDir, time.Now().UTC().Format("20060102")+".jsonl"))
	if !strings.HasPrefix(string(raw), `{"ts":"`) || !strings.Contains(string(raw), `"tag":"producer:technical","tier":"trader","provider":"claude-cli","model":"claude-sonnet-5","wire":"cli","duration_ms":`) ||
		!strings.Contains(string(raw), `"user_prompt":"数据 <x>"`) || strings.Contains(string(raw), `"error"`) {
		t.Errorf("log line: %s", raw)
	}
}

func TestKimiCLIWire(t *testing.T) {
	c := setup(t, cliModels)
	dir := fakeCLI(t, "", `printf '• {"action":"WAIT"}\n• line two\nTo resume this session: kimi -r abc\n'`)
	out, err := c.Complete(context.Background(), Request{User: "U", System: "S", Tier: "flash"})
	if err != nil || out != "{\"action\":\"WAIT\"}\nline two" {
		t.Fatalf("out %q err %v", out, err)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "kimi.argv"))
	if string(argv) != "-p\nS\n\n---\n\nU\n-m\nkimi-k3\n" {
		t.Errorf("argv %q", argv)
	}
	if rec := readLog(t, c)[0]; rec["tag"] != nil || rec["system_prompt"] != "S" {
		t.Errorf("empty tag must log as null: %v", rec)
	}
}

// Python let TimeoutExpired escape before logging; a prompt the model saw
// must still be on record.
func TestHardLesson_TimedOutCallStillLogged(t *testing.T) {
	c := setup(t, `trader = { provider = "claude-cli", model = "m", timeout_sec = 0.5 }
flash = { provider = "claude-cli", model = "m" }`)
	fakeCLI(t, `sleep 30 & wait`, "")
	t0 := time.Now()
	out, err := c.Complete(context.Background(), Request{User: "seen by model", Tier: "trader"})
	if out != "" || err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("out %q err %v", out, err)
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Errorf("timeout took %s: grandchild held the call", d)
	}
	recs := readLog(t, c)
	if len(recs) != 1 || recs[0]["ok"] != false || recs[0]["user_prompt"] != "seen by model" || !strings.Contains(recs[0]["error"].(string), "timed out") {
		t.Fatalf("log: %v", recs)
	}
}

// A non-zero exit is a failed call even with stdout (claude -p prints
// "Error: Exceeded USD budget" there); Python passed that text on as a reply.
func TestCLIFailureIsNotOK(t *testing.T) {
	c := setup(t, cliModels)
	fakeCLI(t, `echo 'Error: Exceeded USD budget (0.05)'; echo 'warning' >&2; exit 1`, "")
	out, err := c.Complete(context.Background(), Request{User: "u", Tier: "trader"})
	if out != "" || err == nil || !strings.Contains(err.Error(), "Exceeded USD budget") {
		t.Fatalf("out %q err %v", out, err)
	}
	if rec := readLog(t, c)[0]; rec["ok"] != false || rec["response"] != "Error: Exceeded USD budget (0.05)\n" {
		t.Fatalf("log: %v", rec)
	}
}

func TestDryRunNoCallNoLog(t *testing.T) {
	c := setup(t, cliModels)
	dir := fakeCLI(t, "echo real", "")
	t.Setenv("FLINTRADE_LLM_DRY", "1")
	out, err := c.Complete(context.Background(), Request{User: "x", Tier: "trader"})
	if err != nil || out != dryResponse {
		t.Fatalf("dry: %q %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "claude.argv")); err == nil {
		t.Error("dry run invoked the CLI")
	}
	if len(readLog(t, c)) != 0 {
		t.Error("dry run wrote the call log")
	}
}

func TestOpenAIWire(t *testing.T) {
	var hits atomic.Int32
	var mode atomic.Value
	mode.Store("ok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		switch mode.Load() {
		case "flaky":
			if n < 3 {
				w.WriteHeader(503)
				return
			}
		case "auth":
			w.WriteHeader(401)
			io.WriteString(w, `{"error":"bad key"}`)
			return
		case "limited":
			w.WriteHeader(429)
			return
		}
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer sk-test" || r.Header.Get("X-Title") != "flintrade" {
			t.Errorf("request: %s %v", r.URL.Path, r.Header)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		msgs := body["messages"].([]any)
		if body["model"] != "deepseek-chat" || body["stream"] != false || body["temperature"] != 0.2 || body["max_tokens"] != 900.0 || len(msgs) != 2 {
			t.Errorf("body: %v", body)
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"hello"}}],"usage":{"total_tokens":3}}`)
	}))
	defer srv.Close()
	c := setup(t, `trader = { provider = "openai_compatible", model = "deepseek-chat", base_url = "`+srv.URL+`/v1/", api_key_env = "TEST_LLM_KEY", temperature = 0.2, max_tokens = 900, extra_headers = { X-Title = "flintrade" } }
flash = { provider = "claude-cli", model = "m" }`)
	t.Setenv("TEST_LLM_KEY", "sk-test")
	ctx := context.Background()
	req := Request{User: "u", System: "s", Tier: "trader"}

	if out, err := c.Complete(ctx, req); out != "hello" || err != nil {
		t.Fatalf("ok: %q %v", out, err)
	}
	mode.Store("flaky")
	hits.Store(0)
	if out, _ := c.Complete(ctx, req); out != "hello" || hits.Load() != 3 {
		t.Fatalf("5xx must retry: %q after %d hits", out, hits.Load())
	}
	mode.Store("auth")
	hits.Store(0)
	if out, err := c.Complete(ctx, req); out != "" || err == nil || hits.Load() != 1 {
		t.Fatalf("401 must not retry: %q %v %d hits", out, err, hits.Load())
	}
	mode.Store("limited")
	hits.Store(0)
	if out, _ := c.Complete(ctx, req); out != "" || hits.Load() != 3 {
		t.Fatalf("429 retried %d times, want 3", hits.Load())
	}
	recs := readLog(t, c)
	if len(recs) != 4 || recs[0]["ok"] != true || recs[2]["ok"] != false || !strings.Contains(recs[2]["error"].(string), "HTTP 401") {
		t.Fatalf("log: %v", recs)
	}
}

func TestAnthropicWire(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/coding/v1/messages" || r.Header.Get("x-api-key") != "plan-key" || r.Header.Get("anthropic-version") != "2023-06-01" ||
			body["system"] != "s" || body["max_tokens"] != 4096.0 || body["model"] != "kimi-for-coding" {
			t.Errorf("request: %s %v %v", r.URL.Path, r.Header, body)
		}
		io.WriteString(w, `{"content":[{"type":"thinking","thinking":"hm"},{"type":"text","text":"a"},{"type":"text","text":"b"}]}`)
	}))
	defer srv.Close()
	c := setup(t, `trader = { provider = "kimi-plan", model = "kimi-for-coding", base_url = "`+srv.URL+`/coding" }
flash = { provider = "claude-cli", model = "m" }`)
	t.Setenv("KIMI_PLAN_API_KEY", "plan-key")
	if out, err := c.Complete(context.Background(), Request{User: "u", System: "s", Tier: "trader"}); out != "ab" || err != nil {
		t.Fatalf("%q %v", out, err)
	}
}

func TestNonHTTPBaseURLRefused(t *testing.T) {
	c := setup(t, `trader = { provider = "openai_compatible", model = "m", base_url = "file:///etc" }
flash = { provider = "claude-cli", model = "m" }`)
	if out, err := c.Complete(context.Background(), Request{User: "u", Tier: "trader"}); out != "" || err == nil || !strings.Contains(err.Error(), "non-http") {
		t.Fatalf("%q %v", out, err)
	}
}

func TestLogPurgeAndFailureTolerance(t *testing.T) {
	c := setup(t, cliModels)
	fakeCLI(t, "echo hi", "")
	os.MkdirAll(c.LogDir, 0o755)
	old := filepath.Join(c.LogDir, "20250101.jsonl")
	keep := filepath.Join(c.LogDir, "notes.jsonl")
	os.WriteFile(old, nil, 0o644)
	os.WriteFile(keep, nil, 0o644)
	c.Complete(context.Background(), Request{User: "u", Tier: "trader"})
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("expired log not purged")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("non-date file purged")
	}
	// An unwritable log dir must not fail the call.
	c2 := setup(t, cliModels)
	c2.LogDir = "/dev/null/llm_calls"
	if out, err := c2.Complete(context.Background(), Request{User: "u", Tier: "trader"}); out != "hi\n" || err != nil {
		t.Fatalf("logging failure broke the call: %q %v", out, err)
	}
}
