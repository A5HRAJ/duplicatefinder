// The daemon: the Server state, its locks and the rules for what may run
// when, startup (bind, token, ready file, restored results), the daemon
// token middleware, the rotating log and the JSON response helpers.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

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
	saveErr   string                 // the last failed state save, for /api/state; cleared by a successful save
	// recordsWarn says earlier move records could not be read back (records.go);
	// shown until the user acknowledges it, and persisted until then.
	recordsWarn string
	bandLo      float64 // the progress band the running pass reports into (setBand)
	bandHi      float64
	nextID      int
	authToken   string // shared secret required on /api/*; "" disables (dev/tests)

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
	// moveOut records where moves put things (moveout.go), so a moved copy
	// is never taken for the copy that stays. Replaced, never mutated, under
	// mu — written by handleMove under moveMu — so a reader holds the map
	// it read without a lock.
	moveOut moveOutSet
	// lastMove is the report of the most recent move (movereport.go), kept
	// until the app has shown it. Replaced, never mutated, under mu;
	// reportMu orders its replacements and their writes to disk.
	lastMove *moveReport
	reportMu sync.Mutex
	// stopping is set when the daemon has been asked to stop (SIGTERM from
	// the package's stop script, DSM shutting down, a package upgrade). A
	// move batch in progress finishes the file it is on, reports the rest as
	// not attempted, and saves what moved, instead of being killed with its
	// bookkeeping half-written.
	stopping atomic.Bool
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
	Running  bool     `json:"running"`
	Tool     string   `json:"tool"`
	Tools    []string `json:"tools,omitempty"` // every tool this scan runs
	Progress float64  `json:"progress"`
	Label    string   `json:"label"`
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
	ID      string `json:"id"`    // the request's id, under which its report is kept
}

// scanEnd records how the most recent scan run ended, whatever the outcome.
// lastTool names only the last scan that COMPLETED, so on its own a poller
// that sees a run stop cannot tell a cancel from a finish, and would replay
// the previous finish's announcements after a Stop.
type scanEnd struct {
	Tool      string
	Tools     []string // every tool the run scanned
	Completed bool
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
	mux.HandleFunc("/api/refs", s.handleRefs)
	mux.HandleFunc("/api/cancel", s.handleCancel)
	mux.HandleFunc("/api/results", s.handleResults)
	mux.HandleFunc("/api/move", s.handleMove)
	mux.HandleFunc("/api/move/report", s.handleMoveReport)
	mux.HandleFunc("/api/records/ack", s.handleRecordsAck)
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
	s.loadMoveReport()
	s.replayMoveJournal()
	s.interrupted = s.loadMarker()
	log.Printf("Duplicate Finder %s listening on %s", appVersion, addr)
	srv := newHTTPServer(handler)
	// The package's stop script sends SIGTERM and, ten seconds later, SIGKILL.
	// Without a handler the first signal ended the process on the spot, mid-
	// write if that is where it was. Now the listener closes, requests in
	// flight get shutdownGrace to finish — a move batch stops starting files
	// (s.stopping) and saves what it moved — and the process exits cleanly. A
	// scan in flight is left to die with the process on purpose: its marker
	// stays on disk, which is exactly what makes the next start offer to
	// resume it.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	done := make(chan struct{})
	go func() {
		sig := <-stop
		log.Printf("stopping (%v)", sig)
		s.stopping.Store(true)
		ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("stopped with a request still running: %v", err)
		}
		close(done)
	}()
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-done
	log.Printf("stopped")
}

// shutdownGrace is how long requests in flight get after a stop signal —
// inside the ten seconds the package's stop script allows before it kills.
const shutdownGrace = 8 * time.Second

// newHTTPServer is the daemon's server, with the timeouts a server that is
// never meant to be left holding a dead connection needs — and only those:
// a read or write deadline on the whole request would cut off a 64 MiB move
// list or a move batch that legitimately runs for hours, so the header
// timeout guards the connection setup and the idle timeout reaps keep-alive
// connections nobody is using.
func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
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

// maxRequestBody bounds every request body the daemon (and the CGI proxy in
// front of it) will read. A move naming 100,000 files is a few megabytes; no
// request the app sends comes near this, and decoding an arbitrary one would
// cost the daemon memory in the middle of a scan or a move.
const maxRequestBody = 64 << 20

// withAuth requires the shared token on every API request and bounds the
// body. Without a token configured (dev/test runs without -var) the token
// check is a passthrough, so the local dev flow and the smoke suite are
// unaffected.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authToken != "" &&
			subtle.ConstantTimeCompare([]byte(r.Header.Get(authTokenHeader)), []byte(s.authToken)) != 1 {
			writeErr(w, http.StatusUnauthorized, "missing or invalid daemon token")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		next.ServeHTTP(w, r)
	})
}

// ------------------------------------------------------------ http helpers

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	// Encoded before the status line goes out: an encoding failure is then a
	// 500 with a reason, not a 200 with an empty body.
	b, err := marshalBytesafe(v)
	if err != nil {
		code, b = 500, []byte(`{"error":"internal error: response could not be encoded"}`)
	}
	w.WriteHeader(code)
	w.Write(append(b, '\n'))
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
