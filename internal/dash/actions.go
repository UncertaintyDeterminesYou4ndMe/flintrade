package dash

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"flintrade/internal/config"
	"flintrade/internal/creds"
	"flintrade/internal/paths"
)

// File is one file the strategy editor may open.
type File struct {
	Name  string // shown in the list: risk.toml, prompts/technical_intent.md
	Path  string
	TOML  bool // validated by config.Parse; prompts only need to be non-empty
	Gated bool // risk.toml: risk numbers need a backtest first, so read-only until the Go backtest exists
}

// ConfigFiles lists the editable config and prompt files.
func ConfigFiles(root string) []File {
	cd := paths.ConfigDir(root)
	out := []File{
		{Name: config.StrategiesFile, Path: filepath.Join(cd, config.StrategiesFile), TOML: true},
		{Name: config.TradingFile, Path: filepath.Join(cd, config.TradingFile), TOML: true},
		{Name: config.RiskFile, Path: filepath.Join(cd, config.RiskFile), TOML: true, Gated: true},
	}
	ms, _ := filepath.Glob(filepath.Join(paths.PromptsDir(root), "*.md"))
	sort.Strings(ms)
	for _, p := range ms {
		out = append(out, File{Name: "prompts/" + filepath.Base(p), Path: p})
	}
	return out
}

// Validate checks content before it may be saved.
func Validate(f File, content string) error {
	if f.Gated {
		return errors.New("risk.toml 改动需要先过回测;Go 版回测落地前只读")
	}
	if strings.TrimSpace(content) == "" {
		return errors.New("内容为空")
	}
	if f.TOML {
		return config.Parse(filepath.Base(f.Path), []byte(content))
	}
	return nil
}

// Save validates and writes content atomically (temp file + rename in the
// same directory), keeping the file's mode. A daemon reading mid-save sees
// either the old or the new file, never half of one.
func Save(f File, content string) error {
	if err := Validate(f, content); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(f.Path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.Path), "."+filepath.Base(f.Path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), f.Path)
}

// RestartDaemon has launchd kill and restart the daemon. It rereads config
// and the credentials file on start; the plist itself is not reread.
func RestartDaemon(ctx context.Context) error {
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), Label)
	out, err := exec.CommandContext(ctx, "launchctl", "kickstart", "-k", target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl kickstart: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// CredKeys are the credentials the app edits. FLINTRADE_DRY_RUN stays a manual
// edit of the file: flipping live trading is not a form field.
var CredKeys = []string{creds.AppKey, creds.AppSecret, creds.AccessToken}

// LongbridgeBin is the CLI; launchd and Finder start the app with a bare PATH.
var LongbridgeBin = func() string {
	for _, p := range []string{"/opt/homebrew/bin/longbridge", "/usr/local/bin/longbridge"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "longbridge"
}()

// VerifyCreds checks a new credential set before it replaces the old one:
// offline (token parses, matches the app key, not expired), then one
// read-only account call with exactly these values.
func VerifyCreds(ctx context.Context, vals map[string]string, now time.Time) error {
	if _, probs := creds.Check(vals, now, 0); len(probs) > 0 {
		var msgs []string
		for _, p := range probs {
			if p.Fatal {
				msgs = append(msgs, p.Message)
			}
		}
		if len(msgs) > 0 {
			return errors.New(strings.Join(msgs, ";"))
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, LongbridgeBin, "assets", "--format", "json")
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "LONGBRIDGE_") {
			env = append(env, kv)
		}
	}
	for _, k := range CredKeys {
		env = append(env, k+"="+vals[k])
	}
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = nil // balances are not needed, only whether the call succeeds
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		for _, k := range CredKeys {
			if v := vals[k]; v != "" {
				msg = strings.ReplaceAll(msg, v, "***")
			}
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return fmt.Errorf("longbridge 拒绝了这组凭证: %v %s", err, msg)
	}
	return nil
}

// SaveCreds verifies and writes the credentials (atomic, 0600, locked).
// Keys left empty keep their current value.
func SaveCreds(ctx context.Context, path string, current, edits map[string]string, now time.Time) error {
	merged := map[string]string{}
	for k, v := range current {
		merged[k] = v
	}
	updates := map[string]string{}
	for _, k := range CredKeys {
		if v := strings.TrimSpace(edits[k]); v != "" {
			merged[k] = v
			updates[k] = v
		}
	}
	if len(updates) == 0 {
		return errors.New("没有改动")
	}
	if err := VerifyCreds(ctx, merged, now); err != nil {
		return err
	}
	return creds.Update(path, updates)
}

// Mask shows only the last 4 characters of a secret.
func Mask(v string) string {
	if v == "" {
		return "(未设置)"
	}
	if len(v) <= 12 {
		return strings.Repeat("•", 8)
	}
	return strings.Repeat("•", 8) + v[len(v)-4:]
}
