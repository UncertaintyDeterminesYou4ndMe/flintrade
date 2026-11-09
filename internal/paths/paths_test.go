package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRootFromEnvAndWalk(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "agent", "config"), 0o755)
	os.WriteFile(filepath.Join(root, marker), nil, 0o644)
	deep := filepath.Join(root, "a", "b")
	os.MkdirAll(deep, 0o755)
	realRoot, _ := filepath.EvalSymlinks(root)

	t.Setenv("FLINTRADE_DIR", "")
	t.Chdir(deep)
	got, err := Root()
	if err != nil {
		t.Fatal(err)
	}
	if g, _ := filepath.EvalSymlinks(got); g != realRoot {
		t.Fatalf("walk-up root = %s, want %s", got, realRoot)
	}

	t.Setenv("FLINTRADE_DIR", root)
	t.Chdir(t.TempDir())
	if got, err := Root(); err != nil || got != root {
		t.Fatalf("env root = %s %v", got, err)
	}

	t.Setenv("FLINTRADE_DIR", t.TempDir())
	if _, err := Root(); err == nil {
		t.Fatal("FLINTRADE_DIR without marker accepted")
	}

	t.Setenv("FLINTRADE_DB", "/x/y.db")
	if DBPath(root) != "/x/y.db" {
		t.Fatal("FLINTRADE_DB ignored")
	}
}
