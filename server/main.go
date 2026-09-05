// Duplicate Finder for Synology DSM — backend service.
//
// Runs in two modes:
//
//	-mode daemon : long-running HTTP server on 127.0.0.1 that performs scans
//	-mode cgi    : CGI shim executed by DSM's web server; proxies the request
//	               to the local daemon so the UI can talk to it through the
//	               authenticated /webman/3rdparty/ path.
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"dupfinder/internal/dirhandle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httputil"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// appVersion is the package's full version, stamped at build time with
// -ldflags -X (build.sh's VERSION). A var rather than a const because -X
// cannot write a const. /api/info reports it, so the installed build can be
// identified without going through Package Center. The bare fallback is what
// a plain `go build` (dev runs, tests) produces.
var appVersion = "1.0.0-dev"

// defaultPort is the one port the daemon, the package's start script and the
// CGI shim agree on. There is deliberately no environment override: the CGI
// shim is exec'd by DSM's web server with an environment the package does not
// control, so a port read from the environment by the daemon and the start
// script would be one the shim could never learn.
const defaultPort = 9807

func main() {
	mode := flag.String("mode", "daemon", "daemon | cgi")
	port := flag.Int("port", defaultPort, "daemon listen port (127.0.0.1)")
	varDir := flag.String("var", "", "writable state dir (logs)")
	flag.Parse()

	if *mode == "cgi" {
		runCGI(*port)
		return
	}
	runDaemon(*port, *varDir)
}

// ---------------------------------------------------------------- CGI proxy

// Auth between the CGI shim and the daemon: the daemon only serves /api/*
// requests carrying the shared secret in this header. The secret lives in
// the package var dir, readable only by the package user both processes run
// as — so other local users/packages cannot drive the daemon directly.
const (
	authTokenHeader = "X-DupFinder-Token"
	authTokenFile   = ".authtoken"
	// readyFile is written (with the port) once the daemon is bound and its
	// token is loaded; the start script waits for it.
	readyFile = "dupfinder.ready"
)

// serviceUser is the name of the account this process runs as, "" when
// the platform cannot say. Pure-Go os/user reads /etc/passwd, where DSM
// lists its package accounts, so this works in the CGO_ENABLED=0 build.
func serviceUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

// cgiVarDir resolves the package var dir for the CGI process. api.cgi
// exports DUPFINDER_VAR. Failing that, DSM's layout is the fallback: the
// shim execs /var/packages/<id>/target/bin/dupfinder, and the var dir is
// target's sibling — for whatever <id> this build installed under (the
// hand-built spk and the SynoCommunity build use different ids, so none is
// hardcoded). argv[0] is used, not os.Executable: the latter resolves the
// symlinks to /volumeN/@appstore/<id>/bin/dupfinder, whose parent holds no
// var dir. With neither, the token cannot be found and the daemon refuses
// the request — a visible failure, not a guess.
func cgiVarDir() string {
	if d := os.Getenv("DUPFINDER_VAR"); d != "" {
		return d
	}
	if exe := os.Args[0]; filepath.IsAbs(exe) {
		return filepath.Join(filepath.Dir(filepath.Dir(exe)), "..", "var")
	}
	log.Printf("cgi: DUPFINDER_VAR unset and argv[0] %q is not absolute; cannot locate the package var dir", os.Args[0])
	return ""
}

func runCGI(port int) {
	target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	proxy := httputil.NewSingleHostReverseProxy(target)
	token := ""
	if b, err := os.ReadFile(filepath.Join(cgiVarDir(), authTokenFile)); err == nil {
		token = strings.TrimSpace(string(b))
	}
	// The daemon executes file mutations through DSM's File Station Web API
	// as the logged-in admin. Their session (cookie + SynoToken) already
	// rides the proxied request headers; the CGI environment tells us which
	// local scheme/port that session is valid against.
	scheme := "http"
	if os.Getenv("HTTPS") == "on" {
		scheme = "https"
	}
	dsmPort := os.Getenv("SERVER_PORT")
	if dsmPort == "" {
		dsmPort = "5000"
	}
	dsmBase := scheme + "://127.0.0.1:" + dsmPort
	orig := proxy.Director
	proxy.Director = func(r *http.Request) {
		orig(r)
		r.Header.Set(dsmBaseHeader, dsmBase)
		// Incoming path looks like /webman/3rdparty/<id>/api.cgi/scan
		// — keep only what follows api.cgi and mount it under /api.
		p := r.URL.Path
		if i := strings.Index(p, "api.cgi"); i >= 0 {
			p = p[i+len("api.cgi"):]
		}
		if p == "" {
			p = "/"
		}
		r.URL.Path = "/api" + strings.TrimSuffix(p, "/")
		r.Host = target.Host
		if token != "" {
			r.Header.Set(authTokenHeader, token)
		}
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "Duplicate Finder service is not running. Start the package in Package Center.",
		})
	}
	if err := cgi.Serve(proxy); err != nil {
		fmt.Fprintf(os.Stderr, "cgi: %v\n", err)
		os.Exit(1)
	}
}

// ------------------------------------------------------------------ daemon

// Server holds scan results and job state for the daemon.
type Server struct {
	mu        sync.Mutex
	job       jobState
	move      moveState              // per-file progress of the /api/move in flight, for /api/state
	results   map[string]*toolResult // keyed by tool id
	view      *resultView            // derived paged view (results.go); nil after results change
	dateRange map[string]dateRanges  // per-tool date span over the whole result; nil after results change
	refDirs   []string               // reference dirs from the most recent scan
	lastTool  string                 // tool whose scan finished (and stored) last
	lastEnd   scanEnd                // how the most recent scan run ended, completed or not
	nextID    int
	authToken string // shared secret required on /api/*; "" disables (dev/tests)

	varDir      string      // package var dir; with devStateDir, where persistence lives
	persistMu   sync.Mutex  // serializes state-file writes (persist.go)
	interrupted *scanMarker // scan marker found at startup: a scan a restart killed

	// moveMu serializes /api/move from group snapshot through pruneMoved:
	// two concurrent requests vetted against the same snapshot could each
	// see the other's file as an unrequested survivor and drain a duplicate
	// group between them. keepers holds the canonical paths of survivors of
	// dissolved groups (pruned below two members), so the last in-place
	// copy stays protected across requests until the next duplicates scan
	// re-derives the groups. keepers is written only under moveMu (via
	// pruneMoved) or cleared under mu (scan completion); reads copy it
	// under mu.
	moveMu sync.Mutex
	// moveActive is the SCAN side's view of "a move is in flight". moveMu
	// alone cannot serve that purpose: handleScan must never wait on it —
	// that inverts the moveMu -> mu order into a deadlock, and would block
	// /api/scan for the minutes a large batch takes — so the fact of a move
	// has to be readable under mu. s.move.Running cannot serve it either: it
	// is first published after the destination checks, leaving the whole
	// preamble invisible. Claimed by beginMove under moveMu and released by
	// endMove, so it brackets vet -> execute -> prune -> save entirely.
	moveActive bool
	keepers    map[string]bool
}

// scanAdmissionLocked reports the 409 message that must refuse a new scan, or
// "" if one may start. Caller holds mu.
//
// Scans and moves must not overlap in EITHER direction. A move mutates the
// tree a scan is enumerating and hashing: relocated files fail their pinned
// re-open and are silently dropped, so the scan under-reports, and a group
// that falls below two members is discarded whole. handleMove guards the
// other direction through beginMove.
func (s *Server) scanAdmissionLocked() string {
	if s.job.Running {
		return "a scan is already running"
	}
	if s.moveActive {
		return "a move is running — wait for it to finish before scanning"
	}
	return ""
}

// beginMove claims the move slot, reporting false if a scan got in first.
// MUST be called with moveMu already held: the claim and the scan re-read are
// one mu critical section, which is what closes the gap between handleMove's
// cheap pre-lock check and the lock itself (a File Station round trip apart,
// and unbounded for a second move queued behind a first).
func (s *Server) beginMove() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job.Running {
		return false
	}
	s.moveActive = true
	return true
}

// endMove releases the claim. Deferred immediately after beginMove so it runs
// before moveMu is dropped and on every exit path, panics included.
func (s *Server) endMove() {
	s.mu.Lock()
	s.moveActive = false
	s.mu.Unlock()
}

type jobState struct {
	Running  bool    `json:"running"`
	Tool     string  `json:"tool"`
	Progress float64 `json:"progress"`
	Label    string  `json:"label"`
	cancel   chan struct{}
}

// moveState is how far the /api/move in flight has got. A move is one long
// blocking request — File Station copies the bytes when the destination is on
// another volume or an ext4 one, so a single large file can take minutes —
// and this is what lets the UI show more than an indeterminate spinner for
// the whole batch. It is written under mu (never moveMu), so /api/state can
// report it while the move itself holds moveMu for the entire run.
type moveState struct {
	Running bool   `json:"running"`
	Done    int    `json:"done"`  // files fully dealt with, moved or refused
	Total   int    `json:"total"` // files this request was asked to move
	Name    string `json:"name"`  // basename of the file currently in flight
}

type toolResult struct {
	Tool string `json:"tool"`
	// Roots maps each scan root's display form to its canonical (symlink-
	// resolved) path. The walk skips symlinks below a root, so a row's
	// canonical path is its root's canonical path plus the same relative
	// tail — which lets the reference-folder protection be decided on
	// canonical paths without a syscall per row (results.go refMatcher).
	Roots     []RootMap  `json:"roots,omitempty"`
	Groups    []Group    `json:"groups,omitempty"`
	Files     []FileEnt  `json:"files,omitempty"`
	Errors    []string   `json:"errors,omitempty"`
	Match     *MatchOpts `json:"match,omitempty"`     // criteria applied at scan time
	Truncated *TruncInfo `json:"truncated,omitempty"` // results found beyond the stored cap
	Scanned   string     `json:"scannedAt"`
}

// RootMap is one scan root: Raw as the user gave it (and as rows display it),
// Canon as the filesystem resolves it.
type RootMap struct {
	Raw   string `json:"raw"`
	Canon string `json:"canon"`
}

// scanEnd records how the most recent scan run ended, whatever the outcome.
// lastTool names only the last scan that COMPLETED, so on its own a poller
// that sees a run stop cannot tell a cancel from a finish, and would replay
// the previous finish's announcements after a Stop.
type scanEnd struct {
	Tool      string
	Completed bool
}

// TruncInfo reports what a scan found but did not keep, so no results cap is
// ever silent: the UI tells the user how much more exists and that narrowing
// the scan scope will surface it. Immutable once stored.
type TruncInfo struct {
	Groups int `json:"groups,omitempty"` // duplicates: whole groups dropped
	Files  int `json:"files"`            // files (rows) dropped
	Cap    int `json:"cap"`              // the stored-results cap that applied
}

// FileEnt is one row in the results table. Dir is the containing directory
// (the "Location" column); the full path is Dir + "/" + Name.
type FileEnt struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Dir  string `json:"path"`
	Size int64  `json:"size"`
	Mod  string `json:"date"`
	// ModUnix is the same instant as Mod as an epoch value, and is what the
	// move-time identity check compares: Mod is a zone-less local-time
	// string, so a NAS whose time zone changed between scan and move would
	// otherwise refuse every file as "changed". Absent from results written
	// by older builds, which fall back to comparing Mod.
	ModUnix  int64  `json:"modUnix,omitempty"`
	Created  string `json:"created"`
	Captured string `json:"captured,omitempty"`
	Hash     string `json:"hash,omitempty"`
	// Pfx is the first half of the file's 64 KiB-prefix BLAKE3, recorded by
	// the duplicates scan only — it is the one tool whose result asserts
	// something about a file's CONTENT, and the move re-checks it before
	// touching anything. Results written by an older build carry none, and
	// the check is simply skipped for them.
	Pfx   string `json:"pfx,omitempty"`
	Ext   string `json:"ext"`
	IsDir bool   `json:"isDir,omitempty"`
	// Verdict and Evidence are written by the corrupted-files scan only:
	// "corrupt", "intact" or "unknown", plus the sentence explaining why.
	// Both are omitempty, so every other tool's rows — and every result
	// written by an older build — serialize exactly as they did before.
	Verdict  string `json:"verdict,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	// NoMove marks a row File Station cannot address, so no move can ever
	// succeed for it (see fsCannotAddress). The grid draws these with DSM's
	// DISABLED checkbox — the same sprite the last-copy-of-a-group case uses,
	// because it says the same thing: this row cannot be acted on. Derived
	// from the name at scan time and sent to the client so the box is right
	// before anyone clicks it; the daemon refuses these independently, so the
	// disabled box is presentation, not the enforcement.
	NoMove bool `json:"nomove,omitempty"`
	// Prot marks a read-only reference copy. Decided by the daemon per page
	// (results.go) on CANONICAL paths — the same comparison the move refuses
	// with — so the padlock the grid draws and the refusal the move issues
	// can never disagree about a folder given through a symlink alias.
	Prot bool `json:"prot,omitempty"`
}

type Group struct {
	ID    string    `json:"id"`
	Ext   string    `json:"ext"`
	Size  int64     `json:"size"`
	Hash  string    `json:"hash"`
	Files []FileEnt `json:"files"`
	// Corrupted-files sets only. A set is defined by its members DISAGREEING,
	// so Hash above is necessarily empty for one and cannot identify it; Mod
	// is the modified time the whole set shares, and Variants counts the
	// distinct contents found under it. SameName records that every member
	// also shares a filename — the difference between two copies of one file
	// and two unrelated files that happen to match on size and timestamp.
	Mod      string `json:"mod,omitempty"`
	Variants int    `json:"variants,omitempty"`
	SameName bool   `json:"sameName,omitempty"`
	// Count is the group's TRUE member count, set only when Files carries
	// fewer than that — a page trims very large groups so one of them cannot
	// bury a browser. Zero means Files is the whole group.
	Count int `json:"count,omitempty"`
	// Prot is how many of the group's members are protected reference copies,
	// counted over the WHOLE group before any trimming. The client draws the
	// group header's reclaimable figure from it: recounting protection over a
	// trimmed page mixes a partial protected count with the true member count
	// and overstates what a move could free. Duplicates only, and omitted
	// when no reference folder is set.
	Prot int `json:"prot,omitempty"`
}

// logMaxBytes is the size at which the daemon's log rotates. The package var
// dir is no place to grow a file without limit: a daemon that logs a line per
// unreadable path on a large volume — or simply runs for years — would fill
// it. One generation is kept (dupfinder.log.1), so the pair is bounded at
// twice this. (start-stop-status also points the process's stderr at the
// live file; that stream carries only crashes, and its descriptor follows
// the rotated inode.)
const logMaxBytes = 2 << 20

// rotatingLog is the log destination: an appending file that renames itself
// aside once it is full. Every failure degrades to "keep the log we have" —
// logging must never be what takes the daemon down.
type rotatingLog struct {
	mu   sync.Mutex
	path string
	f    *os.File
	n    int64
}

func newRotatingLog(path string) (*rotatingLog, error) {
	// 0600 like every other var-dir artifact: the log names files whose
	// moves parked or failed verification, in shares other local users may
	// have no access to.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	var size int64
	if fi, err := f.Stat(); err == nil {
		size = fi.Size()
	}
	return &rotatingLog{path: path, f: f, n: size}, nil
}

func (l *rotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n+int64(len(p)) > logMaxBytes {
		l.rotateLocked()
	}
	n, err := l.f.Write(p)
	l.n += int64(n)
	return n, err
}

// rotateLocked never closes the descriptor it is writing to until it holds a
// working replacement: renaming works on an open file, so every failure path
// simply keeps logging to the file it already has rather than leaving a
// closed descriptor behind and losing every later line.
func (l *rotatingLog) rotateLocked() {
	l.n = 0 // whatever happens below, do not retry the rotation on every write
	if err := os.Rename(l.path, l.path+".1"); err != nil {
		return // cannot rotate (read-only dir, race): keep appending
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return // the old descriptor still writes, now into the .1 file
	}
	old := l.f
	l.f = f
	old.Close()
}

func runDaemon(port int, varDir string) {
	if varDir != "" {
		if w, err := newRotatingLog(filepath.Join(varDir, "dupfinder.log")); err == nil {
			log.SetOutput(w)
		}
	}
	// Loopback-only by design; devBindHost widens this in dev builds only
	// (the ARM smoke run reaches a containerized daemon through Docker's
	// published ports, which never forward to container loopback).
	host := "127.0.0.1"
	if h := devBindHost(); h != "" {
		host = h
	}
	s := &Server{results: map[string]*toolResult{}, varDir: varDir}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/info", s.handleInfo)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/scan", s.handleScan)
	mux.HandleFunc("/api/cancel", s.handleCancel)
	mux.HandleFunc("/api/results", s.handleResults)
	mux.HandleFunc("/api/move", s.handleMove)
	mux.HandleFunc("/api/export", s.handleExport)

	// devServeUI is the identity function in release builds and wraps the API
	// with static-file serving only under the "dev" build tag.
	handler := devServeUI(s.withAuth(mux))
	addr := fmt.Sprintf("%s:%d", host, port)
	// Bind before touching the token file: a second daemon instance (stale
	// PID file, manual start) must fail right here and never rotate the
	// token out from under the instance that is already serving.
	l, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	if varDir != "" {
		tok, err := loadOrCreateToken(filepath.Join(varDir, authTokenFile))
		if err != nil {
			// Fail closed: without the token every local process could drive
			// /api/move and /api/export. Refusing to start is the safe state.
			log.Fatalf("FATAL: daemon auth token unavailable (%v) — refusing to serve unauthenticated", err)
		}
		s.authToken = tok
	}
	// Everything that can still fail has passed: say so where the start
	// script can see it. It waits for this file instead of guessing with a
	// fixed sleep, so Package Center never reports a daemon that then died.
	if varDir != "" {
		err := writeAtomic(filepath.Join(varDir, readyFile), 0o600, false, func(f *os.File) error {
			_, err := fmt.Fprintf(f, "%d\n", port)
			return err
		})
		if err != nil {
			log.Printf("ready file could not be written: %v", err)
		}
	}
	// Results persisted by an earlier run come back before serving starts —
	// a day-long scan survives a package upgrade or NAS reboot. Loaded AFTER
	// the bind: the load of a 100k-row state file can outlast the start
	// script's patience, and a bind that failed afterwards would leave Package
	// Center showing a running package with no daemon behind it. Connections
	// arriving meanwhile simply wait in the listen backlog. A scan marker
	// left behind means a scan died with the daemon; /api/state reports it
	// so the UI can offer a resume.
	s.loadState()
	s.interrupted = s.loadMarker()
	log.Printf("Duplicate Finder %s listening on %s", appVersion, addr)
	if err := http.Serve(l, handler); err != nil {
		log.Fatal(err)
	}
}

// loadOrCreateToken returns the shared daemon secret, minting one on first
// start. 0600 in the package var dir: only the package user (which both the
// daemon and the CGI shim run as) can read it. Creation is atomic — the
// token is written to a private temp file and no-replace-renamed into place
// — so an existing token is never overwritten and concurrent starters
// converge on whichever token landed first.
func loadOrCreateToken(path string) (string, error) {
	read := func() (string, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		t := strings.TrimSpace(string(b))
		if t == "" {
			return "", fmt.Errorf("token file %s is empty — delete it and restart the package", path)
		}
		return t, nil
	}
	if t, err := read(); err == nil {
		return t, nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	t := hex.EncodeToString(buf)
	tmp, err := os.CreateTemp(filepath.Dir(path), authTokenFile+".tmp-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed into place
	if _, err := tmp.WriteString(t); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := renameNoReplace(tmp.Name(), path); err != nil {
		if errors.Is(err, syscall.EEXIST) {
			return read() // another starter won the race — use its token
		}
		return "", err
	}
	return t, nil
}

// withAuth requires the shared token on every API request. Without a token
// configured (dev/test runs without -var) it is a passthrough, so the local
// dev flow and the smoke suite are unaffected.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authToken != "" &&
			subtle.ConstantTimeCompare([]byte(r.Header.Get(authTokenHeader)), []byte(s.authToken)) != 1 {
			writeErr(w, http.StatusUnauthorized, "missing or invalid daemon token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ------------------------------------------------------------ http helpers

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// ---------------------------------------------------------------- handlers

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
		"running": s.job.Running, "tool": s.job.Tool,
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
	if s.lastEnd.Tool != "" {
		out["lastEnd"] = map[string]any{"tool": s.lastEnd.Tool, "completed": s.lastEnd.Completed}
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
	writeJSON(w, 200, out)
}

// MatchOpts are the optional extra duplicate criteria. When set they join
// the pre-hash candidate key, so files unique in (size + criteria) are never
// read or hashed at all.
type MatchOpts struct {
	Name     bool `json:"name"`
	Modified bool `json:"modified"`
	Created  bool `json:"created"`
}

// ScanReq is the body of POST /api/scan.
type ScanReq struct {
	Tool    string    `json:"tool"`
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
	if !validTools[req.Tool] {
		writeErr(w, 400, "unknown tool")
		return
	}
	// Conflicting Files has no scan of its own — it is filled in by the
	// duplicates scan, which is the only pass that reads file contents.
	// Refused here so a request never starts a job that would do nothing.
	if readOnlyTools[req.Tool] {
		writeErr(w, 400, "Conflicting Files is filled in by the Duplicate Files scan — run that instead")
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
	s.job = jobState{Running: true, Tool: req.Tool, Label: "Initializing scan…", cancel: cancel}
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

// MoveReq is the body of POST /api/move. Files are full paths. Tool names
// the result set the files came from; it is required only when Preserve is
// set, where it selects — through moveFolderNames, never as a path fragment
// — the name of the one folder the batch is moved into.
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
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST required")
		return
	}
	var req MoveReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad request body")
		return
	}
	if len(req.Files) == 0 {
		writeErr(w, 400, "no files to move")
		return
	}
	// A read-only tool's rows are a report, not a work list. Refused here, at
	// the trust boundary, so hiding the button in the UI is presentation
	// rather than the whole enforcement.
	if readOnlyTools[req.Tool] {
		writeErr(w, 400, "Conflicting Files is a report — move these files from File Station once you have decided which copy to keep")
		return
	}
	// Resolved before anything can be created: the destination is not open
	// yet, no DSM session is needed and moveMu is not held, so an unrecognised
	// tool cannot leave a half-built state or an orphaned folder behind. Only
	// preserve mode needs a name, so plain moves keep working without the
	// field. An upgraded daemon serving a browser that still has the previous
	// build's JS cached is the likely source of a miss, so say so.
	folderName := ""
	if req.Preserve {
		var ok bool
		if folderName, ok = moveFolderNames[req.Tool]; !ok {
			writeErr(w, 400, "unknown tool — reload Duplicate Finder, the package was upgraded")
			return
		}
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
	vols := volumeRootsResolved()

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

	refDirs, groups, keepers, cachedEnts := func() ([]string, []Group, map[string]bool, []FileEnt) {
		s.mu.Lock()
		defer s.mu.Unlock()
		k := make(map[string]bool, len(s.keepers))
		for p := range s.keepers {
			k[p] = true
		}
		// Every entry of every cached result: the daemon moves only what a
		// scan surfaced, so these become the move allowlist — and their
		// size/mod/type become the identity the file must still have.
		//
		// A read-only tool's rows are excluded. Otherwise a path that appears
		// ONLY in a corrupted set would be movable through a request naming
		// some other tool, which would make "Conflicting Files never moves
		// anything" true of the button and false of the daemon.
		var ents []FileEnt
		for tool, res := range s.results {
			if readOnlyTools[tool] {
				continue
			}
			ents = append(ents, res.Files...)
			for gi := range res.Groups {
				ents = append(ents, res.Groups[gi].Files...)
			}
		}
		return append([]string{}, s.refDirs...), snapshotGroupsLocked(s.results["duplicates"]), k, ents
	}()

	// Every invariant below compares symlink-resolved paths: a client could
	// otherwise alias a protected reference file or a duplicate-group member
	// through a symlink and slip past the string-prefix guards. Responses
	// still carry the paths as the client sent them.
	canon := newDirResolver()
	canonRefs := make([]string, 0, len(refDirs))
	for _, d := range refDirs {
		canonRefs = append(canonRefs, canon.dir(d))
	}

	// The move allowlist, canonically keyed: a request may name any string,
	// but only paths some scan actually surfaced are movable — the raw API
	// must not double as a generic file mover for anything in the volumes.
	// idents carries what the scan saw at each path (several views when
	// multiple tools surfaced it); the move later refuses a file whose
	// current File Station identity matches none of them.
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
	// then refused below when a held-back copy lives inside it.
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
	exists := groupExistence(sess, groups, requested, canon, idents)
	drops := keepOneDrops(groups, requested, canonRefs, canon, exists)
	// holdsUnder reports whether any held-back path lies inside dir.
	holdsUnder := func(dir string, held map[string]bool) bool {
		for p := range held {
			if strings.HasPrefix(p, dir+"/") {
				return true
			}
		}
		return false
	}

	// In preserve mode the whole batch moves into ONE new folder at the
	// destination, allocated LAZILY and at most ONCE per request.
	//
	// Lazily, because every per-file guard below — the allowlist, keep-one,
	// reference folders, and execMoveFS's own identity and fingerprint checks
	// — refuses before anything is written, and a request whose files are all
	// refused must not leave an empty "Duplicates" behind.
	//
	// Once, because execMoveFS runs per file: allocating there would produce
	// Duplicates, Duplicates (1), Duplicates (2)… one per file, which is the
	// exact scatter this folder exists to prevent. The error is memoized
	// alongside the name, so a destination that refuses folder creation costs
	// one failed attempt for the request rather than one per file.
	//
	// The allocation runs here, inside moveMu, not earlier: probing and
	// creating before the lock would let two overlapping requests both see the
	// name free and end up sharing a folder. moveMu only covers this daemon,
	// so allocBatchFolder still re-probes against outside writers.
	var batchName string
	var batchErr error
	var batchTried bool
	var batchFn func() (string, error)
	if req.Preserve {
		batchFn = func() (string, error) {
			if !batchTried {
				batchTried = true
				batchName, batchErr = sess.allocBatchFolder(destShare, folderName)
			}
			if batchErr != nil {
				return "", batchErr
			}
			return destShare + "/" + batchName, nil
		}
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
		// sits on this iteration for minutes.
		s.setMoveProgress(true, i, len(req.Files), filepath.Base(src))
		if !allowedPath(src) {
			errs = append(errs, map[string]string{"path": src, "error": "outside allowed volumes"})
			continue
		}
		// Pin the source's parent (src itself may be a symlink; the move
		// takes the link, never what it points at) and derive the canonical
		// path from the pinned handle: the guards below AND the File Station
		// move all act on that one canonical path, so the vetted path and
		// the executed path are never two different strings, and a parent
		// swapped for a symlink after the checks cannot redirect the
		// operation to files living elsewhere. (File Station is an external
		// process, so the handle itself cannot carry the move — acting on
		// the handle's canonical path is the strongest available guarantee.)
		parentH, err := dirhandle.Open(filepath.Dir(src))
		if err != nil {
			errs = append(errs, map[string]string{"path": src, "error": "cannot resolve path"})
			continue
		}
		pCanon, err := parentH.Canon()
		if err != nil {
			parentH.Close()
			errs = append(errs, map[string]string{"path": src, "error": "cannot resolve path"})
			continue
		}
		cp := filepath.Join(pCanon, filepath.Base(src))
		var moveErr error
		switch {
		case !isUnder(cp, vols):
			moveErr = errors.New("outside allowed volumes")
		// drops: this request would take a group's last unrequested copy.
		// keepers: an earlier request already dissolved the group — its
		// survivor stays held back until the next duplicates scan.
		case drops[cp] || keepers[cp]:
			moveErr = errors.New("keeping one copy of this duplicate group")
		case isDirIdent(idents[cp]) && (holdsUnder(cp, drops) || holdsUnder(cp, keepers)):
			moveErr = errors.New("keeping one copy of a duplicate group that lives inside this folder")
		case isUnder(cp, canonRefs):
			moveErr = errors.New("read-only reference file")
		// Refused BEFORE File Station is asked, because asking produces "no
		// such file or folder" for a file that plainly exists — a refusal
		// that sends the user looking for a vanished file instead of telling
		// them what is actually true. Checked on the base name of the path
		// being moved, matching what the scan recorded in NoMove.
		case fsCannotAddress(filepath.Base(cp)):
			moveErr = errors.New("DSM's File Station cannot see this file, so it cannot be moved — delete it from the Mac that made it, or move the whole folder that holds it")
		case !allowed[cp]:
			moveErr = errors.New("not part of the current scan results — rescan and try again")
		default:
			base := filepath.Base(src)
			note := func(stage string) {
				s.setMoveProgress(true, i, len(req.Files), base+stage)
			}
			moveErr = execMoveFS(sess, src, cp, destShare, destCanon, idents[cp], batchFn, req.Verify, note)
		}
		parentH.Close()
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
		noteMovedDir := func() {
			if isDirIdent(idents[cp]) {
				movedDirs = append(movedDirs, cp, filepath.Clean(src))
			}
		}
		if moveErr != nil {
			errs = append(errs, map[string]string{"path": src, "error": moveErr.Error()})
			// A failure AFTER File Station completed the move — a verification
			// mismatch, or an entry parked in the staging folder — leaves the
			// source gone: the row must prune like any moved row, or it would
			// linger pointing at a path that no longer exists and count as a
			// phantom keep-one survivor. It stays out of `moved` — the
			// response's error entry is the truth about this entry. A parked
			// DIRECTORY took its contents with it, so its rows beneath prune
			// too.
			var mbe movedButError
			if errors.As(moveErr, &mbe) {
				movedCanon = append(movedCanon, cp)
				noteMovedDir()
				// The toast is transient and the row is about to prune: the
				// daemon log is the durable record of a move that left the
				// source but did not end clean.
				log.Printf("move of %s: %v", src, moveErr)
			}
			continue
		}
		moved = append(moved, src)
		movedCanon = append(movedCanon, cp)
		noteMovedDir()
	}
	// Every file accounted for — report the full count before the response is
	// written, so a poll racing the last file cannot show N-1 of N and then
	// jump straight to "no move running".
	s.setMoveProgress(true, len(req.Files), len(req.Files), "")

	if len(movedCanon) > 0 {
		s.pruneMoved(movedCanon, movedDirs, canon)
		// Pruned rows and new keep-one survivors must survive a restart too,
		// or a reboot would resurrect moved rows and forget protections.
		s.saveState()
	}
	out := map[string]any{"moved": moved, "errors": errs}
	if batchName != "" {
		// Name the folder the files actually landed in: under preserve they
		// are NOT at the path the caller picked, and only the daemon knows
		// which " (n)" variant it ended up allocating. Reported in the same
		// namespace as the request's own paths. Absent when nothing was
		// created — an all-refused request or a failed allocation.
		out["folder"] = filepath.Join(req.Dest, batchName)
	}
	writeJSON(w, 200, out)
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
	isdir := e.IsDir
	if isdir && (destCanon == cp || strings.HasPrefix(destCanon+"/", cp+"/")) {
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
	// Full verification, half one: prove the SOURCE is still exactly the
	// content the scan verified, while it still exists. Where the scan
	// recorded a full hash (duplicates), the fresh hash must equal it —
	// this closes the scan→move window the 64 KiB fingerprint above cannot
	// (rot past the prefix with size and mtime standing). For rows without
	// a recorded hash, the fresh hash becomes the reference the destination
	// must reproduce. Directories are skipped: their move is a rename, and
	// there is no single content to hash. Placed above batch() with every
	// other refusal, so a verify refusal creates nothing.
	preHash := ""
	if verify && !e.IsDir {
		if note != nil {
			note(" — verifying")
		}
		h, herr := verifyRead(cp)
		if herr != nil {
			return errors.New("could not read the file to verify it — nothing was moved")
		}
		if want.hash != "" && h != want.hash {
			return errors.New("file contents changed since the scan — rescan and try again")
		}
		preHash = h
		if note != nil {
			note("")
		}
	}
	outShare := destShare
	if batch != nil {
		// The first side effect in this function, and deliberately the last
		// statement before the move: every refusal above is local and must
		// stay above it.
		root, err := batch()
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(filepath.Dir(src), "/")
		if rel == "" {
			// Unreachable while shareSpacePath refuses volume roots, but a
			// bare join would silently aim the move at the batch folder's
			// own parent — refuse rather than depend on that.
			return errors.New("cannot mirror the folder path of a volume root")
		}
		outShare = filepath.Join(root, rel)
		if err := sess.createFolder(filepath.Dir(outShare), filepath.Base(outShare), true); err != nil {
			return err
		}
	}
	finalName, err := moveViaFS(sess, srcShare, outShare)
	if err != nil {
		// A PARKED outcome means the source is gone and the only copy sits in
		// the destination's staging folder — the exact "moved but" condition:
		// the row must prune, the message must carry where the file is, and
		// when verification was asked for it happens HERE, against the parked
		// copy. The risky transit has already happened; "parked" must never
		// mean "unverified".
		var pe parkedError
		if errors.As(err, &pe) {
			msg := err.Error()
			if verify && !e.IsDir && preHash != "" {
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
		return err
	}
	// Full verification, half two: read the DESTINATION back and require the
	// exact content that left. Within one volume a move is a rename of
	// pointers and this re-reads the same blocks; across volumes or onto a
	// remote mount it is the only proof the data survived the transit. The
	// destination path is derived, never guessed: outShare always extends
	// destShare, so grafting that extension onto the picker's canonical
	// folder plus the name moveViaFS reports (a collision may have forced a
	// " (n)" variant) is exactly where File Station put the file.
	//
	// Both failure modes below happen AFTER the move — the source is gone —
	// so they are reported as movedButError: the row prunes, the message
	// carries the truth. "Could not be read back" is kept distinct from "does
	// not match": the first is almost always the package user lacking read
	// access to the destination share, and telling the user their data was
	// damaged when the daemon merely could not look would be a false alarm
	// with real consequences.
	if verify && !e.IsDir {
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

type ExportReq struct {
	Tool string `json:"tool"`
	Dest string `json:"dest"`
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST required")
		return
	}
	var req ExportReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad request body")
		return
	}
	destH, _, destShare, sess, ok := s.vetDestination(w, r, req.Dest)
	if !ok {
		return
	}
	defer destH.Close()
	// The upload targets the canonical (vetted) destination; destClean only
	// names the written file back to the client in the path form it sent.
	destClean := filepath.Clean(req.Dest)
	res := s.snapshotResult(req.Tool)
	if res == nil {
		writeErr(w, 400, "no results to export — run a scan first")
		return
	}
	name, content, err := exportCSV(req.Tool, res)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// The upload goes through File Station as the logged-in admin; a name
	// collision steps to the next free " (n)" name, never overwriting.
	finalName, err := exportViaFS(sess, destShare, name, content)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"file": filepath.Join(destClean, finalName)})
}

// ------------------------------------------------------------ path helpers

// listVolumes returns the Synology volume roots. In dev builds devVolumeRoots
// can override this (see dev.go); it returns nil in release builds.
//
// External devices are included deliberately: USB and eSATA volumes mount at
// /volumeUSB<n> and /volumeSATA<n>, and moving unwanted files onto an
// external disk is a primary use of the move flow. These globs are the
// security walls every vetted path must stay inside, so widening them widens
// what scans may walk and moves may target — which is exactly the intent.
func listVolumes() []string {
	if roots := devVolumeRoots(); roots != nil {
		return roots
	}
	var matches []string
	for _, pat := range []string{"/volume[0-9]*", "/volumeUSB[0-9]*", "/volumeSATA[0-9]*"} {
		m, _ := filepath.Glob(pat)
		matches = append(matches, m...)
	}
	var out []string
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.IsDir() {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

func allowedPath(p string) bool {
	if p == "" || !filepath.IsAbs(p) {
		return false
	}
	// Clean FIRST, then reject ".." as a path COMPONENT — never as a
	// substring. A substring test would refuse every legitimate name
	// containing an ellipsis or a double dot ("Wait... What.mp4",
	// "Season 1..2"): such a file would be listed by a scan and then
	// refused at move time as "outside allowed volumes", and no folder
	// named that way could be a destination or a scan root. Traversal is
	// still impossible: Clean resolves interior ".." on an absolute path
	// (so "/volume1/../etc" becomes "/etc"), the volume-prefix test below
	// then rejects it, and every caller pairs this with a canonical
	// containment check taken from a pinned directory handle.
	p = filepath.Clean(p)
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return false
		}
	}
	for _, v := range listVolumes() {
		if p == v || strings.HasPrefix(p, v+"/") {
			return true
		}
	}
	return false
}

// volumeRootsResolved returns each allowed volume root in raw and
// symlink-resolved form. Canonical (resolved) paths must be compared against
// the resolved roots too: the roots themselves may sit behind symlinks (dev
// fixtures under macOS /var → /private/var).
func volumeRootsResolved() []string {
	var out []string
	for _, v := range listVolumes() {
		out = append(out, v)
		if rv, err := filepath.EvalSymlinks(v); err == nil && rv != v {
			out = append(out, rv)
		}
	}
	return out
}

// resolveAllowedRoot returns p's symlink-resolved form when p exists and
// stays inside the allowed volumes after resolution. allowedPath alone is a
// string check — a symlink under a volume could point anywhere, and the
// scanner must not follow it outside. The two failures are distinguished so
// callers report the right one: a path that cannot be resolved is "not a
// folder"; a path that resolves outside is a containment refusal.
func resolveAllowedRoot(p string) (string, error) {
	if !allowedPath(p) {
		return "", errors.New("path outside allowed volumes: " + p)
	}
	rp, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", errors.New("not a folder: " + p)
	}
	if !isUnder(rp, volumeRootsResolved()) {
		return "", errors.New("path outside allowed volumes: " + p)
	}
	return rp, nil
}

// dirResolver canonicalizes paths by symlink-resolving their directory part,
// memoized per request. Client-supplied paths, cached scan paths, and
// reference dirs are all compared in this one canonical namespace so a
// symlink alias cannot slip past the prefix-based invariant checks.
type dirResolver struct{ cache map[string]string }

func newDirResolver() *dirResolver { return &dirResolver{cache: map[string]string{}} }

func (r *dirResolver) resolveDir(d string) (string, error) {
	d = filepath.Clean(d)
	if v, ok := r.cache[d]; ok {
		return v, nil
	}
	v, err := filepath.EvalSymlinks(d)
	if err != nil {
		return "", err // failures are not cached: only real resolutions are
	}
	r.cache[d] = v
	return v, nil
}

// dir canonicalizes a directory, falling back to the cleaned input when it
// cannot be resolved, so already-moved/missing paths still compare stably.
func (r *dirResolver) dir(d string) string {
	if v, err := r.resolveDir(d); err == nil {
		return v
	}
	return filepath.Clean(d)
}

// path canonicalizes a file path leniently: resolved parent + original base.
// The base is never resolved — the entry itself may be a symlink that is
// moved as a link, not followed.
func (r *dirResolver) path(p string) string {
	p = filepath.Clean(p)
	return filepath.Join(r.dir(filepath.Dir(p)), filepath.Base(p))
}

// strictPath is path for paths that must be acted on: it fails when the
// parent directory cannot be resolved instead of falling back.
func (r *dirResolver) strictPath(p string) (string, error) {
	p = filepath.Clean(p)
	d, err := r.resolveDir(filepath.Dir(p))
	if err != nil {
		return "", err
	}
	return filepath.Join(d, filepath.Base(p)), nil
}

// skipName filters out Synology/system directories and hidden entries that
// should never be scanned or offered in pickers.
func skipName(name string) bool {
	return strings.HasPrefix(name, "@") || strings.HasPrefix(name, ".") ||
		name == "#recycle" || name == "#snapshot" || name == "lost+found"
}

func uniquePaths(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range in {
		p = filepath.Clean(p)
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func isUnder(p string, roots []string) bool {
	for _, r := range roots {
		if p == r || strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
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
func groupExistence(sess *fsSession, groups []Group, requested map[string]bool, canon *dirResolver, idents map[string][]entIdent) map[string]bool {
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
				if ids := idents[cp]; len(ids) > 0 {
					want[sp] = append(want[sp], ids...)
				} else {
					want[sp] = append(want[sp], identOf(f))
				}
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

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04:05")
}
