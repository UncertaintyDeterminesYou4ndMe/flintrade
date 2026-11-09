// Package creds owns Flintrade's credential file.
//
// Design follows maka-agent's credential store (packages/storage/src/
// credential-store.ts): one plaintext file, owner-only (0600), the OS user
// account as the security boundary, atomic replace (temp file + rename) and a
// cross-process lock so the app and a CLI can never lose each other's write.
// The OS keychain is deliberately not used: the daemon runs unattended under
// launchd, and macOS re-prompts for keychain access after every rebuild —
// with nobody there to click Allow, trading would stall.
//
// The format stays shell-compatible (`export KEY='value'`) so that
// scripts/agentctl.sh can still `source` it — that is the rollback path to
// the Python daemon.
package creds

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

// Keys the Longbridge CLI needs.
const (
	AppKey      = "LONGBRIDGE_APP_KEY"
	AppSecret   = "LONGBRIDGE_APP_SECRET"
	AccessToken = "LONGBRIDGE_ACCESS_TOKEN"
	DryRun      = "FLINTRADE_DRY_RUN"
)

// DefaultPath is $FLINTRADE_CREDENTIALS, else the Application Support file when it
// exists (plan D5), else <root>/flintrade.env.
func DefaultPath(root string) string {
	if p := os.Getenv("FLINTRADE_CREDENTIALS"); p != "" {
		return p
	}
	if p := AppSupportPath(); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(root, "flintrade.env")
}

// AppSupportPath is where credentials live after cutover (plan D5): outside
// the repo, so git, export_oss.sh and repo-reading agents never see them.
func AppSupportPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "Flintrade", "credentials.env")
}

var lineRE = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)
var keyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ErrInsecurePerms means group or others can read the credential file.
var ErrInsecurePerms = errors.New("credential file is readable by group/others; want 0600")

// CheckPerms returns ErrInsecurePerms when path is not owner-only.
func CheckPerms(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w (%s is %o)", ErrInsecurePerms, path, st.Mode().Perm())
	}
	return nil
}

// Load parses path into KEY → value. Comments and blank lines are ignored.
func Load(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(string(b))
}

// Parse reads shell-style assignments: KEY=v, export KEY="v", export KEY='v'.
func Parse(src string) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(src))
	n := 0
	for sc.Scan() {
		n++
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		m := lineRE.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("line %d: not an assignment", n)
		}
		v, err := unquote(m[2])
		if err != nil {
			return nil, fmt.Errorf("line %d (%s): %w", n, m[1], err)
		}
		out[m[1]] = v
	}
	return out, sc.Err()
}

// unquote handles the subset of shell quoting this file uses. Values are
// never echoed in errors.
func unquote(raw string) (string, error) {
	var b strings.Builder
	i := 0
	for i < len(raw) {
		c := raw[i]
		switch {
		case c == '\'':
			j := strings.IndexByte(raw[i+1:], '\'')
			if j < 0 {
				return "", errors.New("unterminated single quote")
			}
			b.WriteString(raw[i+1 : i+1+j])
			i += j + 2
		case c == '"':
			i++
			closed := false
			for i < len(raw) {
				if raw[i] == '\\' && i+1 < len(raw) && strings.IndexByte(`"\$`+"`", raw[i+1]) >= 0 {
					b.WriteByte(raw[i+1])
					i += 2
					continue
				}
				if raw[i] == '"' {
					closed = true
					i++
					break
				}
				if raw[i] == '$' || raw[i] == '`' {
					return "", errors.New("shell expansion inside double quotes is not supported")
				}
				b.WriteByte(raw[i])
				i++
			}
			if !closed {
				return "", errors.New("unterminated double quote")
			}
		case c == ' ' || c == '\t':
			rest := strings.TrimSpace(raw[i:])
			if rest != "" && !strings.HasPrefix(rest, "#") {
				return "", errors.New("unexpected text after value")
			}
			return b.String(), nil
		case c == '$' || c == '`':
			return "", errors.New("shell expansion is not supported")
		case c == '\\' && i+1 < len(raw):
			b.WriteByte(raw[i+1])
			i += 2
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), nil
}

// quote renders v so that `source` yields exactly v.
func quote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// Update sets the given keys, keeping every other line (comments included) in
// place, appending new keys at the end. The write is atomic and owner-only;
// concurrent Updates from different processes serialise on a lock file.
func Update(path string, updates map[string]string) error {
	for k, v := range updates {
		if !keyRE.MatchString(k) {
			return fmt.Errorf("invalid key %q", k)
		}
		if strings.ContainsAny(v, "\n\r\x00") {
			return fmt.Errorf("%s: value contains a newline or NUL", k)
		}
	}
	unlock, err := lockFile(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()

	var lines []string
	if b, err := os.ReadFile(path); err == nil {
		lines = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	done := map[string]bool{}
	for i, line := range lines {
		m := lineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if v, ok := updates[m[1]]; ok {
			lines[i] = "export " + m[1] + "=" + quote(v)
			done[m[1]] = true
		}
	}
	var added []string
	for k := range updates {
		if !done[k] {
			added = append(added, k)
		}
	}
	sort.Strings(added)
	for _, k := range added {
		lines = append(lines, "export "+k+"="+quote(updates[k]))
	}
	content := strings.Join(lines, "\n") + "\n"
	if _, err := Parse(content); err != nil {
		return fmt.Errorf("refusing to write unparsable file: %w", err)
	}
	return writeAtomic(path, []byte(content))
}

// writeAtomic replaces path with data: temp file in the same directory,
// chmod 0600 before any byte is written, fsync, rename, fsync the directory.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// Apply exports vals into this process's environment, so the longbridge CLI
// children inherit them (the job agentctl.sh did with `set -a; source`).
func Apply(vals map[string]string) error {
	for k, v := range vals {
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	return nil
}
