// The move records — which copies Duplicate Finder moved (moveOut) and which
// in-place copies of dissolved groups must stay (keepers) — kept in a small
// file of their own. Results can be rebuilt by scanning again; these cannot:
// once forgotten, a copy moved into a folder the user picked reads as an
// original, and the last real original may follow it there. So they are
// written in three places — this file, the copy it replaces (the previous
// generation), and the results file — and a file that cannot be read is set
// aside rather than overwritten, so nothing is lost to a later save. When no
// copy can be read, the app is told, and keeps saying so until the user has
// seen it.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	recordsFile = "moves.json"
	// recordsPrev is the generation recordsFile replaced: a fallback, one
	// save behind, for a newest file that cannot be read.
	recordsPrev = recordsFile + ".prev"
	// damagedSuffix names a file set aside because it could not be read. One
	// per file: a later damaged copy replaces an earlier one.
	damagedSuffix = ".damaged"
)

// persistedRecords is the schema of recordsFile.
type persistedRecords struct {
	Schema  int      `json:"schema"`
	Keepers []string `json:"keepers"`
	MoveOut []string `json:"moveOut"`
	// Warning is a loss of earlier records the user has not yet
	// acknowledged; it is carried across restarts until they have.
	Warning string `json:"warning,omitempty"`
	SavedAt string `json:"savedAt"`
}

// readRecords reads one records file. exists reports that the file is there
// at all, so a damaged file is told apart from a missing one.
func readRecords(path string) (rec *persistedRecords, exists bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	var r persistedRecords
	if err := decodeBytesafe(bytes.NewReader(b), &r); err != nil {
		return nil, true, err
	}
	if r.Schema != 1 {
		return nil, true, fmt.Errorf("unknown schema %d", r.Schema)
	}
	return &r, true, nil
}

// saveRecords writes the records, keeping the generation it replaces as
// recordsPrev — but only a generation that reads back whole, so the fallback
// is never a damaged file. Both steps are atomic, so a crash at any point
// leaves a readable newest file or a readable previous one.
func saveRecords(dir string, r *persistedRecords) error {
	path := filepath.Join(dir, recordsFile)
	if cur, err := os.ReadFile(path); err == nil {
		var probe persistedRecords
		if decodeBytesafe(bytes.NewReader(cur), &probe) == nil && probe.Schema == 1 {
			if err := writeAtomic(filepath.Join(dir, recordsPrev), 0o600, false, func(f *os.File) error {
				_, err := f.Write(cur)
				return err
			}); err != nil {
				return err
			}
		}
	}
	b, err := marshalBytesafe(r)
	if err != nil {
		return err
	}
	return writeAtomic(path, 0o600, false, func(f *os.File) error {
		_, err := f.Write(b)
		return err
	})
}

// setAside renames a file that could not be read out of the way, so the next
// save cannot overwrite it: it may hold what a repair could still recover.
func setAside(path string) {
	if err := os.Rename(path, path+damagedSuffix); err != nil {
		log.Printf("%s could not be read and could not be set aside: %v", filepath.Base(path), err)
		return
	}
	log.Printf("%s could not be read; kept as %s", filepath.Base(path), filepath.Base(path)+damagedSuffix)
}

// recordsLostWarning is what the app shows when no readable copy of the move
// records survived. stale says an older generation was used.
func recordsLostWarning(stale bool) string {
	what := "Duplicate Finder could not read its record of the copies it moved earlier"
	if stale {
		what = "Duplicate Finder could not read its latest record of the copies it moved, and used the one saved before it — the copies from your most recent move are not in it"
	}
	return what + ". A copy it moved straight into a folder you picked (\"Move all files into same folder\") may now be treated as an original; copies inside its own \"Duplicates\"-style folders are still recognised. Before moving the last copy of anything, check the folders your earlier moves went to. The unreadable record has been kept in the package's data folder."
}

// loadRecords settles the move records at startup from every copy there is:
// the records file (or, when it cannot be read, the generation before it)
// and whatever the results file carried (fromResults; nil when it could not
// be read). The copies are merged — each only ever adds protection — and a
// file that cannot be read is set aside. It returns the warning to show, and
// whether anything changed on disk that a save must now make durable.
func loadRecords(dir string, fromResults *persistedRecords, resultsDamaged bool) (merged persistedRecords, warning string, mustSave bool) {
	merged.Schema = 1
	add := func(r *persistedRecords) {
		if r == nil {
			return
		}
		merged.Keepers = append(merged.Keepers, r.Keepers...)
		merged.MoveOut = append(merged.MoveOut, r.MoveOut...)
		if r.Warning != "" {
			warning = r.Warning
		}
	}
	cur, curExists, curErr := readRecords(filepath.Join(dir, recordsFile))
	add(fromResults)
	if curErr == nil {
		add(cur)
		return merged, warning, false
	}
	if curExists {
		log.Printf("%s unreadable: %v", recordsFile, curErr)
		setAside(filepath.Join(dir, recordsFile))
		mustSave = true
	}
	prev, prevExists, prevErr := readRecords(filepath.Join(dir, recordsPrev))
	if prevErr == nil {
		add(prev)
	} else if prevExists {
		log.Printf("%s unreadable: %v", recordsPrev, prevErr)
		setAside(filepath.Join(dir, recordsPrev))
		mustSave = true
	}
	// The results file is saved after the records file, from the same
	// snapshot, so when it was read it carries everything the newest records
	// file did. Without it, whatever is missing is missing for good.
	if fromResults == nil && (curExists || prevExists || resultsDamaged) {
		warning = recordsLostWarning(prevErr == nil && prev != nil)
		mustSave = true
	}
	return merged, warning, mustSave
}

// handleRecordsAck is POST /api/records/ack: the user has seen the warning
// that earlier move records were lost, so it is no longer shown.
func (s *Server) handleRecordsAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST required")
		return
	}
	io.Copy(io.Discard, io.LimitReader(r.Body, 1<<10))
	s.mu.Lock()
	had := s.recordsWarn != ""
	s.recordsWarn = ""
	s.mu.Unlock()
	if had {
		if err := s.saveState(); err != nil {
			s.noteSaveError(err)
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// nowStamp is the SavedAt form every state file uses.
func nowStamp() string { return time.Now().Format(time.RFC3339) }
