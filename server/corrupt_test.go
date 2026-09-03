package main

// Unit tests for corrupted-files detection and its verdicts. These build real
// files on disk and run the real window pass over them, because every
// interesting case here is about what the bytes actually are.

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixedTime is shared by every file that is meant to look like a copy of
// another: the whole detector keys on size AND modified time agreeing.
var fixedTime = time.Date(2024, 3, 17, 9, 30, 0, 0, time.UTC)

func mkAt(t *testing.T, dir, name string, content []byte, mod time.Time) fEnt {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fEnt{path: p, name: filepath.Base(p), dir: filepath.Dir(p), size: fi.Size(), mod: fi.ModTime()}
}

// runCorrupt drives the real window pass and returns the finished result rows.
func runCorrupt(t *testing.T, files []fEnt) []Group {
	t.Helper()
	s := &Server{}
	acc := newCorruptTop(corruptFileCap)
	cancel := make(chan struct{})
	if !s.corruptWindow(files, nil, nil, cancel, acc, 0, 1) {
		t.Fatal("corruptWindow reported cancellation")
	}
	groups, _ := acc.final(s, corruptFileCap, cancel)
	return groups
}

func verdictOf(g *Group, dir string) string {
	for _, f := range g.Files {
		if filepath.Base(f.Dir) == dir {
			return f.Verdict
		}
	}
	return "<missing>"
}

// The core rule: same size and same modified time, different content. Files
// that agree on all three, or that differ in modified time, are not sets.
func TestCorruptedSetsAreSizeAndTimeCollisionsThatDiffer(t *testing.T) {
	dir := t.TempDir()
	var files []fEnt

	// A real set: identical size and mtime, different bytes, same name.
	files = append(files,
		mkAt(t, dir, "a/report.txt", bytes.Repeat([]byte("A"), 4096), fixedTime),
		mkAt(t, dir, "b/report.txt", bytes.Repeat([]byte("B"), 4096), fixedTime))

	// Genuinely identical copies: a duplicate, not a corruption.
	same := bytes.Repeat([]byte("same"), 512)
	files = append(files,
		mkAt(t, dir, "a/twin.bin", same, fixedTime),
		mkAt(t, dir, "b/twin.bin", same, fixedTime))

	// Same size, different mtime: an ordinary edit, not a corruption.
	files = append(files,
		mkAt(t, dir, "a/edited.bin", bytes.Repeat([]byte("x"), 300), fixedTime),
		mkAt(t, dir, "b/edited.bin", bytes.Repeat([]byte("y"), 300), fixedTime.Add(time.Hour)))

	// A lone file has nothing to disagree with.
	files = append(files, mkAt(t, dir, "a/only.bin", []byte("solitary"), fixedTime))

	groups := runCorrupt(t, files)
	if len(groups) != 1 {
		var got []string
		for _, g := range groups {
			got = append(got, g.Files[0].Name)
		}
		t.Fatalf("want exactly 1 corrupted set, got %d %v", len(groups), got)
	}
	g := groups[0]
	if g.Files[0].Name != "report.txt" {
		t.Fatalf("wrong set reported: %s", g.Files[0].Name)
	}
	if g.Variants != 2 {
		t.Errorf("want 2 variants, got %d", g.Variants)
	}
	if !g.SameName {
		t.Error("both members are report.txt — SameName should be set")
	}
	if g.Mod == "" {
		t.Error("a set must carry the modified time it is built on")
	}
	// Two plain text files give no evidence about which is which, and saying
	// so is the honest answer.
	for _, f := range g.Files {
		if f.Verdict != verdictUnknown {
			t.Errorf("%s: want %q with no evidence available, got %q (%s)", f.Name, verdictUnknown, f.Verdict, f.Evidence)
		}
	}
}

// Files sharing a size and mtime under DIFFERENT names are still reported —
// the pairing rule is size and time — but SameName records that the copy
// relation is not established, which is what ranks the set lower.
func TestDifferentNamesStillFormASetButAreNotNameMatched(t *testing.T) {
	dir := t.TempDir()
	files := []fEnt{
		mkAt(t, dir, "a/alpha.bin", bytes.Repeat([]byte("1"), 2048), fixedTime),
		mkAt(t, dir, "b/beta.bin", bytes.Repeat([]byte("2"), 2048), fixedTime),
	}
	groups := runCorrupt(t, files)
	if len(groups) != 1 {
		t.Fatalf("want 1 set, got %d", len(groups))
	}
	if groups[0].SameName {
		t.Error("alpha.bin and beta.bin do not share a name")
	}
}

// A copy whose tail is zeros where its twin holds data is an interrupted
// transfer, and that direction is decidable.
func TestZeroTailIdentifiesTheTruncatedCopy(t *testing.T) {
	dir := t.TempDir()
	good := make([]byte, 8192)
	for i := range good {
		good[i] = byte(i*7 + 3)
	}
	bad := append([]byte(nil), good...)
	for i := 4096; i < len(bad); i++ {
		bad[i] = 0
	}
	files := []fEnt{
		mkAt(t, dir, "good/movie.dat", good, fixedTime),
		mkAt(t, dir, "bad/movie.dat", bad, fixedTime),
	}
	groups := runCorrupt(t, files)
	if len(groups) != 1 {
		t.Fatalf("want 1 set, got %d", len(groups))
	}
	g := &groups[0]
	if got := verdictOf(g, "bad"); got != verdictCorrupt {
		t.Errorf("zero-filled copy: want %q, got %q", verdictCorrupt, got)
	}
	if got := verdictOf(g, "good"); got != verdictIntact {
		t.Errorf("copy holding data: want %q, got %q", verdictIntact, got)
	}
	for _, f := range g.Files {
		if f.Evidence == "" {
			t.Errorf("%s in %s: a verdict without evidence explains nothing", f.Name, f.Dir)
		}
	}
}

// A single flipped bit proves the set is damaged but is perfectly symmetric.
// Guessing a direction here would be worse than saying nothing, so the test
// pins that it stays Unknown WITH the finding recorded.
func TestSingleBitFlipIsReportedWithoutPickingASide(t *testing.T) {
	dir := t.TempDir()
	a := make([]byte, 4096)
	for i := range a {
		a[i] = byte(i % 251)
	}
	b := append([]byte(nil), a...)
	b[1000] ^= 0x08
	files := []fEnt{
		mkAt(t, dir, "one/data.raw", a, fixedTime),
		mkAt(t, dir, "two/data.raw", b, fixedTime),
	}
	groups := runCorrupt(t, files)
	if len(groups) != 1 {
		t.Fatalf("want 1 set, got %d", len(groups))
	}
	for _, f := range groups[0].Files {
		if f.Verdict != verdictUnknown {
			t.Errorf("%s: a symmetric bit flip must not convict a side, got %q", f.Dir, f.Verdict)
		}
		if f.Evidence == "" {
			t.Errorf("%s: the bit flip itself should still be reported", f.Dir)
		}
	}
}

// The strongest evidence available for the two-copy case: a PNG carries a
// CRC32 on every chunk, so a damaged copy convicts itself with no comparison.
func TestPNGChecksumConvictsTheDamagedCopy(t *testing.T) {
	dir := t.TempDir()
	good := goodPNG(t)
	bad := append([]byte(nil), good...)
	// Flip a byte inside the compressed image data, past the 8-byte signature
	// and the IHDR chunk, leaving the length unchanged.
	bad[len(bad)/2] ^= 0xFF
	if len(bad) != len(good) {
		t.Fatal("the corrupted copy must keep the same size")
	}
	files := []fEnt{
		mkAt(t, dir, "ok/photo.png", good, fixedTime),
		mkAt(t, dir, "rot/photo.png", bad, fixedTime),
	}
	groups := runCorrupt(t, files)
	if len(groups) != 1 {
		t.Fatalf("want 1 set, got %d", len(groups))
	}
	g := &groups[0]
	if got := verdictOf(g, "rot"); got != verdictCorrupt {
		t.Errorf("PNG failing its own CRC: want %q, got %q", verdictCorrupt, got)
	}
	if got := verdictOf(g, "ok"); got != verdictIntact {
		t.Errorf("PNG passing its own CRC beside a damaged twin: want %q, got %q", verdictIntact, got)
	}
}

// Three copies where two agree: the set is reported with the right variant
// count, and the odd one out is still judged on evidence rather than on being
// outnumbered.
func TestThreeCopiesReportVariantCount(t *testing.T) {
	dir := t.TempDir()
	body := bytes.Repeat([]byte("payload"), 300)
	odd := append([]byte(nil), body...)
	odd[10] = 'X'
	files := []fEnt{
		mkAt(t, dir, "a/doc.dat", body, fixedTime),
		mkAt(t, dir, "b/doc.dat", body, fixedTime),
		mkAt(t, dir, "c/doc.dat", odd, fixedTime),
	}
	groups := runCorrupt(t, files)
	if len(groups) != 1 {
		t.Fatalf("want 1 set, got %d", len(groups))
	}
	g := groups[0]
	if len(g.Files) != 3 {
		t.Fatalf("want all 3 members listed, got %d", len(g.Files))
	}
	if g.Variants != 2 {
		t.Errorf("want 2 distinct contents among 3 copies, got %d", g.Variants)
	}
}

// A file whose content changed while its size and modified time did not is
// convicted from the hash cache alone — no twin, no second read.
func TestCachedPrefixDisagreementConvicts(t *testing.T) {
	dir := t.TempDir()
	a := mkAt(t, dir, "a/ledger.bin", bytes.Repeat([]byte("O"), 2048), fixedTime)
	b := mkAt(t, dir, "b/ledger.bin", bytes.Repeat([]byte("N"), 2048), fixedTime)

	// Seed the store as an earlier scan would have, then rot the file
	// underneath it without touching size or mtime.
	cache := &hashCache{ents: map[uint64]hcEnt{}, gen: 1}
	pfx, err := hashFile(a.openContent, 64*1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	full, err := hashFile(a.openContent, -1, nil)
	if err != nil {
		t.Fatal(err)
	}
	cache.record(a.path, a.size, a.mod.Unix(), pfx, full)
	cache.gen++ // the next scan loads the store and advances the generation

	rotted := bytes.Repeat([]byte("O"), 2048)
	rotted[5] = 'Q'
	if err := os.WriteFile(a.path, rotted, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(a.path, fixedTime, fixedTime); err != nil {
		t.Fatal(err)
	}

	s := &Server{}
	acc := newCorruptTop(corruptFileCap)
	cancel := make(chan struct{})
	if !s.corruptWindow([]fEnt{a, b}, cache, nil, cancel, acc, 0, 1) {
		t.Fatal("cancelled")
	}
	groups, _ := acc.final(s, corruptFileCap, cancel)
	if len(groups) != 1 {
		t.Fatalf("want 1 set, got %d", len(groups))
	}
	if got := verdictOf(&groups[0], "a"); got != verdictCorrupt {
		t.Errorf("content changed under an unchanged mtime: want %q, got %q", verdictCorrupt, got)
	}
}

// Rot BEYOND the first 64 KiB leaves size, mtime and prefix all standing.
// The old cross-scan cache served the stale full hash for exactly this file,
// so the rotted copy still counted as identical to its twin and no set was
// ever reported. Now every scan re-reads in full, the pair's disagreement is
// seen, and the fresh hash displacing the old entry convicts the copy that
// moved.
func TestDeepRotBeyondPrefixConvicts(t *testing.T) {
	dir := t.TempDir()
	body := bytes.Repeat([]byte{0x5a}, 80*1024)
	a := mkAt(t, dir, "a/vault.dat", body, fixedTime)
	b := mkAt(t, dir, "b/vault.dat", body, fixedTime)

	// An earlier scan saw both copies identical.
	cache := &hashCache{ents: map[uint64]hcEnt{}, gen: 1}
	for _, f := range []fEnt{a, b} {
		pfx, err := hashFile(f.openContent, 64*1024, nil)
		if err != nil {
			t.Fatal(err)
		}
		full, err := hashFile(f.openContent, -1, nil)
		if err != nil {
			t.Fatal(err)
		}
		cache.record(f.path, f.size, f.mod.Unix(), pfx, full)
	}
	cache.gen++ // the next scan loads the store and advances the generation

	// Copy "a" rots past the prefix; size and mtime are untouched.
	rotted := append([]byte(nil), body...)
	rotted[70*1024] ^= 0xff
	if err := os.WriteFile(a.path, rotted, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(a.path, fixedTime, fixedTime); err != nil {
		t.Fatal(err)
	}

	s := &Server{}
	acc := newCorruptTop(corruptFileCap)
	cancel := make(chan struct{})
	if !s.corruptWindow([]fEnt{a, b}, cache, nil, cancel, acc, 0, 1) {
		t.Fatal("cancelled")
	}
	groups, _ := acc.final(s, corruptFileCap, cancel)
	if len(groups) != 1 {
		t.Fatalf("deep rot went unseen — want 1 set, got %d", len(groups))
	}
	if got := verdictOf(&groups[0], "a"); got != verdictCorrupt {
		t.Errorf("the copy whose full hash moved under unchanged metadata: want %q, got %q", verdictCorrupt, got)
	}
	if got := verdictOf(&groups[0], "b"); got == verdictCorrupt {
		t.Errorf("the unchanged twin must not be convicted, got %q", got)
	}
	for _, fe := range groups[0].Files {
		if fe.Verdict == verdictCorrupt && !strings.Contains(fe.Evidence, "content changed since an earlier scan") {
			t.Errorf("conviction should carry the history evidence, got %q", fe.Evidence)
		}
	}
}

// A user saving a file WHILE the scan runs moves its content and its on-disk
// mtime — but the scan holds only the walk-era stat, so both sides of a
// same-generation comparison carry the one stale observation and the mtime
// "match" is vacuous. That is an edit, not rot, and convicting it as rot with
// "nothing edited this file" is exactly the false conviction the rung must
// refuse. Two guards enforce it: history evidence requires the entry to come
// from an EARLIER generation, and rung 2 re-stats before convicting.
func TestMidScanEditIsNotConvicted(t *testing.T) {
	dir := t.TempDir()
	body := bytes.Repeat([]byte("steady"), 400)
	a := mkAt(t, dir, "a/report.bin", body, fixedTime)
	b := mkAt(t, dir, "b/report.bin", body, fixedTime)

	// This scan's duplicates pass reads and records both copies…
	cache := &hashCache{ents: map[uint64]hcEnt{}, gen: 1}
	for _, f := range []fEnt{a, b} {
		pfx, err := hashFile(f.openContent, 64*1024, nil)
		if err != nil {
			t.Fatal(err)
		}
		full, err := hashFile(f.openContent, -1, nil)
		if err != nil {
			t.Fatal(err)
		}
		cache.record(f.path, f.size, f.mod.Unix(), pfx, full)
	}
	// …and then the user saves a new same-length version of copy "a" while
	// the scan is still running: bytes move AND the real mtime moves, but
	// nothing in the scan ever re-stats.
	if err := os.WriteFile(a.path, bytes.Repeat([]byte("edited"), 400), 0o644); err != nil {
		t.Fatal(err)
	}
	// Asked at this exact moment — bucket still holding this scan's pre-edit
	// read — a same-generation entry must not answer as history. (After the
	// window below, re-records make this ask vacuously false either way.)
	preFresh, err := hashFile(a.openContent, 64*1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cache.priorContentChanged(a.path, a.size, a.mod.Unix(), preFresh) {
		t.Fatal("a same-generation bucket answered as prior-scan history")
	}

	s := &Server{}
	acc := newCorruptTop(corruptFileCap)
	cancel := make(chan struct{})
	if !s.corruptWindow([]fEnt{a, b}, cache, nil, cancel, acc, 0, 1) {
		t.Fatal("cancelled")
	}
	groups, _ := acc.final(s, corruptFileCap, cancel)
	for _, g := range groups {
		for _, fe := range g.Files {
			if fe.Verdict == verdictCorrupt {
				t.Fatalf("a mid-scan edit was convicted as rot: %s — %q", fe.Name, fe.Evidence)
			}
			if strings.Contains(fe.Evidence, "content changed since an earlier scan") {
				t.Fatalf("history evidence fabricated from a same-scan comparison: %q", fe.Evidence)
			}
		}
	}
	// The guards must hold at the EVIDENCE layer too, not only at the
	// conviction layer (the rung-2 re-stat would mask them here): a same-scan
	// comparison may never enter the changed set nor read as prior change.
	if cache.changedPath(a.path) {
		t.Fatal("record() captured a same-generation overwrite as history")
	}
	freshPfx, err := hashFile(a.openContent, 64*1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cache.priorContentChanged(a.path, a.size, a.mod.Unix(), freshPfx) {
		t.Fatal("a same-generation bucket answered as prior-scan history")
	}
}

// The other interleaving: the walk stats the file, THEN the edit lands, then
// the content is read. Against a genuine earlier-scan entry the displacement
// is real — but the on-disk mtime has moved, so rung 2's re-stat must
// withhold the conviction rather than assert "its modified time did not
// change" about a file whose modified time changed.
func TestEditBetweenWalkAndReadIsNotConvicted(t *testing.T) {
	dir := t.TempDir()
	body := bytes.Repeat([]byte("steady"), 400)
	a := mkAt(t, dir, "a/ledger.bin", body, fixedTime)
	b := mkAt(t, dir, "b/ledger.bin", body, fixedTime)

	// An earlier scan recorded both copies.
	cache := &hashCache{ents: map[uint64]hcEnt{}, gen: 1}
	for _, f := range []fEnt{a, b} {
		pfx, err := hashFile(f.openContent, 64*1024, nil)
		if err != nil {
			t.Fatal(err)
		}
		full, err := hashFile(f.openContent, -1, nil)
		if err != nil {
			t.Fatal(err)
		}
		cache.record(f.path, f.size, f.mod.Unix(), pfx, full)
	}
	cache.gen++ // the next scan loads the store and advances the generation

	// This scan's walk has already recorded (size, fixedTime) into the fEnts
	// when the user's save lands: same length, new bytes, mtime moves.
	if err := os.WriteFile(a.path, bytes.Repeat([]byte("edited"), 400), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Server{}
	acc := newCorruptTop(corruptFileCap)
	cancel := make(chan struct{})
	if !s.corruptWindow([]fEnt{a, b}, cache, nil, cancel, acc, 0, 1) {
		t.Fatal("cancelled")
	}
	groups, _ := acc.final(s, corruptFileCap, cancel)
	for _, g := range groups {
		for _, fe := range g.Files {
			if fe.Verdict == verdictCorrupt {
				t.Fatalf("an edit with a moved mtime was convicted as rot: %s — %q", fe.Name, fe.Evidence)
			}
		}
	}
}

// A (size, mtime) family too large for one window must not be capped. Capping
// an unexamined population is precisely how the one copy worth finding gets
// thrown away — so the family is split by content instead, and this hides the
// odd copy at the END, where a "first N records" cap would lose it.
func TestSkewedCorruptKeyStillFindsTheOddCopy(t *testing.T) {
	oldKey, oldWin, oldMax := dupKeyFileCap, dupWindowFiles, dupWindowMax
	dupKeyFileCap, dupWindowFiles, dupWindowMax = 5, 4, 6
	defer func() { dupKeyFileCap, dupWindowFiles, dupWindowMax = oldKey, oldWin, oldMax }()

	dir := t.TempDir()
	sp, err := newSpill(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.close()

	const n = 20
	body := bytes.Repeat([]byte("identical-content"), 8) // 136 bytes
	odd := append([]byte(nil), body...)
	odd[100] = '!'
	var mod time.Time
	for i := 0; i < n; i++ {
		content := body
		if i == n-1 {
			content = odd // last, so a first-N cap would miss it
		}
		rel := fmt.Sprintf("f%02d.bin", i)
		p := filepath.Join(dir, rel)
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, fixedTime, fixedTime); err != nil {
			t.Fatal(err)
		}
		fi, _ := os.Stat(p)
		mod = fi.ModTime()
		if err := sp.add(0, &fEnt{size: fi.Size(), mod: mod, rel: rel, name: rel}); err != nil {
			t.Fatal(err)
		}
	}

	key := candHash(&fEnt{size: int64(len(body)), mod: mod}, corruptMatch)
	// The ordinary window refuses a key this size and hands it back whole.
	ents, over, _, werr := sp.window(0, 1, corruptMatch, []*dirHandle{nil}, []string{dir}, dupKeyFileCap, 0)
	if werr != nil {
		t.Fatal(werr)
	}
	if len(ents) != 0 || len(over) != 1 || over[0] != key {
		t.Fatalf("want the key handed back whole: ents=%d over=%v", len(ents), over)
	}

	s := &Server{}
	acc := newCorruptTop(corruptFileCap)
	cancel := make(chan struct{})
	if !s.corruptSkewedKey(sp, key, []*dirHandle{nil}, []string{dir}, nil, cancel, acc, &toolResult{}, 50) {
		t.Fatal("skew pass reported cancellation")
	}
	groups, trunc := acc.final(s, corruptFileCap, cancel)
	if len(groups) != 1 {
		t.Fatalf("want the one corrupted set, got %d", len(groups))
	}
	var names []string
	for _, f := range groups[0].Files {
		names = append(names, f.Name)
	}
	found := false
	for _, nm := range names {
		if nm == fmt.Sprintf("f%02d.bin", n-1) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the odd copy was capped away — set holds %v", names)
	}
	if groups[0].Variants != 2 {
		t.Errorf("want 2 distinct contents, got %d", groups[0].Variants)
	}
	// Everything left out of the sample must be reported, never silently gone.
	if trunc == nil || trunc.Files == 0 {
		t.Errorf("members left out of the sample must be reported, got %+v", trunc)
	}
}

// A skewed-family member whose rot lives BEYOND its first 64 KiB shares a
// prefix with its intact siblings. The old prefix-granular pass never
// full-hashed it: the family was confirmed conflicting by OTHER members and
// the rotted copy was sampled away by the per-prefix quota — unlisted,
// unconvicted, and silently identical every scan. Full-content tags give it
// its own tag (and record()'s capture marks it), so it must now be listed
// and convicted.
func TestSkewedCorruptKeyConvictsDeepRotBehindSharedPrefix(t *testing.T) {
	oldKey, oldWin, oldMax := dupKeyFileCap, dupWindowFiles, dupWindowMax
	dupKeyFileCap, dupWindowFiles, dupWindowMax = 5, 4, 64
	defer func() { dupKeyFileCap, dupWindowFiles, dupWindowMax = oldKey, oldWin, oldMax }()

	dir := t.TempDir()
	sp, err := newSpill(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.close()

	const n = 12
	headA := bytes.Repeat([]byte{0x77}, 64*1024)
	headB := bytes.Repeat([]byte{0x88}, 64*1024)
	tail := bytes.Repeat([]byte{0x01}, 8*1024)
	// Members 0..9 share prefix A, members 10..11 prefix B — so prefixes
	// already differ across the family and the old shortcut would have
	// stopped before ever full-hashing anyone.
	content := func(i int) []byte {
		h := headA
		if i >= 10 {
			h = headB
		}
		return append(append([]byte(nil), h...), tail...)
	}
	var mod time.Time
	var ents []fEnt
	for i := 0; i < n; i++ {
		rel := fmt.Sprintf("f%02d.bin", i)
		p := filepath.Join(dir, rel)
		if err := os.WriteFile(p, content(i), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, fixedTime, fixedTime); err != nil {
			t.Fatal(err)
		}
		fi, _ := os.Stat(p)
		mod = fi.ModTime()
		e := fEnt{path: p, size: fi.Size(), mod: mod, rel: rel, name: rel}
		ents = append(ents, e)
		if err := sp.add(0, &fEnt{size: fi.Size(), mod: mod, rel: rel, name: rel}); err != nil {
			t.Fatal(err)
		}
	}
	// An earlier scan recorded every member as it then was.
	cache := &hashCache{ents: map[uint64]hcEnt{}, gen: 1}
	for _, e := range ents {
		pfx, err := hashFile(e.openContent, 64*1024, nil)
		if err != nil {
			t.Fatal(err)
		}
		full, err := hashFile(e.openContent, -1, nil)
		if err != nil {
			t.Fatal(err)
		}
		cache.record(e.path, e.size, e.mod.Unix(), pfx, full)
	}
	cache.gen++ // the next scan loads the store and advances the generation

	// Member f05 rots past the prefix; size and mtime untouched. Its prefix
	// still matches nine intact siblings.
	rotted := content(5)
	rotted[70*1024] ^= 0xff
	if err := os.WriteFile(ents[5].path, rotted, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(ents[5].path, fixedTime, fixedTime); err != nil {
		t.Fatal(err)
	}

	key := candHash(&fEnt{size: ents[0].size, mod: mod}, corruptMatch)
	s := &Server{}
	acc := newCorruptTop(corruptFileCap)
	cancel := make(chan struct{})
	if !s.corruptSkewedKey(sp, key, []*dirHandle{nil}, []string{dir}, cache, cancel, acc, &toolResult{}, 50) {
		t.Fatal("skew pass reported cancellation")
	}
	groups, _ := acc.final(s, corruptFileCap, cancel)
	if len(groups) != 1 {
		t.Fatalf("want the one conflicting set, got %d", len(groups))
	}
	var got *FileEnt
	for i := range groups[0].Files {
		if groups[0].Files[i].Name == "f05.bin" {
			got = &groups[0].Files[i]
		}
	}
	if got == nil {
		t.Fatalf("the deep-rotted member was sampled away — set holds %d rows", len(groups[0].Files))
	}
	if got.Verdict != verdictCorrupt || !strings.Contains(got.Evidence, "content changed since an earlier scan") {
		t.Fatalf("deep rot in a skewed family must be convicted from history: verdict %q, evidence %q", got.Verdict, got.Evidence)
	}
	for _, fe := range groups[0].Files {
		if fe.Name != "f05.bin" && fe.Verdict == verdictCorrupt {
			t.Errorf("intact sibling %s convicted: %q", fe.Name, fe.Evidence)
		}
	}
}

// Deep rot found by the skew pass while the store's capped `changed` evidence
// set is already FULL, in a family big enough that the sample quota is spent
// before the rotted member is reached. record() still returns true — the
// detection happened — but the set refuses the entry, so changedPath answers
// false for exactly the file this pass just proved rotted; and the sample has
// already kept corruptSkewSample rows, so the ordinary quota would drop it
// too. The finding must not depend on 131072 other files having claimed the
// evidence slots first: the pass's own rotted map has to carry it past BOTH
// bounds — into the sample, and on to the verdict (by the time the sample
// reaches corruptWindow the store entry is this generation's, so nothing
// there can re-detect the history).
func TestSkewedCorruptKeyConvictsDeepRotWhenChangedSetIsFull(t *testing.T) {
	oldKey, oldWin, oldMax := dupKeyFileCap, dupWindowFiles, dupWindowMax
	dupKeyFileCap, dupWindowFiles, dupWindowMax = 5, 4, 64
	defer func() { dupKeyFileCap, dupWindowFiles, dupWindowMax = oldKey, oldWin, oldMax }()

	dir := t.TempDir()
	sp, err := newSpill(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.close()

	// Enough intact members, in distinct-content pairs, that their per-tag
	// keeps alone saturate the sample before the rotted member (added LAST)
	// is considered: 2 keeps per tag times pairs >= corruptSkewSample by
	// construction, whatever the constant becomes.
	pairs := (corruptSkewSample + 1) / 2
	head := bytes.Repeat([]byte{0x77}, 64*1024)
	content := func(variant int) []byte {
		c := append(append([]byte(nil), head...), bytes.Repeat([]byte{0x01}, 8*1024)...)
		c[64*1024] = byte(variant)
		c[64*1024+1] = byte(variant >> 8)
		return c
	}
	var mod time.Time
	write := func(rel string, data []byte) fEnt {
		p := filepath.Join(dir, rel)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, fixedTime, fixedTime); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		mod = fi.ModTime()
		e := fEnt{path: p, size: fi.Size(), mod: mod, rel: rel, name: rel}
		if err := sp.add(0, &fEnt{size: fi.Size(), mod: mod, rel: rel, name: rel}); err != nil {
			t.Fatal(err)
		}
		return e
	}
	for i := 0; i < pairs*2; i++ {
		write(fmt.Sprintf("f%04d.bin", i), content(i/2))
	}
	rotten := write("rotten.bin", content(pairs))

	// An earlier scan recorded the soon-to-rot member as it then was. (The
	// intact members need no history: identical content makes record() mute
	// for them either way.)
	cache := &hashCache{ents: map[uint64]hcEnt{}, gen: 1}
	pfx, err := hashFile(rotten.openContent, 64*1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	full, err := hashFile(rotten.openContent, -1, nil)
	if err != nil {
		t.Fatal(err)
	}
	cache.record(rotten.path, rotten.size, rotten.mod.Unix(), pfx, full)
	cache.gen++ // the next scan loads the store and advances the generation

	// The member rots past the prefix; size and mtime untouched.
	rot := content(pairs)
	rot[70*1024] ^= 0xff
	if err := os.WriteFile(rotten.path, rot, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(rotten.path, fixedTime, fixedTime); err != nil {
		t.Fatal(err)
	}

	// Earlier windows have filled the evidence set to its cap — every slot
	// taken, none by this family.
	cache.changed = make(map[uint64][16]byte, changedMax)
	for i := uint64(0); len(cache.changed) < changedMax; i++ {
		cache.changed[i] = [16]byte{}
	}

	key := candHash(&fEnt{size: rotten.size, mod: mod}, corruptMatch)
	s := &Server{}
	acc := newCorruptTop(corruptFileCap)
	cancel := make(chan struct{})
	if !s.corruptSkewedKey(sp, key, []*dirHandle{nil}, []string{dir}, cache, cancel, acc, &toolResult{}, 50) {
		t.Fatal("skew pass reported cancellation")
	}
	// Vacuity guard: the capped set really did refuse the entry — if this
	// answers true, the test stopped exercising the overflow path.
	if cache.changedPath(rotten.path) {
		t.Fatal("changed set accepted the entry; the cap-overflow path was not exercised")
	}
	groups, _ := acc.final(s, corruptFileCap, cancel)
	if len(groups) != 1 {
		t.Fatalf("want the one conflicting set, got %d", len(groups))
	}
	var got *FileEnt
	for i := range groups[0].Files {
		if groups[0].Files[i].Name == "rotten.bin" {
			got = &groups[0].Files[i]
		}
	}
	if got == nil {
		t.Fatalf("the rotted member was sampled away — set holds %d rows", len(groups[0].Files))
	}
	if got.Verdict != verdictCorrupt || !strings.Contains(got.Evidence, "content changed since an earlier scan") {
		t.Fatalf("rot found while the evidence set is full must still convict: verdict %q, evidence %q", got.Verdict, got.Evidence)
	}
	for _, fe := range groups[0].Files {
		if fe.Name != "rotten.bin" && fe.Verdict == verdictCorrupt {
			t.Errorf("intact sibling %s convicted: %q", fe.Name, fe.Evidence)
		}
	}
}

// The same family with every member byte-identical is a pile of duplicates,
// not damage: the second rung (full content hash) has to clear it.
func TestSkewedCorruptKeyIgnoresAnAllIdenticalFamily(t *testing.T) {
	oldKey, oldWin, oldMax := dupKeyFileCap, dupWindowFiles, dupWindowMax
	dupKeyFileCap, dupWindowFiles, dupWindowMax = 5, 4, 6
	defer func() { dupKeyFileCap, dupWindowFiles, dupWindowMax = oldKey, oldWin, oldMax }()

	dir := t.TempDir()
	sp, err := newSpill(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.close()
	body := bytes.Repeat([]byte("same"), 40)
	var mod time.Time
	for i := 0; i < 20; i++ {
		rel := fmt.Sprintf("s%02d.bin", i)
		p := filepath.Join(dir, rel)
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, fixedTime, fixedTime); err != nil {
			t.Fatal(err)
		}
		fi, _ := os.Stat(p)
		mod = fi.ModTime()
		if err := sp.add(0, &fEnt{size: fi.Size(), mod: mod, rel: rel, name: rel}); err != nil {
			t.Fatal(err)
		}
	}
	key := candHash(&fEnt{size: int64(len(body)), mod: mod}, corruptMatch)
	s := &Server{}
	acc := newCorruptTop(corruptFileCap)
	cancel := make(chan struct{})
	if !s.corruptSkewedKey(sp, key, []*dirHandle{nil}, []string{dir}, nil, cancel, acc, &toolResult{}, 50) {
		t.Fatal("skew pass reported cancellation")
	}
	groups, _ := acc.final(s, corruptFileCap, cancel)
	if len(groups) != 0 {
		t.Fatalf("identical copies are duplicates, not corruption — got %d sets", len(groups))
	}
}

// A Motion Photo / Live Photo is a complete JPEG with a whole video appended
// after its end-of-image marker. On a NAS full of phone backups that is an
// ordinary file, and a validator that looks for EOI only in the tail calls
// every one of them truncated — convicting the GOOD copy, and (because a
// damaged verdict stops the ladder) suppressing the comparison that would have
// found the real one.
func TestAppendedTrailerIsNotMistakenForTruncation(t *testing.T) {
	dir := t.TempDir()
	base := goodJPEG(t)
	trailer := make([]byte, 8192) // stands in for the appended MP4
	for i := range trailer {
		trailer[i] = byte(i*13 + 7)
	}
	good := append(append([]byte(nil), base...), trailer...)

	if st, why := verifyBytes(t, dir, "motion.jpg", good); st != proven {
		t.Fatalf("a JPEG with an appended trailer must not read as truncated: %v (%s)", st, why)
	}

	// Now the genuinely damaged twin: same size, same mtime, a NUL run punched
	// into the trailer.
	bad := append([]byte(nil), good...)
	for i := len(base) + 2000; i < len(base)+6000; i++ {
		bad[i] = 0
	}
	files := []fEnt{
		mkAt(t, dir, "ok/motion.jpg", good, fixedTime),
		mkAt(t, dir, "rot/motion.jpg", bad, fixedTime),
	}
	groups := runCorrupt(t, files)
	if len(groups) != 1 {
		t.Fatalf("want 1 set, got %d", len(groups))
	}
	g := &groups[0]
	if got := verdictOf(g, "rot"); got != verdictCorrupt {
		t.Errorf("the copy with the zero run: want %q, got %q", verdictCorrupt, got)
	}
	if got := verdictOf(g, "ok"); got != verdictIntact {
		t.Errorf("the untouched Motion Photo: want %q, got %q", verdictIntact, got)
	}
}

// Two members holding exactly the same bytes must receive the same verdict.
// Rung 4 compares two representatives, and without propagating the result to
// hash-identical peers the app labels one good copy Intact and its exact twin
// Undetermined, purely by path order.
func TestIdenticalCopiesShareAVerdict(t *testing.T) {
	dir := t.TempDir()
	good := make([]byte, 8192)
	for i := range good {
		good[i] = byte(i*11 + 5)
	}
	bad := append([]byte(nil), good...)
	for i := 4096; i < len(bad); i++ {
		bad[i] = 0
	}
	files := []fEnt{
		mkAt(t, dir, "a/clip.dat", good, fixedTime),
		mkAt(t, dir, "b/clip.dat", good, fixedTime),
		mkAt(t, dir, "c/clip.dat", bad, fixedTime),
	}
	groups := runCorrupt(t, files)
	if len(groups) != 1 {
		t.Fatalf("want 1 set, got %d", len(groups))
	}
	g := &groups[0]
	if got := verdictOf(g, "c"); got != verdictCorrupt {
		t.Errorf("the zero-filled copy: want %q, got %q", verdictCorrupt, got)
	}
	for _, d := range []string{"a", "b"} {
		if got := verdictOf(g, d); got != verdictIntact {
			t.Errorf("%s/clip.dat is byte-identical to its twin: want %q, got %q", d, verdictIntact, got)
		}
	}
}

// The duplicates pass runs first and rewrites the cache bucket for every file
// it hashes, so asking the live bucket afterwards answers "unchanged" for
// exactly the files that were read. The evidence has to be captured as the
// overwrite happens, or this whole rung is dead code.
func TestPrefixHistorySurvivesTheDuplicatesPassOverwriting(t *testing.T) {
	dir := t.TempDir()
	f := mkAt(t, dir, "ledger.bin", bytes.Repeat([]byte("O"), 2048), fixedTime)
	cache := &hashCache{ents: map[uint64]hcEnt{}, gen: 1}

	oldPfx, err := hashFile(f.openContent, 64*1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	cache.record(f.path, f.size, f.mod.Unix(), oldPfx, oldPfx)
	cache.gen++ // the next scan loads the store and advances the generation

	// Rot the content, keeping size and mtime.
	rotted := bytes.Repeat([]byte("O"), 2048)
	rotted[9] = 'Q'
	if err := os.WriteFile(f.path, rotted, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(f.path, fixedTime, fixedTime); err != nil {
		t.Fatal(err)
	}
	newPfx, err := hashFile(f.openContent, 64*1024, nil)
	if err != nil {
		t.Fatal(err)
	}

	// The duplicates pass gets there first and overwrites the bucket.
	cache.record(f.path, f.size, f.mod.Unix(), newPfx, newPfx)

	if !cache.priorContentChanged(f.path, f.size, f.mod.Unix(), newPfx) {
		t.Fatal("the overwrite destroyed the only evidence that the content moved under an unchanged mtime")
	}
	// A file nothing ever recorded is not evidence either way.
	if cache.priorContentChanged(filepath.Join(dir, "never-seen.bin"), 10, 20, newPfx) {
		t.Error("a path with no history must not read as changed")
	}
}

// Rot beyond the first 64 KiB leaves the prefix identical, so the fresh full
// hash landing in record() is the only witness that the content moved.
// record() must both report it to its caller and remember it in the changed
// set for later rungs.
func TestDeepRotCaughtByRecordOverwrite(t *testing.T) {
	cache := &hashCache{ents: map[uint64]hcEnt{}, gen: 1}
	pfx := strings.Repeat("ab", 32)
	cache.record("/v/deep.bin", 4096, 777, pfx, strings.Repeat("11", 32))
	cache.gen++ // the next scan loads the store and advances the generation
	// Same size, mtime AND prefix — only the full hash differs.
	if !cache.record("/v/deep.bin", 4096, 777, pfx, strings.Repeat("22", 32)) {
		t.Fatal("a full hash moving under an unchanged prefix is the deep-rot signature record() exists to catch")
	}
	if !cache.priorContentChanged("/v/deep.bin", 4096, 777, pfx) {
		t.Fatal("the changed set must remember deep rot after the overwrite")
	}
	// Re-recording identical content is not evidence of anything.
	if cache.record("/v/deep.bin", 4096, 777, pfx, strings.Repeat("22", 32)) {
		t.Fatal("an unchanged file must not read as changed")
	}
}

// The CSV export and the grid's Status column name the same three verdicts in
// two different languages, so nothing but a test keeps them saying the same
// word. They drifted once — the export wrote "Unknown" where the grid showed
// "Undetermined" — which meant a report disagreed with the screen it came from.
// The matching half lives in test/smoke.js, asserting VERDICT_TEXT.
func TestVerdictLabelsMatchTheUI(t *testing.T) {
	want := map[string]string{
		verdictCorrupt: "Corrupted",
		verdictIntact:  "Intact",
		verdictUnknown: "Undetermined",
	}
	for v, label := range want {
		if got := verdictLabel(v); got != label {
			t.Errorf("verdictLabel(%q) = %q, want %q (spk/ui/DuplicateFinder.js VERDICT_TEXT)", v, got, label)
		}
		// Whatever the user is reading on screen must also be typeable into the
		// search box — that is the whole reason the synonym list exists.
		if !strings.Contains(verdictSearchText(v), strings.ToLower(label)) {
			t.Errorf("searching for the displayed word %q cannot match verdict %q (have %q)",
				label, v, verdictSearchText(v))
		}
	}
	if verdictLabel("") != "" {
		t.Error("a row with no verdict must render no label")
	}
}

// ------------------------------------------------------- container checks

func goodPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for x := 0; x < 64; x++ {
		for y := 0; y < 64; y++ {
			img.Set(x, y, color.RGBA{uint8(x * 3), uint8(y * 5), uint8(x ^ y), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func goodJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 48, 48))
	for x := 0; x < 48; x++ {
		for y := 0; y < 48; y++ {
			img.Set(x, y, color.RGBA{uint8(x * 5), uint8(y * 5), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func verifyBytes(t *testing.T, dir, name string, b []byte) (intactness, string) {
	t.Helper()
	e := mkAt(t, dir, name, b, fixedTime)
	return verifyContent(e.openContent, e.name, e.size)
}

func TestVerifyContentPNG(t *testing.T) {
	dir := t.TempDir()
	good := goodPNG(t)
	if st, why := verifyBytes(t, dir, "ok.png", good); st != proven {
		t.Errorf("intact PNG: want proven, got %v (%s)", st, why)
	}
	rot := append([]byte(nil), good...)
	rot[len(rot)/2] ^= 0x01
	if st, _ := verifyBytes(t, dir, "rot.png", rot); st != damaged {
		t.Errorf("PNG with a flipped byte: want damaged, got %v", st)
	}
	if st, _ := verifyBytes(t, dir, "cut.png", good[:len(good)-20]); st != damaged {
		t.Errorf("truncated PNG: want damaged, got %v", st)
	}
}

func TestVerifyContentGzip(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(bytes.Repeat([]byte("compress me please "), 500))
	zw.Close()
	good := buf.Bytes()
	if st, why := verifyBytes(t, dir, "ok.gz", good); st != proven {
		t.Errorf("intact gzip: want proven, got %v (%s)", st, why)
	}
	// Corrupt the payload but leave the trailing CRC32 alone, so only the
	// checksum comparison can catch it.
	rot := append([]byte(nil), good...)
	rot[len(rot)/2] ^= 0x40
	if st, _ := verifyBytes(t, dir, "rot.gz", rot); st != damaged {
		t.Errorf("gzip failing its CRC32: want damaged, got %v", st)
	}
}

func TestVerifyContentZip(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("content.txt")
	w.Write(bytes.Repeat([]byte("spreadsheet row\n"), 400))
	zw.Close()
	good := buf.Bytes()
	if st, why := verifyBytes(t, dir, "ok.zip", good); st != proven {
		t.Errorf("intact zip: want proven, got %v (%s)", st, why)
	}
	rot := append([]byte(nil), good...)
	rot[60] ^= 0x7F
	if st, _ := verifyBytes(t, dir, "rot.zip", rot); st != damaged {
		t.Errorf("zip entry failing its CRC32: want damaged, got %v", st)
	}
}

func TestVerifyContentUnknownTypeStaysUnproven(t *testing.T) {
	dir := t.TempDir()
	if st, _ := verifyBytes(t, dir, "notes.txt", []byte("just some text")); st != unproven {
		t.Errorf("a format with no validator must not read as a clean bill of health, got %v", st)
	}
}

// The validator is chosen by magic bytes, not by the extension — the whole
// question here is whether a file's contents are what they claim to be.
func TestVerifyContentIgnoresAMisleadingExtension(t *testing.T) {
	dir := t.TempDir()
	good := goodPNG(t)
	if st, _ := verifyBytes(t, dir, "actually-a-png.dat", good); st != proven {
		t.Errorf("PNG under a .dat name should still be verified, got %v", st)
	}
}

// A PNG built by hand so the CRC is right, then truncated mid-chunk: catches
// the "claims more bytes than remain" arm, which is the arithmetic that has to
// stay overflow-safe on 32-bit ARM builds.
func TestVerifyPNGRejectsAnOversizedChunkLength(t *testing.T) {
	dir := t.TempDir()
	var b bytes.Buffer
	b.Write([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A})
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], 0x7FFFFFF0) // far past the end of the file
	b.Write(l[:])
	b.WriteString("IHDR")
	b.Write(make([]byte, 13))
	var c [4]byte
	binary.BigEndian.PutUint32(c[:], crc32.ChecksumIEEE(append([]byte("IHDR"), make([]byte, 13)...)))
	b.Write(c[:])
	if st, _ := verifyBytes(t, dir, "huge.png", b.Bytes()); st != damaged {
		t.Errorf("PNG chunk longer than the file: want damaged, got %v", st)
	}
}
