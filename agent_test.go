package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A symlink inside the working directory must not be a door out of it.
func TestSafePathFollowsSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "file-link")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"link/secret", "link/new-file", "file-link", "../escape", "/etc/hosts"} {
		if got, err := safePath(root, p); err == nil {
			t.Errorf("%s resolved to %s, want a refusal", p, got)
		}
	}
	for _, p := range []string{".", "a.txt", "sub/dir/new.txt"} {
		if _, err := safePath(root, p); err != nil {
			t.Errorf("%s refused: %v", p, err)
		}
	}
	// A symlink that stays inside the directory is fine, and resolves.
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := safePath(root, "alias/new.txt"); err != nil {
		t.Errorf("inside link refused: %v", err)
	}
}
