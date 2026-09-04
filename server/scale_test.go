package main

// Phase-4 scale tests: the pieces that keep an 80 TB volume with millions of
// duplicates inside bounded memory. Each one pins a property that cannot be
// observed on the test hardware — the fixture NAS is far too small to reach
// any of these thresholds — so the thresholds themselves are shrunk here.

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// The counter must never say "unique" about a key two files shared. Once it
// degrades to the fixed table it may say "collides" about a key only one
// file had — that costs a wasted hash, not a wrong result — but the reverse
// would silently drop duplicates, which is the whole point of the structure.
func TestKeyCounterDegradesWithoutFalseNegatives(t *testing.T) {
	oldMax, oldBits := keyCounterExactMax, keyCounterTabBits
	keyCounterExactMax, keyCounterTabBits = 64, 14
	defer func() { keyCounterExactMax, keyCounterTabBits = oldMax, oldBits }()

	k := newKeyCounter()
	rng := rand.New(rand.NewSource(7))
	var shared, lone []uint64
	for i := 0; i < 4000; i++ {
		key := rng.Uint64()
		if i%3 == 0 {
			shared = append(shared, key)
			k.add(key)
			k.add(key)
		} else {
			lone = append(lone, key)
			k.add(key)
		}
	}
	if k.exact != nil {
		t.Fatal("counter should have degraded to the fixed table")
	}
	if len(k.tab) != 1<<(keyCounterTabBits-2) {
		t.Fatalf("table sized %d, want %d", len(k.tab), 1<<(keyCounterTabBits-2))
	}
	for _, key := range shared {
		if !k.hot(key) {
			t.Fatal("a key two files shared was reported unique — duplicates would be missed")
		}
	}
	// False positives are allowed but must stay rare enough to be a rounding
	// error on the hashing bill; a broken slot scheme shows up as most of the
	// lone keys reading hot.
	fp := 0
	for _, key := range lone {
		if k.hot(key) {
			fp++
		}
	}
	if fp > len(lone)/4 {
		t.Fatalf("false-positive rate %d/%d is too high to be useful", fp, len(lone))
	}
	k.release()
	if k.tab != nil || k.exact != nil {
		t.Fatal("release must free both representations")
	}
}

// Partitioning the candidate key space is what bounds the scan's memory. It
// is only sound if a group can never straddle two partitions: this drives a
// real duplicate set both ways and demands identical results.
func TestWindowedScanMatchesWholeScan(t *testing.T) {
	dir := t.TempDir()
	var files []fEnt
	for g := 0; g < 12; g++ {
		content := strings.Repeat(fmt.Sprintf("group-%d-", g), 40)
		copies := 2 + g%3
		for c := 0; c < copies; c++ {
			p := filepath.Join(dir, fmt.Sprintf("g%02d-c%d.bin", g, c))
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			fi, _ := os.Stat(p)
			files = append(files, fEnt{
				path: p, name: filepath.Base(p), dir: dir,
				size: fi.Size(), mod: fi.ModTime(),
			})
		}
	}
	// A few singletons that must never group.
	for i := 0; i < 5; i++ {
		p := filepath.Join(dir, fmt.Sprintf("lone%d.bin", i))
		os.WriteFile(p, []byte(strings.Repeat("x", 100+i)), 0o644)
		fi, _ := os.Stat(p)
		files = append(files, fEnt{path: p, name: filepath.Base(p), dir: dir, size: fi.Size(), mod: fi.ModTime()})
	}

	whole, _ := (&Server{}).scanDuplicates(files, MatchOpts{}, nil, make(chan struct{}))
	if len(whole) != 12 {
		t.Fatalf("fixture should yield 12 groups, got %d", len(whole))
	}

	for _, parts := range []int{2, 3, 7, 13} {
		s := &Server{}
		acc := newGroupTop(dupFileCap)
		for p := 0; p < parts; p++ {
			var win []fEnt
			for _, f := range files {
				if candHash(&f, MatchOpts{})%uint64(parts) == uint64(p) {
					win = append(win, f)
				}
			}
			if !s.dupWindow(win, MatchOpts{}, nil, make(chan struct{}), acc, 16, 96) {
				t.Fatal("window reported cancellation")
			}
		}
		got, _ := acc.final(s, dupFileCap, nil)
		if len(got) != len(whole) {
			t.Fatalf("%d partitions produced %d groups, whole scan %d", parts, len(got), len(whole))
		}
		for i := range got {
			if got[i].Hash != whole[i].Hash || got[i].Size != whole[i].Size ||
				len(got[i].Files) != len(whole[i].Files) || got[i].ID != whole[i].ID {
				t.Fatalf("%d partitions diverged at group %d: %+v vs %+v", parts, i, got[i], whole[i])
			}
			for j := range got[i].Files {
				a, b := got[i].Files[j], whole[i].Files[j]
				if a.Name != b.Name || a.Dir != b.Dir || a.Hash != b.Hash {
					t.Fatalf("%d partitions diverged at file %d/%d: %+v vs %+v", parts, i, j, a, b)
				}
			}
		}
	}
}

// The point of the whole skew path: a duplicate pair sitting PAST the per-key
// cap must still be found. Truncating the key — the previous behaviour —
// dropped these two members and reported only a count, so the group vanished
// with nothing but a number to show for it.
func TestSkewedKeyStillFindsDuplicatesPastTheCap(t *testing.T) {
	oldCap, oldWin := dupKeyFileCap, dupWindowFiles
	dupKeyFileCap, dupWindowFiles = 5, 4
	defer func() { dupKeyFileCap, dupWindowFiles = oldCap, oldWin }()

	dir := t.TempDir()
	sp, err := newSpill(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.close()
	// 20 files of identical size. The first 18 all differ; the LAST TWO are
	// identical to each other, so they sit well past the cap.
	const size = 64
	var mod time.Time
	for i := 0; i < 20; i++ {
		body := []byte(fmt.Sprintf("%063d\n", i))
		if i >= 18 {
			body = []byte(fmt.Sprintf("%063d\n", 999)) // the real duplicate pair
		}
		if len(body) != size {
			t.Fatalf("fixture body is %d bytes", len(body))
		}
		rel := fmt.Sprintf("f%02d.bin", i)
		if err := os.WriteFile(filepath.Join(dir, rel), body, 0o644); err != nil {
			t.Fatal(err)
		}
		fi, _ := os.Stat(filepath.Join(dir, rel))
		mod = fi.ModTime()
		if err := sp.add(0, &fEnt{size: size, mod: mod, rel: rel, name: rel}); err != nil {
			t.Fatal(err)
		}
	}
	key := candHash(&fEnt{size: size, mod: mod}, MatchOpts{})

	// The ordinary window refuses the key and hands it back.
	ents, over, skipped, err := sp.window(0, 1, MatchOpts{}, []*dirHandle{nil}, []string{dir}, dupKeyFileCap, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 || len(over) != 1 || over[0] != key || skipped != 0 {
		t.Fatalf("want the key handed back whole: ents=%d over=%v skipped=%d", len(ents), over, skipped)
	}

	// The skew path finds the pair the cap would have hidden.
	s := &Server{}
	acc := newGroupTop(dupFileCap)
	missing := 0
	if err := s.resolveSkewedKey(sp, key, ScanReq{Tool: "duplicates"}, nil, nil,
		[]*dirHandle{nil}, []string{dir}, make(chan struct{}), acc, 16, 96, &missing); err != nil {
		t.Fatal(err)
	}
	groups, _ := acc.final(s, dupFileCap, nil)
	if len(groups) != 1 {
		t.Fatalf("want the one duplicate group, got %d", len(groups))
	}
	if len(groups[0].Files) != 2 {
		t.Fatalf("want the pair, got %d files", len(groups[0].Files))
	}
	names := []string{groups[0].Files[0].Name, groups[0].Files[1].Name}
	if names[0] != "f18.bin" || names[1] != "f19.bin" {
		t.Fatalf("wrong pair found: %v", names)
	}
}

// Splitting a skewed key by content PREFIX is not enough on its own: files
// can share a size and their first 64 KiB and still differ afterwards. A
// duplicate pair hiding behind an over-populated prefix must be found by the
// second step (full content hash), not capped away — the same mistake as
// truncating the key, one level down.
func TestSkewedPrefixStillFindsDuplicatesPastTheCap(t *testing.T) {
	oldCap, oldWin := dupKeyFileCap, dupWindowFiles
	dupKeyFileCap, dupWindowFiles = 5, 4
	defer func() { dupKeyFileCap, dupWindowFiles = oldCap, oldWin }()

	dir := t.TempDir()
	sp, err := newSpill(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.close()
	// Every file: an identical 64 KiB head, so all share one prefix hash, and
	// a distinct tail — except the last two, which are byte-identical.
	head := bytes.Repeat([]byte("H"), 64*1024)
	var mod time.Time
	var size int64
	for i := 0; i < 12; i++ {
		tail := fmt.Sprintf("%016d", i)
		if i >= 10 {
			tail = fmt.Sprintf("%016d", 999)
		}
		rel := fmt.Sprintf("f%02d.bin", i)
		p := filepath.Join(dir, rel)
		if err := os.WriteFile(p, append(append([]byte{}, head...), tail...), 0o644); err != nil {
			t.Fatal(err)
		}
		fi, _ := os.Stat(p)
		mod, size = fi.ModTime(), fi.Size()
		if err := sp.add(0, &fEnt{size: size, mod: mod, rel: rel, name: rel}); err != nil {
			t.Fatal(err)
		}
	}
	key := candHash(&fEnt{size: size, mod: mod}, MatchOpts{})

	s := &Server{}
	acc := newGroupTop(dupFileCap)
	missing := 0
	if err := s.resolveSkewedKey(sp, key, ScanReq{Tool: "duplicates"}, nil, nil,
		[]*dirHandle{nil}, []string{dir}, make(chan struct{}), acc, 16, 96, &missing); err != nil {
		t.Fatal(err)
	}
	groups, _ := acc.final(s, dupFileCap, nil)
	if len(groups) != 1 {
		t.Fatalf("want exactly the one real duplicate group, got %d", len(groups))
	}
	if len(groups[0].Files) != 2 ||
		groups[0].Files[0].Name != "f10.bin" || groups[0].Files[1].Name != "f11.bin" {
		t.Fatalf("wrong group found: %+v", groups[0].Files)
	}
}

// A partition can be too big to hold even when no single tag is oversized —
// just many distinct ones landing together. That must escalate to the next
// rung of the ladder (and finally to one tag at a time), never drop records
// nobody has looked at: a dropped record takes its duplicates with it.
func TestCrowdedLadderPartitionEscalatesInsteadOfDropping(t *testing.T) {
	oldCap, oldWin, oldMax := dupKeyFileCap, dupWindowFiles, dupWindowMax
	dupKeyFileCap, dupWindowFiles, dupWindowMax = 100, 100, 4
	defer func() { dupKeyFileCap, dupWindowFiles, dupWindowMax = oldCap, oldWin, oldMax }()

	dir := t.TempDir()
	sp, err := newSpill(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.close()
	const size = 64
	var mod time.Time
	for i := 0; i < 10; i++ {
		body := []byte(fmt.Sprintf("%063d\n", i))
		if i >= 8 {
			body = []byte(fmt.Sprintf("%063d\n", 555)) // the only real pair
		}
		rel := fmt.Sprintf("f%02d.bin", i)
		if err := os.WriteFile(filepath.Join(dir, rel), body, 0o644); err != nil {
			t.Fatal(err)
		}
		fi, _ := os.Stat(filepath.Join(dir, rel))
		mod = fi.ModTime()
		if err := sp.add(0, &fEnt{size: size, mod: mod, rel: rel, name: rel}); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{}
	acc := newGroupTop(dupFileCap)
	missing := 0
	key := candHash(&fEnt{size: size, mod: mod}, MatchOpts{})
	if err := s.resolveSkewedKey(sp, key, ScanReq{Tool: "duplicates"}, nil, nil,
		[]*dirHandle{nil}, []string{dir}, make(chan struct{}), acc, 16, 96, &missing); err != nil {
		t.Fatal(err)
	}
	groups, trunc := acc.final(s, dupFileCap, nil)
	if len(groups) != 1 || len(groups[0].Files) != 2 ||
		groups[0].Files[0].Name != "f08.bin" || groups[0].Files[1].Name != "f09.bin" {
		t.Fatalf("crowding must not cost the pair: %+v", groups)
	}
	if trunc != nil {
		t.Fatalf("nothing was actually dropped, so nothing may be reported: %+v", trunc)
	}
}

// The ladder used to read a skewed key's files at full length TWICE — once to
// tag them by content, once again in dupWindow to group them. The full rung
// now consults and populates the hash store, so within one scan the second
// read becomes a lookup. That reuse is strictly SAME-scan: a real rescan
// advances the generation, misses every entry, and re-reads everything in
// full by design — the third leg below pins exactly that.
func TestSkewLadderFeedsTheHashCache(t *testing.T) {
	oldCap, oldWin := dupKeyFileCap, dupWindowFiles
	dupKeyFileCap, dupWindowFiles = 5, 4
	defer func() { dupKeyFileCap, dupWindowFiles = oldCap, oldWin }()

	dir := t.TempDir()
	sp, err := newSpill(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.close()
	// One skewed key whose members share a 64 KiB head, so the ladder has to
	// go all the way to its full-content rung.
	head := bytes.Repeat([]byte("H"), 64*1024)
	type want struct {
		path string
		size int64
		mod  time.Time
	}
	var files []want
	for i := 0; i < 12; i++ {
		rel := fmt.Sprintf("f%02d.bin", i)
		p := filepath.Join(dir, rel)
		if err := os.WriteFile(p, append(append([]byte{}, head...), fmt.Sprintf("%016d", i%6)...), 0o644); err != nil {
			t.Fatal(err)
		}
		fi, _ := os.Stat(p)
		files = append(files, want{p, fi.Size(), fi.ModTime()})
		if err := sp.add(0, &fEnt{size: fi.Size(), mod: fi.ModTime(), rel: rel, name: rel}); err != nil {
			t.Fatal(err)
		}
	}
	key := candHash(&fEnt{size: files[0].size, mod: files[0].mod}, MatchOpts{})

	s := &Server{}
	state := t.TempDir()
	cache := loadHashCache(state)
	acc := newGroupTop(dupFileCap)
	missing := 0
	if err := s.resolveSkewedKey(sp, key, ScanReq{Tool: "duplicates"}, nil, cache,
		[]*dirHandle{nil}, []string{dir}, make(chan struct{}), acc, 16, 96, &missing); err != nil {
		t.Fatal(err)
	}
	groups, _ := acc.final(s, dupFileCap, nil)
	if len(groups) != 6 {
		t.Fatalf("want the six content groups, got %d", len(groups))
	}
	// Every member must now be answerable from the cache, keyed by the very
	// prefix dupWindow recomputes — that lookup is the read this saves.
	for _, w := range files {
		pfx, err := hashFile(func() (*os.File, error) { return os.Open(w.path) }, 64*1024, nil)
		if err != nil {
			t.Fatal(err)
		}
		full, ok := cache.lookup(w.path, w.size, w.mod.Unix(), pfx)
		if !ok {
			t.Fatalf("%s was read at full length but not cached — dupWindow will read it again", w.path)
		}
		real, err := hashFile(func() (*os.File, error) { return os.Open(w.path) }, -1, nil)
		if err != nil {
			t.Fatal(err)
		}
		if full != real {
			t.Fatalf("%s cached the wrong hash: %s vs %s", w.path, full, real)
		}
	}
	// And the groups must be unchanged when a later pass of the SAME scan
	// runs entirely off this scan's own records.
	acc2 := newGroupTop(dupFileCap)
	if err := s.resolveSkewedKey(sp, key, ScanReq{Tool: "duplicates"}, nil, cache,
		[]*dirHandle{nil}, []string{dir}, make(chan struct{}), acc2, 16, 96, &missing); err != nil {
		t.Fatal(err)
	}
	g2, _ := acc2.final(s, dupFileCap, nil)
	if len(g2) != len(groups) {
		t.Fatalf("same-scan rerun produced %d groups, first pass %d", len(g2), len(groups))
	}
	for i := range g2 {
		if g2[i].Hash != groups[i].Hash || len(g2[i].Files) != len(groups[i].Files) {
			t.Fatalf("same-scan rerun diverged at group %d: %+v vs %+v", i, g2[i], groups[i])
		}
	}
	// A RESCAN is different: the reloaded store advances the generation, so
	// nothing is served — the skew ladder re-reads every file in full and
	// must still land on the same groups.
	if err := cache.save(); err != nil {
		t.Fatal(err)
	}
	cache2 := loadHashCache(state)
	pfx0, err := hashFile(func() (*os.File, error) { return os.Open(files[0].path) }, 64*1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cache2.lookup(files[0].path, files[0].size, files[0].mod.Unix(), pfx0); ok {
		t.Fatal("a previous scan's hash was served on the skew path — every scan must re-read")
	}
	acc3 := newGroupTop(dupFileCap)
	if err := s.resolveSkewedKey(sp, key, ScanReq{Tool: "duplicates"}, nil, cache2,
		[]*dirHandle{nil}, []string{dir}, make(chan struct{}), acc3, 16, 96, &missing); err != nil {
		t.Fatal(err)
	}
	g3, _ := acc3.final(s, dupFileCap, nil)
	if len(g3) != len(groups) {
		t.Fatalf("rescan re-read produced %d groups, first pass %d", len(g3), len(groups))
	}
	for i := range g3 {
		if g3[i].Hash != groups[i].Hash || len(g3[i].Files) != len(groups[i].Files) {
			t.Fatalf("rescan re-read diverged at group %d", i)
		}
	}
}

// The bounded group accumulator has to reproduce sort-then-cap exactly: same
// surviving groups, same order, and a truncation report that accounts for
// every group it dropped — including the ones the heap evicted before the
// cap was ever applied.
func TestGroupTopMatchesSortAndCap(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	var raw []rawGroup
	for i := 0; i < 400; i++ {
		n := 2 + rng.Intn(3)
		g := rawGroup{size: int64(1 + rng.Intn(50))}
		g.hash = fmt.Sprintf("h%04d", rng.Intn(500))
		for j := 0; j < n; j++ {
			g.files = append(g.files, fEnt{path: fmt.Sprintf("/v/%03d/%02d", i, j), name: "f", size: g.size})
		}
		raw = append(raw, g)
	}
	// Reference: keep everything, order it, cap it.
	ref := append([]rawGroup(nil), raw...)
	sort.Slice(ref, func(i, j int) bool { return betterGroup(&ref[i], &ref[j]) })
	refGroups := make([]Group, 0, len(ref))
	for i, g := range ref {
		grp := Group{ID: "g" + fmt.Sprint(i), Size: g.size, Hash: g.hash}
		for range g.files {
			grp.Files = append(grp.Files, FileEnt{})
		}
		refGroups = append(refGroups, grp)
	}
	const budget = 120
	wantKept, wantTrunc := capDuplicateGroups(refGroups, budget)

	acc := newGroupTop(budget)
	for _, g := range raw {
		acc.add(g)
	}
	got, trunc := acc.final(&Server{}, budget, nil)

	if len(got) != len(wantKept) {
		t.Fatalf("kept %d groups, sort-then-cap kept %d", len(got), len(wantKept))
	}
	for i := range got {
		if got[i].Hash != wantKept[i].Hash || got[i].Size != wantKept[i].Size ||
			len(got[i].Files) != len(wantKept[i].Files) {
			t.Fatalf("group %d differs: %+v vs %+v", i, got[i], wantKept[i])
		}
	}
	if trunc == nil || wantTrunc == nil {
		t.Fatalf("both paths must report truncation: %+v vs %+v", trunc, wantTrunc)
	}
	if trunc.Groups != wantTrunc.Groups || trunc.Files != wantTrunc.Files || trunc.Cap != budget {
		t.Fatalf("truncation must account for every dropped group: %+v vs %+v", trunc, wantTrunc)
	}
}

// Partitioning cannot split a candidate key, so one wildly over-populated
// key (every macOS .DS_Store is 6148 bytes) cannot go through an ordinary
// window. Truncating it would SILENTLY LOSE any duplicate pair that happened
// to sit in the dropped part, so window must hand the key back instead.
func TestWindowHandsBackASkewedKeyInsteadOfTruncating(t *testing.T) {
	dir := t.TempDir()
	sp, err := newSpill(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.close()
	mod := time.Unix(1722173315, 0)
	for i := 0; i < 500; i++ { // the skewed key
		if err := sp.add(0, &fEnt{size: 6148, mod: mod, rel: fmt.Sprintf("d/ds%04d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 20; i++ { // an ordinary one
		if err := sp.add(0, &fEnt{size: 999, mod: mod, rel: fmt.Sprintf("d/other%04d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	got, over, skipped, err := sp.window(0, 1, MatchOpts{}, []*dirHandle{nil}, []string{"/r"}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 {
		t.Fatalf("nothing may be skipped silently, %d were", skipped)
	}
	if len(over) != 1 {
		t.Fatalf("want the skewed key handed back, got %v", over)
	}
	// The window holds the ordinary key only — every member of it.
	if len(got) != 20 {
		t.Fatalf("want the 20 unskewed records, got %d", len(got))
	}
	for _, f := range got {
		if f.size != 999 {
			t.Fatalf("skewed records must not be in the ordinary window: %+v", f)
		}
	}
	// And the skewed key's members are all still there to be re-partitioned.
	n := 0
	if err := sp.each(func(r *spillRec) error {
		if r.key(MatchOpts{}) == over[0] {
			n++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n != 500 {
		t.Fatalf("the skewed key must keep every member for the prefix pass, has %d", n)
	}
}

// The sub-spill is what makes a skewed key tractable: files can only be
// duplicates if their content hash agrees, so partitioning by that tag is
// group-preserving. Like the outer window, an over-populated tag is handed
// back to be split further rather than truncated.
func TestTagWindowPartitionsWithoutLoss(t *testing.T) {
	dir := t.TempDir()
	sub, err := newSpill(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.close()
	for i := 0; i < 60; i++ { // 60 records over 6 distinct content tags
		if err := sub.addRaw(0, 6148, 100, fmt.Sprintf("d/f%03d", i), prefixTag(fmt.Sprintf("pfx%d", i%6))); err != nil {
			t.Fatal(err)
		}
	}
	for _, parts := range []int{1, 2, 3, 5} {
		seen := map[string]int{}
		for p := 0; p < parts; p++ {
			win, over, crowded, err := sub.tagWindow(p, parts, 0, 0, []*dirHandle{nil}, []string{"/r"})
			if err != nil {
				t.Fatal(err)
			}
			if len(over) != 0 || crowded != 0 {
				t.Fatalf("%d parts: nothing is over an unset cap, got over=%v crowded=%d", parts, over, crowded)
			}
			for _, f := range win {
				seen[f.path]++
			}
		}
		if len(seen) != 60 {
			t.Fatalf("%d parts materialized %d of 60 records", parts, len(seen))
		}
		for p, n := range seen {
			if n != 1 {
				t.Fatalf("%d parts returned %s %d times", parts, p, n)
			}
		}
	}
	// With a cap, over-populated tags come back untouched — not truncated.
	win, over, _, err := sub.tagWindow(0, 1, 5, 0, []*dirHandle{nil}, []string{"/r"})
	if err != nil {
		t.Fatal(err)
	}
	if len(over) != 6 || len(win) != 0 {
		t.Fatalf("want all 6 tags handed back whole, got over=%d win=%d", len(over), len(win))
	}
	// tagSlice is the terminal step: one tag, capped, reporting what it left.
	slice, skipped, err := sub.tagSlice(over[0], []*dirHandle{nil}, []string{"/r"}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(slice) != 4 || skipped != 6 {
		t.Fatalf("tagSlice must cap and report: kept %d skipped %d", len(slice), skipped)
	}
}

// capCutIndex always keeps the first group whole, which is what lets a
// genuinely huge group survive the cap at all — but a group must never
// exceed the whole stored-results budget through that door, or the snapshot,
// the state file and the export all inherit an unbounded result.
func TestOversizedGroupIsCappedAndReported(t *testing.T) {
	acc := newGroupTop(dupFileCap)
	const budget = 10
	huge := rawGroup{size: 4, hash: "h"}
	for i := 0; i < 25; i++ {
		huge.files = append(huge.files, fEnt{path: fmt.Sprintf("/v/f%03d", i)})
	}
	huge.extra = 7 // members the scan itself already dropped
	acc.add(huge)
	got, trunc := acc.final(&Server{}, budget, nil)
	if len(got) != 1 {
		t.Fatalf("want the one group kept, got %d", len(got))
	}
	if len(got[0].Files) != budget {
		t.Fatalf("final must cap the group at the budget, kept %d", len(got[0].Files))
	}
	// 7 dropped before the accumulator + the 15 final() had to cut.
	if trunc == nil || trunc.Files != 22 {
		t.Fatalf("every dropped member must be reported: %+v", trunc)
	}
}

// The hash cache's entry cap has to hold in RAM during the scan, not only in
// the file it writes afterwards.
func TestHashCacheTrimsInMemory(t *testing.T) {
	oldMax, oldHigh := hashCacheMax, hashCacheHigh
	hashCacheMax, hashCacheHigh = 100, 125
	defer func() { hashCacheMax, hashCacheHigh = oldMax, oldHigh }()

	c := loadHashCache(t.TempDir())
	pfx := strings.Repeat("ab", 32)
	full := strings.Repeat("cd", 32)
	for i := 0; i < 1000; i++ {
		c.record(fmt.Sprintf("/vol1/f%05d", i), 10, 20, pfx, full)
		if len(c.ents) > hashCacheHigh {
			t.Fatalf("map grew to %d, past the high-water mark", len(c.ents))
		}
	}
	// The high-water mark is the live bound; save() brings it back to the cap.
	if err := c.save(); err != nil {
		t.Fatal(err)
	}
	if len(c.ents) > hashCacheMax {
		t.Fatalf("save must leave at most the cap, holds %d", len(c.ents))
	}
	c2 := loadHashCache(filepath.Dir(c.path))
	if len(c2.ents) > hashCacheMax {
		t.Fatalf("written file holds %d entries, past the cap", len(c2.ents))
	}
}

// The daemon log must not grow without limit in the package var dir.
func TestRotatingLogRotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dupfinder.log")
	l, err := newRotatingLog(path)
	if err != nil {
		t.Fatal(err)
	}
	line := []byte(strings.Repeat("x", 4096) + "\n")
	for written := 0; written < logMaxBytes+len(line)*4; written += len(line) {
		if _, err := l.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > logMaxBytes {
		t.Fatalf("live log is %d bytes, past the rotation size", fi.Size())
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotated generation missing: %v", err)
	}
}

// A spill round trip must survive being distilled and then read back one
// partition at a time — the path every duplicates scan now takes.
func TestSpillDistilAndWindow(t *testing.T) {
	dir := t.TempDir()
	sp, err := newSpill(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.close()
	mod := time.Unix(1722173315, 0)
	counter := newKeyCounter()
	var want []string
	for i := 0; i < 50; i++ {
		size := int64(100 + i/2) // pairs share a size, the odd tail does not
		e := fEnt{size: size, mod: mod, rel: fmt.Sprintf("d/f%02d.bin", i), name: fmt.Sprintf("f%02d.bin", i)}
		counter.add(candHash(&e, MatchOpts{}))
		if err := sp.add(0, &e); err != nil {
			t.Fatal(err)
		}
	}
	cs, err := newSpill(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.close()
	n, err := sp.distil(counter, MatchOpts{}, cs)
	if err != nil {
		t.Fatal(err)
	}
	all, _, _, err := cs.window(0, 1, MatchOpts{}, []*dirHandle{nil}, []string{"/r"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != n {
		t.Fatalf("distil counted %d candidates, one window materialized %d", n, len(all))
	}
	for _, f := range all {
		want = append(want, f.path)
	}
	sort.Strings(want)

	// The same candidates must come back out of any number of partitions,
	// once each.
	for _, parts := range []int{2, 5, 9} {
		var got []string
		for p := 0; p < parts; p++ {
			win, _, _, err := cs.window(p, parts, MatchOpts{}, []*dirHandle{nil}, []string{"/r"}, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range win {
				got = append(got, f.path)
			}
		}
		sort.Strings(got)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("%d partitions lost or duplicated candidates: %d vs %d", parts, len(got), len(want))
		}
	}
}
