// The duplicates pass: windowed hashing over the candidate spill, the
// skew ladder for keys too large for one window, the bounded group
// accumulator's final cut, and the capture-date fill for the rows that
// survive it.
package main

import (
	"dupfinder/internal/dirhandle"
	"dupfinder/internal/media"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Window bounds. Peak scanner memory is a small multiple of these, whatever
// the volume holds: a NAS with ten million duplicate candidates simply runs
// a hundred windows.
// Variables only so the tests can shrink them; reaching these honestly needs
// hundreds of thousands of files.
var (
	// dupWindowFiles is the target number of candidates per window, and what
	// the partition count is derived from.
	dupWindowFiles = 100000
	// dupKeyFileCap is the point at which a candidate key counts as SKEWED.
	// Partitioning cannot split a key — every file sharing it has to be
	// examined together — and keys are not evenly populated: every macOS
	// .DS_Store is 6148 bytes, so on a NAS full of Mac clients a single key
	// can cover hundreds of thousands of files. A key over this size is not
	// truncated (that would silently lose whatever duplicates lived in the
	// dropped part); it is re-partitioned by content prefix instead — see
	// resolveSkewedKey.
	dupKeyFileCap = 25000
	// dupWindowMax is a last-resort backstop for a partition that is large
	// even after its skewed keys have been taken out. ~400k entries ≈ 180 MB.
	// It reports what it skips; reaching it takes an adversarial spread of
	// many just-under-cap keys into one partition.
	dupWindowMax = 400000
)

// enrichCreated fills in File Station's creation times for one candidate
// window and reports how many paths the API could not answer for. Only
// candidates are ever asked — the creation time of a file that cannot group
// changes nothing.
func (s *Server) enrichCreated(sess *fsSession, cands []fEnt, cancel chan struct{}, part, parts int) int {
	if len(cands) == 0 {
		return 0
	}
	paths := make([]string, 0, len(cands))
	for i := range cands {
		paths = append(paths, cands[i].path)
	}
	suffix := ""
	if parts > 1 {
		suffix = fmt.Sprintf(" · part %d of %d", part+1, parts)
	}
	ct := sess.fetchCrtimes(paths, cancel, func(done, total int) {
		s.setProgress(-1, "Fetching creation times… ("+humanCount(done)+" of "+humanCount(total)+")"+suffix)
	})
	missing := 0
	for i := range cands {
		if t, ok := ct[cands[i].path]; ok {
			cands[i].created = t
		} else {
			missing++
		}
	}
	return missing
}

// resolveSkewedKey handles one candidate key holding more files than a window
// should take. The key cannot be split by partitioning — every file sharing it
// has to be compared with the others — so it is split by CONTENT instead, in
// two steps, each one group-preserving:
//
//  1. by the 64 KiB content prefix: two files can only be duplicates if their
//     first 64 KiB agree. This is cheap and settles almost everything.
//  2. for a prefix bucket that is STILL too big, by the full content hash.
//     Only buckets that need it pay for the full read.
//
// After step 2 a bucket means "identical content", i.e. exactly one duplicate
// group — so the cap that finally applies there is the per-group cap, which
// limits the stored result and is reported, rather than hiding some other
// group. Nothing is dropped for being past a cap at any earlier step, which
// is the whole point: truncating a key silently loses whatever duplicates
// happened to sit in the part that was dropped.
//
// The prefix read is not wasted work — dupWindow would read the same 64 KiB
// anyway — and none of this runs for a key small enough for one window.
func (s *Server) resolveSkewedKey(cs *spill, key uint64, req ScanReq, sess *fsSession,
	cache *hashCache, handles []*dirhandle.Handle, rootOf []string, cancel chan struct{},
	acc *groupTop, lo, hi float64, missingCreated *int) error {

	s.setProgress(lo, "Comparing file contents…")
	sub, n, err := s.hashSubSpill(cs, handles, rootOf, cancel, acc, cache, false, nil, nil,
		func(r *spillRec) bool { return r.key(req.Match) == key })
	if err != nil {
		return err
	}
	defer sub.close()
	if cancelled(cancel) {
		return nil
	}

	parts := 1
	if n > dupWindowFiles {
		parts = (n + dupWindowFiles - 1) / dupWindowFiles
	}
	span := (hi - lo) / float64(parts)
	for p := 0; p < parts; p++ {
		win, over, crowded, werr := sub.tagWindow(p, parts, dupKeyFileCap, dupWindowMax, handles, rootOf)
		if werr != nil {
			return errors.New("cannot read the skew spill file: " + werr.Error())
		}
		klo := lo + span*float64(p)
		part, parts := p, parts // captured for the escalation below
		if crowded > 0 {
			// Too many distinct prefixes landed in this partition to hold at
			// once. Splitting it by FULL content is the next rung of the
			// ladder and costs nothing that was not going to be paid anyway —
			// far better than dropping records nobody has looked at.
			win = nil
			if err := s.resolveSkewedPrefix(sub, func(r *spillRec) bool {
				return parts <= 1 || r.tag%uint64(parts) == uint64(part)
			}, req, sess, cache, handles, rootOf, cancel, acc, klo, klo+span, missingCreated); err != nil {
				return err
			}
			if cancelled(cancel) {
				return nil
			}
			continue
		}
		if !s.runSkewWindow(win, req, sess, cache, cancel, acc, klo, klo+span, p, parts, missingCreated) {
			return nil // cancelled
		}
		// A single prefix bucket bigger than a window: split it by full content.
		for _, tag := range over {
			tag := tag
			if err := s.resolveSkewedPrefix(sub, func(r *spillRec) bool { return r.tag == tag },
				req, sess, cache, handles, rootOf, cancel, acc, klo, klo+span, missingCreated); err != nil {
				return err
			}
			if cancelled(cancel) {
				return nil
			}
		}
	}
	return nil
}

// resolveSkewedPrefix is step 2: everything sharing one content prefix, split
// by full content hash. A bucket that is still over the cap after this is a
// single duplicate group, so the cap applied to it is the per-group cap.
func (s *Server) resolveSkewedPrefix(sub *spill, keep func(*spillRec) bool, req ScanReq, sess *fsSession,
	cache *hashCache, handles []*dirhandle.Handle, rootOf []string, cancel chan struct{},
	acc *groupTop, lo, hi float64, missingCreated *int) error {

	full, n, err := s.hashSubSpill(sub, handles, rootOf, cancel, acc, cache, true, nil, nil, keep)
	if err != nil {
		return err
	}
	defer full.close()
	if cancelled(cancel) {
		return nil
	}
	parts := 1
	if n > dupWindowFiles {
		parts = (n + dupWindowFiles - 1) / dupWindowFiles
	}
	span := (hi - lo) / float64(parts)
	for p := 0; p < parts; p++ {
		win, over, crowded, werr := full.tagWindow(p, parts, dupFileCap, dupWindowMax, handles, rootOf)
		if werr != nil {
			return errors.New("cannot read the skew spill file: " + werr.Error())
		}
		klo := lo + span*float64(p)
		if crowded > 0 {
			// This is the bottom of the ladder: a tag here means identical
			// content, so a partition too crowded to hold is many distinct
			// groups at once. Take them one tag at a time instead.
			win = nil
			tags, terr := full.tagsIn(p, parts)
			if terr != nil {
				return errors.New("cannot read the skew spill file: " + terr.Error())
			}
			// REPLACE, don't append: tagsIn applies the same partition
			// predicate as tagWindow but no cap filter, so it already
			// contains every tag `over` holds. Appending would list the
			// over-cap tags twice and emit their groups twice — the same
			// duplicate group under two ids, double-counted reclaimable
			// space, and a doubled truncation report.
			over = tags
		}
		if !s.runSkewWindow(win, req, sess, cache, cancel, acc, klo, klo+span, p, parts, missingCreated) {
			return nil
		}
		for _, t := range over {
			slice, skipped, serr := full.tagSlice(t, handles, rootOf, dupFileCap)
			if serr != nil {
				return errors.New("cannot read the skew spill file: " + serr.Error())
			}
			// Identical content: one group. What the cap leaves is the
			// per-group cap on the stored result, and it is reported.
			acc.noteSkipped(skipped)
			if !s.runSkewWindow(slice, req, sess, cache, cancel, acc, klo, klo+span, p, parts, missingCreated) {
				return nil
			}
		}
	}
	return nil
}

// hashSubSpill streams the records keep accepts, hashes each one and writes
// them to a fresh spill tagged by that hash, so the result can be partitioned
// by content. Files that cannot be read are counted, not silently forgotten.
//
// full picks which rung of the ladder this is. The prefix rung hashes 64 KiB
// and stops. The full rung reads the prefix FIRST and asks the hash store
// about it — which is the same question dupWindow asks a moment later. The
// store only ever answers for reads made THIS scan (a previous scan's hash
// can never vouch for today's bytes), so what this buys is strictly
// deduplication within the run: a file this rung reads in full is recorded,
// and dupWindow's own lookup then hits it instead of reading the whole file
// a second time.
// skipNoter is whatever accumulator the current pass is filling. Both groupTop
// (duplicates) and corruptTop (corrupted files) record the candidates a cap
// meant were never examined, and hashSubSpill needs nothing else from either.
type skipNoter interface{ noteSkipped(int) }

// onRot, when non-nil, is called with each path whose full read here proved
// deep rot (record() returned true: content moved under an unchanged size and
// mtime since an earlier scan), and with the content the path used to hold.
// The store's own `changed` set carries the same fact, but that set is capped
// (changedMax) — the callback is the uncapped carrier for the pass that is
// consuming the evidence right now, so a full set can never cost it the very
// finding this read just made. onUnreadable, when non-nil, receives every
// entry whose content could not be read: such an entry cannot be tagged, but
// the conflicting-files pass must still list and judge it.
func (s *Server) hashSubSpill(src *spill, handles []*dirhandle.Handle, rootOf []string,
	cancel chan struct{}, acc skipNoter, cache *hashCache, full bool,
	onRot func(path, prior string), onUnreadable func(f fEnt, err error), keep func(*spillRec) bool) (*spill, int, error) {

	out, err := newSpill(s.stateDir())
	if err != nil {
		return nil, 0, errors.New("cannot create the skew spill file: " + err.Error())
	}
	n, unreadable, cached := 0, 0, 0
	prev := src.onEach
	src.onEach = func(done, total int) {
		s.setProgress(-1, "Comparing file contents… ("+humanCount(done)+" of "+humanCount(total)+")")
	}
	defer func() { src.onEach = prev }()
	err = src.each(func(r *spillRec) error {
		if cancelled(cancel) {
			return errCancelled
		}
		if r.rootIdx >= len(rootOf) || !keep(r) {
			return nil
		}
		f := fEnt{rel: r.rel, rh: handles[r.rootIdx],
			path: filepath.Join(rootOf[r.rootIdx], r.rel),
			size: r.size, mod: time.Unix(r.mod, 0)}
		h, herr := hashFile(f.openContent, 64*1024, cancel)
		if herr != nil {
			unreadable++ // it cannot group, but the scan should not pretend it never existed
			if onUnreadable != nil {
				onUnreadable(f, herr)
			}
			return nil
		}
		if full {
			if hit, ok := cache.lookup(f.path, f.size, f.mod.Unix(), h); ok {
				h, cached = hit, cached+1
			} else {
				whole, ferr := hashFile(f.openContent, -1, cancel)
				if ferr != nil {
					unreadable++
					if onUnreadable != nil {
						onUnreadable(f, ferr)
					}
					return nil
				}
				if moved, prior := cache.record(f.path, f.size, f.mod.Unix(), h, whole); moved && onRot != nil {
					onRot(f.path, prior)
				}
				h = whole
			}
		}
		n++
		return out.addRaw(r.rootIdx, r.size, r.mod, r.rel, prefixTag(h))
	})
	acc.noteSkipped(unreadable)
	if cached > 0 {
		s.setProgress(-1, "Comparing file contents… ("+humanCount(cached)+" already read this scan)")
	}
	if err == errCancelled || cancelled(cancel) {
		return out, n, nil
	}
	if err != nil {
		out.close()
		return nil, 0, errors.New("cannot read the candidate spill file: " + err.Error())
	}
	return out, n, nil
}

// runSkewWindow enriches and groups one window of a skew pass.
func (s *Server) runSkewWindow(win []fEnt, req ScanReq, sess *fsSession, cache *hashCache,
	cancel chan struct{}, acc *groupTop, lo, hi float64, part, parts int, missingCreated *int) bool {
	if len(win) == 0 {
		return true
	}
	if sess != nil && req.Match.Created {
		*missingCreated += s.enrichCreated(sess, win, cancel, part, parts)
	}
	return s.dupWindow(win, req.Match, cache, cancel, acc, lo, hi)
}

// errCancelled unwinds a spill stream when the scan is cancelled.
var errCancelled = errors.New("cancelled")

// prefixTag folds a hex prefix hash into the 64-bit tag a sub-spill is
// partitioned by. Files with the same content always land together; a
// collision here only puts two unrelated files in one window, where their
// content hashes separate them again.
func prefixTag(pfxHex string) uint64 {
	h := fnv.New64a()
	io.WriteString(h, pfxHex)
	return h.Sum64()
}

// scanDuplicates groups one whole candidate set in memory. runScan drives
// the windowed path instead (dupWindow straight into the bounded
// accumulator); this form remains for the unit tests, which build their
// candidates by hand, and is exactly a single-window scan.
func (s *Server) scanDuplicates(files []fEnt, match MatchOpts, cache *hashCache, cancel chan struct{}) ([]Group, *TruncInfo) {
	acc := newGroupTop(dupFileCap)
	if !s.dupWindow(files, match, cache, cancel, acc, 16, 96) {
		return nil, nil
	}
	return acc.final(s, dupFileCap, nil)
}

// dupWindow hashes and groups one window of candidates, folding whatever it
// finds into the bounded accumulator. lo/hi bound the progress this window
// may report, so a scan split into many windows still advances once. It
// returns false when the scan was cancelled.
func (s *Server) dupWindow(files []fEnt, match MatchOpts, cache *hashCache, cancel chan struct{}, acc *groupTop, lo, hi float64) bool {
	span := hi - lo
	// Candidate key: size plus any selected match criteria. Files whose key
	// is unique can never form a group, so they skip hashing entirely.
	candKey := func(f fEnt) string {
		k := strconv.FormatInt(f.size, 10)
		if match.Name {
			k += "|" + f.name
		}
		if match.Modified {
			k += "|" + fmtTime(f.mod)
		}
		if match.Created {
			if f.created.IsZero() {
				// Unknown creation time (File Station could not answer):
				// a per-path sentinel so unknowns never accidentally count
				// as "same Created Date" — the file simply cannot group.
				k += "|?" + f.path
			} else {
				k += "|" + fmtTime(f.created)
			}
		}
		return k
	}

	s.setProgress(lo, "Grouping candidates by size…")
	// Only keys held by two or more files can produce a group. The exact key
	// is computed once per file and carried alongside it: under created-date
	// matching it embeds the path, so recomputing it in each later pass would
	// allocate several strings per file per pass.
	var pending []pEnt
	{
		keys := make([]string, len(files))
		keyCount := make(map[string]int, len(files))
		for i := range files {
			if files[i].isDir || files[i].size == 0 {
				continue
			}
			keys[i] = candKey(files[i])
			keyCount[keys[i]]++
		}
		for i := range files {
			if keys[i] != "" && keyCount[keys[i]] >= 2 {
				pending = append(pending, pEnt{f: files[i], k: keys[i]})
			}
		}
	}

	// Pass 1: hash the first 64 KiB to split candidate groups cheaply.
	s.setProgress(lo+span*0.03, "Comparing file contents…")
	type pkey struct {
		ck  string
		pfx string
	}
	var pmu sync.Mutex
	prefixGroups := map[pkey][]pEnt{}
	parallelEach(pending, cancel, func(e pEnt) {
		pfx, err := hashFile(e.f.openContent, 64*1024, cancel)
		if err != nil {
			return
		}
		pmu.Lock()
		k := pkey{e.k, pfx}
		prefixGroups[k] = append(prefixGroups[k], e)
		pmu.Unlock()
	}, func(done, total int) {
		s.setProgress(lo+span*(0.03+0.12*float64(done)/float64(max(total, 1))), "Comparing file contents…")
	})
	if cancelled(cancel) {
		return false
	}
	pending = nil

	// Pass 2: full-content hash for groups that still collide. The hash
	// store answers only for files already read in full THIS scan (the skew
	// ladder and earlier windows share it); nothing from a previous scan is
	// ever served, so every group reported below rests on bytes read during
	// this scan — a rescan re-reads everything by design, because a stale
	// hash is exactly where rot beyond the 64 KiB prefix could hide.
	var hmu sync.Mutex
	byHash := map[string][]fEnt{}
	// Every file in a content-hash bucket has the same content, so they share
	// the 64 KiB prefix too. It is kept because the MOVE re-checks it: size
	// and mtime can both be restored on a rewritten file, the prefix cannot.
	pfxOf := map[string]string{}
	addHash := func(e pEnt, pfx, full string) {
		k := e.k + "\x00" + full
		hmu.Lock()
		byHash[k] = append(byHash[k], e.f)
		if _, seen := pfxOf[k]; !seen && len(pfx) >= 32 {
			pfxOf[k] = pfx[:32]
		}
		hmu.Unlock()
	}
	var misses []pEnt
	missPfx := map[string]string{} // read-only during the parallel pass
	var totalBytes int64
	cachedN := 0
	for k, g := range prefixGroups {
		if len(g) < 2 {
			continue
		}
		for _, e := range g {
			if full, ok := cache.lookup(e.f.path, e.f.size, e.f.mod.Unix(), k.pfx); ok {
				addHash(e, k.pfx, full)
				cachedN++
				continue
			}
			misses = append(misses, e)
			missPfx[e.f.path] = k.pfx
			totalBytes += e.f.size
		}
	}
	prefixGroups = nil
	label := "Computing " + hashAlgoName + " hashes…"
	if cachedN > 0 {
		label = "Computing " + hashAlgoName + " hashes… (" + humanCount(cachedN) + " already read this scan)"
	}
	var hashedBytes int64
	parallelEach(misses, cancel, func(e pEnt) {
		h, err := hashFile(e.f.openContent, -1, cancel)
		done := atomic.AddInt64(&hashedBytes, e.f.size)
		s.setProgress(lo+span*(0.15+0.85*float64(done)/float64(max64(totalBytes, 1))), label)
		if err != nil {
			return
		}
		pfx := missPfx[e.f.path]
		cache.record(e.f.path, e.f.size, e.f.mod.Unix(), pfx, h)
		addHash(e, pfx, h)
	}, nil)
	if cancelled(cancel) {
		return false
	}
	s.setProgress(hi, label) // an all-reused pass (skew ladder read everything first) never reports otherwise

	// Groups go into the bounded accumulator as raw scanner entries: result
	// IDs and the per-file EXIF read cost real work, and only the groups that
	// survive the stored-results cap are ever worth it.
	for key, g := range byHash {
		if len(g) < 2 {
			continue
		}
		sort.Slice(g, func(i, j int) bool { return g[i].path < g[j].path })
		// One group may not exceed the whole stored-results budget. Keeping
		// the first group whole is what lets a genuinely huge group survive
		// the cap at all (capCutIndex), but a million copies of one file
		// would then blow past every downstream bound — the snapshot, the
		// state file, the export — through that same door. The members past
		// the cap are reported like any other truncation, and dropping
		// members can only under-report a group, never mis-group anything.
		extra := 0
		if len(g) > dupFileCap {
			extra = len(g) - dupFileCap
			g = g[:dupFileCap]
		}
		acc.add(rawGroup{
			size:  g[0].size,
			hash:  key[strings.LastIndex(key, "\x00")+1:],
			pfx:   pfxOf[key],
			files: g,
			extra: extra,
		})
	}
	return true
}

// pEnt is a candidate paired with its exact candidate key.
type pEnt struct {
	f fEnt
	k string
}

// final orders the accumulated groups, applies the stored-results cap on
// group boundaries — the same rule capDuplicateGroups states — and
// materializes only the survivors, so IDs and EXIF reads are spent on rows
// the user will actually see. Groups the accumulator itself had to drop are
// added into the truncation report, so the cut stays honest whatever path
// dropped a group.
func (t *groupTop) final(s *Server, budget int, cancel chan struct{}) ([]Group, *TruncInfo) {
	gs := t.sorted()
	cut := capCutIndex(len(gs), func(i int) int { return len(gs[i].files) }, budget)
	dropG, dropF := t.droppedGroups, t.droppedFiles
	for i, g := range gs {
		if i >= cut {
			dropG++
			dropF += len(g.files)
		}
		dropF += g.extra // members the per-group cap dropped, kept or not
	}
	out := make([]Group, 0, cut)
	var captures []captureSlot
	for i, g := range gs[:cut] {
		// No single group may exceed the whole budget. dupWindow already caps
		// what it adds, but capCutIndex deliberately keeps the first group
		// whatever its size — so the guarantee has to hold here too, or a
		// caller that builds groups another way inherits an uncapped result.
		if len(g.files) > budget {
			dropF += len(g.files) - budget
			g.files = g.files[:budget]
		}
		grp := Group{ID: "g" + strconv.Itoa(i), Ext: extOf(g.files[0].name), Size: g.size, Hash: g.hash}
		for fi, f := range g.files {
			fe := s.fileEnt(f)
			fe.Hash = g.hash
			fe.Pfx = g.pfx
			grp.Files = append(grp.Files, fe)
			captures = append(captures, captureSlot{gi: i, fi: fi, f: f})
		}
		out = append(out, grp)
	}
	s.fillCaptured(out, captures, cancel)
	if dropG == 0 && dropF == 0 {
		return out, nil
	}
	return out, &TruncInfo{Groups: dropG, Files: dropF, Cap: budget}
}

// captureSlot names one result row whose capture date is still to be read:
// the row's position in the finished groups plus the walk entry that can
// open it through its pinned root.
type captureSlot struct {
	gi, fi int
	f      fEnt
}

// fillCaptured reads the capture date of every listed row, in parallel and
// cancellably, with its own progress label. Done inline on the scan
// goroutine it would be up to 100k re-opens that Stop could not interrupt,
// under a label still saying the hashes were being computed.
func (s *Server) fillCaptured(out []Group, slots []captureSlot, cancel chan struct{}) {
	if len(slots) == 0 {
		return
	}
	parallelEach(slots, cancel, func(sl captureSlot) {
		out[sl.gi].Files[sl.fi].Captured = media.Captured(sl.f.openContent, sl.f.name)
	}, func(done, total int) {
		s.setProgress(-1, "Reading capture dates… ("+humanCount(done)+" of "+humanCount(total)+")")
	})
}

// capDuplicateGroups keeps the top groups by reclaimable space (the sort
// order above) up to a total-file budget, cutting only on group boundaries —
// a split group would break every keep-one invariant downstream. At least
// one group is always kept, even if it alone exceeds the budget. The dropped
// remainder is reported, never silently discarded.
//
// Nothing in production calls it. The live scan applies this same rule in
// groupTop.final above, on the bounded heap, while the windows are still
// running; this whole-slice form is the reference the tests diff that
// result against (scale_test.go, results_test.go). Keep both: a change to
// the cap rule belongs in both places, and the tests say when they drift.
func capDuplicateGroups(groups []Group, budget int) ([]Group, *TruncInfo) {
	cut := capCutIndex(len(groups), func(i int) int { return len(groups[i].Files) }, budget)
	if cut >= len(groups) {
		return groups, nil
	}
	t := &TruncInfo{Groups: len(groups) - cut, Cap: budget}
	for _, g := range groups[cut:] {
		t.Files += len(g.Files)
	}
	return groups[:cut:cut], t
}

// capCutIndex is the shared cut rule for a listing capped on group
// boundaries: walk the (already ordered) groups, stop before the first one
// that would push the total past the budget, and always keep at least the
// first group even when it alone exceeds it. capDuplicateGroups states the
// rule for finished result groups; groupTop.final applies the same one to
// raw scanner groups before they are ever materialized.
func capCutIndex(n int, filesAt func(i int) int, budget int) int {
	total := 0
	for i := 0; i < n; i++ {
		c := filesAt(i)
		if i > 0 && total+c > budget {
			return i
		}
		total += c
	}
	return n
}
