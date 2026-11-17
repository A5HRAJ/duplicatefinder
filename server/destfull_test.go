package main

// A destination that fills up partway through a move (something else writing
// to the volume), and a folder moved into itself. In every case the original
// stays where it was, the move is reported as not done, and nothing the app
// cannot prove spare is deleted.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDestinationFillingUpMidMoveLosesNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    int  // what File Station answers
		verify  bool // verified moves across filesystems copy, check, then retire
		partial bool // whether the direct attempt's incomplete copy is still at the name
	}{
		{"no space, unverified", 416, false, true},
		{"generic failure, unverified", 1000, false, true},
		{"no space, verified", 416, true, false},
		{"generic failure, verified", 1000, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forceTransit(t)
			f, sess := newMoveVerifyFake(t)
			dest := filepath.Join(f.root, "dest")
			os.Mkdir(dest, 0o755)
			p, want, body := mkMoveFile(t, f, "a.bin")
			f.fillDest = tc.code
			_, err := execMoveFSAt(sess, p, p, "/Backups/dest", dest, []entIdent{want}, nil, tc.verify, nil)
			if err == nil {
				t.Fatal("a move onto a full destination reported success")
			}
			var mbe movedButError
			var pe parkedError
			if errors.As(err, &mbe) || errors.As(err, &pe) {
				t.Fatalf("reported as moved (%T: %v) while the original never left", err, err)
			}
			if got, rerr := os.ReadFile(p); rerr != nil || !bytes.Equal(got, body) {
				t.Fatalf("the original is not intact: %v", rerr)
			}
			if !strings.Contains(err.Error(), "original") {
				t.Fatalf("the report does not say what became of the original: %v", err)
			}
			if tc.code == 416 && !strings.Contains(err.Error(), "no space left") {
				t.Fatalf("the report does not say the destination is full: %v", err)
			}
			if l := leftovers(t, dest); len(l) != 0 {
				t.Fatalf("staging left behind: %v", l)
			}
			_, serr := os.Stat(filepath.Join(dest, "a.bin"))
			if tc.partial != (serr == nil) {
				t.Fatalf("incomplete copy at the name: present=%v, want %v", serr == nil, tc.partial)
			}
			if tc.partial && !strings.Contains(err.Error(), "incomplete copy at /Backups/dest/a.bin") {
				t.Fatalf("the incomplete copy left at the destination is not reported: %v", err)
			}
		})
	}
}

// A staged move refused after a COMPLETE copy arrived is not cleaned up: with
// the original still in place nothing proves which is spare beyond size and
// time, so the copy stays, reported by path, and the original is not
// reported as moved.
func TestFullCopyLeftInStagingIsKeptAndReported(t *testing.T) {
	f, sess := newMoveVerifyFake(t)
	dest := filepath.Join(f.root, "dest")
	os.Mkdir(dest, 0o755)
	p, want, body := mkMoveFile(t, f, "a.bin")
	// Driven directly: a full-size copy waits in a staging folder, the
	// original is still in place, and File Station's answer was a failure.
	tmp := ".dupfinder-tmp-0000000000000000"
	os.Mkdir(filepath.Join(dest, tmp), 0o755)
	os.WriteFile(filepath.Join(dest, tmp, "a.bin"), body, 0o644)
	got := sess.cutShortCopy(errors.New("File Station CopyMove failed (error 1000)"), "/Backups/dest/"+tmp, "a.bin", "a.bin", want)
	if !strings.Contains(got.Error(), "left a copy in /Backups/dest/"+tmp) {
		t.Fatalf("a full copy left in staging is not reported by path: %v", got)
	}
	if _, serr := os.Stat(filepath.Join(dest, tmp, "a.bin")); serr != nil {
		t.Fatal("a full copy was deleted on size and time alone")
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, body) {
		t.Fatal("the original changed")
	}
}

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

// Results saved before 1.1.12 may hold a group member inside the recycle bin
// or a snapshot (such a folder could then be scanned directly). That copy
// disappears on its own, so it must never be the copy that stays: moving the
// one live copy is held back.
func TestACopyInTheRecycleBinNeverStays(t *testing.T) {
	f, sess := newMoveVerifyFake(t)
	live := filepath.Join(f.root, "photo.jpg")
	bin := filepath.Join(f.root, "#recycle", "photo.jpg")
	os.MkdirAll(filepath.Dir(bin), 0o755)
	for _, p := range []string{live, bin} {
		os.WriteFile(p, []byte("the only live copy"), 0o644)
	}
	ent := func(p string) FileEnt {
		fi, _ := os.Stat(p)
		return FileEnt{Dir: filepath.Dir(p), Name: filepath.Base(p), Size: fi.Size(), Mod: fmtTime(fi.ModTime()), ModUnix: fi.ModTime().Unix()}
	}
	s := &Server{results: map[string]*toolResult{"duplicates": {Tool: "duplicates",
		Groups: []Group{{ID: "g0", Files: []FileEnt{ent(live), ent(bin)}}}}}}
	v := s.newMoveVet(MoveReq{Tool: "duplicates", Files: []string{live}}, sess)
	if !v.drops[v.canon.path(live)] {
		t.Fatalf("the live copy may move while its only other copy sits in the recycle bin (drops %v)", v.drops)
	}
}

// A cut-short copy is removed only when it can be the file's own copy: its
// name, or the name a destination that cannot store some characters gives
// it ("what?.bin" arrives as "what-.bin"). Anything else in the staging
// folder is left and reported, however small it is.
func TestOnlyTheFilesOwnCutShortCopyIsRemoved(t *testing.T) {
	for _, tc := range []struct {
		base, staged string
		removed      bool
	}{
		{"a.bin", "a.bin", true},
		{"what?.bin", "what-.bin", true},
		{"a:b.bin", "a-b.bin", true},
		{"a.bin", "notes.txt", false},
		{"what?.bin", "what_.bin", false},
		{"what?.bin", "other.bin", false},
	} {
		t.Run(tc.base+" as "+tc.staged, func(t *testing.T) {
			f, sess := newMoveVerifyFake(t)
			dest := filepath.Join(f.root, "dest")
			tmp := ".dupfinder-tmp-0000000000000001"
			os.MkdirAll(filepath.Join(dest, tmp), 0o755)
			staged := filepath.Join(dest, tmp, tc.staged)
			os.WriteFile(staged, []byte("small"), 0o644)
			want := entIdent{size: 1 << 20}
			got := sess.cutShortCopy(errors.New("File Station CopyMove failed (error 1000)"), "/Backups/dest/"+tmp, tc.base, tc.staged, want)
			_, serr := os.Stat(staged)
			if removed := os.IsNotExist(serr); removed != tc.removed {
				t.Fatalf("removed=%v, want %v (%v)", removed, tc.removed, got)
			}
			if !tc.removed && !strings.Contains(got.Error(), "left a copy in") {
				t.Fatalf("a file left in staging is not reported: %v", got)
			}
		})
	}
}

// When even the look into the staging folder gets no answer, a file whose
// original is still in place is not reported as parked, and its row is not
// dropped: nothing says it left.
func TestALostLookIntoStagingDoesNotCallTheFileParked(t *testing.T) {
	f, sess := newMoveVerifyFake(t)
	dest := filepath.Join(f.root, "dest")
	os.Mkdir(dest, 0o755)
	p, want, body := mkMoveFile(t, f, "a.bin")
	os.WriteFile(filepath.Join(dest, "a.bin"), []byte("theirs"), 0o644) // forces staging
	f.fillDest = 1000
	f.loseStagingInfo = true // the look into the staging folder gets no answer
	_, err := execMoveFSAt(sess, p, p, "/Backups/dest", dest, []entIdent{want}, nil, false, nil)
	var mbe movedButError
	var pe parkedError
	if err == nil || errors.As(err, &mbe) || errors.As(err, &pe) {
		t.Fatalf("reported as moved or parked (%T: %v) while the original never left", err, err)
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, body) {
		t.Fatal("the original changed")
	}
}
