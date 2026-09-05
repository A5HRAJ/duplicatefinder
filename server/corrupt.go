package main

// Corrupted-files detection — the second thing the duplicates scan looks for.
//
// Two files that share a size AND a modified time are claiming to be the same
// file: every ordinary way of producing a second copy (cp -p, rsync --times, a
// restore, a backup mirror) carries both across, and every ordinary way of
// CHANGING a file moves the mtime. So when their contents disagree, something
// changed the bytes without going through the filesystem's normal path —
// bit rot, a bad sector, an interrupted transfer, a restore that lost blocks.
//
// The detection rides the duplicates scan's own machinery: the same walk, the
// same spill file, the same windowed partitioning and the same hash cache. It
// keys on (size, mtime) ONLY, deliberately independent of the duplicates
// scan's MatchOpts. That independence is a correctness property, not a
// nicety — candKey folds the filename in when Match.Name is set, and gives
// almost every file a unique per-path sentinel when Match.Created is set, so
// a detection built on candKey would silently report less (or nothing at all)
// depending on checkboxes that have nothing to do with corruption.
//
// The cost is one extra distil sweep of the walk spill plus a 64 KiB prefix
// read per candidate. Full-content reads are mostly shared with the duplicates
// pass through the hash store — reads made earlier in the SAME scan, never a
// previous scan's — and the extra ones are spent only on sets already
// confirmed to differ.
//
// GUARANTEE — detection is preferred over speed: every hash this pass
// consumes was computed from bytes read during this scan. A previous scan's
// hash is never served as a stand-in — it is used only the other way around,
// as history: content that moved anywhere in the file while size and mtime
// stood still is rung 2's bit-rot evidence. Rot beyond the first 64 KiB
// therefore cannot hide behind a cache entry; the price is re-reading every
// candidate in full on every rescan.

import (
	"dupfinder/internal/dirhandle"
	"dupfinder/internal/media"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// corruptMatch is the corrupted-files candidate key: size and modified time,
// nothing else. Expressed as MatchOpts so it can reuse candHash, spillRec.key,
// distil and window unchanged.
var corruptMatch = MatchOpts{Modified: true}

// Stored-result caps for this tool, mirroring the rule stated above
// dupFileCap: keep the rows worth acting on and report the rest, never
// silently. Corrupted sets are rare by construction — a result that hits this
// cap is itself the finding.
var corruptFileCap = 20000

// maxDiffBytes bounds the byte-level comparison. Past it the difference shape
// goes unexamined rather than reading two very large files a second time.
const maxDiffBytes = 2 << 30

// Verdict values stored on FileEnt.Verdict. Empty means the row belongs to a
// tool that does not judge files at all.
const (
	verdictCorrupt = "corrupt"
	verdictIntact  = "intact"
	verdictUnknown = "unknown"
)

// corruptFile is one member of a candidate set, carried as a raw scanner entry
// until the set survives the stored-results cap.
type corruptFile struct {
	f fEnt
	// hash is the full-content hash, or "" when the file could not be read.
	hash string
	pfx  string
	// readErr is why the content could not be read, kept because an I/O error
	// is the strongest corruption evidence there is: on btrfs with data
	// checksums the read fails precisely because the stored bytes no longer
	// match what was written.
	readErr error
	// contentChanged records that an earlier scan's hash store held this path,
	// at this size and mtime, with DIFFERENT content — anywhere in the file,
	// prefix or beyond, since every scan reads candidates in full. A
	// timestamped before/after saying the content moved while the metadata
	// did not: the bit-rot signature.
	contentChanged bool
	verdict        string
	evidence       string
}

// corruptSet is one (size, mtime) family whose members do not all agree.
type corruptSet struct {
	size     int64
	mod      time.Time
	files    []corruptFile // sorted by path
	extra    int           // members the per-set cap dropped, for the truncation report
	variants int           // distinct contents present, counting an unreadable member as its own
	sameName bool          // every member shares a basename — a real copy relation, not a coincidence
	flagged  bool          // cheap evidence already says a member is damaged
}

// rank orders sets by how much they are worth looking at, which is what the
// bounded accumulator keeps when it cannot keep everything. Reclaimable space
// — the duplicates ordering — is meaningless here.
//
//	2  a member is already positively damaged (unreadable, or its content moved
//	   under an unchanged mtime)
//	1  the members share a filename, so they really are meant to be one file
//	0  same size and time, different names: possibly just a coincidence
func (c *corruptSet) rank() int {
	switch {
	case c.flagged:
		return 2
	case c.sameName:
		return 1
	}
	return 0
}

// betterSet is the listing order. The tiebreaks make it total, so a result
// assembled from several scan windows is reproducible rather than dependent on
// map iteration order.
func betterSet(a, b *corruptSet) bool {
	if ra, rb := a.rank(), b.rank(); ra != rb {
		return ra > rb
	}
	if a.size != b.size {
		return a.size > b.size
	}
	return a.files[0].f.path < b.files[0].f.path
}

// corruptTop keeps the best sets seen across every scan window in bounded
// memory. Deliberately NOT groupTop: that one ranks by reclaimable space and
// its dropped-file counter feeds the duplicates truncation report, so sharing
// it would let corrupted sets evict real duplicate groups and would make the
// Duplicate Files tool report a truncation that never happened.
type corruptTop struct {
	mu          sync.Mutex
	limit       int
	sets        []corruptSet
	droppedSets int
	droppedFile int
}

func newCorruptTop(budget int) *corruptTop {
	limit := budget/2 + 1
	if limit < 1 {
		limit = 1
	}
	return &corruptTop{limit: limit}
}

func (t *corruptTop) add(c corruptSet) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sets = append(t.sets, c)
	if len(t.sets) <= t.limit {
		return
	}
	// Kept simple on purpose: the accumulator only ever exceeds its limit on a
	// volume where corrupted sets number in the tens of thousands, and one
	// sort at that point costs nothing next to the scan that found them.
	sort.Slice(t.sets, func(i, j int) bool { return betterSet(&t.sets[i], &t.sets[j]) })
	for _, d := range t.sets[t.limit:] {
		t.droppedSets++
		t.droppedFile += len(d.files) + d.extra
	}
	t.sets = t.sets[:t.limit]
}

// noteSkipped records candidates a window cap meant were never examined.
func (t *corruptTop) noteSkipped(n int) {
	t.mu.Lock()
	t.droppedFile += n
	t.mu.Unlock()
}

// ------------------------------------------------------------- the scan pass

// scanCorrupted is the whole corrupted-files pass. It is driven from the
// duplicates case of runScan, after the duplicates result is complete, and
// reads the candidate spill the caller distilled with corruptMatch.
//
// lo/hi bound the progress it may report. Returns false when cancelled.
func (s *Server) scanCorrupted(cs *spill, nCand int, handles []*dirhandle.Handle, rootOf []string,
	cache *hashCache, cancel chan struct{}, acc *corruptTop, res *toolResult, lo, hi float64) bool {

	parts := 1
	if nCand > dupWindowFiles {
		parts = (nCand + dupWindowFiles - 1) / dupWindowFiles
	}
	span := (hi - lo) / float64(parts)
	for p := 0; p < parts; p++ {
		plo := lo + span*float64(p)
		cands, over, skipped, err := cs.window(p, parts, corruptMatch, handles, rootOf, dupKeyFileCap, dupWindowMax)
		acc.noteSkipped(skipped)
		if err != nil {
			// The duplicates result is already complete and stays; but the
			// corrupted listing is now PARTIAL, and a partial listing that
			// claims to be a finished scan is the one thing this must not do.
			res.Errors = append(res.Errors,
				"the corrupted-files check stopped early — the candidate spill file could not be read: "+err.Error()+
					". Some sets may be missing; scan again to complete it.")
			return true
		}
		if !s.corruptWindow(cands, cache, nil, cancel, acc, plo, plo+span*0.9) {
			return false
		}
		cands = nil
		// A (size, mtime) key with more members than one window can hold is
		// split by content instead of capped — see corruptSkewedKey.
		for i, k := range over {
			klo := plo + span*0.9 + (span*0.1)*float64(i)/float64(max(len(over), 1))
			if !s.corruptSkewedKey(cs, k, handles, rootOf, cache, cancel, acc, res, klo) {
				return false
			}
		}
	}
	return true
}

// corruptWindow classifies one window of candidates: bucket by the exact
// (size, mtime) key, then find the keys whose members do not all hold the same
// bytes.
//
// rotted (nil on the ordinary path) carries deep-rot evidence the skew pass's
// own tagging reads already proved — paths whose content moved under an
// unchanged size and mtime. By the time the sample reaches this window the
// store entry is this generation's, so neither priorContentChanged nor a
// fresh record() can see that history again; the map is the only witness left.
func (s *Server) corruptWindow(files []fEnt, cache *hashCache, rotted map[string]bool,
	cancel chan struct{}, acc *corruptTop, lo, hi float64) bool {

	span := hi - lo
	s.setProgress(lo, "Checking for corrupted copies…")

	// Exact key. candHash is a 64-bit hash and may collide; this string key is
	// what actually decides membership, so a hash collision can only cost a
	// little wasted grouping work, never merge two families.
	keyOf := func(f *fEnt) string {
		return strconv.FormatInt(f.size, 10) + "|" + strconv.FormatInt(f.mod.Unix(), 10)
	}
	byKey := map[string][]int{}
	for i := range files {
		if files[i].isDir || files[i].size == 0 {
			continue
		}
		k := keyOf(&files[i])
		byKey[k] = append(byKey[k], i)
	}
	var members []int
	for k, idx := range byKey {
		if len(idx) < 2 {
			delete(byKey, k)
			continue
		}
		members = append(members, idx...)
	}
	if len(members) == 0 {
		s.setProgress(hi, "Checking for corrupted copies…")
		return true
	}

	// Phase A: the 64 KiB prefix of every member. Differing prefixes already
	// prove differing content, so most confirmations cost nothing beyond this.
	info := make([]corruptFile, len(files))
	var mu sync.Mutex
	parallelEach(members, cancel, func(i int) {
		pfx, err := hashFile(files[i].openContent, 64*1024, cancel)
		mu.Lock()
		defer mu.Unlock()
		info[i].f = files[i]
		if err != nil {
			info[i].readErr = err
			return
		}
		info[i].pfx = pfx
		// The `changed` set answers for paths the duplicates pass already
		// rewrote this scan; the live bucket answers (prefix-only, that is all
		// we have read so far) for paths it has not. A deep change in a path
		// first read below in phase B is caught by record()'s return value in
		// hashMembers instead. rotted answers for what the skew pass's tagging
		// reads proved — evidence the capped `changed` set may have refused.
		info[i].contentChanged = rotted[files[i].path] ||
			cache.priorContentChanged(files[i].path, files[i].size, files[i].mod.Unix(), pfx)
	}, func(done, total int) {
		s.setProgress(lo+span*0.4*float64(done)/float64(max(total, 1)), "Checking for corrupted copies…")
	})
	if cancelled(cancel) {
		return false
	}

	// Phase B: a key whose readable members all share a prefix needs full
	// hashes to tell "identical" from "differs after the first 64 KiB". The
	// hash store usually answers without a second read, because the
	// duplicates pass wanted the same files and read them earlier this scan.
	var needFull []int
	for _, idx := range byKey {
		pfxs := map[string]bool{}
		readable := 0
		for _, i := range idx {
			if info[i].readErr != nil {
				continue
			}
			readable++
			pfxs[info[i].pfx] = true
		}
		if readable >= 2 && len(pfxs) == 1 {
			needFull = append(needFull, idx...)
		}
	}
	s.hashMembers(needFull, info, cache, cancel, lo+span*0.4, lo+span*0.8)
	if cancelled(cancel) {
		return false
	}

	// Decide which keys are corrupted sets, and give every member of a
	// confirmed set a full hash so the result can show what it compared.
	var confirmed [][]int
	for _, idx := range byKey {
		if isCorruptedSet(idx, info) {
			confirmed = append(confirmed, idx)
		}
	}
	var fill []int
	for _, idx := range confirmed {
		for _, i := range idx {
			if info[i].hash == "" && info[i].readErr == nil {
				fill = append(fill, i)
			}
		}
	}
	s.hashMembers(fill, info, cache, cancel, lo+span*0.8, hi)
	if cancelled(cancel) {
		return false
	}

	for _, idx := range confirmed {
		acc.add(buildSet(idx, info))
	}
	s.setProgress(hi, "Checking for corrupted copies…")
	return true
}

// hashMembers fills in full-content hashes, reusing reads already made this
// scan (never an earlier scan's). Read errors are kept rather than swallowed —
// an EIO here is the finding, not a nuisance.
func (s *Server) hashMembers(idx []int, info []corruptFile, cache *hashCache, cancel chan struct{}, lo, hi float64) {
	if len(idx) == 0 {
		return
	}
	var mu sync.Mutex
	parallelEach(idx, cancel, func(i int) {
		e := &info[i]
		if e.readErr != nil || e.hash != "" {
			return
		}
		if full, ok := cache.lookup(e.f.path, e.f.size, e.f.mod.Unix(), e.pfx); ok {
			mu.Lock()
			e.hash = full
			mu.Unlock()
			return
		}
		h, err := hashFile(e.f.openContent, -1, cancel)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			e.readErr = err
			return
		}
		e.hash = h
		// record() compares the fresh full hash against an earlier scan's
		// entry; a true return is the deep-rot evidence phase A could not see,
		// because at phase A only the prefix had been read.
		if cache.record(e.f.path, e.f.size, e.f.mod.Unix(), e.pfx, h) {
			e.contentChanged = true
		}
	}, func(done, total int) {
		s.setProgress(lo+(hi-lo)*float64(done)/float64(max(total, 1)), "Checking for corrupted copies…")
	})
}

// unchangedOnDisk re-stats a member through its own opener and reports
// whether the size and modified time the walk recorded are still current.
// One stat per flagged member, spent only where a rung-2 conviction is on
// the table; any failure to observe reads as "cannot confirm", never as
// evidence.
func unchangedOnDisk(f fEnt) bool {
	h, err := f.openContent()
	if err != nil {
		return false
	}
	defer h.Close()
	st, err := h.Stat()
	if err != nil {
		return false
	}
	return st.Size() == f.size && st.ModTime().Unix() == f.mod.Unix()
}

// isCorruptedSet reports whether a (size, mtime) family's members disagree.
// An unreadable member counts as a disagreement: whatever is on the disk there
// cannot be shown to be the same file, and on a checksumming filesystem the
// failure to read IS the corruption.
func isCorruptedSet(idx []int, info []corruptFile) bool {
	seen := map[string]bool{}
	unreadable := 0
	for _, i := range idx {
		if info[i].readErr != nil {
			if isCancelErr(info[i].readErr) {
				return false // a cancelled read says nothing about the file
			}
			unreadable++
			continue
		}
		if info[i].hash != "" {
			seen[info[i].hash] = true
			continue
		}
		// No full hash: the member had a prefix nothing else shared, which
		// already makes it a distinct variant.
		seen["p:"+info[i].pfx] = true
	}
	return len(seen)+unreadable >= 2 && len(idx) >= 2
}

func buildSet(idx []int, info []corruptFile) corruptSet {
	c := corruptSet{sameName: true}
	variants := map[string]bool{}
	for n, i := range idx {
		e := info[i]
		c.files = append(c.files, e)
		if n == 0 {
			c.size, c.mod = e.f.size, e.f.mod
		}
		if !strings.EqualFold(e.f.name, info[idx[0]].f.name) {
			c.sameName = false
		}
		if e.readErr != nil || e.contentChanged {
			c.flagged = true
		}
		switch {
		case e.readErr != nil:
			variants["e:"+e.f.path] = true // each unreadable member stands alone
		case e.hash != "":
			variants[e.hash] = true
		default:
			variants["p:"+e.pfx] = true
		}
	}
	sort.Slice(c.files, func(i, j int) bool { return c.files[i].f.path < c.files[j].f.path })
	c.variants = len(variants)
	// One set may not exceed the whole stored-results budget, for the same
	// reason a duplicate group may not: the snapshot, the state file and the
	// CSV export are all sized by it. Dropped members are reported.
	if len(c.files) > corruptFileCap {
		c.extra = len(c.files) - corruptFileCap
		c.files = c.files[:corruptFileCap]
	}
	return c
}

// corruptSkewSample bounds how many members of a hugely skewed key are listed.
// A key holding tens of thousands of files that all share a size and a
// modified time is an archive unpacked in one go, not a backup copy — the
// finding is the family itself, and a representative slice of it says as much
// as an exhaustive listing nobody could read. Everything left out is counted
// into the truncation report.
const corruptSkewSample = 500

// corruptSkewedKey handles a (size, mtime) key holding more files than a
// window should take. The key cannot be split by partitioning — every file
// under it has to be compared with the others — so it is split by CONTENT,
// exactly as the duplicates ladder splits a skewed candidate key, and for the
// same reason: capping an unexamined population is how a real finding gets
// thrown away. Every member is tagged, and only then is anything dropped.
//
// One rung, by the FULL content hash — deliberately not a prefix shortcut
// that stops as soon as two 64 KiB prefixes differ. A prefix-granular sample
// lets a member whose rot lives beyond its first 64 KiB hide behind the
// prefix it shares with its intact siblings: never full-hashed, never
// listed, never convicted. Full tags make every distinct content its own
// tag, so the variant-spanning sample below cannot miss it, and the full
// read is what feeds record()'s rot capture. Most of these reads are
// answered by this scan's own earlier work anyway — the duplicates pass
// usually wanted the same files. Detection over speed.
//
// What a cap finally applies to is a family already known to disagree, listed
// down to a sample that spans its variants — not an unexamined population.
// A member flagged as changed-under-unchanged-metadata is always kept: it IS
// the finding, and no quota may sample it away. The flag has two carriers —
// the store's `changed` set for what earlier passes recorded, and this pass's
// own rotted map for what its tagging reads just proved — because the store's
// set is capped, and a finding made right here must not depend on whether
// 131072 other files already claimed a slot.
//
// One honest gap at this scale: hashSubSpill counts a member it cannot read
// into the truncation report and drops it, so an I/O error inside a key this
// large is reported as a number rather than as a convicted row.
func (s *Server) corruptSkewedKey(cs *spill, key uint64, handles []*dirhandle.Handle, rootOf []string,
	cache *hashCache, cancel chan struct{}, acc *corruptTop, res *toolResult, lo float64) bool {

	// A failure anywhere below leaves this one family unexamined. Say so:
	// "no sets found" and "we could not look" must never render the same.
	stopped := func(err error) bool {
		res.Errors = append(res.Errors,
			"a large group of same-size, same-date files could not be checked for corruption: "+err.Error())
		return true
	}
	s.setProgress(lo, "Checking for corrupted copies…")
	// Rot the tagging reads prove right here must survive to the sample and
	// the verdict even when the store's capped `changed` set is full and
	// cannot hold it — rotted is this pass's own uncapped carrier, alive
	// only for this one family.
	rotted := map[string]bool{}
	sub, _, err := s.hashSubSpill(cs, handles, rootOf, cancel, acc, cache, true,
		func(path string) { rotted[path] = true },
		func(r *spillRec) bool { return r.key(corruptMatch) == key })
	if err != nil {
		return stopped(err)
	}
	defer sub.close()
	if cancelled(cancel) {
		return false
	}
	differ, terr := sub.tagsDiffer()
	if terr != nil {
		return stopped(terr)
	}
	if !differ {
		return true // every member holds the same bytes: duplicates, not damage
	}
	// Confirmed to disagree. Take a sample that SPANS the variants rather than
	// the first N records, which could all belong to one of them. The kept
	// check is first so that once the sample is full the quota map stops
	// growing — otherwise it would gain an entry per distinct tag, which is
	// the unbounded thing this whole path exists to avoid. A member carrying
	// rot evidence bypasses both bounds: rotted holds what the full pass
	// above just proved, changedPath what earlier passes recorded (cap
	// permitting), and those rows are the point of the report.
	const perTag = 2
	quota := map[uint64]int{}
	kept, dropped := 0, 0
	sample, _, merr := sub.materialize(func(r *spillRec) bool {
		if r.rootIdx < len(rootOf) {
			if p := filepath.Join(rootOf[r.rootIdx], r.rel); rotted[p] || cache.changedPath(p) {
				kept++
				return true
			}
		}
		if kept >= corruptSkewSample || quota[r.tag] >= perTag {
			dropped++
			return false
		}
		quota[r.tag]++
		kept++
		return true
	}, handles, rootOf, 0)
	if merr != nil {
		return stopped(merr)
	}
	acc.noteSkipped(dropped)
	// The sample goes through the ordinary window, so its rows get real hashes
	// and real verdicts rather than a second, parallel code path. rotted rides
	// along: the window's own reads cannot re-detect what the tagging pass
	// already overwrote in the store (a same-generation compare is mute), so
	// the evidence must arrive with the sample.
	return s.corruptWindow(sample, cache, rotted, cancel, acc, lo, lo)
}

// ------------------------------------------------------------ the verdicts

// final orders the surviving sets, applies the stored-results cap on set
// boundaries, and only then spends the expensive evidence — container
// validation and the byte-level comparison — on the sets the user will
// actually see. Sets the accumulator itself dropped are folded into the
// truncation report, so the cut stays honest whatever path made it.
func (t *corruptTop) final(s *Server, budget int, cancel chan struct{}) ([]Group, *TruncInfo) {
	sort.Slice(t.sets, func(i, j int) bool { return betterSet(&t.sets[i], &t.sets[j]) })
	sets := t.sets
	cut := capCutIndex(len(sets), func(i int) int { return len(sets[i].files) }, budget)
	dropS, dropF := t.droppedSets, t.droppedFile
	for i, c := range sets {
		if i >= cut {
			dropS++
			dropF += len(c.files)
		}
		dropF += c.extra
	}
	out := make([]Group, 0, cut)
	var captures []captureSlot
	for i := range sets[:cut] {
		c := &sets[i]
		if len(c.files) > budget {
			dropF += len(c.files) - budget
			c.files = c.files[:budget]
		}
		judgeSet(c, cancel)
		grp := Group{
			ID: "c" + strconv.Itoa(i), Ext: extOf(c.files[0].f.name),
			Size: c.size, Mod: fmtTime(c.mod), Variants: c.variants,
			SameName: c.sameName,
		}
		for fi, m := range c.files {
			fe := s.fileEnt(m.f)
			fe.Hash = m.hash
			fe.Verdict = m.verdict
			fe.Evidence = m.evidence
			grp.Files = append(grp.Files, fe)
			captures = append(captures, captureSlot{gi: len(out), fi: fi, f: m.f})
		}
		out = append(out, grp)
	}
	s.fillCaptured(out, captures, cancel)
	if dropS == 0 && dropF == 0 {
		return out, nil
	}
	return out, &TruncInfo{Groups: dropS, Files: dropF, Cap: budget}
}

// judgeSet decides, for one confirmed set, which members are damaged and which
// are intact. The ladder runs cheapest-and-strongest first and stops as soon as
// a member has a verdict; nothing here ever guesses, and a set that survives
// every rung is left honestly Unknown with both copies shown.
func judgeSet(c *corruptSet, cancel chan struct{}) {
	// Rung 1 — the read itself failed. On a checksumming filesystem this is
	// not evidence of corruption, it is the corruption: the drive returned an
	// error because the stored bytes no longer match their checksum.
	for i := range c.files {
		m := &c.files[i]
		if m.readErr == nil {
			continue
		}
		if errors.Is(m.readErr, syscall.EIO) {
			m.verdict, m.evidence = verdictCorrupt,
				"could not be read — the drive reported an I/O error, which on a checksummed volume means the stored bytes are damaged"
			continue
		}
		m.verdict, m.evidence = verdictUnknown, "could not be read: "+cleanReadErr(m.readErr)
	}

	// Rung 2 — the content moved while the modified time did not. An earlier
	// scan recorded this path's content at this same size and mtime, and it
	// no longer matches — anywhere in the file, since every scan reads its
	// candidates in full. Before convicting, re-observe the metadata the
	// evidence cites: the walk's size and mtime must STILL be on disk now.
	// If they moved, what changed the bytes was an ordinary edit landing
	// mid-scan — the one interleaving that produces this flag without rot —
	// and saying "nothing edited this file" about it convicts a healthy copy.
	for i := range c.files {
		m := &c.files[i]
		if m.verdict == "" && m.contentChanged && unchangedOnDisk(m.f) {
			m.verdict, m.evidence = verdictCorrupt,
				"content changed since an earlier scan although its size and modified time did not — nothing edited this file"
		}
	}

	// Rung 3 — the file's own container. PNG, gzip and ZIP-family members
	// carry checksums that convict a single copy with no comparison at all;
	// the rest carry framing that must reach the end of the file.
	checked := make([]media.Intactness, len(c.files))
	proof := make([]string, len(c.files))
	for i := range c.files {
		// Container validation reads whole files. Without this check Stop sits
		// dead at 96% for the length of the final phase, and then the scan is
		// discarded anyway — so the work was pure waiting.
		if cancelled(cancel) {
			return
		}
		m := &c.files[i]
		if m.readErr != nil {
			continue
		}
		st, why := media.VerifyContent(m.f.openContent, m.f.size)
		checked[i], proof[i] = st, why
		if st == media.Damaged && m.verdict == "" {
			m.verdict, m.evidence = verdictCorrupt, why
		}
	}

	// Rung 4 — the shape of the difference. Only worth reading two files again
	// when the cheaper rungs found nothing, and only when the set holds
	// exactly two contents to compare.
	if !anyVerdict(c, verdictCorrupt) && c.variants == 2 && c.size <= maxDiffBytes {
		a, b := representatives(c)
		if a >= 0 && b >= 0 {
			if d, err := media.CompareContent(c.files[a].f.openContent, c.files[b].f.openContent, c.size, cancel); err == nil && d != nil {
				switch d.Kind {
				case "zeros", "tail":
					lost, kept := a, b
					if d.ZeroSide == 1 {
						lost, kept = b, a
					}
					judgeWithTwins(c, lost, verdictCorrupt, d.Describe(d.ZeroSide))
					judgeWithTwins(c, kept, verdictIntact, d.Describe(1-d.ZeroSide))
				default:
					// A single flipped bit proves the set is damaged but is
					// perfectly symmetric — saying which side rotted would be
					// a guess, so it stays a note rather than a verdict.
					for _, i := range []int{a, b} {
						judgeWithTwins(c, i, "", d.Describe(i))
					}
				}
			}
		}
	}

	// A copy that verifies its own structure, in a set where another copy is
	// positively damaged, is the one to keep.
	if anyVerdict(c, verdictCorrupt) {
		for i := range c.files {
			m := &c.files[i]
			if m.verdict == "" && checked[i] == media.Proven {
				m.verdict = verdictIntact
				if m.evidence == "" {
					// The validator's own words: only some formats carry a
					// checksum, and claiming one for the rest would overstate
					// exactly the evidence this column exists to report.
					m.evidence = proof[i] + ", and another copy in this set is damaged"
				}
			}
		}
	}
	for i := range c.files {
		if c.files[i].verdict == "" {
			c.files[i].verdict = verdictUnknown
		}
	}
}

// verdictSearchText is everything the search box should match a verdict on:
// the stored token plus every label the UI and the export put on screen for
// it. Lowercase, and empty for a row that carries no verdict at all.
func verdictSearchText(v string) string {
	switch v {
	case verdictCorrupt:
		return "corrupt corrupted damaged"
	case verdictIntact:
		return "intact good ok"
	case verdictUnknown:
		return "unknown undetermined unclear"
	}
	return ""
}

// verdictLabel renders a verdict for a human reader, in the CSV export.
//
// KEEP IN SYNC with VERDICT_TEXT in spk/ui/DuplicateFinder.js, which renders
// the same three verdicts in the grid's Status column. They are necessarily
// separate — one is Go, one is the browser — and can drift into a report
// saying "Unknown" beside a grid saying "Undetermined" about the same row.
// TestVerdictLabelsMatchTheUI here and the smoke assertion on VERDICT_TEXT
// pin both halves to the same words.
func verdictLabel(v string) string {
	switch v {
	case verdictCorrupt:
		return "Corrupted"
	case verdictIntact:
		return "Intact"
	case verdictUnknown:
		return "Undetermined"
	}
	return ""
}

// judgeWithTwins labels one member and every still-unjudged member holding
// exactly the same bytes. Rung 4 compares two representatives, but a member
// whose full-content hash equals a representative's IS that content — so the
// finding applies to it as a fact, not as an inference. Without this, two
// byte-identical good copies in a three-copy set come out labelled differently
// purely by path order, which reads as the app contradicting itself.
//
// Deliberately scoped to rung 4. Rung 2's evidence is a statement about one
// path's hash history and must never be copied onto a peer.
//
// An empty verdict copies the evidence only, for the symmetric bit-flip case.
func judgeWithTwins(c *corruptSet, i int, verdict, evidence string) {
	if c.files[i].verdict == "" {
		c.files[i].verdict = verdict
		c.files[i].evidence = evidence
	}
	h := c.files[i].hash
	if h == "" {
		return
	}
	for j := range c.files {
		p := &c.files[j]
		if j == i || p.verdict != "" || p.readErr != nil || p.hash != h {
			continue
		}
		p.verdict, p.evidence = verdict, evidence
	}
}

func anyVerdict(c *corruptSet, v string) bool {
	for i := range c.files {
		if c.files[i].verdict == v {
			return true
		}
	}
	return false
}

// representatives picks one member from each of two different contents, for
// the byte-level comparison. Returns -1,-1 when the set does not offer a
// readable pair.
func representatives(c *corruptSet) (int, int) {
	first := -1
	for i := range c.files {
		if c.files[i].readErr != nil || c.files[i].hash == "" {
			continue
		}
		if first < 0 {
			first = i
			continue
		}
		if c.files[i].hash != c.files[first].hash {
			return first, i
		}
	}
	return -1, -1
}

func isCancelErr(err error) bool {
	return errors.Is(err, errCancelled)
}

// cleanReadErr renders a read failure without leaking the on-disk path (the
// error carries the root-relative name from OpenRel, which is meaningless to
// the user next to the Location column they can already see).
func cleanReadErr(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return strings.TrimSpace(fmt.Sprint(err))
}
