// Package paths resolves where Flintrade's files live.
//
// The Python code located everything relative to its own source file. A Go
// binary can run from anywhere, so the root comes from $FLINTRADE_DIR, or failing
// that from walking up the working directory (launchd sets WorkingDirectory
// to the repo) until agent/config/risk.toml is found.
package paths

import (
	"errors"
	"os"
	"path/filepath"
)

// ErrRootNotFound means neither $FLINTRADE_DIR nor any parent of the working
// directory looks like a Flintrade checkout.
var ErrRootNotFound = errors.New("flintrade root not found: set FLINTRADE_DIR or run inside the repo")

const marker = "agent/config/risk.toml"

// Root returns the Flintrade repo root.
func Root() (string, error) {
	if d := os.Getenv("FLINTRADE_DIR"); d != "" {
		if isRoot(d) {
			return filepath.Abs(d)
		}
		return "", errors.New("FLINTRADE_DIR=" + d + " has no " + marker)
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for d := wd; ; d = filepath.Dir(d) {
		if isRoot(d) {
			return d, nil
		}
		if filepath.Dir(d) == d {
			return "", ErrRootNotFound
		}
	}
}

func isRoot(d string) bool {
	st, err := os.Stat(filepath.Join(d, marker))
	return err == nil && !st.IsDir()
}

// ConfigDir is where risk.toml / trading.toml / strategies.toml live.
func ConfigDir(root string) string { return filepath.Join(root, "agent", "config") }

// PromptsDir holds the producer system prompts.
func PromptsDir(root string) string { return filepath.Join(root, "agent", "prompts") }

// DBPath is $FLINTRADE_DB if set, else <root>/flintrade.db.
func DBPath(root string) string {
	if p := os.Getenv("FLINTRADE_DB"); p != "" {
		return p
	}
	return filepath.Join(root, "flintrade.db")
}
