// Package llm is the pluggable model access layer, ported from agent/llm.py.
//
//   - Providers is the single registry: a new vendor is one entry, no business
//     code changes. Three wires: "cli" (claude / kimi subprocess, subscription
//     auth), "openai" (chat/completions — OpenAI, DeepSeek, Kimi, OpenRouter,
//     Ollama, any compatible endpoint) and "anthropic" (Messages API).
//   - Tier → provider/model lives in trading.toml [models]; switching vendor
//     is a config edit.
//   - No cross-model fallback. A call that still fails after retries returns
//     "" and the caller reads that as WAIT: better to miss a trade than to
//     trade on a different brain.
//   - API keys come only from the environment; base_url must be http(s) so a
//     key is never sent somewhere odd.
//   - Every call that reached a model is logged verbatim to
//     llm_calls/YYYYMMDD.jsonl ("model-visible means logged").
//
// FLINTRADE_LLM_DRY=1 short-circuits Complete with a WAIT placeholder and logs
// nothing, like the broker's dry run.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"flintrade/internal/config"
)

// Provider is a registry entry; tier config may override every field.
type Provider struct {
	Wire      string // cli | openai | anthropic
	CLI       string
	BaseURL   string // openai wire: includes the version segment (…/v1)
	APIKeyEnv string
}

var Providers = map[string]Provider{
	"claude-cli": {Wire: "cli", CLI: "claude"}, // Claude Code subscription
	"kimi-cli":   {Wire: "cli", CLI: "kimi"},   // Kimi Code plan (≠ moonshot pay-as-you-go)
	"anthropic":  {Wire: "anthropic", BaseURL: "https://api.anthropic.com", APIKeyEnv: "ANTHROPIC_API_KEY"},
	"openai":     {Wire: "openai", BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY"},
	"deepseek":   {Wire: "openai", BaseURL: "https://api.deepseek.com", APIKeyEnv: "DEEPSEEK_API_KEY"},
	"moonshot":   {Wire: "openai", BaseURL: "https://api.moonshot.cn/v1", APIKeyEnv: "MOONSHOT_API_KEY"},
	"kimi-plan":  {Wire: "anthropic", BaseURL: "https://api.kimi.com/coding", APIKeyEnv: "KIMI_PLAN_API_KEY"},
	"openrouter": {Wire: "openai", BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OPENROUTER_API_KEY"},
	"ollama":     {Wire: "openai", BaseURL: "http://localhost:11434/v1"},
	// Escape hatch: any OpenAI-compatible endpoint; base_url must be in the tier.
	"openai_compatible": {Wire: "openai", APIKeyEnv: "LLM_API_KEY"},
}

var aliases = map[string]string{"claude": "claude-cli"} // older trading.toml

// Tier is a fully resolved tier: registry defaults plus trading.toml overrides.
type Tier struct {
	Name, Provider, Wire, Model string
	CLI, BaseURL, APIKeyEnv     string
	Temperature                 *float64
	MaxTokens                   *int
	Timeout                     time.Duration
	ExtraHeaders                map[string]string
}

// ResolveTier merges the registry entry with the tier's config.
func ResolveTier(t *config.Trading, tier string) (Tier, error) {
	spec, ok := t.Models[tier]
	if !ok {
		return Tier{}, fmt.Errorf("tier %q is not in trading.toml [models]", tier)
	}
	name := spec.Provider
	if a, ok := aliases[name]; ok {
		name = a
	}
	base, ok := Providers[name]
	if !ok {
		names := make([]string, 0, len(Providers))
		for k := range Providers {
			names = append(names, k)
		}
		sort.Strings(names)
		return Tier{}, fmt.Errorf("unknown chat provider %q (known: %s)", name, strings.Join(names, ", "))
	}
	r := Tier{Name: tier, Provider: name, Wire: base.Wire, Model: spec.Model, CLI: base.CLI,
		BaseURL: base.BaseURL, APIKeyEnv: base.APIKeyEnv, Temperature: spec.Temperature,
		MaxTokens: spec.MaxTokens, Timeout: 300 * time.Second, ExtraHeaders: spec.ExtraHeaders}
	if spec.CLI != "" {
		r.CLI = spec.CLI
	}
	if spec.BaseURL != "" {
		r.BaseURL = spec.BaseURL
	}
	if spec.APIKeyEnv != "" {
		r.APIKeyEnv = spec.APIKeyEnv
	}
	if spec.TimeoutSec != nil {
		r.Timeout = time.Duration(*spec.TimeoutSec * float64(time.Second))
	}
	if r.Model == "" {
		// hard-lesson: an empty model silently means "whatever the CLI defaults to".
		return Tier{}, fmt.Errorf("tier %q (%s) must name a model explicitly", tier, name)
	}
	return r, nil
}

// Client runs completions and embeddings. The zero value is not usable; set
// Config and LogDir.
type Client struct {
	Config config.Loader
	LogDir string // llm_calls/
	HTTP   *http.Client
	Now    func() time.Time

	retryDelays []time.Duration // test hook; default 0, 2s, 8s
	logMu       sync.Mutex
	purgeOnce   sync.Once
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// Dry reports FLINTRADE_LLM_DRY.
func Dry() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FLINTRADE_LLM_DRY"))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

const dryResponse = `{"action":"WAIT","reasoning":"[llm dry-run]"}`

// Request is one completion. Tag is the caller's purpose label, for the log.
type Request struct {
	User, System string
	Tier         string
	Tag          string
	MaxBudgetUSD float64 // claude CLI only; default 1.5
}

// Complete runs req on its tier. On any failure the text is "" (callers treat
// that as WAIT) and err says why; a config error makes no call and no log.
func (c *Client) Complete(ctx context.Context, req Request) (string, error) {
	trading, err := c.Config.Trading()
	if err != nil {
		return "", err
	}
	tier, err := ResolveTier(trading, req.Tier)
	if err != nil {
		return "", err
	}
	if req.MaxBudgetUSD == 0 {
		req.MaxBudgetUSD = 1.5
	}
	if Dry() {
		fmt.Fprintf(os.Stderr, "[llm DRY] %s/%s model=%s prompt=%d chars\n", tier.Wire, tier.Provider, tier.Model, len(req.User))
		return dryResponse, nil
	}
	t0 := time.Now()
	var out string
	switch tier.Wire {
	case "cli":
		out, err = c.completeCLI(ctx, req, tier)
	case "openai":
		out, err = c.completeOpenAI(ctx, req, tier)
	case "anthropic":
		out, err = c.completeAnthropic(ctx, req, tier)
	default:
		return "", fmt.Errorf("unknown wire %q", tier.Wire)
	}
	if err == nil && out == "" {
		err = errors.New("empty response")
	}
	c.logCall(record{
		TS: c.now().UTC().Format("2006-01-02T15:04:05Z"), Tag: nullable(req.Tag), Tier: req.Tier,
		Provider: tier.Provider, Model: tier.Model, Wire: tier.Wire,
		DurationMS: time.Since(t0).Milliseconds(), OK: err == nil,
		SystemPrompt: nullable(req.System), UserPrompt: req.User, Response: out, Error: errText(err),
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ── CLI wire ────────────────────────────────────────────────────────────

// sensitive marks env names withheld from model CLIs: they run an external
// model whose output, logs or crash dumps may echo the environment, and
// broker credentials or other vendors' keys are of no use to them.
var sensitive = []string{"KEY", "SECRET", "TOKEN", "PASSWORD"}

var cliEnvAllow = map[string][]string{
	"claude": {"ANTHROPIC_", "CLAUDE_"},
	"kimi":   {"KIMI_", "MOONSHOT_"},
}

// CLIEnv is environ minus every sensitive name except the CLI's own auth.
func CLIEnv(bin string, environ []string) []string {
	allow := cliEnvAllow[bin]
	var out []string
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		up := strings.ToUpper(k)
		hit := slices.ContainsFunc(sensitive, func(m string) bool { return strings.Contains(up, m) })
		own := slices.ContainsFunc(allow, func(p string) bool { return strings.HasPrefix(up, p) })
		if !hit || own {
			out = append(out, kv)
		}
	}
	return out
}

// cliArgs builds argv (without argv[0]). claude takes the system prompt as a
// file; kimi has no such flag, so it is prepended to the prompt.
func cliArgs(req Request, tier Tier, systemFile string) []string {
	if tier.CLI == "kimi" {
		text := req.User
		if req.System != "" {
			text = req.System + "\n\n---\n\n" + req.User
		}
		return []string{"-p", text, "-m", tier.Model}
	}
	// Only our prompts reach the model. Without these, claude -p also loads
	// the user's global CLAUDE.md, the repo CLAUDE.md (instructions for coding
	// agents), auto-memory and ~16k tokens of tool definitions — measured
	// 2026-11-09: 22k cache-write tokens and 12× the cost of a clean call.
	// (--bare would also do it but disables OAuth, i.e. the subscription.)
	args := []string{"-p", "--setting-sources", "", "--exclude-dynamic-system-prompt-sections",
		"--tools", "", "--allowedTools", "", "--max-budget-usd", strconv.FormatFloat(req.MaxBudgetUSD, 'f', -1, 64)}
	if systemFile != "" {
		args = append(args, "--system-prompt-file", systemFile)
	}
	return append(args, "--model", tier.Model, req.User)
}

// cleanKimi strips `kimi -p` decoration: leading bullets, the resume hint.
func cleanKimi(out string) string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "To resume this session:") {
			continue
		}
		lines = append(lines, strings.TrimPrefix(l, "• "))
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func (c *Client) completeCLI(ctx context.Context, req Request, tier Tier) (string, error) {
	var systemFile string
	if tier.CLI != "kimi" && req.System != "" {
		f, err := os.CreateTemp("", "flintrade-system-*.md")
		if err != nil {
			return "", err
		}
		systemFile = f.Name()
		defer os.Remove(systemFile)
		_, werr := f.WriteString(req.System)
		if cerr := f.Close(); werr != nil || cerr != nil {
			return "", errors.Join(werr, cerr)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, tier.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, tier.CLI, cliArgs(req, tier, systemFile)...)
	cmd.Env = append(CLIEnv(tier.CLI, os.Environ()), "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1")
	// A neutral cwd: CLIs discover CLAUDE.md / AGENTS.md by walking up from it.
	cmd.Dir = os.TempDir()
	// The CLI spawns helpers; a timeout must take the whole group down.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	out := stdout.String()
	if tier.CLI == "kimi" {
		out = cleanKimi(out)
	}
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return out, fmt.Errorf("%s timed out after %s", tier.CLI, tier.Timeout)
	case err != nil:
		// claude -p reports budget/auth failures on stdout, warnings on stderr.
		return out, fmt.Errorf("%s: %v: %s | stderr: %s", tier.CLI, err,
			truncate(strings.TrimSpace(out), 200), truncate(strings.TrimSpace(stderr.String()), 200))
	}
	return out, nil
}

// ── HTTP wires ──────────────────────────────────────────────────────────

type fatalError struct{ error }

// post sends one JSON POST. A fatalError is not worth retrying (4xx other
// than 429, or a non-http(s) URL); any other error is.
func (c *Client) post(ctx context.Context, url string, payload any, headers map[string]string, timeout time.Duration) (map[string]any, error) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, fatalError{fmt.Errorf("refusing non-http(s) base_url %q", url)}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fatalError{err}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fatalError{err}
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		e := fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			return nil, e
		}
		return nil, fatalError{e}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("bad JSON from %s: %w", url, err)
	}
	if usage, ok := out["usage"]; ok && usage != nil {
		b, _ := json.Marshal(usage)
		fmt.Fprintf(os.Stderr, "[llm] usage %v: %s\n", modelOf(payload), b)
	}
	return out, nil
}

func modelOf(payload any) any {
	if m, ok := payload.(map[string]any); ok {
		return m["model"]
	}
	return nil
}

// postRetry tries one endpoint up to three times (0, 2 s, 8 s), giving up at
// once on a fatal error. It never switches model.
func (c *Client) postRetry(ctx context.Context, url string, payload any, headers map[string]string, timeout time.Duration) (map[string]any, error) {
	delays := c.retryDelays
	if delays == nil {
		delays = []time.Duration{0, 2 * time.Second, 8 * time.Second}
	}
	var last error
	for _, d := range delays {
		if d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		out, err := c.post(ctx, url, payload, headers, timeout)
		if err == nil {
			return out, nil
		}
		last = err
		var fe fatalError
		if errors.As(err, &fe) {
			return nil, err
		}
		fmt.Fprintf(os.Stderr, "[llm] POST %s failed (will retry): %v\n", url, err)
	}
	return nil, last
}

func (c *Client) completeOpenAI(ctx context.Context, req Request, tier Tier) (string, error) {
	base := strings.TrimRight(tier.BaseURL, "/")
	if base == "" {
		return "", fmt.Errorf("provider %s has no base_url", tier.Provider)
	}
	headers := map[string]string{}
	if tier.APIKeyEnv != "" {
		if key := os.Getenv(tier.APIKeyEnv); key != "" {
			headers["Authorization"] = "Bearer " + key
		}
	}
	for k, v := range tier.ExtraHeaders {
		headers[k] = v
	}
	var msgs []map[string]string
	if req.System != "" {
		msgs = append(msgs, map[string]string{"role": "system", "content": req.System})
	}
	msgs = append(msgs, map[string]string{"role": "user", "content": req.User})
	payload := map[string]any{"model": tier.Model, "messages": msgs, "stream": false}
	if tier.Temperature != nil {
		payload["temperature"] = *tier.Temperature
	}
	if tier.MaxTokens != nil {
		payload["max_tokens"] = *tier.MaxTokens
	}
	out, err := c.postRetry(ctx, base+"/chat/completions", payload, headers, tier.Timeout)
	if err != nil {
		return "", err
	}
	choices, _ := out["choices"].([]any)
	if len(choices) == 0 {
		return "", errors.New("response has no choices")
	}
	first, _ := choices[0].(map[string]any)
	msg, _ := first["message"].(map[string]any)
	text, _ := msg["content"].(string)
	return text, nil
}

func (c *Client) completeAnthropic(ctx context.Context, req Request, tier Tier) (string, error) {
	base := strings.TrimRight(tier.BaseURL, "/")
	if base == "" {
		base = "https://api.anthropic.com"
	}
	keyEnv := tier.APIKeyEnv
	if keyEnv == "" {
		keyEnv = "ANTHROPIC_API_KEY"
	}
	maxTokens := 4096
	if tier.MaxTokens != nil {
		maxTokens = *tier.MaxTokens
	}
	payload := map[string]any{"model": tier.Model, "max_tokens": maxTokens,
		"messages": []map[string]string{{"role": "user", "content": req.User}}}
	if req.System != "" {
		payload["system"] = req.System
	}
	if tier.Temperature != nil {
		payload["temperature"] = *tier.Temperature
	}
	out, err := c.postRetry(ctx, base+"/v1/messages", payload,
		map[string]string{"x-api-key": os.Getenv(keyEnv), "anthropic-version": "2023-06-01"}, tier.Timeout)
	if err != nil {
		return "", err
	}
	blocks, ok := out["content"].([]any)
	if !ok {
		return "", errors.New("response has no content")
	}
	var sb strings.Builder
	for _, b := range blocks {
		if m, ok := b.(map[string]any); ok && m["type"] == "text" {
			s, _ := m["text"].(string)
			sb.WriteString(s)
		}
	}
	return sb.String(), nil
}

// ── call log ────────────────────────────────────────────────────────────

const logRetentionDays = 30

// record is one llm_calls line; field order matches the Python log so old
// and new lines read the same. Error is new: why a call came back empty.
type record struct {
	TS           string  `json:"ts"`
	Tag          *string `json:"tag"`
	Tier         string  `json:"tier"`
	Provider     string  `json:"provider"`
	Model        string  `json:"model"`
	Wire         string  `json:"wire"`
	DurationMS   int64   `json:"duration_ms"`
	OK           bool    `json:"ok"`
	SystemPrompt *string `json:"system_prompt"`
	UserPrompt   string  `json:"user_prompt"`
	Response     string  `json:"response"`
	Error        string  `json:"error,omitempty"`
}

// logCall appends one line. Logging must never break the trading path: a
// failure is reported on stderr and swallowed.
func (c *Client) logCall(r record) {
	if err := c.appendLog(r); err != nil {
		fmt.Fprintf(os.Stderr, "[llm] call log failed (ignored): %v\n", err)
	}
}

func (c *Client) appendLog(r record) error {
	if err := os.MkdirAll(c.LogDir, 0o755); err != nil {
		return err
	}
	c.purgeOnce.Do(c.purgeOld)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return err
	}
	path := filepath.Join(c.LogDir, c.now().UTC().Format("20060102")+".jsonl")
	c.logMu.Lock() // threads in this process; flock covers other processes
	defer c.logMu.Unlock()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	_, err = f.Write(buf.Bytes())
	return err
}

func (c *Client) purgeOld() {
	cutoff := c.now().UTC().AddDate(0, 0, -logRetentionDays).Format("20060102")
	files, _ := filepath.Glob(filepath.Join(c.LogDir, "*.jsonl"))
	for _, p := range files {
		stem := strings.TrimSuffix(filepath.Base(p), ".jsonl")
		if isDigits(stem) && stem < cutoff {
			os.Remove(p)
		}
	}
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// DefaultLogDir is $FLINTRADE_LLM_LOG_DIR, else root/llm_calls.
func DefaultLogDir(root string) string {
	if d := os.Getenv("FLINTRADE_LLM_LOG_DIR"); d != "" {
		return d
	}
	return filepath.Join(root, "llm_calls")
}

// CheckTier reports whether a tier can run here without calling it: the CLI
// is on PATH, or the API key is in the environment.
func CheckTier(t Tier) (string, bool) {
	if t.Wire == "cli" {
		if p, err := exec.LookPath(t.CLI); err == nil {
			return t.CLI + " CLI at " + p, true
		}
		return t.CLI + " CLI not on PATH", false
	}
	if t.APIKeyEnv == "" {
		return "no key needed", true
	}
	if os.Getenv(t.APIKeyEnv) != "" {
		return "$" + t.APIKeyEnv + " set", true
	}
	return "$" + t.APIKeyEnv + " missing", false
}
