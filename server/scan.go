package main

import (
	"dupfinder/internal/dirhandle"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const hashAlgoName = "Blake3"

// fEnt is an enumerated filesystem entry (internal to the scanner). rh/rel
// address the entry through the pinned root handle it was enumerated under,
// so content reads (hashing, EXIF) can open it without traversing symlinks
// — path is the user-facing display string only.
type fEnt struct {
	path, name, dir string
	size            int64
	mod, created    time.Time
	isDir           bool
	rel             string            // path relative to the pinned root
	rh              *dirhandle.Handle // pinned root; nil only for hand-built test entries
	link            fileLink          // the data behind the name, for hard-link detection
}

// fileLink identifies the data a name refers to. Names sharing a device and
// inode are hard links to one copy; n is how many names the inode has. The
// zero value means "unknown", which counts as a copy of its own.
type fileLink struct {
	dev, ino uint64
	n        uint32
}

// physicalCopies counts the distinct pieces of data a group's members refer
// to: names that share an inode are one copy, every other name is its own.
// It is what "reclaimable" has to be measured in — removing one of three
// hard links to a file frees nothing.
func physicalCopies(files []fEnt) int {
	n := 0
	seen := map[fileLink]bool{}
	for i := range files {
		l := files[i].link
		if l.n <= 1 || l.ino == 0 {
			n++
			continue
		}
		if !seen[l] {
			seen[l] = true
			n++
		}
	}
	return n
}

// dirEnt is a directory as the empty-folder scan keeps it: the fields the
// report needs and nothing more. On a volume with millions of directories a
// full fEnt — four separately allocated path strings plus the pinned handle
// — costs several times as much for data that scan never reads.
type dirEnt struct {
	path string
	size int64
	mod  time.Time
}

// openContent opens the entry for reading through its pinned root handle,
// refusing to traverse any symlink between root and file: a component
// swapped for a symlink after enumeration fails the open instead of
// redirecting the read outside the vetted tree. Entries without a handle
// (unit tests construct fEnt directly) fall back to a plain open.
func (f fEnt) openContent() (*os.File, error) {
	if f.rh != nil {
		return f.rh.OpenRel(f.rel)
	}
	return os.Open(f.path)
}

func (s *Server) setProgress(p float64, label string) {
	s.mu.Lock()
	if s.job.Running {
		if p >= 0 {
			s.job.Progress = s.mapBand(p)
		}
		if label != "" {
			s.job.Label = label
		}
	}
	s.mu.Unlock()
}

// setBand maps the progress a pass reports into one slice of the bar. Every
// pass after the walk reports in the 15→96 frame of a scan that runs alone;
// when several run in one scan, each gets a band of that frame, so the bar
// climbs once from start to finish instead of falling back at every pass
// boundary. Zero clears the band (identity). Called on the scan goroutine
// only; read under mu by setProgress.
func (s *Server) setBand(lo, hi float64) {
	s.mu.Lock()
	s.bandLo, s.bandHi = lo, hi
	s.mu.Unlock()
}

// mapBand applies the current band to a pass-frame percentage. Called with
// mu held.
func (s *Server) mapBand(p float64) float64 {
	if s.bandHi <= s.bandLo {
		return p
	}
	const frameLo, frameHi = 15.0, 96.0
	if p <= frameLo {
		return s.bandLo
	}
	if p >= frameHi {
		return s.bandHi
	}
	return s.bandLo + (p-frameLo)/(frameHi-frameLo)*(s.bandHi-s.bandLo)
}

func cancelled(cancel chan struct{}) bool {
	select {
	case <-cancel:
		return true
	default:
		return false
	}
}

// scanOutcome is how one tool's pass ended: finished, so the result is
// stored; cancelled, so it is dropped; or aborted, so the error the pass
// recorded is stored as the result.
type scanOutcome int

const (
	scanFinished scanOutcome = iota
	scanCancelled
	scanAborted
)

// resumeGen, when non-zero, is the hash-store generation of an interrupted
// scan the user chose to RESUME: the duplicates pass adopts it instead of
// advancing, so files the dead run already read in full are not read again.
// 0 means a normal scan (including a resume of a run that died before it
// opened the store — there is nothing of its reading to continue).
//
// One scan runs every requested tool over ONE walk of the scope. The walk is
// the whole cost of the metadata tools and a large part of the duplicates
// scan, and five tools walking the volume in turn would pay it five times.
// Each tool's result is published the moment its pass finishes, so a scan
// stopped part-way keeps what it had already found.
func (s *Server) runScan(req ScanReq, roots []string, sess *fsSession, cancel chan struct{}, resumeGen uint32) {
	tools, err := req.toolList()
	if err != nil {
		// handleScan validated the request; this is the belt to that brace.
		log.Printf("scan refused: %v", err)
		s.mu.Lock()
		s.job = jobState{}
		s.mu.Unlock()
		return
	}
	want := map[string]bool{}
	for _, t := range tools {
		want[t] = true
	}
	// base collects what every result shares: the resolved roots and the
	// errors of the walk. Results are created from it after the walk.
	base := &toolResult{}
	newResult := func(tool string) *toolResult {
		return &toolResult{Tool: tool, Roots: base.Roots, Errors: append([]string{}, base.Errors...)}
	}
	// A result is published the moment its pass finishes — under mu, so the
	// app's next poll sees it — and remembered here for the bookkeeping at
	// the end. Moves are refused while a scan runs, so a duplicates result
	// landing mid-scan changes nothing a move could act on yet.
	published := map[string]*toolResult{}
	publish := func(res *toolResult) {
		// Nanosecond precision because the app tells a fresh result from the
		// one it already fetched by comparing this string: at whole seconds a
		// rescan that finishes within the second its predecessor did looks
		// unchanged, and the app keeps showing the previous result's page.
		res.Scanned = time.Now().Format(time.RFC3339Nano)
		s.mu.Lock()
		s.results[res.Tool] = res
		// The reference folders the result is protected by travel with it, in
		// the same lock section: a page the app loads the moment the result
		// lands must already see the list this scan was started with, or its
		// padlocks describe the previous scan's list until something reloads.
		s.refDirs = uniquePaths(req.RefDirs)
		s.invalidateDerivedLocked() // views and date spans describe the result just replaced
		if res.Tool == "duplicates" {
			s.keepers = nil // fresh groups supersede recorded survivors
		}
		s.mu.Unlock()
		published[res.Tool] = res
	}
	// current names the pass in flight, so a panic can be written into the
	// result of the tool that was running and nothing else.
	current := ""
	// completed means every requested pass ran to its end. Judged by THIS
	// scan's own progress, not by whether the cancel channel was closed: a
	// cancel that arrives after the work finished can no longer save anything
	// and must not discard results.
	completed := false
	// The marker records this scan in flight; every controlled ending clears
	// it, so a marker found at the next daemon start means an interruption.
	// Gen 0 for now: the content pass rewrites the marker once it has opened
	// the hash store and knows the generation a resume would need.
	s.writeMarker(&req, 0)
	defer func() {
		if r := recover(); r != nil {
			// The results already published were complete when they were
			// published and stand. The pass that panicked surfaces its
			// failure as its tool's result rather than vanishing.
			if current != "" {
				res := newResult(current)
				res.Errors = append(res.Errors, fmt.Sprintf("internal error: %v", r))
				publish(res)
			}
			completed = true
		}
		s.mu.Lock()
		if len(published) > 0 {
			// The view the app opens when the scan ends: the tool it was
			// looking at if that tool was scanned, else the first one.
			s.lastTool = req.openTool(tools)
			// The reference folders a move must respect are the ones the
			// STORED results were scanned with — published with each result
			// and restated here, never at admission: a cancelled scan that
			// published nothing leaves the old results on screen, so it must
			// leave their protection too.
			s.refDirs = uniquePaths(req.RefDirs)
		}
		// How this run ended, for /api/state: the UI's poller has to tell a
		// stop from a finish, and lastTool alone cannot.
		s.lastEnd = scanEnd{Tool: s.lastTool, Tools: tools, Completed: completed}
		s.mu.Unlock()
		// Results reach the disk BEFORE the marker goes and BEFORE the job
		// slot is released: a crash inside saveState must still find the
		// marker, so the loss is reported rather than silent — and a scan
		// admitted in between must not have ITS fresh marker deleted by
		// this one's os.Remove.
		if len(published) > 0 {
			if err := s.saveState(); err != nil {
				// The results are on screen but not on disk. The marker stays:
				// a restart then reports the scan as interrupted and offers to
				// run it again, instead of silently serving the previous state
				// as if nothing had happened. The app shows the failure too.
				s.noteSaveError(err)
				s.mu.Lock()
				s.job = jobState{}
				s.mu.Unlock()
				return
			}
		}
		s.clearMarker()
		s.mu.Lock()
		s.job = jobState{}
		s.mu.Unlock()
	}()

	s.setProgress(2, "Enumerating directories…")
	safeRoots := scanRoots(req, roots, base)

	// The pinned root handles must outlive every content read below —
	// hashing and EXIF open entries through them, never by raw path. They are
	// stored the moment the walk returns, so this close covers a panic
	// anywhere after that point.
	var handles []*dirhandle.Handle
	defer func() {
		for _, h := range handles {
			h.Close()
		}
	}()

	// The consumers of the one walk: the empty-folder frames (which also need
	// the directories emitted), the flat tools' bounded lists, and the
	// content tools' spill log with its two candidate-key counters.
	var ef *emptyFolderScan
	if want["empty_folders"] {
		ef = newEmptyFolderScan()
	}
	flatKeep := map[string]func(*fEnt) bool{
		"empty_files": func(f *fEnt) bool { return f.size == 0 },
		"temp_files":  func(f *fEnt) bool { return isTempName(f.name) },
	}
	tops := map[string]*boundedTop{}
	for t := range flatKeep {
		if want[t] {
			tops[t] = newBoundedTop(flatFileCap, func(a, b *fEnt) bool { return a.path < b.path })
		}
	}
	contentTools := []string{}
	for _, t := range []string{"duplicates", "corrupted_files"} {
		if want[t] {
			contentTools = append(contentTools, t)
		}
	}
	var sp *spill
	var counter, corrCounter *keyCounter
	var spillErr error
	if len(contentTools) > 0 {
		if sp, err = newSpill(s.stateDir()); err != nil {
			for _, t := range contentTools {
				res := newResult(t)
				res.Errors = append(res.Errors, "scan aborted — cannot create the scan spill file: "+err.Error())
				publish(res)
			}
			completed = true
			return
		}
		defer sp.close()
		// Two candidate keys are counted over the one walk: the duplicates
		// key, which folds in whatever match criteria were requested, and the
		// conflicting-files key, which is size + modified time and nothing
		// else. They share the counter's fixed memory budget.
		counter = newKeyCounterShare(1)
		corrCounter = newKeyCounterShare(1)
	}
	var unreadable func(string)
	if ef != nil {
		unreadable = ef.noteUnreadable
	}
	walkErrs, hs, rootOf := s.walkStream(safeRoots, req.Recurse, ef != nil, cancel, func(idx int, f fEnt) {
		if ef != nil {
			ef.visit(idx, f)
		}
		if f.isDir {
			return
		}
		for t, top := range tops {
			if flatKeep[t](&f) {
				top.add(f)
			}
		}
		if sp != nil && f.size > 0 {
			counter.add(candHash(&f, req.Match))
			corrCounter.add(candHash(&f, corruptMatch))
			if spillErr == nil {
				spillErr = sp.add(idx, &f)
			}
		}
	}, unreadable)
	handles = hs
	base.Errors = append(base.Errors, walkErrs...)
	if cancelled(cancel) {
		return // the walk is shared: a stop here has nothing complete to keep
	}

	// The flat tools are finished the moment the walk is: their lists are
	// already bounded and ordered.
	for _, t := range []string{"empty_files", "temp_files"} {
		top := tops[t]
		if top == nil {
			continue
		}
		current = t
		res := newResult(t)
		ents, trunc := top.final()
		res.Files = s.fileEnts(ents)
		res.Truncated = trunc
		publish(res)
	}

	// The passes that still have work to do share the 15→96 band of the
	// progress bar in proportion to what they cost: confirming empty folders
	// is one round trip per candidate, the content pass reads every candidate
	// in full.
	bands := progressBands(ef != nil, len(contentTools) > 0)

	if ef != nil {
		current = "empty_folders"
		s.setBand(bands["empty_folders"][0], bands["empty_folders"][1])
		res := newResult("empty_folders")
		var confirmErrs []string
		res.Files, res.Truncated, confirmErrs = ef.finish(s, cancel, func(p string) (bool, error) {
			return confirmEmpty(p, sess)
		})
		if cancelled(cancel) {
			return // finish stopped part-way: the list is not the answer
		}
		res.Errors = append(res.Errors, confirmErrs...)
		publish(res)
	}

	if len(contentTools) > 0 {
		current = contentTools[0]
		s.setBand(bands["content"][0], bands["content"][1])
		if spillErr != nil {
			for _, t := range contentTools {
				res := newResult(t)
				res.Errors = append(res.Errors, "scan aborted — cannot write the scan spill file (disk full?): "+spillErr.Error())
				publish(res)
			}
			completed = true
			return
		}
		// The duplicates result is always built — the conflicting-files pass
		// rides on its hashing — but stored only when it was asked for.
		res := newResult("duplicates")
		var corrRes *toolResult
		if want["corrupted_files"] {
			corrRes = newResult("corrupted_files")
		}
		outcome := s.scanContent(req, sess, cancel, resumeGen, sp, counter, corrCounter, hs, rootOf, res, corrRes)
		if outcome == scanCancelled || cancelled(cancel) {
			return
		}
		if outcome == scanFinished {
			s.setBand(0, 0)
			s.fetchCreatedDates(sess, res, corrRes, cancel)
		}
		// scanAborted: the pass recorded why, and that error is the result.
		if want["duplicates"] {
			publish(res)
		}
		if corrRes != nil {
			publish(corrRes)
		}
	}
	s.setBand(0, 0)
	completed = true
	s.setProgress(100, "Finalizing results…")
}

// progressBands splits the 15→96 span of the bar between the passes that run
// after the walk, weighting the content pass four to one over the empty-folder
// confirmation. A pass that is not requested gets no band.
func progressBands(emptyFolders, content bool) map[string][2]float64 {
	const lo, hi = 15.0, 96.0
	weights := map[string]float64{}
	if emptyFolders {
		weights["empty_folders"] = 1
	}
	if content {
		weights["content"] = 4
	}
	total := 0.0
	for _, w := range weights {
		total += w
	}
	out := map[string][2]float64{}
	at := lo
	for _, name := range []string{"empty_folders", "content"} {
		w, ok := weights[name]
		if !ok {
			continue
		}
		next := at + (hi-lo)*w/total
		out[name] = [2]float64{at, next}
		at = next
	}
	return out
}

// scanRoots re-resolves the requested roots and de-overlaps them by canonical
// path, recording each kept root's raw and canonical form in res. The roots
// were validated when the scan was requested, but the walk happens later and
// asynchronously: re-resolving refuses a root swapped for an outside-pointing
// symlink in between rather than enumerating it. De-overlapping matters
// because the streaming walk has no global seen-set: a root that duplicates —
// or, when recursing, sits inside — another kept root would visit the same
// files twice under different display paths and fabricate phantom duplicate
// pairs (an aliased scope would pair every file with itself).
func scanRoots(req ScanReq, roots []string, res *toolResult) []string {
	type walkRoot struct{ raw, canon string }
	var wroots []walkRoot
	for _, r := range roots {
		rp, err := resolveAllowedRoot(r)
		if err != nil {
			res.Errors = append(res.Errors, r+": no longer inside the allowed volumes")
			continue
		}
		wroots = append(wroots, walkRoot{raw: r, canon: rp})
	}
	sort.Slice(wroots, func(i, j int) bool { return len(wroots[i].canon) < len(wroots[j].canon) })
	var safeRoots, canons []string
	for _, wr := range wroots {
		covered := false
		for _, kc := range canons {
			if wr.canon == kc || (req.Recurse && strings.HasPrefix(wr.canon, kc+"/")) {
				covered = true
				break
			}
		}
		if !covered {
			canons = append(canons, wr.canon)
			safeRoots = append(safeRoots, wr.raw)
			res.Roots = append(res.Roots, RootMap{Raw: wr.raw, Canon: wr.canon})
		}
	}
	return safeRoots
}

// scanContent is the content pass over the walk's spill log: the duplicates
// scan, and the conflicting-files pass that rides on its hashing. It fills res
// and, when non-nil, corrRes. The spill, the counters and the pinned handles
// come from the shared walk.
func (s *Server) scanContent(req ScanReq, sess *fsSession, cancel chan struct{}, resumeGen uint32, sp *spill, counter, corrCounter *keyCounter, hs []*dirhandle.Handle, rootOf []string, res, corrRes *toolResult) scanOutcome {
	// Pass 2: distil the collision candidates into a second, much smaller
	// spill — still one record at a time, so the candidate population never
	// has to fit in RAM to be identified.
	cs, nCand, ccs, nCorr, ok := s.distilCandidates(req, sp, counter, corrCounter, res, corrRes)
	if !ok {
		return scanAborted
	}
	defer cs.close()
	if ccs != nil {
		defer ccs.close()
	}
	// The persistent hash store lives in RAM only for the scan's duration. It
	// never lets this scan skip a read — every candidate is hashed in full
	// every scan — but it carries earlier scans' hashes, which is what lets
	// record() catch content that moved under an unchanged size and mtime:
	// bit rot. The one exception is an explicit RESUME of an interrupted
	// scan: continuing the dead run's generation makes its own reads servable
	// again, because resuming is that same scan carrying on, not a new scan
	// borrowing old answers.
	var cache *hashCache
	if resumeGen != 0 {
		cache = loadHashCacheResume(s.stateDir(), resumeGen)
	} else {
		cache = loadHashCache(s.stateDir())
	}
	// Rewrite the marker with the live generation: if THIS run dies, a
	// resume of it must continue this generation — and without the rewrite a
	// resume could only ever degenerate to a full re-read.
	s.writeMarker(&req, cache.gen)
	acc := newGroupTop(dupFileCap)
	if outcome := s.hashCandidateWindows(req, cs, nCand, sess, cache, hs, rootOf, cancel, acc, res); outcome != scanFinished {
		return outcome
	}
	res.Groups, res.Truncated = acc.final(s, dupFileCap, cancel)
	m := req.Match
	res.Match = &m
	// The conflicting-files pass, over its own candidates. It runs after the
	// duplicates result is complete so a failure here can never cost that
	// result, and it shares the hash cache — most of the content it needs
	// has just been read.
	if corrRes != nil && ccs != nil {
		cacc := newCorruptTop(corruptFileCap)
		if !s.scanCorrupted(ccs, nCorr, hs, rootOf, cache, cancel, cacc, corrRes, 78, 96) {
			return scanCancelled
		}
		corrRes.Groups, corrRes.Truncated = cacc.final(s, corruptFileCap, cancel)
	}
	if err := cache.save(); err != nil {
		log.Printf("hash cache save failed: %v", err)
	}
	return scanFinished
}

// distilCandidates streams the walk log twice — once for the duplicates
// candidates under the requested criteria, once for the conflicting-files
// candidates under size and modified time alone — and then releases the log.
// The duplicates spill is required: a failure there records the error on res
// and reports !ok. The conflicting-files sweep is not: its key is (size,
// mtime) whatever req.Match says, so the category means the same thing however
// the duplicates criteria are set, and a failure building it is recorded on
// corrRes and reported as a nil ccs — the duplicates result is complete and
// worth keeping on its own.
func (s *Server) distilCandidates(req ScanReq, sp *spill, counter, corrCounter *keyCounter, res, corrRes *toolResult) (cs *spill, nCand int, ccs *spill, nCorr int, ok bool) {
	s.setProgress(15, "Indexing duplicate candidates…")
	sp.onEach = func(done, total int) {
		s.setProgress(15+float64(done)/float64(max(total, 1)),
			"Indexing duplicate candidates… ("+humanCount(done)+" of "+humanCount(total)+")")
	}
	cs, err := newSpill(s.stateDir())
	if err != nil {
		res.Errors = append(res.Errors, "scan aborted — cannot create the candidate spill file: "+err.Error())
		return nil, 0, nil, 0, false
	}
	nCand, err = sp.distil(counter, req.Match, cs)
	counter.release()
	if err != nil {
		cs.close()
		res.Errors = append(res.Errors, "scan aborted — cannot read the scan spill file: "+err.Error())
		return nil, 0, nil, 0, false
	}
	if corrRes == nil {
		// Conflicting Files was not asked for: no second sweep, no second spill.
		sp.onEach = nil
		corrCounter.release()
		sp.close()
		return cs, nCand, nil, 0, true
	}
	// The label follows the work: without this the second sweep replays the
	// first one's progress band saying "Indexing duplicate candidates…",
	// which is a different thing entirely.
	sp.onEach = func(done, total int) {
		s.setProgress(16, "Indexing files to check for corruption… ("+humanCount(done)+" of "+humanCount(total)+")")
	}
	var cerr error
	if ccs, cerr = newSpill(s.stateDir()); cerr != nil {
		cerr = fmt.Errorf("the candidate spill file could not be created: %w", cerr)
	} else if nCorr, cerr = sp.distil(corrCounter, corruptMatch, ccs); cerr != nil {
		cerr = fmt.Errorf("the scan spill file could not be re-read: %w", cerr)
	}
	if cerr != nil {
		// corrRes is kept, not discarded: it is stored under the same
		// completed gate as the duplicates result, so dropping it here would
		// leave the PREVIOUS scan's corrupted listing on screen beside fresh
		// duplicates — and the error explaining why would have gone into the
		// struct being thrown away.
		corrRes.Errors = append(corrRes.Errors, "could not check for corrupted copies — "+cerr.Error())
		if ccs != nil {
			ccs.close()
		}
		ccs, nCorr = nil, 0
	}
	sp.onEach = nil
	corrCounter.release()
	sp.close() // the walk log's blocks come back now, not at scan end
	return cs, nCand, ccs, nCorr, true
}

// hashCandidateWindows is pass 3: hash and group the candidates one PARTITION
// of the key space at a time, folding every group into acc. Every file sharing
// a candidate key shares its partition, so a duplicate group is never split
// across windows — while peak memory follows the window, not the number of
// duplicates on the volume. The price is one re-read of the compact candidate
// spill per partition, which is nothing next to reading the files those
// records point at. Keys too big for one window get their own
// prefix-partitioned pass (resolveSkewedKey), so none of their members go
// unexamined.
func (s *Server) hashCandidateWindows(req ScanReq, cs *spill, nCand int, sess *fsSession, cache *hashCache, handles []*dirhandle.Handle, rootOf []string, cancel chan struct{}, acc *groupTop, res *toolResult) scanOutcome {
	parts := 1
	if nCand > dupWindowFiles {
		parts = (nCand + dupWindowFiles - 1) / dupWindowFiles
	}
	missingCreated := 0
	// Progress budget for the rest of this scan: the duplicates windows own
	// 16→78, the corrupted-files pass 78→96, and the finalize created-date
	// fetch 96→100.
	const dupLo, dupSpan = 16.0, 62.0
	for p := 0; p < parts; p++ {
		lo0 := dupLo + dupSpan*float64(p)/float64(parts)
		cs.onEach = func(done, total int) {
			s.setProgress(lo0, "Indexing duplicate candidates… ("+humanCount(done)+" of "+humanCount(total)+")")
		}
		cands, over, skipped, werr := cs.window(p, parts, req.Match, handles, rootOf, dupKeyFileCap, dupWindowMax)
		cs.onEach = nil
		acc.noteSkipped(skipped)
		if werr != nil {
			res.Errors = append(res.Errors, "scan aborted — cannot read the candidate spill file: "+werr.Error())
			return scanAborted
		}
		// Matching by created date must compare the same values the UI shows:
		// enrich this window's candidates (the only files whose created time
		// can influence grouping) from File Station.
		if sess != nil && req.Match.Created {
			missingCreated += s.enrichCreated(sess, cands, cancel, p, parts)
		}
		lo := dupLo + dupSpan*float64(p)/float64(parts)
		hi := dupLo + dupSpan*float64(p+1)/float64(parts)
		span := hi - lo
		if len(over) > 0 {
			span = (hi - lo) / float64(len(over)+1)
		}
		if !s.dupWindow(cands, req.Match, cache, cancel, acc, lo, lo+span) {
			return scanCancelled // cancelled mid-window
		}
		cands = nil
		for i, k := range over {
			klo := lo + span*float64(i+1)
			serr := s.resolveSkewedKey(cs, k, req, sess, cache, handles, rootOf, cancel, acc,
				klo, klo+span, &missingCreated)
			if serr != nil {
				res.Errors = append(res.Errors, "scan aborted — "+serr.Error())
				return scanAborted
			}
			if cancelled(cancel) {
				return scanCancelled
			}
		}
		// Saving per window bounds what a crash can lose of the NEXT scan's
		// rot-detection baseline — the hashes themselves are re-read every
		// scan regardless. saveMid, not save: the final trim must not run
		// while the corrupted-files pass still needs the in-RAM history.
		if err := cache.saveMid(); err != nil {
			log.Printf("hash cache save failed: %v", err)
		}
	}
	// Candidates File Station could not answer for never group under
	// created-date matching (candKey gives them a per-path sentinel) — tell
	// the user rather than silently thinning the results.
	if missingCreated > 0 && !cancelled(cancel) {
		res.Errors = append(res.Errors, fmt.Sprintf(
			"%d files had no File Station creation time and were excluded from created-date matching", missingCreated))
	}
	return scanFinished
}

// fetchCreatedDates fills the Created column of a finished scan from File
// Station. Hashing tops out at 96%; this fills 96–100 so a large result set
// never looks stuck on a silent "Finalizing…". The corrupted-files rows carry
// the same column, filled from the same place, and go FIRST, holding the bar
// at 96: their sets are a small fraction of the duplicates result, and running
// them afterwards would let the bar reach 100 and then fall back to 96, which
// reads as a scan that restarted itself.
func (s *Server) fetchCreatedDates(sess *fsSession, res, corrRes *toolResult, cancel chan struct{}) {
	if sess == nil {
		return
	}
	if corrRes != nil {
		applyCrtimes(sess, corrRes, cancel, func(done, total int) {
			s.setProgress(96, "Fetching creation times… ("+humanCount(done)+" of "+humanCount(total)+")")
		})
	}
	applyCrtimes(sess, res, cancel, func(done, total int) {
		s.setProgress(96+4*float64(done)/float64(max(total, 1)),
			"Fetching creation times… ("+humanCount(done)+" of "+humanCount(total)+")")
	})
	// Stop pressed during this phase keeps the results — the scan's real work
	// is done — but must not pass as a complete scan: the rows left without a
	// Created Date would otherwise vanish under a created-date filter with no
	// word about why. The gap is written into the scan's own issue list.
	if cancelled(cancel) {
		if n := blankCreated(res) + blankCreated(corrRes); n > 0 {
			res.Errors = append(res.Errors, "stopped before every Created Date was fetched — "+humanCount(n)+
				" files show none; scan again to fill them in")
		}
	}
}

// blankCreated counts the rows of a result that carry no Created Date.
func blankCreated(res *toolResult) int {
	if res == nil {
		return 0
	}
	n := 0
	for i := range res.Files {
		if res.Files[i].Created == "" {
			n++
		}
	}
	for gi := range res.Groups {
		for fi := range res.Groups[gi].Files {
			if res.Groups[gi].Files[fi].Created == "" {
				n++
			}
		}
	}
	return n
}

// Stored-result caps. The results live in daemon memory on NAS hardware that
// may have little of it — and are persisted, paged and exported from there —
// so every tool keeps the rows worth acting on first and reports the rest.
// No cap here is ever silent: each one produces a TruncInfo the UI shows.
// duplicates: the groups with the most reclaimable space, on group
// boundaries, and the ceiling on any single group. 100k files ≈ tens of MB
// cached. A variable only so the tests can shrink it.
var dupFileCap = 100000

const (
	// empty_files / temp_files: the first by path.
	flatFileCap = 20000
	// empty_folders: the first by path. This also bounds the confirmation
	// round trips — every candidate costs one File Station call.
	emptyFolderCap = 20000
)

// ---------------------------------------------------------------- helpers

func (s *Server) fileEnt(f fEnt) FileEnt {
	// nextID is persisted under mu; take it here too rather than relying on
	// the scan/move protocol to keep every caller off the same field.
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	s.mu.Unlock()
	var modUnix int64
	if !f.mod.IsZero() {
		modUnix = f.mod.Unix()
	}
	fe := FileEnt{
		ID: "f" + strconv.Itoa(id), Name: f.name, Dir: f.dir,
		Size: f.size, Mod: fmtTime(f.mod), ModUnix: modUnix, Created: fmtTime(f.created),
		Ext: extOf(f.name),
		// Set here rather than per tool: it is a property of the NAME, true
		// of any row any tool surfaces, so every one of them gets it right
		// without each scan case having to remember.
		NoMove: fsCannotAddress(f.name),
	}
	if f.link.n > 1 {
		fe.Links = int(f.link.n)
		fe.Ino = strconv.FormatUint(f.link.dev, 16) + ":" + strconv.FormatUint(f.link.ino, 16)
	}
	return fe
}

func (s *Server) fileEnts(in []fEnt) []FileEnt {
	out := make([]FileEnt, 0, len(in))
	for _, f := range in {
		out = append(out, s.fileEnt(f))
	}
	return out
}

func extOf(name string) string {
	e := strings.TrimPrefix(filepath.Ext(name), ".")
	if e == "" {
		return "FILE"
	}
	return strings.ToUpper(e)
}

func humanCount(n int) string {
	s := strconv.Itoa(n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// scrubErr rewrites pinned-handle addresses (/proc/self/fd/N) in an error to
// the user-facing paths they stand for, so handle plumbing never leaks into
// API responses; any address not in the map is shortened to "…".
func scrubErr(err error, repl map[string]string) error {
	if err == nil {
		return nil
	}
	s := err.Error()
	for from, to := range repl {
		if strings.HasPrefix(from, "/proc/") {
			s = strings.ReplaceAll(s, from, to)
		}
	}
	s = procFdRe.ReplaceAllString(s, "…")
	if s == err.Error() {
		return err // nothing leaked: keep the typed error intact
	}
	return errors.New(s)
}

var procFdRe = regexp.MustCompile(`/proc/self/fd/\d+`)
