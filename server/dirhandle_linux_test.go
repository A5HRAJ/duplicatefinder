//go:build linux

package main

// The property the move and scan vetting rest on: once a directory handle is
// open, renaming the original path away and planting a symlink (or imposter
// dir) in its place must not redirect operations that go through the handle.
// Only the Linux implementation pins; this test documents and enforces it.

import (
	"os"
	"path/filepath"
	"testing"
)

func pathExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func TestDirHandleSurvivesPathSwap(t *testing.T) {
	dir := t.TempDir()
	orig := filepath.Join(dir, "photos")
	if err := os.MkdirAll(orig, 0o755); err != nil {
		t.Fatal(err)
	}
	h, err := openDirHandle(orig)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	// Attacker: rename the vetted dir away and swap in a symlink elsewhere.
	renamed := filepath.Join(dir, "renamed")
	if err := os.Rename(orig, renamed); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, orig); err != nil {
		t.Fatal(err)
	}

	// Canon follows the pinned object, not the swapped-in path.
	if got, err := h.Canon(); err != nil || got != renamed {
		t.Fatalf("Canon after swap: got %q (%v), want %q", got, err, renamed)
	}
	// Writes through the handle land in the pinned dir, never through the link.
	if err := os.WriteFile(filepath.Join(h.Path(), "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !pathExists(filepath.Join(renamed, "f.txt")) {
		t.Fatal("write did not land in the pinned directory")
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("write escaped through the swapped-in symlink: %v", ents)
	}
}
