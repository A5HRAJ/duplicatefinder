// The move flow: POST /api/move, the vetting sets built once per request,
// the per-file step, the identity checks a file must still pass at move
// time, the verification halves, and the pruning of moved rows. Execution
// itself is File Station's (fsapi.go); this file decides whether a move may
// happen at all.
package main

import (
	"dupfinder/internal/dirhandle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MoveReq is the body of POST /api/move. Files are full paths. Tool names
// the result set the files came from and is required for every move: the
// allowlist and the identities the files must still match are that tool's
// alone, and in preserve mode it also selects — through moveFolderNames,
// never as a path fragment — the name of the one folder the batch moves into.
type MoveReq struct {
	Files    []string `json:"files"`
	Dest     string   `json:"dest"`
	Preserve bool     `json:"preserve"`
	Tool     string   `json:"tool"`
	// Verify asks for full content verification of every moved FILE: the
	// source is re-read and hashed before the move (and must still match the
	// scan's recorded hash where one exists), and the destination is read
	// back and hashed after it. The dialog sends it explicitly (checked by
	// default); absent means false, so a caller that does not know the field
	// gets an unverified move. Directories are moved without content
	// verification — their move is a single rename and their contents are the
	// junk the tools exist to shed.
	Verify bool `json:"verify"`
}

func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) {
	req, folderName, ok := decodeMoveRequest(w, r)
	if !ok {
		return
	}
	// A move is vetted against the STORED results, and a scan in flight is
	// about to replace them: acting on the outgoing set while the incoming
	// one is being built means moving files on the strength of a view the
	// user is no longer looking at. Wait for it.
	//
	// This is a CHEAP REJECTION ONLY — it is not the guard. It fails fast
	// before the destination round trips below, but the authoritative check
	// is beginMove, after moveMu is held. Do not be tempted to treat this as
	// sufficient: mu is dropped on the next line.
	s.mu.Lock()
	scanning := s.job.Running
	s.mu.Unlock()
	if scanning {
		writeErr(w, 409, "a scan is running — wait for it to finish before moving files")
		return
	}
	destH, destCanon, destShare, sess, ok := s.vetDestination(w, r, req.Dest)
	if !ok {
		return
	}
	defer destH.Close()

	// One move request at a time: the keep-one vetting below is only sound
	// when vet → execute → prune runs as an uninterrupted critical section,
	// so each request is checked against the state the previous one left
	// behind. A second request is REFUSED rather than queued: a queued
	// request published no progress of its own while it waited, so its
	// caller's dialog showed the first move's counts and file names as its
	// own for as long as that took.
	if !s.moveMu.TryLock() {
		writeErr(w, 409, "a move is already running — wait for it to finish")
		return
	}
	defer s.moveMu.Unlock()

	// THE guard. The pre-lock check above was taken before a File Station
	// round trip, and a second move queued here waits for however long the
	// first one runs — a scan can start in either window, and nothing re-read
	// the flag afterwards. beginMove re-reads it and claims the slot in one
	// mu section; two separate acquisitions would rebuild the same TOCTOU.
	//
	// endMove is deferred here rather than at the end so it covers every exit
	// path, and because it is registered after moveMu's Unlock it runs first —
	// the claim is always dropped while moveMu is still held.
	if !s.beginMove() {
		writeErr(w, 409, "a scan is running — wait for it to finish before moving files")
		return
	}
	defer s.endMove()

	v := s.newMoveVet(req, sess)
	// A reference folder is read-only in both directions: nothing leaves it,
	// and nothing is moved INTO it either. A move that fills a master library
	// with "Duplicates" folders modifies exactly the tree the user asked to
	// have left alone.
	if isUnder(destCanon, v.canonRefs) {
		writeErr(w, 400, "the destination is inside a read-only reference folder")
		return
	}

	// In preserve mode the whole batch moves into ONE new folder at the
	// destination (batchFolder). It is allocated here, inside moveMu, not
	// earlier: probing and creating before the lock would let two overlapping
	// requests both see the name free and end up sharing a folder. moveMu
	// only covers this daemon, so allocBatchFolder still re-probes against
	// outside writers.
	var batch *batchFolder
	var batchFn func() (string, error)
	if req.Preserve {
		batch = &batchFolder{sess: sess, destShare: destShare, base: folderName}
		batchFn = batch.share
	}

	// Publish per-file progress for /api/state. Set before the first file so a
	// poll that lands early sees 0-of-N rather than "no move running", and
	// cleared on every exit path — including a panic, which would otherwise
	// leave the UI showing a move that is no longer happening.
	s.setMoveProgress(true, 0, len(req.Files), "")
	defer s.setMoveProgress(false, 0, 0, "")

	moved := []string{}
	var movedCanon, movedDirs []string
	errs := []map[string]string{}
	for i, src := range req.Files {
		src = filepath.Clean(src)
		// Announce the file BEFORE working on it: the whole point is to name
		// what is currently taking the time, and a large cross-volume copy
		// sits on this iteration for minutes. note lets the per-file step say
		// when the time is going into verification rather than the move.
		base := filepath.Base(src)
		note := func(stage string) {
			s.setMoveProgress(true, i, len(req.Files), base+stage)
		}
		note("")
		cp, gone, err := s.moveOne(v, batchFn, sess, destShare, destCanon, req.Verify, note, src)
		if gone {
			movedCanon = append(movedCanon, cp)
			// A moved DIRECTORY takes everything under it (a junk-only "empty"
			// folder moves with its junk inside), so pruneMoved must also drop
			// rows beneath it. The scan's own record says what moved: any ident
			// for this path being a directory is enough — a path that was a dir
			// to one tool and a file to another has changed since some scan, and
			// the identity check would have refused the move. BOTH forms, and the
			// display path is not redundant: once the directory is gone,
			// dirResolver.dir() can no longer resolve it, so every row beneath it
			// falls back to its raw stored path (that fallback is deliberate — see
			// dir()). Matching only the canonical form then silently prunes
			// nothing wherever the two differ: a symlinked share, or a scope
			// naming an alias of a real directory.
			if isDirIdent(v.idents[cp]) {
				movedDirs = append(movedDirs, cp, src)
			}
		}
		if err != nil {
			errs = append(errs, map[string]string{"path": src, "error": err.Error()})
			if gone {
				// A failure AFTER File Station completed the move — a
				// verification mismatch, or an entry parked in the staging
				// folder — leaves the source gone: the row prunes like any moved
				// row (above), or it would linger pointing at a path that no
				// longer exists and count as a phantom keep-one survivor. It
				// stays out of `moved` — the response's error entry is the truth
				// about this entry — and the daemon log is the durable record of
				// a move that left the source but did not end clean.
				log.Printf("move of %s: %v", src, err)
			}
			continue
		}
		moved = append(moved, src)
	}
	// Every file accounted for — report the full count before the response is
	// written, so a poll racing the last file cannot show N-1 of N and then
	// jump straight to "no move running".
	s.setMoveProgress(true, len(req.Files), len(req.Files), "")

	if len(movedCanon) > 0 {
		s.pruneMoved(movedCanon, movedDirs, v.canon)
		// Pruned rows and new keep-one survivors must survive a restart too,
		// or a reboot would resurrect moved rows and forget protections.
		if err := s.saveState(); err != nil {
			s.noteSaveError(err)
		}
	}
	out := map[string]any{"moved": moved, "errors": errs}
	if batch != nil && batch.name != "" {
		// Name the folder the files actually landed in: under preserve they
		// are NOT at the path the caller picked, and only the daemon knows
		// which " (n)" variant it ended up allocating. Reported in the same
		// namespace as the request's own paths. Absent when nothing was
		// created — an all-refused request or a failed allocation.
		out["folder"] = filepath.Join(req.Dest, batch.name)
	}
	writeJSON(w, 200, out)
}

// decodeMoveRequest reads and vets the body of POST /api/move as far as it
// can before anything is created and before any lock is held: the method, the
// body, the tool, and — for preserve mode — the folder name the tool maps to.
// It writes the refusal itself and reports false.
func decodeMoveRequest(w http.ResponseWriter, r *http.Request) (req MoveReq, folderName string, ok bool) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST required")
		return req, "", false
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad request body")
		return req, "", false
	}
	if len(req.Files) == 0 {
		writeErr(w, 400, "no files to move")
		return req, "", false
	}
	// A read-only tool's rows are a report, not a work list. Refused here, at
	// the trust boundary, so hiding the button in the UI is presentation
	// rather than the whole enforcement.
	if readOnlyTools[req.Tool] {
		writeErr(w, 400, "Conflicting Files is a report — move these files from File Station once you have decided which copy to keep")
		return req, "", false
	}
	// Every move names the result set its files come from. The allowlist and
	// the identities a file must still match are built from that tool's rows
	// alone, so a row another tool listed — possibly with a newer identity,
	// after the file was rewritten — cannot stand in for it, and the
	// preserve-mode folder is that tool's by construction. Resolved before
	// anything can be created: the destination is not open yet, no DSM
	// session is needed and moveMu is not held, so an unrecognised tool cannot
	// leave a half-built state or an orphaned folder behind. An upgraded
	// daemon serving a browser that still has the previous build's JS cached
	// is the likely source of a miss, so say so.
	var found bool
	if folderName, found = moveFolderNames[req.Tool]; !found {
		writeErr(w, 400, "unknown tool — reload Duplicate Finder, the package was upgraded")
		return req, "", false
	}
	return req, folderName, true
}

// moveVet is everything one move request is checked against, computed once
// under the move lock: the reference folders, the allowlist of paths some scan
// surfaced and the identities it recorded for them, the keep-one survivors of
// earlier requests, the copies this request must hold back, and the volume
// walls. Every path in it is canonical (symlink-resolved): a client could
// otherwise alias a protected reference file or a duplicate-group member
// through a symlink and slip past the string-prefix guards. Responses still
// carry the paths as the client sent them.
type moveVet struct {
	tool      string // the result set this request acts on
	canon     *dirResolver
	canonRefs []string
	allowed   map[string]bool
	idents    map[string][]entIdent
	keepers   map[string]bool
	drops     map[string]bool
	vols      []string
}

// newMoveVet snapshots the stored results and derives the vetting sets.
func (s *Server) newMoveVet(req MoveReq, sess *fsSession) *moveVet {
	refDirs, groups, keepers, cachedEnts := func() ([]string, []Group, map[string]bool, []FileEnt) {
		s.mu.Lock()
		defer s.mu.Unlock()
		k := make(map[string]bool, len(s.keepers))
		for p := range s.keepers {
			k[p] = true
		}
		// Every entry of the REQUESTED tool's result, and no other: the
		// daemon moves only what a scan surfaced, so these become the move
		// allowlist — and their size/mod/type become the identity the file
		// must still have. Another tool's row for the same path is no
		// substitute: recorded later, after the file was rewritten, it would
		// vouch for content the requested tool never saw, and a duplicates
		// row would lose its prefix re-read to a temp-files row that has none.
		// (decodeMoveRequest has already refused the read-only tools, so a
		// path that appears only in a corrupted set is never movable.)
		var ents []FileEnt
		if res := s.results[req.Tool]; res != nil {
			ents = append(ents, res.Files...)
			for gi := range res.Groups {
				ents = append(ents, res.Groups[gi].Files...)
			}
		}
		return append([]string{}, s.refDirs...), snapshotGroupsLocked(s.results["duplicates"]), k, ents
	}()

	canon := newDirResolver()
	canonRefs := make([]string, 0, len(refDirs))
	for _, d := range refDirs {
		canonRefs = append(canonRefs, canon.dir(d))
	}

	// The move allowlist, canonically keyed: a request may name any string,
	// but only paths this tool's scan actually surfaced are movable — the raw
	// API must not double as a generic file mover for anything in the
	// volumes. idents carries what that scan saw at each path (more than one
	// view when aliases of one directory were both in scope); the move later
	// refuses a file whose current File Station identity matches none of them.
	allowed := make(map[string]bool, len(cachedEnts))
	idents := make(map[string][]entIdent, len(cachedEnts))
	for _, f := range cachedEnts {
		cp := canon.path(filepath.Join(f.Dir, f.Name))
		allowed[cp] = true
		idents[cp] = append(idents[cp], identOf(f))
	}

	// The UI never submits every file of a duplicate group, but the daemon
	// must not trust any client: hold back one file per group that would
	// otherwise lose its last copy. Files whose parent cannot be resolved
	// stay out of the requested set — they cannot move, so they survive.
	requested := map[string]bool{}
	var reqDirs []string
	for _, f := range req.Files {
		if cp, err := canon.strictPath(f); err == nil {
			requested[cp] = true
			for _, id := range idents[cp] {
				if id.isDir {
					reqDirs = append(reqDirs, cp)
					break
				}
			}
		}
	}
	// A moved DIRECTORY takes every duplicate-group member beneath it, so
	// those members are requested too as far as keep-one is concerned — or
	// two junk-only folders holding the same Thumbs.db could be moved one
	// after the other and drain the group in place. The directory itself is
	// then refused (refuse) when a held-back copy lives inside it.
	if len(reqDirs) > 0 {
		for _, g := range groups {
			for _, f := range g.Files {
				if p := canon.path(filepath.Join(f.Dir, f.Name)); isUnder(p, reqDirs) {
					requested[p] = true
				}
			}
		}
	}
	// The scan snapshot may be stale: a cached group member that vanished
	// since the scan must not count as a surviving copy — and a member that
	// no longer has the scan's identity is not a surviving copy of the
	// group's CONTENT either — so File Station is asked which members of
	// the touched groups still exist as recorded.
	exists := groupExistence(sess, groups, requested, canon)
	return &moveVet{
		tool:      req.Tool,
		canon:     canon,
		canonRefs: canonRefs,
		allowed:   allowed,
		idents:    idents,
		keepers:   keepers,
		drops:     keepOneDrops(groups, requested, canonRefs, canon, exists),
		vols:      volumeRootsResolved(),
	}
}

// holdsUnder reports whether any held-back path lies inside dir.
func holdsUnder(dir string, held map[string]bool) bool {
	for p := range held {
		if strings.HasPrefix(p, dir+"/") {
			return true
		}
	}
	return false
}

// refuse returns why the canonical path cp may not move at all, or nil when
// the request-level checks pass and execMoveFS's own identity checks decide.
func (v *moveVet) refuse(cp string) error {
	switch {
	case !isUnder(cp, v.vols):
		return errors.New("outside allowed volumes")
	// drops: this request would take a group's last unrequested copy.
	// keepers: an earlier request already dissolved the group — its
	// survivor stays held back until the next duplicates scan.
	case v.drops[cp] || v.keepers[cp]:
		return errors.New("keeping one copy of this duplicate group")
	case isDirIdent(v.idents[cp]) && (holdsUnder(cp, v.drops) || holdsUnder(cp, v.keepers)):
		return errors.New("keeping one copy of a duplicate group that lives inside this folder")
	case isUnder(cp, v.canonRefs):
		return errors.New("read-only reference file")
	// Refused BEFORE File Station is asked, because asking produces "no
	// such file or folder" for a file that plainly exists — a refusal
	// that sends the user looking for a vanished file instead of telling
	// them what is actually true. Checked on the base name of the path
	// being moved, matching what the scan recorded in NoMove.
	case fsCannotAddress(filepath.Base(cp)):
		return errors.New("DSM's File Station cannot see this file, so it cannot be moved — delete it from the Mac that made it, or move the whole folder that holds it")
	case !v.allowed[cp]:
		return errors.New("not part of the current scan results — rescan and try again")
	}
	return nil
}

// batchFolder is the preserve-mode tool folder, allocated LAZILY and at most
// ONCE per request.
//
// Lazily, because every per-file guard — the allowlist, keep-one, reference
// folders, and execMoveFS's own identity and fingerprint checks — refuses
// before anything is written, and a request whose files are all refused must
// not leave an empty "Duplicates" behind.
//
// Once, because execMoveFS runs per file: allocating there would produce
// Duplicates, Duplicates (1), Duplicates (2)… one per file, which is the exact
// scatter this folder exists to prevent. The error is memoized alongside the
// name, so a destination that refuses folder creation costs one failed attempt
// for the request rather than one per file.
type batchFolder struct {
	sess      *fsSession
	destShare string
	base      string
	tried     bool
	name      string
	err       error
}

// share returns the folder's share-space path, creating it on the first call.
func (b *batchFolder) share() (string, error) {
	if !b.tried {
		b.tried = true
		b.name, b.err = b.sess.allocBatchFolder(b.destShare, b.base)
	}
	if b.err != nil {
		return "", b.err
	}
	return b.destShare + "/" + b.name, nil
}

// moveOne vets and executes one requested path. It pins the source's parent,
// derives the canonical path that every check and the move itself act on,
// applies the request-level refusals and hands the rest to execMoveFS. gone
// reports that the source left its place — a clean move, or a failure after
// File Station had already moved it — so the caller prunes the row either way.
func (s *Server) moveOne(v *moveVet, batch func() (string, error), sess *fsSession, destShare, destCanon string, verify bool, note func(string), src string) (cp string, gone bool, err error) {
	if !allowedPath(src) {
		return "", false, errors.New("outside allowed volumes")
	}
	// Pin the source's parent (src itself may be a symlink; the move takes
	// the link, never what it points at) and derive the canonical path from
	// the pinned handle: the guards below AND the File Station move all act
	// on that one canonical path, so the vetted path and the executed path
	// are never two different strings, and a parent swapped for a symlink
	// after the checks cannot redirect the operation to files living
	// elsewhere. (File Station is an external process, so the handle itself
	// cannot carry the move — acting on the handle's canonical path is the
	// strongest available guarantee.)
	parentH, err := dirhandle.Open(filepath.Dir(src))
	if err != nil {
		return "", false, errors.New("cannot resolve path")
	}
	defer parentH.Close()
	pCanon, err := parentH.Canon()
	if err != nil {
		return "", false, errors.New("cannot resolve path")
	}
	cp = filepath.Join(pCanon, filepath.Base(src))
	if err := v.refuse(cp); err != nil {
		return cp, false, err
	}
	// An empty-folder row is a claim about the folder's CONTENTS, and the
	// identity check in execMoveFS sees only its type and modified time. Ask
	// again, now, whether it still holds nothing but junk: content that
	// arrived since the scan must not ride along into the cleanup folder.
	if v.tool == "empty_folders" && isDirIdent(v.idents[cp]) {
		empty, cerr := confirmEmpty(cp, sess)
		if cerr != nil {
			return cp, false, errors.New("could not confirm the folder is still empty — " + cerr.Error())
		}
		if !empty {
			return cp, false, errors.New("the folder is no longer empty — rescan and try again")
		}
	}
	err = execMoveFS(sess, src, cp, destShare, destCanon, v.idents[cp], batch, verify, note)
	var mbe movedButError
	gone = err == nil || errors.As(err, &mbe)
	return cp, gone, err
}

// setMoveProgress publishes how far the move in flight has got. It takes mu
// only — never moveMu, which handleMove holds for its whole run — so
// /api/state stays answerable throughout a move that may last minutes.
func (s *Server) setMoveProgress(running bool, done, total int, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.move = moveState{Running: running, Done: done, Total: total, Name: name}
}

// entIdent is what a scan recorded about a result entry: enough identity
// to notice, at move time, that the file at that path is no longer the one
// the scan saw.
type entIdent struct {
	size int64
	mod  string // fmtTime form, matching FileEnt.Mod
	// modUnix is the same instant as an epoch value; 0 for rows an older
	// build persisted, which fall back to the string form.
	modUnix int64
	isDir   bool
	pfx     string // content fingerprint; "" when the scan recorded none
	// hash is the scan's full-content hash; "" when the scan recorded none
	// (only duplicates rows carry one). When verification is on, the source
	// must still hash to exactly this before it is moved.
	hash string
}

// identOf is the identity a cached row carries into the move checks.
func identOf(f FileEnt) entIdent {
	return entIdent{size: f.Size, mod: f.Mod, modUnix: f.ModUnix, isDir: f.IsDir, pfx: f.Pfx, hash: f.Hash}
}

// isDirIdent reports whether any scan recorded this path as a directory.
func isDirIdent(ids []entIdent) bool {
	for _, id := range ids {
		if id.isDir {
			return true
		}
	}
	return false
}

// identMatches reports whether File Station's current view of a file agrees
// with any identity a scan recorded for its path. Directories compare type
// and mtime only (their reported size is not meaningful — but any content
// change bumps the mtime, which is exactly what must block moving a
// "confirmed empty" folder that gained content).
func identMatches(e fsEntry, wants []entIdent) (entIdent, bool) {
	mod := fmtTime(time.Unix(e.Additional.Time.Mtime, 0))
	// Prefer the strongest identity available — a full-content hash first,
	// then a prefix fingerprint — so the caller gets to make the strongest
	// check any scan recorded.
	var best entIdent
	found := false
	for _, w := range wants {
		if e.IsDir != w.isDir {
			continue
		}
		// The modification time compares as an epoch value where the scan
		// recorded one. The string form is a zone-less local time: a NAS
		// whose time zone changed between the scan and the move would
		// otherwise refuse every file as "changed since the scan".
		if w.modUnix != 0 {
			if e.Additional.Time.Mtime != w.modUnix {
				continue
			}
		} else if mod != w.mod {
			continue
		}
		if !e.IsDir && e.Additional.Size != w.size {
			continue
		}
		if w.hash != "" {
			return w, true
		}
		if !found || (best.pfx == "" && w.pfx != "") {
			best, found = w, true
		}
	}
	return best, found
}

// execMoveFS maps one vetted move into share space, applies the remaining
// structural checks (identity, self-containment, already-in-place), and
// delegates execution to File Station. The share-space source is derived
// from cp — the canonical path the guards vetted — never from the raw
// request path, so File Station is always asked to move exactly what was
// checked; and the file's current size/mtime/type must still match what
// the scan recorded, so a file replaced or modified since the scan is
// refused rather than moved as something it no longer is. When batch is
// non-nil (preserve mode) it yields the share-space path of the one folder
// this request moves into — created on its first call, so a request that
// refuses every file creates nothing — and the source's directory chain (as
// the user knows it, so the raw src) is mirrored INSIDE that folder via
// CreateFolder(force_parent), so the file's origin is always recorded. One
// folder per batch is what stops a batch merging into an earlier one's tree,
// and it is why moved files always keep their original names.
//
// Two known consequences of that folder, both accepted deliberately: a move
// that fails after the folder was allocated leaves it behind empty (removing
// it would race with concurrent writers, and File Station's Delete is
// recursive, so a compensating cleanup could destroy a file parked mid-
// staging); and creating it bumps the destination folder's mtime, so a
// destination that some scan itself recorded is afterwards refused by the
// identity check below — conservative, and correct.
// movedButError marks a failure that happened AFTER File Station completed
// the move: the file is at the destination, the source is gone, so the row
// must still prune — only the report differs. errors.As is the test.
type movedButError struct{ msg string }

func (e movedButError) Error() string { return e.msg }

// verifyRead hashes a file at an absolute filesystem path. Plain os.Open,
// matching contentPrefixUnchanged: the paths handed in are either the
// canonical vetted source or a destination chain this very request created
// under the picker's canonical folder.
func verifyRead(p string) (string, error) {
	return hashFile(func() (*os.File, error) { return os.Open(p) }, -1, nil)
}

// note lets the per-file progress line say when the time is going into
// verification rather than the move itself; nil means no verification.
func execMoveFS(sess *fsSession, src, cp, destShare, destCanon string, wants []entIdent, batch func() (string, error), verify bool, note func(string)) error {
	srcShare, err := sess.shareSpacePath(cp)
	if err != nil {
		return errors.New("outside allowed volumes")
	}
	info, err := sess.getInfo([]string{srcShare}, []string{"size", "time"})
	if err != nil {
		var fe *fsError
		if errors.As(err, &fe) && fe.hasCode(408) {
			return errors.New("no such file or folder")
		}
		return err
	}
	e, ok := info[srcShare]
	if !ok || !e.exists() {
		return errors.New("no such file or folder")
	}
	want, ok2 := identMatches(e, wants)
	if !ok2 {
		return errors.New("file changed since the scan — rescan and try again")
	}
	// File Station has confirmed the type, size and modification time still
	// agree with the scan. That still leaves a file rewritten in place with
	// content of the same length and its mtime put back — a restored older
	// version, or a deliberate swap — which for a DUPLICATES result would
	// move a file that is no longer the duplicate the user was shown. Where
	// the scan recorded a content fingerprint, re-read the first 64 KiB and
	// insist it still matches. Any error is a refusal: this gate exists to
	// say no.
	if want.pfx != "" && !contentPrefixUnchanged(cp, want.pfx) {
		return errors.New("file contents changed since the scan — rescan and try again")
	}
	if e.IsDir && (destCanon == cp || strings.HasPrefix(destCanon+"/", cp+"/")) {
		return errors.New("destination is inside the folder being moved")
	}
	// Hoisted above the allocation below, so that no purely LOCAL refusal can
	// happen after something has been created at the destination. The check
	// is scoped to plain moves on purpose: a preserve batch folder is brand
	// new, so a file can never already be inside it, and a file sitting in an
	// EARLIER batch's mirrored tree now moves into the new batch rather than
	// being refused — which is what "batches never merge" means.
	if batch == nil && filepath.Dir(srcShare) == destShare {
		return errors.New("file is already in the destination")
	}
	// Directories are never content-verified: their move is a rename, and
	// there is no single content to hash. Placed above batch() with every
	// other refusal, so a verify refusal creates nothing.
	preHash := ""
	if verify && !e.IsDir {
		if preHash, err = preMoveHash(cp, want, note); err != nil {
			return err
		}
	}
	outShare := destShare
	if batch != nil {
		if outShare, err = batchTarget(sess, batch, src); err != nil {
			return err
		}
	}
	finalName, err := moveViaFS(sess, srcShare, outShare, want)
	if err != nil {
		var pe parkedError
		if errors.As(err, &pe) {
			return parkedOutcome(pe, destShare, destCanon, preHash)
		}
		return err
	}
	if verify && !e.IsDir {
		return verifyMoved(destCanon, destShare, outShare, finalName, preHash, note)
	}
	return nil
}

// preMoveHash is verification's first half: prove the SOURCE is still exactly
// the content the scan verified, while it still exists. Where the scan
// recorded a full hash (duplicates), the fresh hash must equal it — this
// closes the scan→move window the 64 KiB fingerprint cannot (rot past the
// prefix with size and mtime standing). For rows without a recorded hash, the
// fresh hash becomes the reference the destination must reproduce.
func preMoveHash(cp string, want entIdent, note func(string)) (string, error) {
	if note != nil {
		note(" — verifying")
	}
	h, err := verifyRead(cp)
	if err != nil {
		return "", errors.New("could not read the file to verify it — nothing was moved")
	}
	if want.hash != "" && h != want.hash {
		return "", errors.New("file contents changed since the scan — rescan and try again")
	}
	if note != nil {
		note("")
	}
	return h, nil
}

// batchTarget allocates the batch folder (its first side effect, and
// deliberately the last step before the move: every refusal above it is
// local) and mirrors the source's directory chain — as the user knows it, so
// the raw src — inside it via CreateFolder(force_parent), so the file's origin
// is always recorded. It returns the share-space folder the file moves into.
func batchTarget(sess *fsSession, batch func() (string, error), src string) (string, error) {
	root, err := batch()
	if err != nil {
		return "", err
	}
	rel := strings.TrimPrefix(filepath.Dir(src), "/")
	if rel == "" {
		// Unreachable while shareSpacePath refuses volume roots, but a bare
		// join would silently aim the move at the batch folder's own parent
		// — refuse rather than depend on that.
		return "", errors.New("cannot mirror the folder path of a volume root")
	}
	outShare := filepath.Join(root, rel)
	if err := sess.createFolder(filepath.Dir(outShare), filepath.Base(outShare), true); err != nil {
		return "", err
	}
	return outShare, nil
}

// parkedOutcome reports a move that ended with the file inside the
// destination's staging folder. The source is gone and the only copy sits in
// the staging folder — the exact "moved but" condition: the row must prune,
// the message must carry where the file is, and when verification was asked
// for (preHash is set) it happens HERE, against the parked copy. The risky
// transit has already happened; "parked" must never mean "unverified".
func parkedOutcome(pe parkedError, destShare, destCanon, preHash string) movedButError {
	msg := pe.Error()
	if preHash != "" {
		parkedFS := filepath.Join(destCanon, strings.TrimPrefix(pe.tmpShare, destShare), pe.name)
		if h, herr := verifyRead(parkedFS); herr != nil {
			msg += "; the parked copy could not be read back to verify it"
		} else if h != preHash {
			msg += "; the parked copy does NOT match the original content — check it before deleting any other copy"
		} else {
			msg += "; the parked copy verified intact"
		}
	}
	return movedButError{msg}
}

// verifyMoved is verification's second half: read the DESTINATION back and
// require the exact content that left. Within one volume a move is a rename
// of pointers and this re-reads the same blocks; across volumes or onto a
// remote mount it is the only proof the data survived the transit. The
// destination path is derived, never guessed: outShare always extends
// destShare, so grafting that extension onto the picker's canonical folder
// plus the name moveViaFS reports (a collision may have forced a " (n)"
// variant) is exactly where File Station put the file.
//
// Both failure modes happen AFTER the move — the source is gone — so they are
// reported as movedButError: the row prunes, the message carries the truth.
// "Could not be read back" is kept distinct from "does not match": the first
// is almost always the package user lacking read access to the destination
// share, and telling the user their data was damaged when the daemon merely
// could not look would be a false alarm with real consequences.
func verifyMoved(destCanon, destShare, outShare, finalName, preHash string, note func(string)) error {
	if note != nil {
		note(" — verifying the moved copy")
	}
	destFS := filepath.Join(destCanon, strings.TrimPrefix(outShare, destShare), finalName)
	h, herr := verifyRead(destFS)
	if herr != nil {
		return movedButError{"moved, but the copy could not be read back to verify it — grant the package user read access to the destination share, or compare the copies yourself before deleting anything"}
	}
	if h != preHash {
		return movedButError{"moved, but the copy at the destination does not match the original content — the data may have been damaged in transit; check it before deleting any other copy"}
	}
	return nil
}

// pruneMoved removes moved paths from every cached result set. canonPaths
// are canonical (symlink-resolved), and cached paths are canonicalized the
// same way, so a move requested through an alias still evicts the matching
// cached entry — a stale entry would later count as a phantom keep-one
// survivor.
//
// dirPaths names the moved entries that were DIRECTORIES: everything under
// one moved with it, so rows beneath those prune too. This exists for the
// junk-only "empty" folder: it moves with its junk inside, and that junk may
// hold temp_files rows — left in place they would point at paths that no
// longer exist and fail any later move with a misleading refusal. (A truly
// empty folder can contain no rows, which is why file-only moves never
// needed this.)
func (s *Server) pruneMoved(canonPaths, dirPaths []string, canon *dirResolver) {
	gone := map[string]bool{}
	names := map[string]bool{} // base names of the moved entries
	for _, p := range canonPaths {
		gone[p] = true
		names[filepath.Base(p)] = true
	}
	// A row can only have moved if its NAME is one of the moved entries' — or,
	// when a directory moved, if it lies beneath it. Rows failing both never
	// need canonicalizing, which is the whole cost here: the resolver is
	// handleMove's, already warm for every cached directory, and the loop
	// runs under s.mu, where a syscall per row stalled /api/state for the
	// duration of a 100k-row prune.
	candidate := func(f *FileEnt) bool {
		return names[f.Name] || len(dirPaths) > 0
	}
	// raw is the row's stored path, cp its canonicalized form. Both are tested
	// against dirPaths because a row under a JUST-MOVED directory can no
	// longer be canonicalized — its parent is gone, so canon.path() falls
	// back to the raw path — while dirPaths carries the resolved form the
	// move executed against. Testing one alone misses whenever the two
	// differ, and handleMove supplies both forms for the same reason.
	dropped := func(cp, raw string) bool {
		if gone[cp] || gone[raw] {
			return true
		}
		return len(dirPaths) > 0 && (isUnder(cp, dirPaths) || isUnder(raw, dirPaths))
	}
	if canon == nil {
		canon = newDirResolver()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidateDerivedLocked() // cached view/date span describe rows that may just have moved
	for tool, res := range s.results {
		if groupedTools[tool] {
			out := res.Groups[:0]
			for _, g := range res.Groups {
				files := g.Files[:0]
				for _, f := range g.Files {
					raw := filepath.Join(f.Dir, f.Name)
					if !candidate(&f) || !dropped(canon.path(raw), raw) {
						files = append(files, f)
					}
				}
				g.Files = files
				if len(g.Files) >= 2 {
					out = append(out, g)
				} else if len(g.Files) == 1 && tool == "duplicates" {
					// The group dissolves, but its content must keep one
					// in-place copy: remember the survivor so a later move
					// request cannot take it before the next duplicates
					// scan re-derives the groups.
					//
					// Deliberately duplicates-only. A corrupted set losing
					// members down to one is not a group holding back a last
					// copy — recording a keeper here would pin an arbitrary
					// file as unmovable from the DUPLICATES view, refused with
					// wording about a duplicate group it is no longer in, and
					// only a completed duplicates scan clears it.
					if s.keepers == nil {
						s.keepers = map[string]bool{}
					}
					s.keepers[canon.path(filepath.Join(g.Files[0].Dir, g.Files[0].Name))] = true
				}
			}
			res.Groups = out
		} else {
			out := res.Files[:0]
			for _, f := range res.Files {
				raw := filepath.Join(f.Dir, f.Name)
				if !candidate(&f) || !dropped(canon.path(raw), raw) {
					out = append(out, f)
				}
			}
			res.Files = out
		}
	}
}

// groupExistence answers, for every member of a duplicate group this
// request touches, whether the file still exists per File Station AS THE
// SCAN RECORDED IT. The scan snapshot may be stale — a member deleted or
// moved externally since the scan must not excuse moving the last remaining
// copy, and neither must a member rewritten in place: a survivor is a copy
// of the group's CONTENT, and a file that no longer has the scan's size and
// mtime is not one, whatever its name. Untouched groups are skipped (their
// keep-one outcome cannot change). On a File Station error the map stays
// empty, which fails in the safe direction: unknown existence counts as
// missing, so more is held back, never less.
func groupExistence(sess *fsSession, groups []Group, requested map[string]bool, canon *dirResolver) map[string]bool {
	byShare := map[string][]string{} // share path → canonical members
	want := map[string][]entIdent{}  // share path → identities the scan recorded
	var shares []string
	for _, g := range groups {
		touched := false
		for _, f := range g.Files {
			if requested[canon.path(filepath.Join(f.Dir, f.Name))] {
				touched = true
				break
			}
		}
		if !touched {
			continue
		}
		for _, f := range g.Files {
			cp := canon.path(filepath.Join(f.Dir, f.Name))
			if sp, err := sess.shareSpacePath(cp); err == nil {
				if len(byShare[sp]) == 0 {
					shares = append(shares, sp)
				}
				byShare[sp] = append(byShare[sp], cp)
				// The GROUP ROW's own identity, never another tool's record of
				// the same path: a member rewritten in place and then listed by
				// a later temp-files scan would otherwise pass as a surviving
				// copy of content it no longer holds.
				want[sp] = append(want[sp], identOf(f))
			}
		}
	}
	out := map[string]bool{}
	if len(shares) == 0 {
		return out
	}
	info, err := sess.getInfo(shares, []string{"size", "time"})
	if err != nil {
		return out
	}
	for sp, cps := range byShare {
		e, ok := info[sp]
		if !ok || !e.exists() {
			continue
		}
		if _, same := identMatches(e, want[sp]); !same {
			continue
		}
		for _, cp := range cps {
			out[cp] = true
		}
	}
	return out
}

// keepOneDrops returns, for each duplicate group whose files were all
// requested for moving (and none is a protected reference file, which cannot
// move and therefore always survives), the one path to hold back so a copy
// of the group's content always remains. All comparisons are canonical:
// requested and refDirs must already be resolved, and the cached group paths
// are resolved here — a symlink alias for a group member must not skew the
// survivor count. exists is File Station's view of which members remain on
// disk: an unrequested member only counts as a survivor if it still exists,
// and the held-back copy is chosen among existing members when possible.
// The returned drop keys are canonical too.
func keepOneDrops(groups []Group, requested map[string]bool, refDirs []string, canon *dirResolver, exists map[string]bool) map[string]bool {
	drops := map[string]bool{}
	for _, g := range groups {
		survivors := 0
		last, lastExisting := "", ""
		for _, f := range g.Files {
			p := canon.path(filepath.Join(f.Dir, f.Name))
			if !requested[p] || isUnder(p, refDirs) {
				if exists[p] {
					survivors++
				}
				continue
			}
			last = p
			if exists[p] {
				lastExisting = p
			}
		}
		if survivors == 0 {
			if lastExisting != "" {
				drops[lastExisting] = true
			} else if last != "" {
				drops[last] = true
			}
		}
	}
	return drops
}
