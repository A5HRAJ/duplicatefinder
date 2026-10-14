package main

// Verified moves between filesystems (transit.go): the original must stay in
// place until a copy at the destination has been read back and matched, and
// whatever goes wrong before that point must leave the original exactly as it
// was. The fake File Station really copies on remove_src=false, so the
// verification reads run against real bytes.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// forceTransit makes every move in the test take the copy-verify-remove flow.
func forceTransit(t *testing.T) {
	t.Helper()
	old := crossesFS
	crossesFS = func(string, string) bool { return true }
	t.Cleanup(func() { crossesFS = old })
}

// leftovers lists the hidden staging or retirement entries under dir.
func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && strings.HasPrefix(fi.Name(), ".dupfinder-") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func TestTransitRemovesTheOriginalOnlyAfterTheCopyVerifies(t *testing.T) {
	forceTransit(t)
	f, sess := newMoveVerifyFake(t)
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	p, want, body := mkMoveFile(t, f, "a.bin")
	var origAtVerify bool
	f.afterCopy = func(string) {
		_, err := os.Stat(p)
		origAtVerify = err == nil
	}
	at, err := execMoveFSAt(sess, p, p, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, nil, true, nil)
	if err != nil {
		t.Fatalf("verified transit failed: %v", err)
	}
	if f.copies != 1 {
		t.Fatalf("the transit must COPY first, saw %d copies", f.copies)
	}
	if !origAtVerify {
		t.Fatal("the original was gone while its copy was still being verified")
	}
	if at != filepath.Join(f.root, "dest", "a.bin") {
		t.Fatalf("landing path %q", at)
	}
	if got, rerr := os.ReadFile(at); rerr != nil || !bytes.Equal(got, body) {
		t.Fatalf("copy not intact at the destination: %v", rerr)
	}
	if _, serr := os.Stat(p); !os.IsNotExist(serr) {
		t.Fatal("the original must be removed once its copy is verified and in place")
	}
	if l := leftovers(t, f.root); len(l) != 0 {
		t.Fatalf("staging or retirement entries left behind: %v", l)
	}
}

// The case this flow exists for: the copy is damaged on its way. The original
// must still be there, untouched, and the damaged copy must not be left at the
// destination looking like a finished move.
func TestTransitDamagedCopyLeavesTheOriginalUntouched(t *testing.T) {
	forceTransit(t)
	f, sess := newMoveVerifyFake(t)
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	p, want, body := mkMoveFile(t, f, "a.bin")
	f.afterCopy = func(dest string) {
		fh, _ := os.OpenFile(dest, os.O_WRONLY, 0)
		fh.WriteAt([]byte{0x00}, 70*1024)
		fh.Close()
	}
	at, err := execMoveFSAt(sess, p, p, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, nil, true, nil)
	if err == nil || !strings.Contains(err.Error(), "does not match the original") ||
		!strings.Contains(err.Error(), "the original is untouched") {
		t.Fatalf("a damaged copy must be refused with the original untouched, got %v", err)
	}
	var mbe movedButError
	if errors.As(err, &mbe) {
		t.Fatal("nothing moved, so the row must not prune — this is not a moved-but error")
	}
	if at != "" {
		t.Fatalf("nothing landed, yet a landing path was reported: %q", at)
	}
	if got, rerr := os.ReadFile(p); rerr != nil || !bytes.Equal(got, body) {
		t.Fatalf("the original must be exactly as it was: %v", rerr)
	}
	ents, _ := os.ReadDir(filepath.Join(f.root, "dest"))
	if len(ents) != 0 {
		t.Fatalf("the damaged copy (or its staging folder) was left at the destination: %v", ents)
	}
}

// An original that changes after its copy was verified is no longer the
// content the copy holds: it stays, under its own name, and both exist.
func TestTransitOriginalChangedAfterTheCopyIsKept(t *testing.T) {
	forceTransit(t)
	f, sess := newMoveVerifyFake(t)
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	p, want, _ := mkMoveFile(t, f, "a.bin")
	f.afterMove = func(dest string) { // the final hop into place
		os.WriteFile(p, []byte("rewritten meanwhile"), 0o644)
	}
	at, err := execMoveFSAt(sess, p, p, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, nil, true, nil)
	if err == nil || !strings.Contains(err.Error(), "both copies now exist") {
		t.Fatalf("a changed original must be kept beside its copy, got %v", err)
	}
	var mbe movedButError
	if errors.As(err, &mbe) {
		t.Fatal("the original is still at its path — the row must not prune")
	}
	if at == "" {
		t.Fatal("the verified copy is at the destination and must be reported as landed")
	}
	if got, _ := os.ReadFile(p); string(got) != "rewritten meanwhile" {
		t.Fatalf("the changed original must be back under its own name, got %q", got)
	}
	if l := leftovers(t, f.root); len(l) != 0 {
		t.Fatalf("hidden entries left behind: %v", l)
	}
}

func mkJunkFolder(t *testing.T, f *moveVerifyFake, name string) (string, entIdent) {
	t.Helper()
	d := filepath.Join(f.root, name)
	os.MkdirAll(filepath.Join(d, "@eaDir"), 0o755)
	os.WriteFile(filepath.Join(d, "x.tmp"), []byte("tmp bytes"), 0o644)
	os.WriteFile(filepath.Join(d, "film.part"), bytes.Repeat([]byte{7}, 5000), 0o644)
	os.WriteFile(filepath.Join(d, "@eaDir", "thumb"), []byte("t"), 0o644)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(d, old, old)
	fi, _ := os.Stat(d)
	return d, entIdent{isDir: true, mod: fmtTime(fi.ModTime()), modUnix: fi.ModTime().Unix()}
}

// A junk-only folder carries its junk across with it, and junk can be
// large — a half-finished download of a film. Every file in the copy is
// checked before the original folder goes.
func TestTransitFolderIsCheckedFileByFileThenRemoved(t *testing.T) {
	forceTransit(t)
	f, sess := newMoveVerifyFake(t)
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	d, want := mkJunkFolder(t, f, "junk")
	at, err := execMoveFSAt(sess, d, d, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, nil, true, nil)
	if err != nil {
		t.Fatalf("verified folder transit failed: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(at, "film.part")); len(got) != 5000 {
		t.Fatal("the folder's files did not arrive")
	}
	if _, serr := os.Stat(d); !os.IsNotExist(serr) {
		t.Fatal("the original folder must be removed once its copy is verified")
	}
	if l := leftovers(t, f.root); len(l) != 0 {
		t.Fatalf("hidden entries left behind: %v", l)
	}
}

func TestTransitDamagedFileInsideAFolderKeepsTheFolder(t *testing.T) {
	forceTransit(t)
	f, sess := newMoveVerifyFake(t)
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	d, want := mkJunkFolder(t, f, "junk")
	f.afterCopy = func(dest string) {
		os.WriteFile(filepath.Join(dest, "film.part"), bytes.Repeat([]byte{8}, 5000), 0o644)
	}
	_, err := execMoveFSAt(sess, d, d, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, nil, true, nil)
	if err == nil || !strings.Contains(err.Error(), "the original is untouched") {
		t.Fatalf("a damaged file inside the copy must keep the folder, got %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(d, "film.part")); !bytes.Equal(got, bytes.Repeat([]byte{7}, 5000)) {
		t.Fatal("the original folder's files must be untouched")
	}
	if ents, _ := os.ReadDir(filepath.Join(f.root, "dest")); len(ents) != 0 {
		t.Fatalf("the damaged copy was left at the destination: %v", ents)
	}
}

// A file that lands in the original folder after the copy was taken would be
// deleted with the folder. It is not: the folder is renamed out of reach and
// compared entry by entry first, and put back when anything differs.
func TestTransitFolderThatGainedAFileIsKept(t *testing.T) {
	forceTransit(t)
	f, sess := newMoveVerifyFake(t)
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	d, want := mkJunkFolder(t, f, "junk")
	f.afterMove = func(string) {
		os.WriteFile(filepath.Join(d, "new.tmp"), []byte("arrived late"), 0o644)
		old := time.Unix(want.modUnix, 0)
		os.Chtimes(d, old, old) // even with the folder's own time put back
	}
	_, err := execMoveFSAt(sess, d, d, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, nil, true, nil)
	if err == nil || !strings.Contains(err.Error(), "both copies now exist") {
		t.Fatalf("a folder that gained a file must be kept, got %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(d, "new.tmp")); string(got) != "arrived late" {
		t.Fatal("the late file must survive, in the original folder under its own name")
	}
	for _, p := range f.deleted {
		if strings.Contains(p, ".dupfinder-del-") {
			t.Fatalf("the retired original was deleted despite the difference: %s", p)
		}
	}
}

// On one filesystem a verified move stays a single File Station move — no
// copy, no extra space, no extra read of the original.
func TestVerifiedMoveOnOneFilesystemDoesNotCopy(t *testing.T) {
	old := crossesFS
	crossesFS = func(string, string) bool { return false }
	t.Cleanup(func() { crossesFS = old })
	f, sess := newMoveVerifyFake(t)
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	p, want, _ := mkMoveFile(t, f, "a.bin")
	if err := execMoveFS(sess, p, p, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, nil, true, nil); err != nil {
		t.Fatal(err)
	}
	if f.copies != 0 {
		t.Fatalf("a move on one filesystem made %d copies", f.copies)
	}
}

// Unverified moves keep File Station's single move whatever the filesystems:
// the user chose not to pay for the check.
func TestUnverifiedTransitIsASingleMove(t *testing.T) {
	forceTransit(t)
	f, sess := newMoveVerifyFake(t)
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	p, want, _ := mkMoveFile(t, f, "a.bin")
	if err := execMoveFS(sess, p, p, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	if f.copies != 0 {
		t.Fatalf("an unverified move made %d copies", f.copies)
	}
}

// A name clash at the destination still never overwrites: the verified copy
// lands under the next free name.
func TestTransitNameClashLandsBesideTheExistingFile(t *testing.T) {
	forceTransit(t)
	f, sess := newMoveVerifyFake(t)
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	os.WriteFile(filepath.Join(f.root, "dest", "a.bin"), []byte("theirs"), 0o644)
	p, want, body := mkMoveFile(t, f, "a.bin")
	at, err := execMoveFSAt(sess, p, p, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(at) != "a (1).bin" {
		t.Fatalf("landed as %q", at)
	}
	if got, _ := os.ReadFile(filepath.Join(f.root, "dest", "a.bin")); string(got) != "theirs" {
		t.Fatal("the existing file was overwritten")
	}
	if got, _ := os.ReadFile(at); !bytes.Equal(got, body) {
		t.Fatal("the copy is not intact")
	}
}
