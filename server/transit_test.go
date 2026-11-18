package main

// Verified moves between filesystems (transit.go): the original must stay in
// place until a copy at the destination has been read back and matched, and
// whatever goes wrong before that point must leave the original exactly as it
// was. The fake File Station really copies on remove_src=false, so the
// verification reads run against real bytes.

import (
	"bytes"
	"errors"
	"io/fs"
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

// exfatName is what File Station made of a name on the DS124's exFAT USB
// disk (DSM 7.4.1): every character exFAT refuses became "-".
func exfatName(name string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(`"*:<>?\|`, r) {
			return '-'
		}
		return r
	}, name)
}

// A destination that cannot store the name as it is gets the copy under the
// name File Station chose. The verified move still completes, the original
// goes only once that copy has been read back, and the landing path names
// the file that is really there — not the name it was asked to have.
func TestTransitFollowsANameTheDestinationChanged(t *testing.T) {
	forceTransit(t)
	f, sess := newMoveVerifyFake(t)
	f.storeAs = exfatName
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	p, want, body := mkMoveFile(t, f, "what?.bin")
	at, err := execMoveFSAt(sess, p, p, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, nil, true, nil)
	if err != nil {
		t.Fatalf("the move failed: %v", err)
	}
	if at != filepath.Join(f.root, "dest", "what-.bin") {
		t.Fatalf("landing path %q, want the name File Station gave it", at)
	}
	if got, rerr := os.ReadFile(at); rerr != nil || !bytes.Equal(got, body) {
		t.Fatalf("the copy is not intact: %v", rerr)
	}
	if _, serr := os.Stat(p); !os.IsNotExist(serr) {
		t.Fatal("the original must go once its copy is verified")
	}
	if l := leftovers(t, f.root); len(l) != 0 {
		t.Fatalf("staging entries left behind: %v", l)
	}
	// A clash is judged on the name it will really have.
	p2, want2, _ := mkMoveFile(t, f, "what:.bin")
	at2, err := execMoveFSAt(sess, p2, p2, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want2}, nil, true, nil)
	if err != nil || filepath.Base(at2) != "what- (1).bin" {
		t.Fatalf("second file landed as %q (%v), want beside the first", at2, err)
	}
	if got, _ := os.ReadFile(at); !bytes.Equal(got, body) {
		t.Fatal("the first copy was overwritten")
	}
}

// The same without verification: the move records where the file really
// landed, so a copy this app moved is never mistaken for an original under
// a name nobody looks at.
func TestUnverifiedMoveFollowsANameTheDestinationChanged(t *testing.T) {
	forceTransit(t)
	f, sess := newMoveVerifyFake(t)
	f.storeAs = exfatName
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	p, want, body := mkMoveFile(t, f, "a:b.bin")
	at, err := execMoveFSAt(sess, p, p, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, nil, false, nil)
	if err != nil {
		t.Fatalf("the move failed: %v", err)
	}
	if at != filepath.Join(f.root, "dest", "a-b.bin") {
		t.Fatalf("landing path %q, want the name File Station gave it", at)
	}
	if got, rerr := os.ReadFile(at); rerr != nil || !bytes.Equal(got, body) {
		t.Fatalf("the moved file is not there: %v", rerr)
	}
	if l := leftovers(t, f.root); len(l) != 0 {
		t.Fatalf("staging entries left behind: %v", l)
	}
}

// A folder whose contents the destination renames is not "damaged in
// transit": the copy is discarded, the original stays, and the reason says
// what really happened.
func TestTransitFolderWithNamesTheDestinationChangesSaysSo(t *testing.T) {
	forceTransit(t)
	f, sess := newMoveVerifyFake(t)
	f.storeAs = exfatName
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	d, want := mkJunkFolder(t, f, "junk")
	os.WriteFile(filepath.Join(d, "odd?.tmp"), []byte("tmp"), 0o644)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(d, old, old)
	fi, _ := os.Stat(d)
	want.mod, want.modUnix = fmtTime(fi.ModTime()), fi.ModTime().Unix()
	// copyTree keeps inner names; rename the inner entry the way the
	// destination would, once the copy lands.
	f.afterCopy = func(dst string) {
		os.Rename(filepath.Join(dst, "odd?.tmp"), filepath.Join(dst, "odd-.tmp"))
	}
	_, err := execMoveFSAt(sess, d, d, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, nil, true, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot store every name") || strings.Contains(err.Error(), "damaged") {
		t.Fatalf("want the renaming named as the reason, got %v", err)
	}
	if _, serr := os.Stat(filepath.Join(d, "odd?.tmp")); serr != nil {
		t.Fatal("the original folder must be untouched")
	}
	if l := leftovers(t, f.root); len(l) != 0 {
		t.Fatalf("staging entries left behind: %v", l)
	}
}

// A copy that cannot be read back for a reason other than access does not
// send the user to the share's permissions.
func TestReadBackFailureBlamesAccessOnlyWhenItIsAccess(t *testing.T) {
	if m := readBackFailure(fs.ErrPermission); !strings.Contains(m, "grant the package user") {
		t.Errorf("a refused read must point at permissions: %q", m)
	}
	if m := readBackFailure(fs.ErrNotExist); strings.Contains(m, "grant") {
		t.Errorf("a missing file must not point at permissions: %q", m)
	}
}

// mkNestedJunkFolder is mkJunkFolder with folders inside: an empty one, and
// one holding junk of its own. That is what an Empty Folders row now is
// whenever the walk found only empty folders under the one it offers.
func mkNestedJunkFolder(t *testing.T, f *moveVerifyFake, name string) (string, entIdent) {
	t.Helper()
	d, _ := mkJunkFolder(t, f, name)
	os.MkdirAll(filepath.Join(d, "empty", "deeper"), 0o755)
	os.MkdirAll(filepath.Join(d, "sub", "@eaDir"), 0o755)
	os.WriteFile(filepath.Join(d, "sub", "y.tmp"), bytes.Repeat([]byte{9}, 3000), 0o644)
	old := time.Now().Add(-time.Hour)
	for _, p := range []string{filepath.Join(d, "empty", "deeper"), filepath.Join(d, "empty"), filepath.Join(d, "sub"), d} {
		os.Chtimes(p, old, old)
	}
	fi, _ := os.Stat(d)
	return d, entIdent{isDir: true, mod: fmtTime(fi.ModTime()), modUnix: fi.ModTime().Unix()}
}

// Across filesystems a folder of folders is copied whole, every file at
// every depth checked, and only then is the original removed.
func TestTransitFolderOfFoldersIsCheckedThenRemoved(t *testing.T) {
	forceTransit(t)
	f, sess := newMoveVerifyFake(t)
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	d, want := mkNestedJunkFolder(t, f, "junk")
	at, err := execMoveFSAt(sess, d, d, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, nil, true, nil)
	if err != nil {
		t.Fatalf("verified transit of a folder holding folders failed: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(at, "sub", "y.tmp")); len(got) != 3000 {
		t.Fatal("the nested folder's junk did not arrive")
	}
	if fi, serr := os.Stat(filepath.Join(at, "empty", "deeper")); serr != nil || !fi.IsDir() {
		t.Fatal("the nested empty folders did not arrive")
	}
	if _, serr := os.Stat(d); !os.IsNotExist(serr) {
		t.Fatal("the original folder must be removed once its copy is verified")
	}
	if l := leftovers(t, f.root); len(l) != 0 {
		t.Fatalf("hidden entries left behind: %v", l)
	}
}

// A file damaged on its way, two folders down, keeps the original.
func TestTransitDamagedFileInANestedFolderKeepsTheFolder(t *testing.T) {
	forceTransit(t)
	f, sess := newMoveVerifyFake(t)
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	d, want := mkNestedJunkFolder(t, f, "junk")
	f.afterCopy = func(dest string) {
		os.WriteFile(filepath.Join(dest, "sub", "y.tmp"), bytes.Repeat([]byte{8}, 3000), 0o644)
	}
	_, err := execMoveFSAt(sess, d, d, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, nil, true, nil)
	if err == nil || !strings.Contains(err.Error(), "the original is untouched") {
		t.Fatalf("a damaged file deep inside the copy must keep the folder, got %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(d, "sub", "y.tmp")); !bytes.Equal(got, bytes.Repeat([]byte{9}, 3000)) {
		t.Fatal("the original's nested files must be untouched")
	}
	if ents, _ := os.ReadDir(filepath.Join(f.root, "dest")); len(ents) != 0 {
		t.Fatalf("the damaged copy was left at the destination: %v", ents)
	}
}

// A file that lands in a NESTED folder after the copy was taken would be
// deleted with the original. It is not, even with every folder's own time
// put back: the comparison before removal reaches every depth.
func TestTransitNestedFolderThatGainedAFileIsKept(t *testing.T) {
	forceTransit(t)
	f, sess := newMoveVerifyFake(t)
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	d, want := mkNestedJunkFolder(t, f, "junk")
	deeper := filepath.Join(d, "empty", "deeper")
	fi, _ := os.Stat(deeper)
	f.afterMove = func(string) {
		os.WriteFile(filepath.Join(deeper, "late.txt"), []byte("arrived late"), 0o644)
		os.Chtimes(deeper, fi.ModTime(), fi.ModTime())
	}
	_, err := execMoveFSAt(sess, d, d, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, nil, true, nil)
	if err == nil || !strings.Contains(err.Error(), "both copies now exist") {
		t.Fatalf("a folder whose nested folder gained a file must be kept, got %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(deeper, "late.txt")); string(got) != "arrived late" {
		t.Fatal("the late file must survive in the original folder")
	}
	for _, p := range f.deleted {
		if strings.Contains(p, ".dupfinder-del-") {
			t.Fatalf("the retired original was deleted despite the difference: %s", p)
		}
	}
}

// sameSecond returns a time in the same whole second as t but not equal to
// it: what a write lands on when it follows another within the second.
func sameSecond(t time.Time) time.Time {
	s := t.Truncate(time.Second)
	return s.Add(time.Duration((t.Nanosecond() + 500_000_000) % 1_000_000_000))
}

// An original rewritten in place after its copy was verified — same length,
// within the same second — is newer than the copy at the destination. It
// must be kept, never deleted on a size-and-seconds match.
func TestTransitKeepsANewerVersionWrittenInTheSameSecond(t *testing.T) {
	forceTransit(t)
	f, sess := newMoveVerifyFake(t)
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	p, want, body := mkMoveFile(t, f, "a.bin")
	fi0, _ := os.Stat(p)
	newer := bytes.Repeat([]byte{0x43}, len(body))
	f.afterMove = func(string) {
		os.WriteFile(p, newer, 0o644)
		ts := sameSecond(fi0.ModTime())
		os.Chtimes(p, ts, ts)
	}
	_, err := execMoveFSAt(sess, p, p, "/Backups/dest", filepath.Join(f.root, "dest"), []entIdent{want}, nil, true, nil)
	if err == nil || !strings.Contains(err.Error(), "both copies now exist") {
		t.Fatalf("a newer original must be kept beside its older copy, got %v", err)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, newer) {
		t.Fatal("the newer version of the original was lost")
	}
}

// The same inside a folder: a junk file rewritten in place after the copy
// was verified keeps the folder.
func TestTransitKeepsAFolderWhoseFileWasRewrittenInTheSameSecond(t *testing.T) {
	forceTransit(t)
	f, sess := newMoveVerifyFake(t)
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	d, want := mkJunkFolder(t, f, "junk")
	inner := filepath.Join(d, "film.part")
	fi0, _ := os.Stat(inner)
	newer := bytes.Repeat([]byte{9}, int(fi0.Size()))
	f.afterMove = func(string) {
		os.WriteFile(inner, newer, 0o644)
		ts := sameSecond(fi0.ModTime())
		os.Chtimes(inner, ts, ts)
	}
	_, err := execMoveFSAt(sess, d, d, "/Backups/dest", filepath.Join(f.root, "dest"), []entIdent{want}, nil, true, nil)
	if err == nil || !strings.Contains(err.Error(), "both copies now exist") {
		t.Fatalf("a folder whose file was rewritten must be kept, got %v", err)
	}
	if got, _ := os.ReadFile(inner); !bytes.Equal(got, newer) {
		t.Fatal("the newer version of the folder's file was lost")
	}
}

// A real file that arrives inside an "empty" folder after the move-time
// emptiness check, but before the folder's contents are read for the copy,
// must not ride along: the contents read for the copy are held to the same
// rule. It lands in a subfolder, so the folder's own identity still matches.
func TestTransitRefusesAFolderThatGainedARealFileBeforeItsCopy(t *testing.T) {
	forceTransit(t)
	f, sess := newMoveVerifyFake(t)
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	d := filepath.Join(f.root, "archive")
	os.MkdirAll(filepath.Join(d, "sub"), 0o755)
	os.WriteFile(filepath.Join(d, "film.part"), []byte("junk"), 0o644)
	fi, _ := os.Stat(d)
	want := entIdent{isDir: true, mod: fmtTime(fi.ModTime()), modUnix: fi.ModTime().Unix()}
	late := filepath.Join(d, "sub", "report.pdf")
	os.WriteFile(late, []byte("a real file, arrived late"), 0o644)
	_, err := execMoveFSAt(sess, d, d, "/Backups/dest", filepath.Join(f.root, "dest"), []entIdent{want}, nil, true, nil)
	if err == nil || !strings.Contains(err.Error(), "no longer empty") {
		t.Fatalf("a folder that gained a real file must be refused, got %v", err)
	}
	if _, serr := os.Stat(late); serr != nil {
		t.Fatal("the real file left its folder")
	}
	if ents, _ := os.ReadDir(filepath.Join(f.root, "dest")); len(ents) != 0 {
		t.Fatalf("something reached the destination: %v", ents)
	}
}

// The production filesystem check, not a stub: two folders on one
// filesystem are not "across filesystems", so a verified move between them
// is a rename that copies nothing.
func TestTwoFoldersOnOneFilesystemDoNotCross(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	os.Mkdir(a, 0o755)
	os.Mkdir(b, 0o755)
	if crossesFS(a, b) || crossesFS(a, a) {
		t.Fatal("two folders on one filesystem were taken for two filesystems")
	}
}
