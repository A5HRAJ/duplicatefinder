// The report of the most recent move, kept after its request ends. A move is
// one long request, and the browser's connection to it can die while the
// daemon carries on — DSM's web server gives up on long requests, a laptop
// sleeps, a window is closed. The per-file outcome then never reaches the
// app through the request, and among those entries can be the one this whole
// feature exists to deliver: a copy that did not verify. So the daemon keeps
// the report, says on /api/state that one is waiting, and serves it until the
// app confirms it was shown — across a package restart too.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

const moveReportFile = "lastmove.json"

// moveReport is everything POST /api/move answers, plus what the app needs to
// present it on its own: which request it was (the id the app sent), the tool
// and destination it named, and how many entries it asked to move. A report
// is never mutated once published — a change publishes a new one — so a
// reader holds what it read without a lock.
type moveReport struct {
	ID        string              `json:"id"`
	Tool      string              `json:"tool"`
	Dest      string              `json:"dest"`
	Total     int                 `json:"total"`
	Moved     []string            `json:"moved"`
	Errors    []map[string]string `json:"errors"`
	Folder    string              `json:"folder,omitempty"`
	SaveError string              `json:"saveError,omitempty"`
	// Seen is set once the app has shown the report to the user.
	Seen bool `json:"seen"`
}

// publishMoveReport makes r the latest report and writes it to disk. A
// failed write costs only the report's survival across a restart, and is
// logged; the move itself is already done and saved.
func (s *Server) publishMoveReport(r *moveReport) {
	s.reportMu.Lock()
	defer s.reportMu.Unlock()
	s.publishMoveReportLocked(r)
}

// publishMoveReportLocked is publishMoveReport with reportMu held, which
// orders the in-memory report and the file alike: a newer move's report can
// never be overwritten by an older one's acknowledgement.
func (s *Server) publishMoveReportLocked(r *moveReport) {
	s.mu.Lock()
	s.lastMove = r
	s.mu.Unlock()
	dir := s.stateDir()
	if dir == "" {
		return
	}
	err := writeAtomic(filepath.Join(dir, moveReportFile), 0o600, false, func(tmp *os.File) error {
		b, err := marshalBytesafe(r)
		if err != nil {
			return err
		}
		_, err = tmp.Write(b)
		return err
	})
	if err != nil {
		log.Printf("move report not saved: %v", err)
	}
}

// loadMoveReport restores the latest report at daemon start. A missing or
// unreadable file means no report, never an invented one.
func (s *Server) loadMoveReport() {
	dir := s.stateDir()
	if dir == "" {
		return
	}
	f, err := os.Open(filepath.Join(dir, moveReportFile))
	if err != nil {
		return
	}
	defer f.Close()
	var r moveReport
	if err := decodeBytesafe(f, &r); err != nil || r.ID == "" {
		return
	}
	s.mu.Lock()
	s.lastMove = &r
	s.mu.Unlock()
}

// handleMoveReport serves the report kept for a move (GET ?id=) and records
// that the app has shown it (POST {"id"}). Any other id — or none — answers
// found=false: the app asked about a move whose report has since been
// replaced, or one that never had an id.
func (s *Server) handleMoveReport(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		id := r.URL.Query().Get("id")
		s.mu.Lock()
		rep := s.lastMove
		s.mu.Unlock()
		if rep == nil || id == "" || rep.ID != id {
			writeJSON(w, 200, map[string]any{"found": false})
			return
		}
		writeJSON(w, 200, map[string]any{"found": true, "report": rep})
	case http.MethodPost:
		var body struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
			writeErr(w, 400, "bad request body")
			return
		}
		s.reportMu.Lock()
		s.mu.Lock()
		rep := s.lastMove
		s.mu.Unlock()
		if rep != nil && rep.ID == body.ID && !rep.Seen {
			seen := *rep
			seen.Seen = true
			s.publishMoveReportLocked(&seen)
		}
		s.reportMu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		writeErr(w, 405, "GET or POST required")
	}
}
