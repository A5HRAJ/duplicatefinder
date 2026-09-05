package main

// Tests pinning the daemon's hardening properties: canonical reference
// protection, spill bounds, synced atomic writes and epoch identity. The
// parser bounds live with the parsers, in internal/media.

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A reference folder given through a symlink alias protects the rows that
// display under the real path — decided through the root table, without a
// syscall per row — exactly as the move refuses them.
func TestRefMatcherProtectsThroughAlias(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "Backups")
	if err := os.MkdirAll(filepath.Join(real, "B"), 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "Balias")
	if err := os.Symlink(filepath.Join(real, "B"), alias); err != nil {
		t.Fatal(err)
	}
	canon, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	m := newRefMatcher([]string{alias}, []RootMap{{Raw: real, Canon: canon}})
	if !m.protects(filepath.Join(real, "B"), "clip.mov") {
		t.Fatal("aliased reference folder must protect the row under its real path")
	}
	if m.protects(filepath.Join(real, "A"), "clip.mov") {
		t.Fatal("unrelated row protected")
	}
	// Results persisted without a root table still match on the raw form.
	m2 := newRefMatcher([]string{filepath.Join(real, "B")}, nil)
	if !m2.protects(filepath.Join(real, "B"), "x") {
		t.Fatal("raw prefix must still protect")
	}
	if !newRefMatcher(nil, nil).empty() || newRefMatcher([]string{"/v/x"}, nil).empty() {
		t.Fatal("empty() wrong")
	}
	groups := []Group{{Size: 5, Files: []FileEnt{{Dir: filepath.Join(real, "B"), Name: "a"}, {Dir: filepath.Join(real, "A"), Name: "a"}}}}
	markGroupProt(groups, m)
	if groups[0].Prot != 1 || !groups[0].Files[0].Prot || groups[0].Files[1].Prot {
		t.Fatalf("per-row prot not flagged: %+v", groups[0])
	}
	if tot := dupTotals(groups, m); tot.Reclaimable != 5 {
		t.Fatalf("one protected copy of two: reclaimable = %d, want 5", tot.Reclaimable)
	}
}

// A corrupt record length must fail the scan, not allocate terabytes.
func TestSpillRejectsCorruptRecordLengths(t *testing.T) {
	sp, err := newSpill(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer sp.close()
	var buf [binary.MaxVarintLen64]byte
	put := func(v uint64) { n := binary.PutUvarint(buf[:], v); sp.w.Write(buf[:n]) }
	put(0)  // rootIdx
	put(10) // size
	n := binary.PutVarint(buf[:], 0)
	sp.w.Write(buf[:n]) // mod
	put(0)              // tag
	put(1 << 40)        // relLen: a terabyte
	sp.n = 1
	err = sp.each(func(*spillRec) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("corrupt spill accepted: %v", err)
	}
}

func TestWriteAtomicPersistsAndRefusesReplace(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := writeAtomic(p, 0o600, false, func(f *os.File) error { _, err := f.WriteString("one"); return err }); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "one" {
		t.Fatalf("content = %q", b)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode())
	}
	if err := writeAtomic(p, 0o600, true, func(f *os.File) error { _, err := f.WriteString("two"); return err }); err == nil {
		t.Fatal("noReplace must refuse an existing file")
	}
	if b, _ := os.ReadFile(p); string(b) != "one" {
		t.Fatalf("refused write changed the file: %q", b)
	}
	if err := writeAtomic(p, 0o600, false, func(f *os.File) error { _, err := f.WriteString("two"); return err }); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "two" {
		t.Fatalf("replace failed: %q", b)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
}

// The move-time identity compares the epoch mtime where the scan recorded
// one, so a NAS whose time zone changed since the scan does not refuse every
// file; rows an older build persisted fall back to the string form.
func TestIdentMatchesUsesEpochWhenRecorded(t *testing.T) {
	var e fsEntry
	e.Additional.Size = 10
	e.Additional.Time.Mtime = 1700000000
	if _, ok := identMatches(e, []entIdent{{size: 10, mod: "some other zone's string", modUnix: 1700000000}}); !ok {
		t.Fatal("the epoch must decide when recorded")
	}
	if _, ok := identMatches(e, []entIdent{{size: 10, mod: fmtTime(time.Unix(1700000000, 0))}}); !ok {
		t.Fatal("string fallback must still match older rows")
	}
	if _, ok := identMatches(e, []entIdent{{size: 10, modUnix: 1700000001}}); ok {
		t.Fatal("a different epoch must not match")
	}
	if _, ok := identMatches(e, []entIdent{{size: 11, modUnix: 1700000000}}); ok {
		t.Fatal("a different size must not match")
	}
}
