// The HTTP handlers for everything but moves and exports: the volume
// overview, daemon state, scan admission and cancellation, and the results
// endpoints (the paged form lives in results.go). vetDestination is shared
// by the move and export handlers.
package main

import (
	"dupfinder/internal/dirhandle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

type volInfo struct {
	Name   string     `json:"name"`
	Path   string     `json:"path"`
	Total  int64      `json:"totalBytes"`
	Used   int64      `json:"usedBytes"`
	Shares []shareRef `json:"shares"`
}

// shareRef pairs a share's File Station name with its real filesystem path.
// The two are NOT derivable from each other: internal shares happen to use
// their name as the directory (/volume1/Backups ↔ "Backups"), but external
// ones do not (share "usbshare1" lives at /volumeUSB1/usbshare). Every
// mapping between share space and volume space must go through a table of
// these pairs; building either side by concatenation makes USB destinations
// unusable.
type shareRef struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// handleInfo reports the volume/share overview from File Station's own
// shared-folder listing (list_share + volume_status). Without a DSM session
// (raw daemon calls) there is no picker data — scans still work with
// explicit paths.
func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	out := []volInfo{}
	warn := ""
	if sess, err := fsSessionFrom(r); err != nil {
		warn = err.Error()
	} else if v, err := sess.listShares(); err != nil {
		warn = err.Error() // session present but File Station failed — say so
	} else {
		out = v
	}
	resp := map[string]any{
		"version": appVersion, "hashAlgo": hashAlgoName, "volumes": out,
		// The account the daemon runs as. DSM names it after the package
		// (DuplicateFinder for the hand-built spk, sc-duplicatefinder for the
		// SynoCommunity build), and it is the account the user must grant
		// shared-folder access to — so the UI's permission hint asks the
		// daemon rather than guessing a name.
		"user": serviceUser(),
	}
	if warn != "" {
		// An empty picker must be distinguishable from a broken session:
		// the UI surfaces this instead of silently showing no volumes.
		resp["warning"] = warn
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	scanned := map[string]any{}
	for tool, res := range s.results {
		n := len(res.Files)
		g := 0
		if groupedTools[tool] {
			g = len(res.Groups)
			n = 0
			for _, gr := range res.Groups {
				n += len(gr.Files)
			}
		}
		scanned[tool] = map[string]any{"files": n, "groups": g, "scannedAt": res.Scanned}
	}
	out := map[string]any{
		"running": s.job.Running, "tool": s.job.Tool, "tools": s.job.Tools,
		"progress": s.job.Progress, "label": s.job.Label,
		// lastTool names the scan that finished (and stored results) most
		// recently — s.job is cleared on completion, so without it the UI's
		// completion poll could not tell which tool's results just arrived.
		"lastTool": s.lastTool,
		"scanned":  scanned, "refDirs": s.refDirs,
	}
	// How the most recent run ended, completed or not: a poller that sees
	// running flip to false needs this to tell Stop from a finish — lastTool
	// alone names the last scan that completed, which after a Stop is the
	// previous one, and replaying its completion is exactly wrong.
	if s.lastEnd.Tool != "" || len(s.lastEnd.Tools) > 0 {
		out["lastEnd"] = map[string]any{"tool": s.lastEnd.Tool, "tools": s.lastEnd.Tools, "completed": s.lastEnd.Completed}
	}
	// Only while a move is actually in flight: its absence is what tells the
	// UI's poller to stop.
	if s.move.Running {
		out["move"] = s.move
	}
	// A scan the previous daemon run never finished: the UI offers to resume
	// it or start over (see scanMarker).
	if s.interrupted != nil {
		out["interrupted"] = s.interrupted
	}
	// A state save that failed: the results on screen are real but will not
	// survive a restart, and the app says so.
	if s.saveErr != "" {
		out["saveError"] = s.saveErr
	}
	writeJSON(w, 200, out)
}

// ScanReq is the body of POST /api/scan.
type ScanReq struct {
	// Tool is the view the app is looking at, which the scan opens when it
	// ends; Tools names every tool the scan runs. A request naming only Tool
	// (an older client, the raw API) asks for that one tool.
	Tool    string    `json:"tool"`
	Tools   []string  `json:"tools"`
	Dirs    []string  `json:"dirs"`
	RefDirs []string  `json:"refDirs"`
	Recurse bool      `json:"recurse"`
	Match   MatchOpts `json:"match"`
	// Resume asks to CONTINUE the interrupted scan the marker records,
	// reusing the full-content hashes that run already computed. Honored
	// only while the interruption notice stands and only for a request
	// identical to the interrupted run's — same tool, scope, reference
	// folders, recursion and match criteria (scanMarker.matches); in every
	// other case the flag is ignored and the scan re-reads everything, so
	// no caller can talk a completed scan's hashes — or a DIFFERENT scan's
	// — back into service.
	Resume bool `json:"resume"`
}

// scanOrder is the order the passes run in when several tools are requested:
// the walk-only tools first, the content pass last, so the quick answers land
// while the slow one is still reading.
var scanOrder = []string{"empty_folders", "empty_files", "temp_files", "duplicates", "corrupted_files"}

// toolList returns the tools a request asks for, in scan order, validated and
// de-duplicated. A request naming only Tool asks for that one tool.
func (r *ScanReq) toolList() ([]string, error) {
	asked := r.Tools
	if len(asked) == 0 && r.Tool != "" {
		asked = []string{r.Tool}
	}
	want := map[string]bool{}
	for _, t := range asked {
		if !validTools[t] {
			return nil, fmt.Errorf("unknown tool %q", t)
		}
		want[t] = true
	}
	var out []string
	for _, t := range scanOrder {
		if want[t] {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("choose at least one tool to scan for")
	}
	return out, nil
}

// openTool is the view the app opens when the scan ends: the tool it was
// looking at when that tool was scanned, else the first tool scanned.
func (r *ScanReq) openTool(tools []string) string {
	for _, t := range tools {
		if t == r.Tool {
			return t
		}
	}
	if len(tools) == 0 {
		return r.Tool
	}
	return tools[0]
}

// moveFolderNames maps a tool id to the folder a preserve-mode move creates
// at the destination. The VALUES are compile-time literals and the client's
// string is only ever a KEY: a request may name any tool, but nothing it
// sends reaches a path component, and a miss is a 400 rather than a fallback
// to the requested string. None of the four names contains a dot, which is
// what makes firstFreeName's extension splitting enumerate them as
// "Empty Files (1)" rather than "Empty (1).Files".
var moveFolderNames = map[string]string{
	"duplicates": "Duplicates", "empty_folders": "Empty Folders",
	"empty_files": "Empty Files", "temp_files": "Temporary Files",
}

// readOnlyTools are the tools whose results the app reports but never acts on.
// corrupted_files is one: telling a damaged copy from an intact one is a
// judgement made from evidence, and evidence can be silent — moving files on
// the strength of a verdict the scan itself marks "unknown" is how the good
// copy gets destroyed. The list is a report; the user acts in File Station.
//
// This is enforced here rather than only in the UI because the daemon is the
// trust boundary — a raw API caller must be refused too.
var readOnlyTools = map[string]bool{
	"corrupted_files": true,
}

// groupedTools produce Groups rather than a flat Files list. Paging, totals,
// state counts and prune all branch on this rather than on a literal tool id.
var groupedTools = map[string]bool{
	"duplicates":      true,
	"corrupted_files": true,
}

// Every tool the API will answer for. Derived from the move-folder table plus
// the read-only list so the two cannot drift apart as tools are added: a tool
// that can be moved from needs a destination folder name, and one that cannot
// must never have one.
var validTools = func() map[string]bool {
	m := make(map[string]bool, len(moveFolderNames)+len(readOnlyTools))
	for k := range moveFolderNames {
		m[k] = true
	}
	for k := range readOnlyTools {
		m[k] = true
	}
	return m
}()

func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST required")
		return
	}
	var req ScanReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad request body")
		return
	}
	tools, err := req.toolList()
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	roots := uniquePaths(append(append([]string{}, req.Dirs...), req.RefDirs...))
	if len(roots) == 0 {
		writeErr(w, 400, "add at least one folder to the scope")
		return
	}
	// Containment vetting first (native by design — the canonical-path
	// exception): a scan root that is (or sits behind) a symlink pointing
	// outside the volumes must not be enumerated — the walk follows the
	// root even though it skips symlinks below it.
	canonRoots := make([]string, len(roots))
	for i, d := range roots {
		rp, err := resolveAllowedRoot(d)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		canonRoots[i] = rp
	}
	// Scans require the caller's DSM session, exactly like moves and
	// exports: the app is operated through the DSM UI only, and the session
	// is what lets File Station validate the roots and answer creation
	// times and the empty-folder confirmations. A raw daemon call (token
	// but no session) can read cached state and results, nothing more.
	sess, err := fsSessionFrom(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := validateRoots(roots, canonRoots, sess); err != nil {
		writeErr(w, 400, err.Error())
		return
	}

	s.mu.Lock()
	if why := s.scanAdmissionLocked(); why != "" {
		s.mu.Unlock()
		writeErr(w, 409, why)
		return
	}
	cancel := make(chan struct{})
	s.job = jobState{Running: true, Tool: req.openTool(tools), Tools: tools, Label: "Initializing scan…", cancel: cancel}
	// s.refDirs is NOT updated here. It describes the stored results, so it
	// changes when results do — at completion, in runScan's defer. Published
	// at admission, a cancelled scan (of any tool) would silently change
	// which files the OLD results' moves refuse.
	// Resume is decided here, under mu, against the notice this scan is
	// about to supersede: the request must MATCH the dead run's — same
	// tool, same normalized roots and reference folders, same recursion
	// and match criteria — or the flag is ignored and this scan re-reads
	// everything. The tool alone would let a rescoped "resume" adopt the
	// dead run's generation and be served reads that run made for a
	// different scan. The generation travels by value — the notice itself
	// is gone the moment this scan is admitted.
	var resumeGen uint32
	if req.Resume && s.interrupted != nil && s.interrupted.matches(&req) {
		resumeGen = s.interrupted.Gen
	}
	s.interrupted = nil // a new scan supersedes the interrupted-scan notice
	s.mu.Unlock()

	go s.runScan(req, roots, sess, cancel, resumeGen)
	writeJSON(w, 200, map[string]any{"started": true})
}

// validateRoots checks that every requested scan root is a folder, per File
// Station's getinfo asked about the canonical (vetted) root — the same view
// the folder picker showed the user, and the same canonical namespace the
// move flow acts in. Every root must live inside a shared folder: a bare
// volume root has no share-space address, so File Station cannot answer for
// it — and the UI's picker can never produce one — so it is refused
// outright rather than answered natively. roots and canonRoots are
// parallel: raw paths name folders back to the client, canonical paths are
// what gets asked about.
func validateRoots(roots, canonRoots []string, sess *fsSession) error {
	byShare := map[string]string{}
	shares := make([]string, 0, len(canonRoots))
	for i, cp := range canonRoots {
		sp, err := sess.shareSpacePath(cp)
		if err != nil {
			return errors.New("scan roots must be inside a shared folder: " + roots[i])
		}
		shares = append(shares, sp)
		byShare[sp] = roots[i]
	}
	info, err := sess.getInfo(shares, nil)
	if err != nil {
		return err
	}
	for _, sp := range shares {
		if e, ok := info[sp]; !ok || !e.exists() || !e.IsDir {
			return errors.New("not a folder: " + byShare[sp])
		}
	}
	return nil
}

// handleRefs replaces the read-only reference folders: POST {"refDirs":
// [...]}. The list is one shared setting for every tool and view — the
// padlocks, the reclaimable totals and the move's refusals all read
// s.refDirs — so a change takes effect at once, not at the next scan. Each
// folder is vetted like a scan root (containment, then File Station's word
// that it is a folder), a running scan refuses the change (its completion
// would publish the list it was started with), and the new list is persisted
// with the results.
func (s *Server) handleRefs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST required")
		return
	}
	var req struct {
		RefDirs []string `json:"refDirs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad request body")
		return
	}
	dirs := uniquePaths(req.RefDirs)
	canon := make([]string, len(dirs))
	for i, d := range dirs {
		rp, err := resolveAllowedRoot(d)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		canon[i] = rp
	}
	sess, err := fsSessionFrom(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if len(dirs) > 0 {
		if err := validateRoots(dirs, canon, sess); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	s.mu.Lock()
	if s.job.Running {
		s.mu.Unlock()
		writeErr(w, 409, "a scan is running — change the reference folders when it finishes")
		return
	}
	s.refDirs = dirs
	s.invalidateDerivedLocked() // the padlocks and totals of every cached view follow the list
	s.mu.Unlock()
	if err := s.saveState(); err != nil {
		s.noteSaveError(err)
	}
	writeJSON(w, 200, map[string]any{"refDirs": dirs})
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST required")
		return
	}
	// Like every other mutation: the raw API (token, no DSM session) reads
	// state and results, nothing more. A cancel is not harmless — it ends a
	// scan that may be hours in, and clears its marker with it.
	if _, err := fsSessionFrom(r); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.mu.Lock()
	if s.job.Running && s.job.cancel != nil {
		close(s.job.cancel)
		s.job.cancel = nil
	}
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// snapshotGroupsLocked deep-copies a result's duplicate groups (caller
// holds s.mu). pruneMoved rewrites the cached slices in place, so any
// group data used outside the lock must be a copy, never the live slices.
func snapshotGroupsLocked(res *toolResult) []Group {
	if res == nil {
		return nil
	}
	out := make([]Group, len(res.Groups))
	for i, g := range res.Groups {
		out[i] = g
		out[i].Files = append(make([]FileEnt, 0, len(g.Files)), g.Files...)
	}
	return out
}

// snapshotResult returns a deep copy of the tool's cached result, taken
// under s.mu. Handlers serialize or iterate the copy after unlocking;
// handing out the live pointer would race with pruneMoved, which mutates
// the same slices during concurrent /api/move requests.
func (s *Server) snapshotResult(tool string) *toolResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := s.results[tool]
	if res == nil {
		return nil
	}
	cp := *res
	cp.Groups = snapshotGroupsLocked(res)
	// make(...,0,n) rather than append to a nil slice: appending NOTHING to
	// nil leaves nil, which marshals as `null`, and the GET dump would then
	// answer "files": null for any empty result — a raw caller reading
	// .files.length crashes on it, and the paged POST beside it guarantees
	// [] (see slicePage). An empty result set is a real, ordinary answer:
	// every tool reports one before its first scan, and a move that prunes
	// the last row produces one too.
	cp.Files = append(make([]FileEnt, 0, len(res.Files)), res.Files...)
	cp.Errors = append(make([]string, 0, len(res.Errors)), res.Errors...)
	if res.Match != nil {
		m := *res.Match
		cp.Match = &m
	}
	return &cp
}

// handleResults reads cached results. POST is the paged form the UI uses
// (results.go); GET remains the legacy full dump for raw localhost callers —
// for large result sets the paged form is the one that scales.
func (s *Server) handleResults(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.handlePagedResults(w, r)
		return
	}
	tool := r.URL.Query().Get("tool")
	res := s.snapshotResult(tool)
	if res == nil {
		writeJSON(w, 200, map[string]any{"tool": tool, "scanned": false})
		return
	}
	out := map[string]any{
		"tool": tool, "scanned": true, "groups": res.Groups,
		"files": res.Files, "errors": res.Errors, "match": res.Match,
		"scannedAt": res.Scanned,
	}
	if res.Truncated != nil {
		out["truncated"] = res.Truncated
	}
	writeJSON(w, 200, out)
}

// vetDestination pins and vets a move or export destination and returns the
// pinned handle (the caller closes it), its canonical and share-space paths
// and the caller's session. One implementation for both handlers, so the
// checks — and their wording — cannot drift apart.
//
// Containment is decided from the pinned object's canonical path, and the
// File Station operation later targets that same canonical path — so
// swapping the destination for a symlink after validation cannot launder an
// outside destination past the vetting, and the vetted destination and the
// executed destination are never two different strings. The canonical
// containment stays native (the vetting exception); "is this a usable folder
// for this DSM session" is File Station's question.
func (s *Server) vetDestination(w http.ResponseWriter, r *http.Request, dest string) (destH *dirhandle.Handle, destCanon, destShare string, sess *fsSession, ok bool) {
	destH, err := dirhandle.Open(dest)
	if err != nil {
		writeErr(w, 400, "destination is not a folder")
		return nil, "", "", nil, false
	}
	fail := func(code int, msg string) (*dirhandle.Handle, string, string, *fsSession, bool) {
		destH.Close()
		writeErr(w, code, msg)
		return nil, "", "", nil, false
	}
	destCanon, err = destH.Canon()
	if err != nil || !allowedPath(dest) || !isUnder(destCanon, volumeRootsResolved()) {
		return fail(400, "destination outside allowed volumes")
	}
	// Execution is delegated to File Station with the caller's DSM session;
	// without one (a raw daemon call from outside DSM) mutations are refused.
	sess, err = fsSessionFrom(r)
	if err != nil {
		return fail(400, err.Error())
	}
	destShare, err = sess.shareSpacePath(destCanon)
	if err != nil {
		return fail(400, "destination outside allowed volumes")
	}
	if exists, isdir, err := sess.statShare(destShare); err != nil {
		return fail(400, err.Error())
	} else if !exists || !isdir {
		return fail(400, "destination is not a folder")
	}
	return destH, destCanon, destShare, sess, true
}
