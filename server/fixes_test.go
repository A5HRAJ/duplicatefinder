package main

// Tests pinning the 2026-09-04 review fixes: parser bounds on the 32-bit
// build, the archive entry cap, canonical reference protection, spill
// bounds, synced atomic writes, epoch identity and date plausibility.

import (
	"archive/zip"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// isoBox builds an ISOBMFF box: uint32 size, 4-char type, payload.
func isoBox(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[0:4], uint32(8+len(payload)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

func fileWith(t *testing.T, data []byte) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "box-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// An infe size just under 2^31 used to wrap `pos+size` negative on the
// 32-bit build, pass the bound and panic in the slice; the guard is now a
// subtraction from len(buf), which cannot wrap.
func TestHeifChildSizeNearIntMaxIsRefused(t *testing.T) {
	payload := []byte{0, 0, 0, 0, 0, 1} // version 0, flags, entry_count = 1
	child := make([]byte, 24)
	binary.BigEndian.PutUint32(child[0:4], 0x7FFFFFF0)
	copy(child[4:8], "infe")
	payload = append(payload, child...)
	data := isoBox("iinf", payload)
	f := fileWith(t, data)
	if id, ok := heifExifItemID(f, 0, int64(len(data))); ok {
		t.Fatalf("hostile size accepted: id=%d", id)
	}
}

func TestQtKeySizeNearIntMaxIsRefused(t *testing.T) {
	kp := []byte{0, 0, 0, 0, 0, 0, 0, 1} // version/flags, count = 1
	entry := make([]byte, 16)
	binary.BigEndian.PutUint32(entry[0:4], 0x7FFFFFF0)
	copy(entry[4:8], "mdta")
	kp = append(kp, entry...)
	data := isoBox("keys", kp)
	f := fileWith(t, data)
	if d := qtKeysIlst(f, 0, int64(len(data))); d != "" {
		t.Fatalf("hostile size produced a date %q", d)
	}
}

// Extents whose offset, length and index fields occupy zero bytes consume
// nothing, so count × extent_count iterations ran on a buffer heifBoxMax
// keeps small — hours of CPU from one crafted file. Refused outright now.
func TestHeifIlocZeroSizedExtentsIsBounded(t *testing.T) {
	p := []byte{0, 0, 0, 0, 0x00, 0x00} // version 0, flags, all size nibbles 0
	const count = 20000
	p = append(p, byte(count>>8), byte(count&0xFF))
	for i := 0; i < count; i++ {
		p = append(p, 0, 1, 0, 0, 0xFF, 0xFF) // item_id, data_ref_idx, extent_count = 65535
	}
	data := isoBox("iloc", p)
	f := fileWith(t, data)
	start := time.Now()
	if _, _, ok := heifItemLocation(f, 0, int64(len(data)), 1); ok {
		t.Fatal("zero-sized extents located an item")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("iloc walk took %v", d)
	}
}

func TestZipEntryCountReadsTheDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, n := range []string{"a", "b", "c"} {
		w, _ := zw.Create(n)
		w.Write([]byte("x"))
	}
	zw.Close()
	f.Close()
	rf, _ := os.Open(path)
	defer rf.Close()
	st, _ := rf.Stat()
	if n, ok := zipEntryCount(rf, st.Size()); !ok || n != 3 {
		t.Fatalf("entry count = %d ok=%v, want 3", n, ok)
	}
	nf := fileWith(t, []byte("not a zip at all, but long enough to hold an EOCD record"))
	if _, ok := zipEntryCount(nf, 60); ok {
		t.Fatal("no EOCD must report !ok")
	}
}

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

func TestCaptureDateWindowAndFormatter(t *testing.T) {
	if captureDate(time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)) != "" {
		t.Fatal("a 1904 date must read as absent")
	}
	if parseISODate("1904-01-01T00:00:00Z") != "" || parseISODate("0001-01-01T00:00:00") != "" {
		t.Fatal("implausible ISO dates must read as absent")
	}
	if parseISODate("2023-06-22T01:11:21+0200") == "" {
		t.Fatal("a plausible ISO date must format")
	}
	for n, want := range map[int64]string{512: "512 B", 1536: "1.50 KB", 15360: "15.0 KB", 153600: "150 KB", 0: "0 B"} {
		if got := fmtBytesGo(n); got != want {
			t.Fatalf("fmtBytesGo(%d) = %q, want %q", n, got, want)
		}
	}
}
