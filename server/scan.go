package main

import (
	"bytes"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lukechampine.com/blake3"
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
	rel             string     // path relative to the pinned root
	rh              *dirHandle // pinned root; nil only for hand-built test entries
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
			s.job.Progress = p
		}
		if label != "" {
			s.job.Label = label
		}
	}
	s.mu.Unlock()
}

func cancelled(cancel chan struct{}) bool {
	select {
	case <-cancel:
		return true
	default:
		return false
	}
}

// resumeGen, when non-zero, is the hash-store generation of an interrupted
// scan the user chose to RESUME: the duplicates pass adopts it instead of
// advancing, so files the dead run already read in full are not read again.
// 0 means a normal scan (including a resume of a run that died before it
// opened the store — there is nothing of its reading to continue).
func (s *Server) runScan(req ScanReq, roots []string, sess *fsSession, cancel chan struct{}, resumeGen uint32) {
	res := &toolResult{Tool: req.Tool}
	// completed means the scan ran its work to the end. Results are stored
	// only then — but judged by THIS scan's own progress, not by whether
	// the cancel channel was closed: a cancel that arrives after the work
	// finished can no longer save anything and must not discard results.
	completed := false
	// corrRes is the duplicates scan's second result: the corrupted-files
	// category. It is stored under the same completed gate and inside the same
	// lock section as res, so the two always describe the same scan — a cancel
	// or a panic that drops one must never leave the other on screen claiming
	// to be current. Nil for every other tool, and nilled out if the pass could
	// not run, in which case the previous one is left alone rather than
	// replaced by a blank.
	var corrRes *toolResult
	// The marker records this scan in flight; every controlled ending clears
	// it, so a marker found at the next daemon start means an interruption.
	// Gen 0 for now: the duplicates pass rewrites the marker once it has
	// opened the hash store and knows the generation a resume would need.
	s.writeMarker(&req, 0)
	defer func() {
		if r := recover(); r != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("internal error: %v", r))
			completed = true // surface the failure to the UI rather than vanishing
			// A panic mid-pass leaves the corrupted-files result half-built;
			// storing it would show the user a set list from a scan that did
			// not finish.
			corrRes = nil
		}
		res.Scanned = time.Now().Format(time.RFC3339)
		s.mu.Lock()
		if completed {
			s.results[req.Tool] = res
			if corrRes != nil {
				corrRes.Scanned = res.Scanned
				s.results["corrupted_files"] = corrRes
			}
			s.invalidateDerivedLocked() // views and date spans describe the results just replaced
			// Deliberately the REQUESTED tool, not the side-effect one: the
			// UI's completion poll opens whatever lastTool names.
			s.lastTool = req.Tool
			// The reference folders a move must respect are the ones the
			// STORED results were scanned with — published here, at
			// completion, never at admission: a cancelled scan leaves the
			// old results on screen, so it must leave their protection too.
			s.refDirs = uniquePaths(req.RefDirs)
			if req.Tool == "duplicates" {
				s.keepers = nil // fresh groups supersede recorded survivors
			}
		}
		// How this run ended, for /api/state: the UI's poller has to tell a
		// stop from a finish, and lastTool alone cannot.
		s.lastEnd = scanEnd{Tool: req.Tool, Completed: completed}
		s.mu.Unlock()
		// Results reach the disk BEFORE the marker goes and BEFORE the job
		// slot is released: a crash inside saveState must still find the
		// marker, so the loss is reported rather than silent — and a scan
		// admitted in between must not have ITS fresh marker deleted by
		// this one's os.Remove.
		if completed {
			s.saveState() // a finished scan survives a daemon restart
		}
		s.clearMarker()
		s.mu.Lock()
		s.job = jobState{}
		s.mu.Unlock()
	}()

	s.setProgress(2, "Enumerating directories…")
	// The roots were validated when the scan was requested, but the walk
	// happens here, later and asynchronously: re-resolve them so a root
	// swapped for an outside-pointing symlink in between is refused rather
	// than enumerated. The resolved roots are then de-overlapped
	// canonically: the streaming walk has no global seen-set, so a root
	// that duplicates — or, when recursing, sits inside — another kept root
	// would visit the same files twice under different display paths and
	// fabricate phantom duplicate pairs (an aliased scope used to pair
	// every file with itself).
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

	// The pinned root handles must outlive every content read below —
	// hashing and EXIF open entries through them, never by raw path.
	var handles []*dirHandle
	defer func() {
		for _, h := range handles {
			h.Close()
		}
	}()

	switch req.Tool {
	case "duplicates":
		corrRes = &toolResult{Tool: "corrupted_files", Roots: res.Roots}
		// Pass 1: stream the walk into a compact spill file plus a bounded
		// candidate-key counter — nothing per-file stays in RAM.
		sp, err := newSpill(s.stateDir())
		if err != nil {
			res.Errors = append(res.Errors, "scan aborted — cannot create the scan spill file: "+err.Error())
			completed = true
			return
		}
		defer sp.close()
		// Two candidate keys are counted over the one walk: the duplicates key,
		// which folds in whatever match criteria were requested, and the
		// corrupted-files key, which is size + modified time and nothing else.
		// They share the counter's fixed memory budget rather than doubling it.
		counter := newKeyCounterShare(1)
		corrCounter := newKeyCounterShare(1)
		var spillErr error
		var walkErrs []string
		var rootOf []string
		walkErrs, handles, rootOf = s.walkStream(safeRoots, req.Recurse, false, cancel, func(idx int, f fEnt) {
			if f.isDir || f.size == 0 {
				return
			}
			counter.add(candHash(&f, req.Match))
			corrCounter.add(candHash(&f, corruptMatch))
			if spillErr == nil {
				spillErr = sp.add(idx, &f)
			}
		}, nil)
		res.Errors = append(res.Errors, walkErrs...)
		if cancelled(cancel) {
			return
		}
		if spillErr != nil {
			res.Errors = append(res.Errors, "scan aborted — cannot write the scan spill file (disk full?): "+spillErr.Error())
			completed = true
			return
		}
		// Pass 2: distil the collision candidates into a second, much
		// smaller spill — still one record at a time, so the candidate
		// population never has to fit in RAM to be identified.
		s.setProgress(15, "Indexing duplicate candidates…")
		sp.onEach = func(done, total int) {
			s.setProgress(15+float64(done)/float64(max(total, 1)),
				"Indexing duplicate candidates… ("+humanCount(done)+" of "+humanCount(total)+")")
		}
		cs, err := newSpill(s.stateDir())
		if err != nil {
			res.Errors = append(res.Errors, "scan aborted — cannot create the candidate spill file: "+err.Error())
			completed = true
			return
		}
		defer cs.close()
		nCand, err := sp.distil(counter, req.Match, cs)
		counter.release()
		if err != nil {
			sp.close()
			res.Errors = append(res.Errors, "scan aborted — cannot read the scan spill file: "+err.Error())
			completed = true
			return
		}
		// The corrupted-files candidates come out of the SAME walk log, in a
		// second sweep taken before it is released. Its key is (size, mtime)
		// whatever req.Match says, so the category means the same thing however
		// the duplicates criteria are set. A failure here is never fatal: the
		// duplicates result is complete and worth keeping on its own.
		var ccs *spill
		nCorr := 0
		{
			// The label follows the work: without this the second sweep replays
			// the first one's progress band saying "Indexing duplicate
			// candidates…", which is a different thing entirely.
			sp.onEach = func(done, total int) {
				s.setProgress(16, "Indexing files to check for corruption… ("+humanCount(done)+" of "+humanCount(total)+")")
			}
			var cerr error
			if ccs, cerr = newSpill(s.stateDir()); cerr != nil {
				cerr = fmt.Errorf("the candidate spill file could not be created: %w", cerr)
			} else if nCorr, cerr = sp.distil(corrCounter, corruptMatch, ccs); cerr != nil {
				cerr = fmt.Errorf("the scan spill file could not be re-read: %w", cerr)
			}
			if ccs != nil {
				defer ccs.close()
			}
			if cerr != nil {
				// corrRes is kept, not discarded: it is stored under the same
				// completed gate as the duplicates result, so dropping it here
				// would leave the PREVIOUS scan's corrupted listing on screen
				// beside fresh duplicates — and the error explaining why would
				// have gone into the struct being thrown away.
				corrRes.Errors = append(corrRes.Errors,
					"could not check for corrupted copies — "+cerr.Error())
				ccs = nil
			}
			sp.onEach = nil
		}
		corrCounter.release()
		sp.close() // the walk log's blocks come back now, not at scan end
		// Pass 3: hash and group the candidates one PARTITION of the key
		// space at a time. Every file sharing a candidate key shares its
		// partition, so a duplicate group is never split across windows —
		// while peak memory follows the window, not the number of duplicates
		// on the volume. The price is one re-read of the compact candidate
		// spill per partition, which is nothing next to reading the files
		// those records point at.
		parts := 1
		if nCand > dupWindowFiles {
			parts = (nCand + dupWindowFiles - 1) / dupWindowFiles
		}
		// The persistent hash store lives in RAM only for the scan's
		// duration. It never lets this scan skip a read — every candidate is
		// hashed in full every scan — but it carries earlier scans' hashes,
		// which is what lets record() catch content that moved under an
		// unchanged size and mtime: bit rot. The one exception is an
		// explicit RESUME of an interrupted scan: continuing the dead run's
		// generation makes its own reads servable again, because resuming
		// is that same scan carrying on, not a new scan borrowing old
		// answers.
		var cache *hashCache
		if resumeGen != 0 {
			cache = loadHashCacheResume(s.stateDir(), resumeGen)
		} else {
			cache = loadHashCache(s.stateDir())
		}
		// Rewrite the marker with the live generation: if THIS run dies, a
		// resume of it must continue this generation — and without the
		// rewrite a resume could only ever degenerate to a full re-read.
		s.writeMarker(&req, cache.gen)
		acc := newGroupTop(dupFileCap)
		missingCreated := 0
		// Progress budget for the rest of this scan: the duplicates windows
		// own 16→78, the corrupted-files pass 78→96, and the finalize
		// created-date fetch 96→100.
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
				completed = true
				return
			}
			// Matching by created date must compare the same values the UI
			// shows: enrich this window's candidates (the only files whose
			// created time can influence grouping) from File Station.
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
				return // cancelled mid-window
			}
			cands = nil
			// Keys too big for one window get their own prefix-partitioned
			// pass, so none of their members go unexamined.
			for i, k := range over {
				klo := lo + span*float64(i+1)
				serr := s.resolveSkewedKey(cs, k, req, sess, cache, handles, rootOf, cancel, acc,
					klo, klo+span, &missingCreated)
				if serr != nil {
					res.Errors = append(res.Errors, "scan aborted — "+serr.Error())
					completed = true
					return
				}
				if cancelled(cancel) {
					return
				}
			}
			// Saving per window bounds what a crash can lose of the NEXT
			// scan's rot-detection baseline — the hashes themselves are
			// re-read every scan regardless. saveMid, not save: the final
			// trim must not run while the corrupted-files pass still needs
			// the in-RAM history.
			if err := cache.saveMid(); err != nil {
				log.Printf("hash cache save failed: %v", err)
			}
		}
		// Candidates File Station could not answer for never group under
		// created-date matching (candKey gives them a per-path sentinel)
		// — tell the user rather than silently thinning the results.
		if missingCreated > 0 && !cancelled(cancel) {
			res.Errors = append(res.Errors, fmt.Sprintf(
				"%d files had no File Station creation time and were excluded from created-date matching", missingCreated))
		}
		res.Groups, res.Truncated = acc.final(s, dupFileCap, cancel)
		m := req.Match
		res.Match = &m
		// The corrupted-files pass, over its own candidates. It runs after the
		// duplicates result is complete so a failure here can never cost that
		// result, and it shares the hash cache — most of the content it needs
		// has just been read.
		if corrRes != nil && ccs != nil {
			cacc := newCorruptTop(corruptFileCap)
			if !s.scanCorrupted(ccs, nCorr, handles, rootOf, cache, cancel, cacc, corrRes, 78, 96) {
				return // cancelled mid-pass
			}
			corrRes.Groups, corrRes.Truncated = cacc.final(s, corruptFileCap, cancel)
			if err := cache.save(); err != nil {
				log.Printf("hash cache save failed: %v", err)
			}
		}
	case "empty_files", "temp_files":
		keep := func(f *fEnt) bool { return f.size == 0 }
		if req.Tool == "temp_files" {
			keep = func(f *fEnt) bool { return isTempName(f.name) }
		}
		top := newBoundedTop(flatFileCap, func(a, b *fEnt) bool { return a.path < b.path })
		var walkErrs []string
		walkErrs, handles, _ = s.walkStream(safeRoots, req.Recurse, false, cancel, func(_ int, f fEnt) {
			if !f.isDir && keep(&f) {
				top.add(f)
			}
		}, nil)
		res.Errors = append(res.Errors, walkErrs...)
		if cancelled(cancel) {
			return
		}
		ents, trunc := top.final()
		res.Files = s.fileEnts(ents)
		res.Truncated = trunc
	case "empty_folders":
		// The walk is consumed as it arrives: a stack of open directories,
		// one frame per level, decides topmost-emptiness on the way back up.
		// Nothing proportional to the directory count is ever held.
		ef := newEmptyFolderScan()
		var walkErrs []string
		walkErrs, handles, _ = s.walkStream(safeRoots, req.Recurse, true, cancel,
			ef.visit, ef.noteUnreadable)
		res.Errors = append(res.Errors, walkErrs...)
		if cancelled(cancel) {
			return
		}
		var confirmErrs []string
		res.Files, res.Truncated, confirmErrs = ef.finish(s, cancel, func(p string) (bool, error) {
			return confirmEmpty(p, sess)
		})
		res.Errors = append(res.Errors, confirmErrs...)
	default:
		// This switch is the only thing that decides what a scan DOES, and it
		// is reached with any id validTools accepts. Without this arm, adding a
		// tool id and forgetting its case would run nothing, fall through to
		// completed = true below, and store an EMPTY result over whatever the
		// tool had — silent data loss caused by registration alone. Refusing
		// leaves the existing result untouched (completed stays false).
		log.Printf("scan requested for tool %q, which has no scan implementation", req.Tool)
		return
	}
	if cancelled(cancel) {
		return // cancelled mid-scan: partial results are dropped
	}
	completed = true
	// Hashing tops out at 96%; the finalize crtime fetch fills 96–100 so a
	// large result set never looks stuck on a silent "Finalizing…".
	if sess != nil {
		// The corrupted-files rows carry the same Created column, filled from
		// the same place. It goes FIRST, holding the bar at 96: its sets are a
		// small fraction of the duplicates result, and running it afterwards
		// let the bar reach 100 and then fall back to 96, which reads as a
		// scan that restarted itself.
		if corrRes != nil {
			applyCrtimes(sess, corrRes, cancel, func(done, total int) {
				s.setProgress(96, "Fetching creation times… ("+humanCount(done)+" of "+humanCount(total)+")")
			})
		}
		applyCrtimes(sess, res, cancel, func(done, total int) {
			s.setProgress(96+4*float64(done)/float64(max(total, 1)),
				"Fetching creation times… ("+humanCount(done)+" of "+humanCount(total)+")")
		})
	}
	s.setProgress(100, "Finalizing results…")
}

// walkStream enumerates entries under each root, handing every one to visit
// instead of materializing the tree in memory (scale phase 3): what a scan
// keeps is decided by its accumulator, so daemon memory no longer grows
// with the volume. When recurse is false only the direct children of each
// root are visited; directories are visited only when withDirs is true
// (empty-folder scan). The unreadable result lists paths whose contents
// could not be read (permission denied etc.) — such directories have
// unknown contents and must never be treated as empty. The returned handles
// are the pinned roots (rootOf holds each handle's raw root path, and visit
// receives the handle's index); they stay open so later content reads go
// through them, and the caller closes them when the scan is done. Roots
// must already be de-overlapped (runScan) — there is no seen-set here.
func (s *Server) walkStream(roots []string, recurse, withDirs bool, cancel chan struct{}, visit func(rootIdx int, f fEnt), unreadable func(path string)) (errs []string, handles []*dirHandle, rootOf []string) {
	count := 0
	// The error list is capped so a volume full of permission-denied
	// subtrees cannot grow it without bound — but no cap here is silent, so
	// the overflow is counted and reported as one closing line.
	dropped := 0
	addErr := func(e string) {
		if len(errs) < 50 {
			errs = append(errs, e)
		} else {
			dropped++
		}
	}
	defer func() {
		if dropped > 0 {
			errs = append(errs, fmt.Sprintf("… and %d more locations could not be read", dropped))
		}
	}()
	// Only the empty-folder scan takes the unreadable callback (a directory
	// whose contents cannot be read must never be called empty), and it takes
	// it INTERLEAVED with the entries: its accumulator tracks where the walk
	// currently is, so an unreadable path has to arrive in walk order rather
	// than in a list afterwards. The list is gone with it — on a volume with
	// many permission-denied paths it grew for nothing.
	noteUnreadable := func(p string) {
		if unreadable != nil {
			unreadable(p)
		}
	}
	// path is the user-facing location stored in results; rel/rh address the
	// same entry through its pinned root for later content reads. created is
	// left zero on purpose: Created Dates come from File Station only (see
	// applyCrtimes) — never from a native statx/ctime approximation.
	emit := func(rootIdx int, path, rel string, rh *dirHandle, d fs.DirEntry) {
		info, err := d.Info()
		if err != nil {
			return
		}
		visit(rootIdx, fEnt{
			path: path, name: d.Name(), dir: filepath.Dir(path),
			size: info.Size(), mod: info.ModTime(),
			isDir: d.IsDir(), rel: rel, rh: rh,
		})
		count++
		if count%2000 == 0 {
			// The total is unknown until the walk ends, so the bar approaches
			// the end of this phase asymptotically rather than sitting still:
			// a bar that has not moved in an hour reads as a hung scan.
			frac := float64(count) / float64(count+2000000)
			s.setProgress(2+13*frac, "Collecting file metadata… ("+humanCount(count)+" items)")
		}
	}

	vols := volumeRootsResolved()
	for _, root := range roots {
		if cancelled(cancel) {
			break
		}
		// Label only (-1 keeps the current percentage): the 2–15 band belongs
		// to emit's count-based tick, which is monotonic across all roots.
		// Writing a root-proportional percentage here as well put two
		// formulas on one band — each new root jumped the bar forward and the
		// next tick pulled it back, the same "scan restarted itself" reading
		// the finalize path is careful to avoid.
		s.setProgress(-1, "Collecting file metadata…")
		// Pin the root and re-check containment from the pinned object: the
		// enumeration below goes through the handle, so a root swapped for
		// an outside-pointing symlink after validation is never followed.
		h, err := openDirHandle(root)
		if err != nil {
			addErr(root + ": " + err.Error())
			noteUnreadable(root)
			continue
		}
		canon, cerr := h.Canon()
		if cerr != nil || !isUnder(canon, vols) {
			h.Close()
			addErr(root + ": no longer inside the allowed volumes")
			noteUnreadable(root)
			continue
		}
		// The handle stays open past enumeration: content reads (hashing,
		// EXIF) open entries through it, so it is closed by the caller
		// only when the whole scan is finished.
		handles = append(handles, h)
		rootOf = append(rootOf, root)
		idx := len(handles) - 1
		scrub := map[string]string{h.Path(): root}
		if !recurse {
			ents, err := os.ReadDir(h.Path())
			if err != nil {
				addErr(root + ": " + scrubErr(err, scrub).Error())
				noteUnreadable(root)
				continue
			}
			for _, e := range ents {
				if e.Type()&fs.ModeSymlink != 0 {
					continue
				}
				if e.IsDir() {
					if withDirs && !skipName(e.Name()) {
						emit(idx, filepath.Join(root, e.Name()), e.Name(), h, e)
					}
					continue
				}
				emit(idx, filepath.Join(root, e.Name()), e.Name(), h, e)
			}
			continue
		}
		// Walk through the pinned root (DirFS: WalkDir itself would Lstat
		// the proc magic link), storing display paths under the raw root.
		walkErr := fs.WalkDir(os.DirFS(h.Path()), ".", func(rel string, d fs.DirEntry, err error) error {
			if cancelled(cancel) {
				return fs.SkipAll
			}
			display := root
			if rel != "." {
				display = filepath.Join(root, rel)
			}
			if err != nil {
				addErr(display + ": " + scrubErr(err, scrub).Error())
				noteUnreadable(display)
				return nil
			}
			if d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			if d.IsDir() {
				if rel != "." && skipName(d.Name()) {
					return fs.SkipDir
				}
				if withDirs && rel != "." {
					emit(idx, display, rel, h, d)
				}
				return nil
			}
			emit(idx, display, rel, h, d)
			return nil
		})
		if walkErr != nil {
			addErr(root + ": " + scrubErr(walkErr, scrub).Error())
			noteUnreadable(root)
		}
	}
	return errs, handles, rootOf
}

// ------------------------------------------------------------- duplicates

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
	cache *hashCache, handles []*dirHandle, rootOf []string, cancel chan struct{},
	acc *groupTop, lo, hi float64, missingCreated *int) error {

	s.setProgress(lo, "Comparing file contents…")
	sub, n, err := s.hashSubSpill(cs, handles, rootOf, cancel, acc, cache, false, nil,
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
	cache *hashCache, handles []*dirHandle, rootOf []string, cancel chan struct{},
	acc *groupTop, lo, hi float64, missingCreated *int) error {

	full, n, err := s.hashSubSpill(sub, handles, rootOf, cancel, acc, cache, true, nil, keep)
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
			// contains every tag `over` holds. Appending listed the
			// over-cap tags twice and emitted their groups twice — the
			// same duplicate group under two ids, double-counted
			// reclaimable space, and a doubled truncation report.
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
// a second time. Before this the ladder read a skewed key's files at full
// length twice, once to tag them and once to group them.
// skipNoter is whatever accumulator the current pass is filling. Both groupTop
// (duplicates) and corruptTop (corrupted files) record the candidates a cap
// meant were never examined, and hashSubSpill needs nothing else from either.
type skipNoter interface{ noteSkipped(int) }

// onRot, when non-nil, is called with each path whose full read here proved
// deep rot (record() returned true: content moved under an unchanged size and
// mtime since an earlier scan). The store's own `changed` set carries the same
// fact, but that set is capped (changedMax) — the callback is the uncapped
// carrier for the pass that is consuming the evidence right now, so a full
// set can never cost it the very finding this read just made.
func (s *Server) hashSubSpill(src *spill, handles []*dirHandle, rootOf []string,
	cancel chan struct{}, acc skipNoter, cache *hashCache, full bool,
	onRot func(path string), keep func(*spillRec) bool) (*spill, int, error) {

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
			return nil
		}
		if full {
			if hit, ok := cache.lookup(f.path, f.size, f.mod.Unix(), h); ok {
				h, cached = hit, cached+1
			} else {
				whole, ferr := hashFile(f.openContent, -1, cancel)
				if ferr != nil {
					unreadable++
					return nil
				}
				if cache.record(f.path, f.size, f.mod.Unix(), h, whole) && onRot != nil {
					onRot(f.path)
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
	// hash is exactly where rot beyond the 64 KiB prefix used to hide.
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
// cancellably, with its own progress label. It used to happen inline, one
// row after another on the scan goroutine, with no cancel check: up to 100k
// re-opens that Stop could not interrupt, sitting under a label that still
// said the hashes were being computed.
func (s *Server) fillCaptured(out []Group, slots []captureSlot, cancel chan struct{}) {
	if len(slots) == 0 {
		return
	}
	parallelEach(slots, cancel, func(sl captureSlot) {
		out[sl.gi].Files[sl.fi].Captured = exifCaptured(sl.f.openContent, sl.f.name)
	}, func(done, total int) {
		s.setProgress(-1, "Reading capture dates… ("+humanCount(done)+" of "+humanCount(total)+")")
	})
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

// parallelEach runs fn over items with a small worker pool, reporting
// progress. The pool is capped well below the CPU count: the work is
// dominated by disk reads, and more readers on a NAS array means more
// seeking, not more throughput.
func parallelEach[T any](items []T, cancel chan struct{}, fn func(T), prog func(done, total int)) {
	workers := runtime.NumCPU()
	if workers > 4 {
		workers = 4
	}
	if workers < 1 {
		workers = 1
	}
	ch := make(chan T)
	var wg sync.WaitGroup
	var done int64
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range ch {
				if cancelled(cancel) {
					continue
				}
				fn(f)
				d := atomic.AddInt64(&done, 1)
				if prog != nil && d%16 == 0 {
					prog(int(d), len(items))
				}
			}
		}()
	}
	for _, f := range items {
		if cancelled(cancel) {
			break
		}
		ch <- f
	}
	close(ch)
	wg.Wait()
}

// contentPrefixUnchanged re-reads a file's first 64 KiB and reports whether
// it still hashes to what the scan recorded. cp is the canonical path the
// move has already vetted; any failure to read reports "changed", because
// the caller uses this only to refuse.
func contentPrefixUnchanged(cp, want string) bool {
	if want == "" {
		return true
	}
	h, err := hashFile(func() (*os.File, error) { return os.Open(cp) }, 64*1024, nil)
	if err != nil || len(h) < len(want) {
		return false
	}
	return h[:len(want)] == want
}

// hashBufPool recycles hashFile's read buffer. Allocating (and zeroing) a
// fresh megabyte per call cost as much as the warm 64 KiB prefix read it
// wrapped, and put a GC cycle every hundred files under the hashing workers.
var hashBufPool = sync.Pool{New: func() any { b := make([]byte, 1024*1024); return &b }}

// hashFile hashes up to limit bytes of a file (-1 = whole file). open is
// the entry's pinned-handle opener (fEnt.openContent), so the bytes hashed
// are read from inside the vetted tree — never through a symlink swapped
// in after enumeration.
func hashFile(open func() (*os.File, error), limit int64, cancel chan struct{}) (string, error) {
	f, err := open()
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := blake3.New(32, nil)
	var r io.Reader = f
	if limit > 0 {
		r = io.LimitReader(f, limit)
	}
	bp := hashBufPool.Get().(*[]byte)
	defer hashBufPool.Put(bp)
	buf := *bp
	for {
		if cancelled(cancel) {
			return "", errCancelled
		}
		n, err := r.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ------------------------------------------------------------ other scans

var tempExact = map[string]bool{
	"thumbs.db": true, ".ds_store": true, "desktop.ini": true,
	"ehthumbs.db": true, ".apdisk": true, "npm-debug.log": true,
}
var tempSuffix = []string{".tmp", ".temp", ".bak", ".old", ".crdownload",
	".part", ".partial", ".swp", ".swo", "~"}
var tempPrefix = []string{"~$", ".~lock.", "._"}

func isTempName(name string) bool {
	l := strings.ToLower(name)
	if tempExact[l] {
		return true
	}
	for _, suf := range tempSuffix {
		if strings.HasSuffix(l, suf) {
			return true
		}
	}
	for _, pre := range tempPrefix {
		if strings.HasPrefix(l, pre) {
			return true
		}
	}
	return false
}

// confirmEmpty reports whether a candidate folder holds nothing the user
// could miss, per File Station's own listing (see folderHoldsOnlyJunk):
// zero entries, or entries that are all junk — files the Temporary Files
// tool itself would list, plus Synology's @eaDir thumbnail cache. Any other
// entry counts as "not empty": this gate must never over-report emptiness.
// An error is returned rather than folded into "not empty", because the
// caller has to REPORT it: a File Station outage mid-confirmation must read
// as "these could not be checked", not as "these were not empty". Candidates
// always sit inside a shared folder — scan roots must, and candidates lie
// strictly below the roots — so share space can always address them.
func confirmEmpty(p string, sess *fsSession) (bool, error) {
	sp, err := sess.shareSpacePath(p)
	if err != nil {
		return false, err
	}
	return sess.folderHoldsOnlyJunk(sp)
}

// emptyFolderScan turns the walk's entry stream into topmost-empty-folder
// candidates without ever holding the directory tree.
//
// "Empty" means the folder holds nothing the user could miss: zero entries,
// or nothing but junk files — the Temporary Files tool's own name list
// (visit skips those when marking hasFile; the junk rides along when the
// folder is moved). The walk finds the TOPMOST directory whose subtree
// holds no real file, and only that one is offered; a candidate is reported
// only after the confirm callback (File Station's own listing in production
// — see confirmEmpty) verifies every entry it can see is junk too. The
// walker skips hidden/system DIRECTORIES (".*", "@eaDir", "#recycle", …),
// so a candidate may hold one — confirmation accepts @eaDir (Synology's
// thumbnail cache, regenerated on demand) and rejects every other
// directory: a folder whose only child is .git is a repository, not
// clutter, and a folder holding just an empty folder reports neither (the
// long-standing b/c rule — data-safe for a tool that must never cause
// loss). Directories whose contents could not be read have unknown contents
// — e.g. a Hyper Backup .hbk vault the package user cannot open — so they
// and their ancestors count as non-empty rather than being offered for
// moving.
//
// The old form built maps of every directory, every file-holding directory
// and every ancestor mark; on a volume with millions of directories that was
// the last accumulator here that grew with the filesystem. This one keeps a
// frame per OPEN directory — tree depth, not tree size — because fs.WalkDir
// hands entries out depth-first: a directory's fate is decided when the walk
// leaves it, and leaving is visible as the first entry that is no longer
// underneath it.
type emptyFolderScan struct {
	stack   []efFrame
	top     *boundedTop // topmost-empty candidates, first emptyFolderCap by path
	dropped int         // candidates dropped by a frame's own cap
	// Which scan root the frames on the stack belong to. runScan only
	// de-overlaps nested roots when the scan is RECURSIVE, so a
	// non-recursive scope legitimately walks both /R and /R/D — and a
	// reference folder inside a scanned folder is exactly that shape,
	// since handleScan appends refDirs to the roots.
	curRoot int
}

// efFrame is one directory the walk is currently inside. empties holds its
// child directories whose subtrees turned out to hold no file: whether they
// are TOPMOST is not known until this directory's own fate is (if this one
// also holds no file, it supersedes them), so they wait here. They are
// flushed the moment a file shows up, which is why the list is normally
// empty — it only ever holds what was seen before the first file.
type efFrame struct {
	path    string
	size    int64
	mod     time.Time
	isRoot  bool
	hasFile bool
	empties []dirEnt
	dropped int
}

func newEmptyFolderScan() *emptyFolderScan {
	return &emptyFolderScan{
		top:     newBoundedTop(emptyFolderCap, func(a, b *fEnt) bool { return a.path < b.path }),
		curRoot: -1,
	}
}

func (e *emptyFolderScan) emit(d dirEnt) {
	e.top.add(fEnt{
		path: d.path, name: filepath.Base(d.path), dir: filepath.Dir(d.path),
		size: d.size, mod: d.mod, isDir: true,
	})
}

// markHasFile records that a directory's subtree holds content, and releases
// the empty children it was holding back — they are topmost now.
func (e *emptyFolderScan) markHasFile(f *efFrame) {
	if f.hasFile {
		return
	}
	f.hasFile = true
	for _, d := range f.empties {
		e.emit(d)
	}
	e.dropped += f.dropped
	f.empties, f.dropped = nil, 0
}

// addEmpty hands an empty directory to its parent: emitted straight away if
// the parent already has content (or is a scan root, whose children are
// always topmost), held otherwise.
func (e *emptyFolderScan) addEmpty(p *efFrame, d dirEnt) {
	if p.hasFile || p.isRoot {
		e.emit(d)
		return
	}
	if len(p.empties) < emptyFolderCap {
		p.empties = append(p.empties, d)
		return
	}
	p.dropped++
}

// popTo leaves every directory the walk is no longer inside. A frame stays
// while p is itself that directory or sits underneath it.
func (e *emptyFolderScan) popTo(p string) {
	for len(e.stack) > 0 {
		f := &e.stack[len(e.stack)-1]
		if f.path == p || strings.HasPrefix(p, f.path+"/") {
			return
		}
		e.pop()
	}
}

func (e *emptyFolderScan) pop() {
	f := e.stack[len(e.stack)-1]
	e.stack = e.stack[:len(e.stack)-1]
	var parent *efFrame
	if len(e.stack) > 0 {
		parent = &e.stack[len(e.stack)-1]
	}
	if f.hasFile || f.isRoot {
		for _, d := range f.empties {
			e.emit(d)
		}
		e.dropped += f.dropped
		if f.hasFile && parent != nil {
			e.markHasFile(parent)
		}
		return
	}
	// Nothing but (possibly) empty directories below: this one supersedes
	// them as the topmost empty folder, and they are dropped with it.
	if parent != nil {
		e.addEmpty(parent, dirEnt{path: f.path, size: f.size, mod: f.mod})
	}
}

func (e *emptyFolderScan) visit(rootIdx int, f fEnt) {
	// Crossing into a new scan root closes the previous root's frames. Without
	// this, a nested root inherits the OUTER walk's still-open frame for its
	// own directory: popTo keeps that frame (the new entries sit under it),
	// so the `len(e.stack) == 0` branch below never runs, the nested root
	// never gets the isRoot frame that makes its children topmost, and pop()
	// then treats it as an ordinary childless directory — superseding and
	// DISCARDING the genuinely empty folders inside it, while offering the
	// root itself as a candidate, which it must never be. Whether that
	// happened depended on nothing more than where the nested root sorted in
	// its parent's listing.
	if rootIdx != e.curRoot {
		for len(e.stack) > 0 {
			e.pop()
		}
		e.curRoot = rootIdx
	}
	e.popTo(f.path)
	if len(e.stack) == 0 {
		// First entry under a scan root: the root is this entry's parent.
		// A root is never a candidate itself, and its children are always
		// topmost — that is what isRoot says.
		e.stack = append(e.stack, efFrame{path: filepath.Dir(f.path), isRoot: true})
	}
	if f.isDir {
		e.stack = append(e.stack, efFrame{path: f.path, size: f.size, mod: f.mod})
		return
	}
	// A junk file does not make its folder non-empty (maintainer directive,
	// 2026-08-10): a folder holding nothing but the Temporary Files tool's
	// own junk — .DS_Store, Thumbs.db, desktop.ini, editor droppings — is as
	// disposable as a bare one, and the junk simply rides along when the
	// folder is moved. The definition is shared with that tool on purpose:
	// one list decides what "useless" means everywhere.
	//
	// Deliberately NAMES only, never sizes: a zero-byte file is not junk
	// here. A .gitkeep exists precisely to keep its folder non-empty, and a
	// zero-byte placeholder someone created is theirs — the Empty Files tool
	// lists those individually instead.
	if isTempName(f.name) {
		return
	}
	e.markHasFile(&e.stack[len(e.stack)-1])
}

// noteUnreadable marks the directory the walk could not read — and with it
// every ancestor, once they pop — as holding content. Unknown contents must
// never read as empty.
func (e *emptyFolderScan) noteUnreadable(p string) {
	e.popTo(p)
	if len(e.stack) == 0 {
		return // a scan root the walk never entered: it has no children here
	}
	e.markHasFile(&e.stack[len(e.stack)-1])
}

// finish closes the remaining frames, then confirms the candidates through
// File Station. Confirmation happens only for the capped set, so the round
// trips are bounded along with the result.
func (e *emptyFolderScan) finish(s *Server, cancel chan struct{}, confirm func(string) (bool, error)) ([]FileEnt, *TruncInfo, []string) {
	for len(e.stack) > 0 {
		e.pop()
	}
	cands, trunc := e.top.final()
	if e.dropped > 0 {
		if trunc == nil {
			trunc = &TruncInfo{Cap: emptyFolderCap}
		}
		trunc.Files += e.dropped
	}
	out := make([]FileEnt, 0, len(cands))
	failed, firstErr := 0, ""
	for i, c := range cands {
		// One File Station round trip per candidate, up to the cap: this
		// loop has to answer Stop and move the bar, or twenty thousand
		// sequential calls read as a hung scan that cannot be stopped.
		if cancelled(cancel) {
			break // runScan discards the result; nothing left to confirm
		}
		if i%25 == 0 {
			s.setProgress(15+81*float64(i)/float64(max(len(cands), 1)),
				"Confirming empty folders… ("+humanCount(i)+" of "+humanCount(len(cands))+")")
		}
		// Hard confirmation: re-check the directory itself. Any entry at all
		// (hidden, system) means it is not safely empty. This also closes the
		// gap for entries whose Info() failed during the walk and shrinks the
		// scan→move staleness window.
		empty, err := confirm(c.path)
		if err != nil {
			// Not empty as far as this scan is concerned — the gate never
			// over-reports — but counted and reported: an expired session
			// mid-confirmation must not read as "700 empty folders" when the
			// truth is "700 confirmed, 3,300 never checked".
			failed++
			if firstErr == "" {
				firstErr = err.Error()
			}
			continue
		}
		if !empty {
			continue
		}
		fe := s.fileEnt(c)
		fe.IsDir = true
		fe.Ext = "DIR"
		out = append(out, fe)
	}
	var errs []string
	if failed > 0 {
		errs = append(errs, fmt.Sprintf("%d empty-folder candidates could not be confirmed through File Station and were left out (%s)", failed, firstErr))
	}
	return out, trunc, errs
}

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
	return FileEnt{
		ID: "f" + strconv.Itoa(id), Name: f.name, Dir: f.dir,
		Size: f.size, Mod: fmtTime(f.mod), ModUnix: modUnix, Created: fmtTime(f.created),
		Ext: extOf(f.name),
		// Set here rather than per tool: it is a property of the NAME, true
		// of any row any tool surfaces, so every one of them gets it right
		// without each scan case having to remember.
		NoMove: fsCannotAddress(f.name),
	}
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

// ----------------------------------------------------------------- export

// csvCell neutralizes spreadsheet formula injection: a cell whose first
// meaningful character is =, +, -, or @ would otherwise execute as a live
// formula when the admin opens the report in Excel/LibreOffice. Importers
// skip leading whitespace and control characters inconsistently, so the
// check skips them too — "\n=HYPERLINK(…)" and " \t-2+3" are as dangerous
// as their bare forms. The leading apostrophe is the spreadsheet
// convention for "literal text".
func csvCell(s string) string {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '=' || c == '+' || c == '-' || c == '@':
			return "'" + s
		case c == ' ' || c < 0x20:
			// leading whitespace/control: keep looking for a marker
		default:
			return s
		}
	}
	return s
}

// exportCSV renders the report and returns its default filename plus the
// CSV bytes; writing (and " (n)" collision naming) happens in the File
// Station upload the handler performs. Cells that carry filesystem-derived
// strings pass through csvCell.
func exportCSV(tool string, res *toolResult) (string, []byte, error) {
	name := map[string]string{
		"duplicates":      "duplicate_report.csv",
		"empty_files":     "empty_files_report.csv",
		"temp_files":      "temp_files_report.csv",
		"empty_folders":   "empty_folders_report.csv",
		"corrupted_files": "conflicting_files_report.csv",
	}[tool]
	if name == "" {
		return "", nil, fmt.Errorf("unknown tool")
	}
	// The report is assembled in memory and handed to File Station's upload
	// as one body. That is bounded by the stored-results caps — every tool
	// now caps its stored rows, so the worst case is dupFileCap rows of a few
	// hundred bytes — and pre-sizing keeps the buffer from doubling its way
	// up to that.
	var buf bytes.Buffer
	buf.Grow(1 << 20)
	w := csv.NewWriter(&buf)
	if tool == "corrupted_files" {
		// The verdict and the reason for it are the point of this report, and
		// the export runs over the whole stored result rather than the filtered
		// view — which is why both are stored on the row at scan time instead
		// of being derived when the grid draws.
		w.Write([]string{"Set", "Name", "Location", "Size (bytes)", "Modified", "Content Hash", "Status", "Evidence"})
		for gi, g := range res.Groups {
			for _, fe := range g.Files {
				w.Write([]string{strconv.Itoa(gi + 1), csvCell(fe.Name), csvCell(fe.Dir),
					strconv.FormatInt(fe.Size, 10), fe.Mod, fe.Hash,
					csvCell(verdictLabel(fe.Verdict)), csvCell(fe.Evidence)})
			}
		}
	} else if tool == "duplicates" {
		w.Write([]string{"Group", "Hash", "Name", "Location", "Size (bytes)", "Modified", "Created", "Captured"})
		for gi, g := range res.Groups {
			for _, fe := range g.Files {
				w.Write([]string{strconv.Itoa(gi + 1), g.Hash, csvCell(fe.Name), csvCell(fe.Dir),
					strconv.FormatInt(fe.Size, 10), fe.Mod, fe.Created, csvCell(fe.Captured)})
			}
		}
	} else {
		w.Write([]string{"Name", "Location", "Size (bytes)", "Modified", "Created", "Type"})
		for _, fe := range res.Files {
			w.Write([]string{csvCell(fe.Name), csvCell(fe.Dir), strconv.FormatInt(fe.Size, 10), fe.Mod, fe.Created, csvCell(fe.Ext)})
		}
	}
	w.Flush()
	return name, buf.Bytes(), w.Error()
}
