// The move journal: what the batch in flight has done so far, written to disk
// as it happens. A batch saves its records, prunes its rows and keeps its
// report only once it ends, and a daemon told to stop gives a request in
// flight a few seconds — a large file being copied or verified outlasts
// them, and the process exits mid-batch. Without the journal, every file the
// batch had already moved was then forgotten: a copy moved straight into a
// folder the user picked would read as an original at the next scan, and the
// last real original could follow it there. With it, the next start finishes
// the bookkeeping (replayMoveJournal) and tells the user what happened.
//
// Every step that can put an entry somewhere is preceded by a line naming
// where, flushed to disk before File Station is asked — File Station carries
// on with a move after the daemon is gone, so the record has to come first.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const journalFile = "moves.journal"

// journalLine is one line of the journal. Exactly one of Begin, Start, Ahead,
// Staging, Placed, Done or End is set.
type journalLine struct {
	// Begin opens a batch: the request it serves.
	Begin *journalBegin `json:"begin,omitempty"`
	// Start names the entry the batch is now working on (as requested).
	Start string `json:"start,omitempty"`
	// Ahead is where the entry is about to land (canonical), written before
	// File Station is asked to put it there. From is where it comes from.
	Ahead string   `json:"ahead,omitempty"`
	From  []string `json:"from,omitempty"`
	// Staging is the hidden staging folder (canonical) the entry is about to
	// be moved or copied into.
	Staging string `json:"staging,omitempty"`
	// Placed is where the entry now is (canonical); what remains is
	// verification or retiring the original.
	Placed string `json:"placed,omitempty"`
	// Where is the share-space form of Ahead, Staging or Placed — the path
	// the user knows from File Station, for the report.
	Where string `json:"where,omitempty"`
	// Done ends an entry (as requested): whether it moved, its error, what it
	// added to the move records, and the paths that left their place.
	Done    string   `json:"done,omitempty"`
	Moved   bool     `json:"moved,omitempty"`
	Err     string   `json:"err,omitempty"`
	MoveOut []string `json:"moveOut,omitempty"`
	Gone    []string `json:"gone,omitempty"`
	Dirs    []string `json:"dirs,omitempty"`
	Folder  string   `json:"folder,omitempty"`
	// End closes a batch whose outcome reached the records and its report.
	End bool `json:"end,omitempty"`
}

// journalBegin is the request a batch serves, enough to report it.
type journalBegin struct {
	ID    string   `json:"id"`
	Tool  string   `json:"tool"`
	Dest  string   `json:"dest"`
	Files []string `json:"files"`
}

// moveJournal appends to the journal file. A nil journal (persistence off)
// records nothing and never fails.
type moveJournal struct {
	path string
	f    *os.File
}

// openJournal opens the journal for appending. A journal left by a batch
// whose end could not be saved is appended to, not replaced: it still holds
// records nothing else does.
func openJournal(dir string) (*moveJournal, error) {
	if dir == "" {
		return nil, nil
	}
	path := filepath.Join(dir, journalFile)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	return &moveJournal{path: path, f: f}, nil
}

// write appends one line; durable also flushes it to disk before returning,
// for every line a later start would need to protect a file.
func (j *moveJournal) write(l journalLine, durable bool) error {
	if j == nil {
		return nil
	}
	b, err := marshalBytesafe(&l)
	if err != nil {
		return err
	}
	if _, err := j.f.Write(append(b, '\n')); err != nil {
		return err
	}
	if durable {
		return j.f.Sync()
	}
	return nil
}

// finish closes the journal once the batch has ended. saved says its
// outcome reached the records: everything the journal holds is then in the
// saved state (the daemon applies each line as it writes it, and replays
// any journal it starts with), so the file goes. Otherwise the batch is
// marked ended and the file stays, for the next start to replay.
func (j *moveJournal) finish(saved bool) {
	if j == nil {
		return
	}
	if !saved {
		if err := j.write(journalLine{End: true}, true); err != nil {
			log.Printf("move journal: %v", err)
		}
		j.f.Close()
		return
	}
	j.f.Close()
	if err := os.Remove(j.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("move journal could not be removed: %v", err)
	}
}

// journalWriteError is the refusal for an entry whose record could not be
// written: nothing may move that the app could forget it moved.
func journalWriteError(err error) error {
	return fmt.Errorf("not moved — Duplicate Finder could not write its record of this move (%v)", err)
}

// fileTrack journals the one entry being moved. It rides on the request's
// fsSession, so the move paths deep in fsapi.go and transit.go record each
// step where it happens. Its methods are no-ops on a nil track.
type fileTrack struct {
	j       *moveJournal
	src, cp string
	// destShare/destCanon map a share-space path under the destination onto
	// its canonical filesystem path.
	destShare, destCanon string
}

func (t *fileTrack) fsPath(share string) string {
	return filepath.Join(t.destCanon, strings.TrimPrefix(share, t.destShare))
}

// ahead records that File Station is about to put the entry at share. A
// failure is returned, and the caller must not go on with the move.
func (t *fileTrack) ahead(share string) error {
	if t == nil {
		return nil
	}
	if err := t.j.write(journalLine{Ahead: t.fsPath(share), From: []string{t.cp, t.src}, Where: share}, true); err != nil {
		return journalWriteError(err)
	}
	return nil
}

// staging records that File Station is about to move or copy the entry into
// the hidden staging folder share.
func (t *fileTrack) staging(share string) error {
	if t == nil {
		return nil
	}
	if err := t.j.write(journalLine{Staging: t.fsPath(share), Where: share}, true); err != nil {
		return journalWriteError(err)
	}
	return nil
}

// placed records that the entry is now at share. It has already happened,
// so a failure to write is only logged: the batch's own save still records
// it if the daemon lives to the end.
func (t *fileTrack) placed(share string) {
	if t == nil {
		return
	}
	if err := t.j.write(journalLine{Placed: t.fsPath(share), Where: share}, true); err != nil {
		log.Printf("move journal: %v", err)
	}
}

// inflight is the entry a batch was working on when its journal ends.
type inflight struct {
	src                   string
	aheads, from, placed  []string
	staging, where, phase string
}

// settle returns the move records an entry with no outcome must add: its
// outcome is unknown, so — as for a move whose answer was lost — wherever it
// was about to land, and wherever it was placed, holds a copy this app put
// there, and where it came from is an original.
func (in *inflight) settle() []string {
	var out []string
	for _, p := range append(append([]string{}, in.aheads...), in.placed...) {
		out = append(out, p)
		out = append(out, landedData(p)...)
	}
	for _, p := range in.from {
		if p != "" {
			out = append(out, fromKey+p)
		}
	}
	return out
}

// report is what the batch's report says about the entry.
func (in *inflight) report() string {
	switch in.phase {
	case "placed":
		msg := "Duplicate Finder stopped while finishing this move. The moved copy is at " + in.where + " — check whether the original is still in its folder before deleting anything"
		if in.staging != "" {
			msg += " (if it is, it may carry a hidden name beginning \".dupfinder-del-\")"
		}
		return msg
	case "ahead":
		return "Duplicate Finder stopped while File Station was moving this, and File Station may have finished on its own: if it is no longer in its folder, it is at " + in.where
	case "staging":
		return "Duplicate Finder stopped while File Station was moving or copying this into a hidden folder at the destination: if it is no longer in its folder, it is in " + in.where + "; if it is, any copy left there can be deleted"
	}
	return "Duplicate Finder stopped before moving this — it is still where it was"
}

// readJournal reads the journal's lines. A last line without its newline
// was cut off mid-write and is ignored; any other line that cannot be read
// makes the journal damaged.
func readJournal(path string) (lines []journalLine, exists, damaged bool) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, false
	}
	if err != nil {
		log.Printf("move journal unreadable: %v", err)
		return nil, true, true
	}
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			break // cut off mid-write
		}
		var l journalLine
		if err := decodeBytesafe(bytes.NewReader(b[:i]), &l); err != nil {
			damaged = true
		} else {
			lines = append(lines, l)
		}
		b = b[i+1:]
	}
	return lines, true, damaged
}

// replayMoveJournal finishes, at startup, the bookkeeping of a batch the
// daemon did not live to end: the records of what it moved, the rows that
// left, and its report — what moved, what was in flight and where to look
// for it, what was never reached. Called after loadState and loadMoveReport.
func (s *Server) replayMoveJournal() {
	dir := s.stateDir()
	if dir == "" {
		return
	}
	path := filepath.Join(dir, journalFile)
	lines, exists, damaged := readJournal(path)
	if !exists {
		return
	}
	var out, gone, dirs []string
	var begin *journalBegin
	ended := false
	var cur *inflight
	done := map[string]journalLine{}
	folder := ""
	for _, l := range lines {
		switch {
		case l.Begin != nil:
			if cur != nil {
				out = append(out, cur.settle()...)
			}
			begin, ended, cur, done, folder = l.Begin, false, nil, map[string]journalLine{}, ""
		case l.Start != "":
			if cur != nil {
				out = append(out, cur.settle()...)
			}
			cur = &inflight{src: l.Start}
		case l.Ahead != "" && cur != nil:
			cur.aheads = append(cur.aheads, l.Ahead)
			cur.from = l.From
			cur.phase, cur.where = "ahead", l.Where
		case l.Staging != "" && cur != nil:
			cur.staging = l.Staging
			cur.phase, cur.where = "staging", l.Where
		case l.Placed != "" && cur != nil:
			cur.placed = append(cur.placed, l.Placed)
			cur.phase, cur.where = "placed", l.Where
		case l.Done != "":
			out = append(out, l.MoveOut...)
			gone = append(gone, l.Gone...)
			dirs = append(dirs, l.Dirs...)
			if l.Folder != "" {
				folder = l.Folder
			}
			done[l.Done] = l
			cur = nil
		case l.End:
			ended = true
		}
	}
	var unsettled *inflight
	if cur != nil {
		out = append(out, cur.settle()...)
		unsettled = cur
	}
	if len(out) > 0 {
		s.mu.Lock()
		s.moveOut = s.moveOut.with(out)
		s.mu.Unlock()
	}
	if len(gone) > 0 {
		s.pruneMoved(gone, dirs, nil)
	}
	if damaged {
		s.mu.Lock()
		s.recordsWarn = recordsLostWarning(false)
		s.mu.Unlock()
		setAside(path)
	}
	if begin != nil && !ended {
		s.mu.Lock()
		reported := s.lastMove != nil && s.lastMove.ID == begin.ID
		s.mu.Unlock()
		if !reported {
			s.publishMoveReport(interruptedReport(begin, done, unsettled, folder))
		}
	}
	if err := s.saveState(); err != nil {
		s.noteSaveError(err)
		return // the journal stays: it holds what the saved state does not
	}
	if !damaged {
		if err := os.Remove(path); err != nil {
			log.Printf("move journal could not be removed: %v", err)
		}
	}
	log.Printf("move journal replayed: a move was cut short when Duplicate Finder stopped")
}

// interruptedReport is the report of a batch cut short, in request order.
func interruptedReport(b *journalBegin, done map[string]journalLine, cur *inflight, folder string) *moveReport {
	id := b.ID
	if id == "" {
		id = fmt.Sprintf("interrupted-%d", time.Now().UnixNano())
	}
	rep := &moveReport{ID: id, Tool: b.Tool, Dest: b.Dest, Total: len(b.Files), Moved: []string{},
		Errors: []map[string]string{}, Folder: folder, Interrupted: true}
	for _, src := range b.Files {
		src = filepath.Clean(src)
		if l, ok := done[src]; ok {
			if l.Moved {
				rep.Moved = append(rep.Moved, src)
			} else {
				rep.Errors = append(rep.Errors, map[string]string{"path": src, "error": l.Err})
			}
			continue
		}
		if cur != nil && cur.src == src {
			rep.Errors = append(rep.Errors, map[string]string{"path": src, "error": cur.report()})
			continue
		}
		rep.Errors = append(rep.Errors, map[string]string{"path": src, "error": "not attempted — Duplicate Finder stopped before reaching it; it is still where it was"})
	}
	return rep
}
