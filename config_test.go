package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A Homebrew upgrade moves the install into a new versioned directory, so the
// paths remembered from the last run stop existing. Loading must fall back
// rather than carrying a dead path into start.
func TestStalePathsAreDropped(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".hum"), 0o755); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(home, "worker.py")
	if err := os.WriteFile(live, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	builtinWorker, builtinPython = live, live
	t.Cleanup(func() { builtinWorker, builtinPython = "", "" })

	cfg := defaultConfig()
	cfg.Worker = filepath.Join(home, "gone", "worker.py") // as if upgraded away
	cfg.Python = filepath.Join(home, "gone", "python")
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	got, _ := loadConfig()
	if got.Worker != live {
		t.Errorf("worker = %q, want the built-in %q", got.Worker, live)
	}
	if got.Python != live {
		t.Errorf("python = %q, want the built-in %q", got.Python, live)
	}
}
