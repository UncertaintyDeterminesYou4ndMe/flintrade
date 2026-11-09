// Package testutil holds helpers shared by package tests.
package testutil

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"flintrade/internal/config"
	"flintrade/internal/db"
)

// DB returns an initialised temp flintrade.db, closed at test end.
func DB(t testing.TB) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "flintrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return d
}

// SetEquity seeds risk_state like a fresh $10K sleeve.
func SetEquity(t testing.TB, d *db.DB, equity float64) {
	t.Helper()
	err := d.MustAs(db.RoleMigrate).UpdateRisk(context.Background(),
		db.Fields{"equity": equity, "day_start_equity": equity, "open_risk": 0.0, "day_realized_pnl": 0.0})
	if err != nil {
		t.Fatal(err)
	}
}

// Config copies the live agent/config/*.toml into a temp dir, optionally
// replacing files (name → content), and returns a loader for it.
func Config(t testing.TB, replace map[string]string) config.Loader {
	t.Helper()
	src := filepath.Join(repoRoot(t), "agent", "config")
	dir := t.TempDir()
	for _, name := range []string{"risk.toml", "trading.toml", "strategies.toml"} {
		b, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		if r, ok := replace[name]; ok {
			b = []byte(r)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return config.Loader{Dir: dir}
}

// ConfigFile returns the live config file's text, for tests that edit it.
func ConfigFile(t testing.TB, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "agent", "config", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func repoRoot(t testing.TB) string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
