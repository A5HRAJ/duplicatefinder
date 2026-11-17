package main

// A folder moved into itself: the original stays where it was, the move is
// reported as not done, and nothing is created at the destination.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A folder is never moved into itself or into a folder inside it, and the
// refusal comes before a batch folder is made.
func TestMoveRefusesADestinationInsideTheFolderBeingMoved(t *testing.T) {
	f, sess := newMoveVerifyFake(t)
	ef := filepath.Join(f.root, "ef")
	if err := os.MkdirAll(filepath.Join(ef, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(ef)
	want := entIdent{isDir: true, mod: fmtTime(fi.ModTime()), modUnix: fi.ModTime().Unix()}
	batched := false
	batch := func() (string, error) { batched = true; return "/Backups/ef/inner/Empty Folders", nil }
	for _, d := range []struct{ share, canon string }{
		{"/Backups/ef", ef},
		{"/Backups/ef/inner", filepath.Join(ef, "inner")},
	} {
		for _, b := range []func() (string, error){nil, batch} {
			_, err := execMoveFSAt(sess, ef, ef, d.share, d.canon, []entIdent{want}, b, false, nil)
			if err == nil || !strings.Contains(err.Error(), "inside the folder being moved") {
				t.Fatalf("moving %s into %s (batch %v): want the refusal, got %v", ef, d.canon, b != nil, err)
			}
		}
	}
	if batched {
		t.Fatal("a batch folder was allocated before the refusal")
	}
	if ents, _ := os.ReadDir(filepath.Join(ef, "inner")); len(ents) != 0 {
		t.Fatalf("something was created inside the folder: %v", ents)
	}
}
