package main

// On-disk result persistence: a day-long scan must survive a daemon restart
// (package upgrade, NAS reboot, crash). The daemon persists its cached
// results — together with the reference dirs, the keep-one survivors and the
// ID counter, so every move-safety invariant carries across the restart — as
// gzipped JSON in the state dir, written atomically after each scan and after
// each move's prune. A marker file records a scan in flight; if the daemon
// comes back up with the marker still present, the scan was interrupted and
// /api/state says so, and the UI offers to resume it or start over (see
// scanMarker). Scans themselves are never auto-restarted — they require the
// caller's DSM session.

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

const (
	stateFile  = "results.json.gz"
	markerFile = "scan.interrupted"
)

// persistedState is the schema of the state file. Unknown schema versions
// (or unparsable files) are ignored: the daemon starts empty rather than
// guessing.
type persistedState struct {
	Schema   int                    `json:"schema"`
	Results  map[string]*toolResult `json:"results"`
	RefDirs  []string               `json:"refDirs"`
	Keepers  []string               `json:"keepers"`
	LastTool string                 `json:"lastTool"`
	NextID   int                    `json:"nextID"`
	SavedAt  string                 `json:"savedAt"`
}

// scanMarker marks a scan in flight; found at startup it means that scan
// never finished.
type scanMarker struct {
	Tool string `json:"tool"`
	// Gen is the hash-store generation the interrupted run was recording
	// under; a resume continues it. 0 = the run died before opening the
	// store.
	Gen uint32 `json:"gen,omitempty"`
	// The rest of the request the interrupted run was serving, normalized
	// by normPaths. A resume adopts the dead run's generation, which hands
	// that run's reads to the new scan — sound only when the new scan IS
	// the same run: same roots, same reference folders, same recursion and
	// match criteria. matches() checks all of it; a marker from a build
	// that recorded none of it matches nothing, degrading to the safe full
	// re-read.
	Dirs      []string  `json:"dirs,omitempty"`
	RefDirs   []string  `json:"refDirs,omitempty"`
	Recurse   bool      `json:"recurse,omitempty"`
	Match     MatchOpts `json:"match"`
	StartedAt string    `json:"startedAt"`
}

// matches reports whether req asks for the very scan the marker records.
// The tool alone is not identity: two duplicates scans with different
// scopes or match criteria are different scans, and serving one the other's
// reads is exactly the cross-scan reuse the generation gate exists to
// prevent.
func (m *scanMarker) matches(req *ScanReq) bool {
	return m.Tool == req.Tool && m.Recurse == req.Recurse &&
		m.Match == req.Match &&
		eqStrings(m.Dirs, normPaths(req.Dirs)) &&
		eqStrings(m.RefDirs, normPaths(req.RefDirs))
}

// normPaths is the marker's canonical form of a path list: cleaned,
// deduplicated, sorted. Requests differing only in order, repetition or
// redundant path syntax are the same request.
func normPaths(in []string) []string {
	out := uniquePaths(in)
	sort.Strings(out)
	return out
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// stateDir is where persistent artifacts live: the package var dir in
// release builds, the dev DUPFINDER_STATE hook off-NAS, or "" (persistence
// disabled — everything still works, nothing survives a restart).
func (s *Server) stateDir() string {
	if s.varDir != "" {
		return s.varDir
	}
	return devStateDir()
}

// writeAtomic writes path through a temporary sibling that is synced to disk
// before it is renamed into place. Temp+rename alone only promises that a
// reader never sees a half-written file; without the fsync, a power loss
// shortly after the rename can leave the FINAL name pointing at a zero-length
// or truncated file — on ext4 and btrfs alike — which the loaders then skip
// or truncate silently. body writes the content; noReplace makes the final
// step refuse to clobber an existing file (the token writer's contract).
func writeAtomic(path string, mode os.FileMode, noReplace bool, body func(*os.File) error) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once renamed
	if err := body(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if noReplace {
		err = renameNoReplace(name, path)
	} else {
		err = os.Rename(name, path)
	}
	if err != nil {
		return err
	}
	// The rename lives in the directory: sync that too, so the new name
	// survives the same power loss the data now does. A filesystem that does
	// not support syncing a directory says so with EINVAL or ENOTSUP, which is
	// not a failed write; anything else is.
	if d, derr := os.Open(dir); derr == nil {
		serr := d.Sync()
		d.Close()
		if serr != nil && !errors.Is(serr, syscall.EINVAL) && !errors.Is(serr, syscall.ENOTSUP) {
			return serr
		}
	}
	return nil
}

// saveState snapshots the cached results under s.mu and writes them
// atomically (writeAtomic: temp + fsync + rename, 0600). A failure is
// returned so the caller can say so — a NAS with a full system partition
// keeps scanning, but the user must learn that the results will not survive a
// restart — and cleared from the state when a later save succeeds.
func (s *Server) saveState() error {
	dir := s.stateDir()
	if dir == "" {
		return nil
	}
	s.mu.Lock()
	ps := persistedState{
		Schema:   1,
		Results:  make(map[string]*toolResult, len(s.results)),
		RefDirs:  append([]string{}, s.refDirs...),
		LastTool: s.lastTool,
		NextID:   s.nextID,
		SavedAt:  time.Now().Format(time.RFC3339),
	}
	for tool := range s.results {
		cp := *s.results[tool]
		cp.Groups = snapshotGroupsLocked(s.results[tool])
		cp.Files = append([]FileEnt{}, s.results[tool].Files...)
		cp.Errors = append([]string{}, s.results[tool].Errors...)
		ps.Results[tool] = &cp
	}
	for p := range s.keepers {
		ps.Keepers = append(ps.Keepers, p)
	}
	s.mu.Unlock()

	// One writer at a time; the snapshot above is already consistent.
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	path := filepath.Join(dir, stateFile)
	err := writeAtomic(path, 0o600, false, func(tmp *os.File) error {
		zw, _ := gzip.NewWriterLevel(tmp, gzip.BestSpeed)
		if err := json.NewEncoder(zw).Encode(&ps); err != nil {
			return err
		}
		return zw.Close()
	})
	if err != nil {
		log.Printf("state save failed: %v", err)
		return err
	}
	s.mu.Lock()
	s.saveErr = ""
	s.mu.Unlock()
	return nil
}

// noteSaveError records a failed state save for /api/state, where the app
// turns it into a warning that the results on screen will not survive a
// restart. The next successful save clears it.
func (s *Server) noteSaveError(err error) {
	s.mu.Lock()
	s.saveErr = "results could not be saved to disk (" + err.Error() + ") — they will be lost when the package restarts"
	s.mu.Unlock()
}

// lacksFingerprints reports whether any row of a duplicates result carries no
// prefix fingerprint — the mark of a result an older build persisted.
func lacksFingerprints(res *toolResult) bool {
	for gi := range res.Groups {
		for _, f := range res.Groups[gi].Files {
			if f.Pfx == "" {
				return true
			}
		}
	}
	return false
}

// loadState restores the persisted results at daemon start. Anything wrong
// with the file (missing, corrupt, unknown schema) means starting empty —
// stale-looking data is worse than no data, and every move is re-verified
// against File Station at move time anyway.
func (s *Server) loadState() {
	dir := s.stateDir()
	if dir == "" {
		return
	}
	f, err := os.Open(filepath.Join(dir, stateFile))
	if err != nil {
		return
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		log.Printf("state load skipped: %v", err)
		return
	}
	defer zr.Close()
	var ps persistedState
	if err := json.NewDecoder(zr).Decode(&ps); err != nil || ps.Schema != 1 {
		log.Printf("state load skipped: schema/parse (%v)", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ps.Results != nil {
		// Drop results for tools this build no longer has. A state file
		// written by an older build carries them, and they are not inert:
		// handleMove's allowlist is built from EVERY cached result, so a
		// retired tool's rows would stay movable — and a keep-one survivor
		// listed only there would stay exposed — with no UI able to show
		// them or rescan them away. Retiring a tool must retire its data.
		for tool, r := range ps.Results {
			if !validTools[tool] {
				delete(ps.Results, tool)
				log.Printf("dropped persisted results for retired tool %q", tool)
				continue
			}
			// A `null` entry (a hand-edited file — the daemon never writes
			// one) would be a nil pointer every consumer dereferences.
			if r == nil {
				delete(ps.Results, tool)
			}
		}
		// A duplicates row without a content fingerprint was recorded by a
		// build that kept none, and the move's prefix re-read — promised for
		// every duplicate — would silently be skipped for it. Such results are
		// dropped, with the conflicting-files set that rode on the same scan,
		// rather than served: the next scan records them properly, and until
		// then the app shows no duplicates to move.
		if dup := ps.Results["duplicates"]; dup != nil && lacksFingerprints(dup) {
			delete(ps.Results, "duplicates")
			delete(ps.Results, "corrupted_files")
			log.Printf("dropped persisted duplicates results recorded without content fingerprints; scan again to rebuild them")
		}
		s.results = ps.Results
		s.invalidateDerivedLocked() // anything derived describes the pre-load results
	}
	s.refDirs = ps.RefDirs
	s.lastTool = ps.LastTool
	if !validTools[s.lastTool] {
		s.lastTool = "" // the UI would open on a tool that no longer exists
	}
	if ps.NextID > s.nextID {
		s.nextID = ps.NextID
	}
	if len(ps.Keepers) > 0 {
		s.keepers = map[string]bool{}
		for _, p := range ps.Keepers {
			s.keepers[p] = true
		}
	}
	n := 0
	for _, r := range s.results {
		n += len(r.Files)
		for _, g := range r.Groups {
			n += len(g.Files)
		}
	}
	log.Printf("restored persisted results (%d rows, saved %s)", n, ps.SavedAt)
}

// writeMarker records the scan now starting; clearMarker removes it on any
// controlled end (completion, cancellation, failure). A marker present at
// startup therefore means an interrupted scan.
//
// gen is the hash-store generation the scan runs at, and it is what makes
// RESUME sound: a resume continues exactly that generation, so the dead run's
// own reads are servable and everything older stays history. It is 0 until
// the duplicates pass has opened the store (other tools never do), and a
// resume against gen 0 deliberately degenerates to a full re-read — a run
// that died before opening the store had read nothing worth continuing, and
// guessing a generation here is how a COMPLETED scan's hashes would leak
// into a later run as if freshly read.
//
// The marker also records the request itself (normalized), because the
// generation is only resumable into the SAME request — matches() is the
// other half of the gate.
func (s *Server) writeMarker(req *ScanReq, gen uint32) {
	dir := s.stateDir()
	if dir == "" {
		return
	}
	b, _ := json.Marshal(scanMarker{Tool: req.Tool, Gen: gen,
		Dirs: normPaths(req.Dirs), RefDirs: normPaths(req.RefDirs),
		Recurse: req.Recurse, Match: req.Match,
		StartedAt: time.Now().Format(time.RFC3339)})
	// Atomic like the state file: a crash mid-write would otherwise leave an
	// unparsable marker, which loadMarker reads as "no interruption".
	err := writeAtomic(filepath.Join(dir, markerFile), 0o600, false, func(f *os.File) error {
		_, err := f.Write(b)
		return err
	})
	if err != nil {
		log.Printf("scan marker write failed: %v", err)
	}
}

func (s *Server) clearMarker() {
	dir := s.stateDir()
	if dir == "" {
		return
	}
	p := filepath.Join(dir, markerFile)
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		// A marker that survives a COMPLETED scan would make that scan
		// resumable — the one door the generation gate exists to keep shut.
		// If it cannot be removed, neutralize it in place: loadMarker
		// rejects a marker with no tool.
		log.Printf("scan marker could not be removed (%v) — neutralizing in place", err)
		if werr := os.WriteFile(p, []byte("{}"), 0o600); werr != nil {
			log.Printf("scan marker could not be neutralized either: %v", werr)
		}
	}
}

// loadMarker reports the interrupted scan, if any, at daemon start.
func (s *Server) loadMarker() *scanMarker {
	dir := s.stateDir()
	if dir == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(dir, markerFile))
	if err != nil {
		return nil
	}
	var m scanMarker
	if json.Unmarshal(b, &m) != nil || m.Tool == "" {
		return nil
	}
	return &m
}
