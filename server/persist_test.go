package main

// Phase-3 scale tests: the spill/candidate pass, the persistent hash cache,
// result persistence across daemon restarts, and the interrupted-scan
// marker.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSpillCandidates(t *testing.T) {
	sp, err := newSpill(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer sp.close()
	mod := time.Unix(1722173315, 0)
	ents := []fEnt{
		{size: 5, mod: mod, rel: "x/α ,b.bin", name: "α ,b.bin"},
		{size: 5, mod: mod, rel: "y/other.bin", name: "other.bin"},
		{size: 7, mod: mod, rel: "z/unique.bin", name: "unique.bin"},
	}
	counter := newKeyCounter()
	for i := range ents {
		counter.add(candHash(&ents[i], MatchOpts{}))
		if err := sp.add(0, &ents[i]); err != nil {
			t.Fatal(err)
		}
	}
	cs, err := newSpill(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cs.close()
	if _, err := sp.distil(counter, MatchOpts{}, cs); err != nil {
		t.Fatal(err)
	}
	cands, _, _, err := cs.window(0, 1, MatchOpts{}, []*dirHandle{nil}, []string{"/vol1/root"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("want the size-5 pair as candidates, got %+v", cands)
	}
	f := cands[0]
	if f.path != "/vol1/root/x/α ,b.bin" || f.name != "α ,b.bin" ||
		f.dir != "/vol1/root/x" || f.size != 5 || !f.mod.Equal(mod) || f.rel != "x/α ,b.bin" {
		t.Fatalf("candidate fields wrong after round-trip: %+v", f)
	}
}

// Match criteria join the candidate key: same-size files with different
// names are not candidates under match-by-name.
func TestSpillCandidatesMatchName(t *testing.T) {
	sp, err := newSpill(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer sp.close()
	m := MatchOpts{Name: true}
	ents := []fEnt{
		{size: 5, mod: time.Unix(1, 0), rel: "x/a.bin", name: "a.bin"},
		{size: 5, mod: time.Unix(2, 0), rel: "y/b.bin", name: "b.bin"},
	}
	counter := newKeyCounter()
	for i := range ents {
		counter.add(candHash(&ents[i], m))
		sp.add(0, &ents[i])
	}
	cs, err := newSpill(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cs.close()
	if _, err := sp.distil(counter, m, cs); err != nil {
		t.Fatal(err)
	}
	cands, _, _, err := cs.window(0, 1, m, []*dirHandle{nil}, []string{"/r"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("different names must not collide under match-by-name: %+v", cands)
	}
}

func TestHashCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	pfx := strings.Repeat("ab", 32)  // 64 hex chars — a full prefix hash
	full := strings.Repeat("cd", 32) // 64 hex chars — a full content hash
	c := loadHashCache(dir)
	if c.gen != 1 {
		t.Fatalf("fresh cache gen = %d", c.gen)
	}
	c.record("/v/a.bin", 10, 100, pfx, full)
	if h, ok := c.lookup("/v/a.bin", 10, 100, pfx); !ok || h != full {
		t.Fatalf("lookup after record failed: %q %v", h, ok)
	}
	// Any identity mismatch — size, mtime, prefix — must miss.
	if _, ok := c.lookup("/v/a.bin", 11, 100, pfx); ok {
		t.Fatal("size mismatch must miss")
	}
	if _, ok := c.lookup("/v/a.bin", 10, 101, pfx); ok {
		t.Fatal("mtime mismatch must miss")
	}
	if _, ok := c.lookup("/v/a.bin", 10, 100, strings.Repeat("ef", 32)); ok {
		t.Fatal("prefix mismatch must miss")
	}
	if err := c.save(); err != nil {
		t.Fatal(err)
	}
	// A later scan loads the file and advances the generation. The entry is
	// now HISTORY: a previous scan's hash can never vouch for today's bytes,
	// so it must not be served even though size, mtime and prefix all match.
	c2 := loadHashCache(dir)
	if c2.gen != 2 {
		t.Fatalf("reloaded gen = %d, want 2", c2.gen)
	}
	if _, ok := c2.lookup("/v/a.bin", 10, 100, pfx); ok {
		t.Fatal("a previous scan's hash was served — every scan must re-read")
	}
	// As history it still works both ways: a different prefix under the same
	// size and mtime is rot evidence via the live bucket…
	if !c2.priorContentChanged("/v/a.bin", 10, 100, strings.Repeat("ef", 32)) {
		t.Fatal("prefix moved under unchanged metadata — history must say so")
	}
	// …and a different FULL hash behind an identical prefix (rot beyond the
	// first 64 KiB) is seen by record() as the fresh hash lands.
	fresh := strings.Repeat("55", 32)
	if !c2.record("/v/a.bin", 10, 100, pfx, fresh) {
		t.Fatal("full hash moved under unchanged metadata and prefix — record must report it")
	}
	if !c2.priorContentChanged("/v/a.bin", 10, 100, pfx) {
		t.Fatal("the changed set must remember the rot after the overwrite")
	}
	// Once recorded THIS scan, the fresh hash is reused for the rest of it.
	if h, ok := c2.lookup("/v/a.bin", 10, 100, pfx); !ok || h != fresh {
		t.Fatalf("same-scan reuse failed: %q %v", h, ok)
	}
	// Corrupt cache file: load starts empty instead of failing.
	if err := os.WriteFile(filepath.Join(dir, hashCacheFile), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	c3 := loadHashCache(dir)
	if len(c3.ents) != 0 {
		t.Fatalf("corrupt cache should load empty, got %d entries", len(c3.ents))
	}
	// A nil cache is inert but safe (unit tests pass nil into scanDuplicates).
	var nc *hashCache
	nc.record("/x", 1, 1, pfx, full)
	if _, ok := nc.lookup("/x", 1, 1, pfx); ok {
		t.Fatal("nil cache must miss")
	}
}

// While a scan is still running, the RAM bound must be paid with THIS scan's
// own entries (re-creatable by a re-read, evidence already captured), never
// with prior-scan history — the corrupted-files pass at the end of the scan
// still needs that history for its rot comparisons. The old keep-newest trim
// deleted exactly the wrong side: at a full store, one partition's worth of
// new records pushed the entire prior generation out mid-scan.
func TestMidScanTrimProtectsHistory(t *testing.T) {
	oldMax, oldHigh := hashCacheMax, hashCacheHigh
	hashCacheMax, hashCacheHigh = 8, 10
	defer func() { hashCacheMax, hashCacheHigh = oldMax, oldHigh }()
	c := &hashCache{ents: map[uint64]hcEnt{}, gen: 2}
	for i := 0; i < 8; i++ {
		c.ents[uint64(1000+i)] = hcEnt{Size: 1, Mod: 1, Gen: 1}
	}
	pfx := strings.Repeat("ab", 32)
	for i := 0; i < 50; i++ {
		c.record(fmt.Sprintf("/v/new%03d.bin", i), int64(i), 9, pfx, strings.Repeat("cd", 32))
	}
	if len(c.ents) > hashCacheHigh {
		t.Fatalf("RAM bound broken: %d entries", len(c.ents))
	}
	for i := 0; i < 8; i++ {
		if _, ok := c.ents[uint64(1000+i)]; !ok {
			t.Fatal("a mid-scan trim deleted prior-scan history this scan still needs")
		}
	}
}

// A mid-scan save is crash insurance for the NEXT scan; it must not cost THIS
// scan anything. The file it writes is capped (newest generations first, so a
// reload gets the freshest baselines), but the in-RAM map — including the
// history the corrupted-files pass has not consumed yet — stays intact.
func TestMidScanSaveKeepsHistoryInRAM(t *testing.T) {
	oldMax, oldHigh := hashCacheMax, hashCacheHigh
	hashCacheMax, hashCacheHigh = 4, 100
	defer func() { hashCacheMax, hashCacheHigh = oldMax, oldHigh }()
	dir := t.TempDir()
	c := loadHashCache(dir)
	c.gen = 2
	oldPaths := []string{"/v/h0.bin", "/v/h1.bin", "/v/h2.bin"}
	for _, p := range oldPaths {
		c.ents[pathKey(p)] = hcEnt{Size: 10, Mod: 20, Gen: 1, Tag: pathTag(p)}
	}
	pfx := strings.Repeat("ab", 32)
	for i := 0; i < 3; i++ {
		c.record(fmt.Sprintf("/v/new%d.bin", i), int64(i), 9, pfx, strings.Repeat("cd", 32))
	}
	if err := c.saveMid(); err != nil {
		t.Fatal(err)
	}
	if len(c.ents) != 6 {
		t.Fatalf("saveMid deleted from RAM: %d entries left of 6", len(c.ents))
	}
	// The unconsumed history must still answer for rot detection…
	if !c.priorContentChanged(oldPaths[0], 10, 20, pfx) {
		t.Fatal("history stopped answering after a mid-scan save")
	}
	// …while the file on disk honors the cap, newest generations first.
	c2 := loadHashCache(dir)
	if len(c2.ents) != 4 {
		t.Fatalf("mid-scan save wrote %d entries, cap is 4", len(c2.ents))
	}
	newest := 0
	for _, e := range c2.ents {
		if e.Gen == 2 {
			newest++
		}
	}
	if newest != 3 {
		t.Fatalf("the capped view must keep all 3 newest entries, kept %d", newest)
	}
}

// Resuming an interrupted scan continues its generation: what that run
// already read is served rather than re-read — that is what resuming MEANS,
// and the user chose it. Pinned by the one observable difference: a file
// rotted AFTER the interrupted run read it is not noticed by the resumed
// run (it trusts its own earlier read), while Start Over — a normal load,
// generation advanced — catches it. Also pinned: the gen-0 fallback (a run
// that died before opening the store has nothing to continue) and that a
// resumed run's completion does not leak its generation to the NEXT scan.
func TestResumeContinuesInterruptedScan(t *testing.T) {
	dir := t.TempDir()
	body := bytes.Repeat([]byte{0x3c}, 80*1024)
	a := filepath.Join(dir, "a.bin")
	b := filepath.Join(dir, "b.bin")
	if err := os.WriteFile(a, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, body, 0o644); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(a)
	if err := os.Chtimes(b, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	files := []fEnt{
		{path: a, name: "a.bin", dir: dir, size: fi.Size(), mod: fi.ModTime()},
		{path: b, name: "b.bin", dir: dir, size: fi.Size(), mod: fi.ModTime()},
	}
	state := t.TempDir()
	s := &Server{}
	// The "interrupted" run: it read both files in full and saved, then died.
	c1 := loadHashCache(state)
	deadGen := c1.gen
	if g, _ := s.scanDuplicates(files, MatchOpts{}, c1, make(chan struct{})); len(g) != 1 {
		t.Fatalf("setup scan should group the pair: %+v", g)
	}
	if err := c1.save(); err != nil {
		t.Fatal(err)
	}
	// Rot lands after that run's read, past the prefix, metadata restored.
	rotted := append([]byte(nil), body...)
	rotted[70*1024] ^= 0xff
	if err := os.WriteFile(b, rotted, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(b, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	// RESUME: the dead run's generation continues, its reads are served, and
	// the pair still groups — the resumed scan is that same scan, blind to
	// what happened after its own reads, exactly as the user asked.
	cr := loadHashCacheResume(state, deadGen)
	if cr.gen != deadGen {
		t.Fatalf("resume must continue gen %d, got %d", deadGen, cr.gen)
	}
	if g, _ := s.scanDuplicates(files, MatchOpts{}, cr, make(chan struct{})); len(g) != 1 {
		t.Fatalf("resume must serve the dead run's reads: %+v", g)
	}
	// START OVER: a normal load advances the generation, everything is
	// re-read, and the rot breaks the pair.
	if g, _ := s.scanDuplicates(files, MatchOpts{}, loadHashCache(state), make(chan struct{})); len(g) != 0 {
		t.Fatalf("start over must re-read and catch the rot: %+v", g)
	}
	// Gen-0 fallback: nothing to continue, behaves as a fresh scan.
	if c0 := loadHashCacheResume(state, 0); c0.gen == deadGen {
		t.Fatal("a gen-0 resume must fall back to a normal load")
	}
	// A resumed run's save does not leak: the next NORMAL scan advances past
	// the dead run's generation and re-reads.
	if err := cr.save(); err != nil {
		t.Fatal(err)
	}
	if cn := loadHashCache(state); cn.gen <= deadGen {
		t.Fatalf("after a resumed run completes, the next scan must advance past gen %d, got %d", deadGen, cn.gen)
	}
}

// The changed set is keyed by 64-bit FNV, which is not collision-resistant;
// the stored BLAKE3 tag is what stops a colliding path from inheriting
// another file's bit-rot conviction. Plant the capture under a different
// path's bucket key — exactly what an FNV collision would produce — and the
// query must refuse it.
func TestChangedSetGuardsPathIdentity(t *testing.T) {
	c := &hashCache{ents: map[uint64]hcEnt{}, gen: 1}
	pfx := strings.Repeat("ab", 32)
	c.record("/v/p1.bin", 100, 200, pfx, strings.Repeat("11", 32))
	c.gen++
	if !c.record("/v/p1.bin", 100, 200, pfx, strings.Repeat("22", 32)) {
		t.Fatal("genuine displacement must be captured")
	}
	if !c.priorContentChanged("/v/p1.bin", 100, 200, pfx) {
		t.Fatal("the changed path itself must read as changed")
	}
	c.mu.Lock()
	c.changed[pathKey("/v/other.bin")] = c.changed[pathKey("/v/p1.bin")]
	c.mu.Unlock()
	if c.priorContentChanged("/v/other.bin", 100, 200, pfx) {
		t.Fatal("a path-hash collision handed one file another file's rot evidence")
	}
	if !c.changedPath("/v/p1.bin") || c.changedPath("/v/other.bin") {
		t.Fatal("changedPath must apply the same tag guard")
	}
}

// The save-time trim keeps the newest generations when the cap is exceeded.
func TestHashCacheTrim(t *testing.T) {
	dir := t.TempDir()
	c := loadHashCache(dir)
	c.gen = 5
	old := hcEnt{Size: 1, Mod: 1, Gen: 1}
	for i := 0; i < hashCacheMax; i++ {
		c.ents[uint64(i)] = hcEnt{Size: 1, Mod: 1, Gen: 5}
	}
	c.ents[uint64(hashCacheMax)] = old // one stale entry over the cap
	c.dirty = true
	if err := c.save(); err != nil {
		t.Fatal(err)
	}
	c2 := loadHashCache(dir)
	if len(c2.ents) != hashCacheMax {
		t.Fatalf("trim kept %d entries, want %d", len(c2.ents), hashCacheMax)
	}
	if _, ok := c2.ents[uint64(hashCacheMax)]; ok {
		t.Fatal("the stale generation survived the trim")
	}
}

func TestStatePersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := &Server{varDir: dir, nextID: 42, lastTool: "duplicates",
		refDirs: []string{"/v/ref"},
		keepers: map[string]bool{"/v/keep/one.bin": true},
		results: map[string]*toolResult{
			"duplicates": {
				Tool: "duplicates",
				Groups: []Group{mkGroup("g0", 9,
					FileEnt{Name: "a", Dir: "/v/x"}, FileEnt{Name: "a", Dir: "/v/y"})},
				Truncated: &TruncInfo{Groups: 1, Files: 3, Cap: 100},
				Match:     &MatchOpts{Name: true},
				Scanned:   "then",
			},
			"empty_files": {Tool: "empty_files", Files: []FileEnt{{ID: "f1", Name: "e", Dir: "/v"}}, Scanned: "then"},
		}}
	s.saveState()

	s2 := &Server{varDir: dir, results: map[string]*toolResult{}}
	s2.loadState()
	dup := s2.results["duplicates"]
	if dup == nil || len(dup.Groups) != 1 || len(dup.Groups[0].Files) != 2 ||
		dup.Truncated == nil || dup.Truncated.Files != 3 || dup.Match == nil || !dup.Match.Name {
		t.Fatalf("duplicates result did not round-trip: %+v", dup)
	}
	if ef := s2.results["empty_files"]; ef == nil || len(ef.Files) != 1 {
		t.Fatalf("flat result did not round-trip: %+v", ef)
	}
	if !s2.keepers["/v/keep/one.bin"] || s2.nextID != 42 || s2.lastTool != "duplicates" ||
		len(s2.refDirs) != 1 || s2.refDirs[0] != "/v/ref" {
		t.Fatalf("state fields did not round-trip: keepers=%v nextID=%d lastTool=%q refDirs=%v",
			s2.keepers, s2.nextID, s2.lastTool, s2.refDirs)
	}

	// Corrupt state file: the daemon starts empty rather than failing.
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte("not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	s3 := &Server{varDir: dir, results: map[string]*toolResult{}}
	s3.loadState()
	if len(s3.results) != 0 {
		t.Fatalf("corrupt state should load empty, got %d results", len(s3.results))
	}
}

func TestScanMarkerLifecycle(t *testing.T) {
	s := &Server{varDir: t.TempDir()}
	if m := s.loadMarker(); m != nil {
		t.Fatalf("no marker expected, got %+v", m)
	}
	req := ScanReq{Tool: "duplicates",
		// Unsorted, duplicated and trailing-slashed on purpose: the marker
		// must store the normalized form, or two spellings of the same
		// request would refuse to resume each other.
		Dirs:    []string{"/volume1/b/", "/volume1/a", "/volume1/a"},
		RefDirs: []string{"/volume1/ref"},
		Recurse: true, Match: MatchOpts{Name: true}}
	s.writeMarker(&req, 7)
	m := s.loadMarker()
	if m == nil || m.Tool != "duplicates" || m.StartedAt == "" {
		t.Fatalf("marker did not round-trip: %+v", m)
	}
	// Gen is what a resume continues — a marker that loses it silently turns
	// every resume into a full re-read.
	if m.Gen != 7 {
		t.Fatalf("marker generation did not round-trip: %+v", m)
	}
	// The request is the OTHER half of the resume gate: a marker that loses
	// any of it silently widens what may adopt the dead run's generation.
	if !eqStrings(m.Dirs, []string{"/volume1/a", "/volume1/b"}) ||
		!eqStrings(m.RefDirs, []string{"/volume1/ref"}) ||
		!m.Recurse || m.Match != (MatchOpts{Name: true}) {
		t.Fatalf("marker request did not round-trip normalized: %+v", m)
	}
	if !m.matches(&req) {
		t.Fatalf("a marker must match the very request it recorded: %+v", m)
	}
	// test/run.sh greps the marker JSON for the prefix {"tool": — Tool must
	// stay the first serialized field however the struct grows.
	if b, err := os.ReadFile(filepath.Join(s.varDir, markerFile)); err != nil || !strings.HasPrefix(string(b), `{"tool":`) {
		t.Fatalf("marker JSON must begin with the tool field: %s (%v)", b, err)
	}
	s.clearMarker()
	if m := s.loadMarker(); m != nil {
		t.Fatalf("marker should be cleared, got %+v", m)
	}
	// Persistence disabled (no state dir): everything is a quiet no-op.
	s2 := &Server{}
	s2.writeMarker(&ScanReq{Tool: "temp_files"}, 0)
	if m := s2.loadMarker(); m != nil {
		t.Fatal("stateless daemon must not report markers")
	}
}

// A resume adopts the dead run's hash-store generation, handing that run's
// reads to the new scan — sound only when the new scan IS that run. matches()
// is the gate: the request must be identical in everything that shapes what
// the scan reads and how it groups, not merely name the same tool. Tool-only
// gating let a RESCOPED "resume" be served reads the dead run made for a
// different scan — a rotted file whose size and mtime survived could then
// stay grouped as bit-for-bit identical, the exact hole the generation gate
// exists to close.
func TestResumeMatchesExactRequestOnly(t *testing.T) {
	base := func() ScanReq {
		return ScanReq{Tool: "duplicates",
			Dirs:    []string{"/volume1/a", "/volume1/b"},
			RefDirs: []string{"/volume1/ref"},
			Recurse: true, Match: MatchOpts{Modified: true}}
	}
	b := base()
	m := scanMarker{Tool: b.Tool, Gen: 4, Dirs: normPaths(b.Dirs),
		RefDirs: normPaths(b.RefDirs), Recurse: b.Recurse, Match: b.Match}

	same := base()
	// Order, repetition and trailing slashes are spelling, not identity.
	spelled := base()
	spelled.Dirs = []string{"/volume1/b", "/volume1/a/", "/volume1/a"}

	rescoped := base()
	rescoped.Dirs = []string{"/volume1/a"}
	extraDir := base()
	extraDir.Dirs = append(extraDir.Dirs, "/volume1/c")
	refsGone := base()
	refsGone.RefDirs = nil
	// The same PATHS with a folder promoted from scope to reference change
	// what is protected and what may move — different scan.
	promoted := base()
	promoted.Dirs = []string{"/volume1/a"}
	promoted.RefDirs = []string{"/volume1/b", "/volume1/ref"}
	matchOff := base()
	matchOff.Match = MatchOpts{}
	noRecurse := base()
	noRecurse.Recurse = false
	otherTool := base()
	otherTool.Tool = "temp_files"

	cases := []struct {
		name string
		req  ScanReq
		want bool
	}{
		{"identical request", same, true},
		{"same request spelled differently", spelled, true},
		{"narrower scope", rescoped, false},
		{"extra scan root", extraDir, false},
		{"reference folders removed", refsGone, false},
		{"root promoted to reference folder", promoted, false},
		{"match criteria changed", matchOff, false},
		{"recursion changed", noRecurse, false},
		{"different tool", otherTool, false},
	}
	for _, c := range cases {
		if got := m.matches(&c.req); got != c.want {
			t.Errorf("%s: matches = %v, want %v", c.name, got, c.want)
		}
	}
	// A marker from a build that recorded no request (tool and gen only)
	// must match nothing: it cannot prove the resume is the same scan, and
	// the safe degradation is the full re-read.
	legacy := scanMarker{Tool: "duplicates", Gen: 4}
	if legacy.matches(&b) {
		t.Fatal("a marker without a recorded request must never allow a resume")
	}
}

// A duplicates rescan takes NOTHING from a previous scan's hashes: every
// candidate is re-read in full, so the groups a rescan reports are proven
// identical by that rescan's own reads. The discriminating case is rot BEYOND
// the 64 KiB prefix under an unchanged size and mtime — exactly the file the
// old cross-scan cache kept serving a stale hash for, which held a
// no-longer-identical pair grouped until the entry aged out.
func TestRescanRereadsEverything(t *testing.T) {
	dir := t.TempDir()
	body := bytes.Repeat([]byte{0x5a}, 80*1024) // big enough that rot can hide past 64 KiB
	a := filepath.Join(dir, "a.bin")
	b := filepath.Join(dir, "b.bin")
	if err := os.WriteFile(a, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, body, 0o644); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(a)
	if err := os.Chtimes(b, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	files := []fEnt{
		{path: a, name: "a.bin", dir: dir, size: fi.Size(), mod: fi.ModTime()},
		{path: b, name: "b.bin", dir: dir, size: fi.Size(), mod: fi.ModTime()},
	}
	state := t.TempDir()
	s := &Server{}
	c1 := loadHashCache(state)
	g1, _ := s.scanDuplicates(files, MatchOpts{}, c1, make(chan struct{}))
	if len(g1) != 1 || len(g1[0].Files) != 2 {
		t.Fatalf("first scan should group the pair: %+v", g1)
	}
	if err := c1.save(); err != nil {
		t.Fatal(err)
	}
	// An unchanged rescan reproduces the same groups — from its own reads.
	c2 := loadHashCache(state)
	if len(c2.ents) != 2 {
		t.Fatalf("store should hold both files, has %d", len(c2.ents))
	}
	g2, _ := s.scanDuplicates(files, MatchOpts{}, c2, make(chan struct{}))
	if len(g2) != 1 || g2[0].Hash != g1[0].Hash {
		t.Fatalf("rescan diverged: %+v vs %+v", g2, g1)
	}
	if err := c2.save(); err != nil {
		t.Fatal(err)
	}
	// Rot one copy past the prefix; size, mtime and first 64 KiB all still
	// stand, so only a full re-read can tell the pair apart. A scan that
	// still groups them is lying.
	rotted := append([]byte(nil), body...)
	rotted[70*1024] ^= 0xff
	if err := os.WriteFile(b, rotted, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(b, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	c3 := loadHashCache(state)
	g3, _ := s.scanDuplicates(files, MatchOpts{}, c3, make(chan struct{}))
	if len(g3) != 0 {
		t.Fatalf("rot beyond the prefix survived as a duplicate — a stale hash was served: %+v", g3)
	}
	// And the history knows why the pair fell apart: the fresh full hash
	// displaced a comparable entry holding different content.
	pfx, err := hashFile(files[1].openContent, 64*1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !c3.priorContentChanged(b, fi.Size(), fi.ModTime().Unix(), pfx) {
		t.Fatal("deep rot must be captured as content-changed history")
	}
	if c3.priorContentChanged(a, fi.Size(), fi.ModTime().Unix(), pfx) {
		t.Fatal("the unchanged copy must not read as changed")
	}
}

func TestCandHashDiscriminates(t *testing.T) {
	base := fEnt{size: 5, name: "a", mod: time.Unix(100, 0)}
	other := base
	other.size = 6
	if candHash(&base, MatchOpts{}) == candHash(&other, MatchOpts{}) {
		t.Fatal("different sizes must hash apart")
	}
	renamed := base
	renamed.name = "b"
	if candHash(&base, MatchOpts{}) != candHash(&renamed, MatchOpts{}) {
		t.Fatal("names must not matter without match-by-name")
	}
	if candHash(&base, MatchOpts{Name: true}) == candHash(&renamed, MatchOpts{Name: true}) {
		t.Fatal("names must matter under match-by-name")
	}
	touched := base
	touched.mod = time.Unix(101, 0)
	if candHash(&base, MatchOpts{Modified: true}) == candHash(&touched, MatchOpts{Modified: true}) {
		t.Fatal("mtimes must matter under match-by-modified")
	}
}

// Retiring a tool must retire its data. A state file written by a build that
// still had the tool carries its rows, and they are NOT inert: handleMove's
// allowlist is built from every cached result, so a retired tool's files would
// stay movable — including a keep-one survivor listed only there — with no UI
// able to display them or rescan them away. "big_files" is the concrete case
// (retired in 0115); the assertion is on unknown tools generally.
func TestLoadStateDropsRetiredTools(t *testing.T) {
	dir := t.TempDir()
	s := &Server{varDir: dir, lastTool: "big_files",
		results: map[string]*toolResult{
			"duplicates": {Tool: "duplicates", Scanned: "then",
				Groups: []Group{mkGroup("g0", 9,
					FileEnt{Name: "a", Dir: "/v/x"}, FileEnt{Name: "a", Dir: "/v/y"})}},
			"big_files": {Tool: "big_files", Scanned: "then",
				Files: []FileEnt{{ID: "f9", Name: "huge.iso", Dir: "/v/z", Size: 1 << 30}}},
		}}
	s.saveState()

	s2 := &Server{varDir: dir, results: map[string]*toolResult{}}
	s2.loadState()
	if _, present := s2.results["big_files"]; present {
		t.Error("a retired tool's results survived the load — its rows stay in the move allowlist")
	}
	if dup := s2.results["duplicates"]; dup == nil || len(dup.Groups) != 1 {
		t.Errorf("dropping the retired tool damaged the surviving results: %+v", dup)
	}
	if s2.lastTool != "" {
		t.Errorf("lastTool still names a retired tool (%q) — the UI would open on a tool that no longer exists", s2.lastTool)
	}
}
