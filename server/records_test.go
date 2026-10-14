package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recordsServer is a server whose state dir holds one save carrying move
// records: a moved copy and a keep-one survivor.
func recordsServer(t *testing.T) (string, *Server) {
	t.Helper()
	dir := t.TempDir()
	s := &Server{varDir: dir, results: map[string]*toolResult{},
		moveOut: moveOutSet{}.with([]string{"/volume1/out/a.jpg"}),
		keepers: map[string]bool{"/volume1/photos/b.jpg": true}}
	if err := s.saveState(); err != nil {
		t.Fatal(err)
	}
	return dir, s
}

func damage(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("\x00\x01 not what was written"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func reload(dir string) *Server {
	s := &Server{varDir: dir, results: map[string]*toolResult{}}
	s.loadState()
	return s
}

func hasRecords(s *Server) bool {
	return s.moveOut["/volume1/out/a.jpg"] && s.keepers["/volume1/photos/b.jpg"]
}

// The records live in their own file, and a build that kept them only in the
// results file is read too: an upgrade loses nothing and warns about nothing.
func TestRecordsSurviveAndUpgradeFromResultsOnly(t *testing.T) {
	dir, _ := recordsServer(t)
	if _, err := os.Stat(filepath.Join(dir, recordsFile)); err != nil {
		t.Fatalf("the records file was not written: %v", err)
	}
	os.Remove(filepath.Join(dir, recordsFile)) // as an older build left it
	s := reload(dir)
	if !hasRecords(s) || s.recordsWarn != "" {
		t.Fatalf("records from the results file alone must load silently: out=%v keepers=%v warn=%q", s.moveOut, s.keepers, s.recordsWarn)
	}
}

// A records file that cannot be read is set aside, never overwritten, and the
// results file — saved after it, from the same snapshot — supplies everything.
func TestDamagedRecordsRecoveredFromResults(t *testing.T) {
	dir, _ := recordsServer(t)
	damage(t, filepath.Join(dir, recordsFile))
	s := reload(dir)
	if !hasRecords(s) || s.recordsWarn != "" {
		t.Fatalf("records must be recovered from the results file without a warning: warn=%q", s.recordsWarn)
	}
	if b, err := os.ReadFile(filepath.Join(dir, recordsFile+damagedSuffix)); err != nil || !strings.Contains(string(b), "not what was written") {
		t.Fatalf("the unreadable file must be kept aside (%v)", err)
	}
	if r, _, err := readRecords(filepath.Join(dir, recordsFile)); err != nil || len(r.MoveOut) != 1 {
		t.Fatalf("a good records file must be written back at once (%v)", err)
	}
}

// A results file that cannot be read is set aside rather than overwritten by
// the next save, and the records file still supplies the records.
func TestDamagedResultsSetAsideRecordsKept(t *testing.T) {
	dir, _ := recordsServer(t)
	damage(t, filepath.Join(dir, stateFile))
	s := reload(dir)
	if !hasRecords(s) || s.recordsWarn != "" {
		t.Fatalf("the records file alone must carry the records: warn=%q", s.recordsWarn)
	}
	if _, err := os.Stat(filepath.Join(dir, stateFile+damagedSuffix)); err != nil {
		t.Fatalf("the unreadable results file must be kept aside (%v)", err)
	}
}

// With both the newest records and the results unreadable, the generation
// before is used — and the user is told it is one move behind.
func TestPreviousGenerationUsedWithAWarning(t *testing.T) {
	dir, s := recordsServer(t)
	s.mu.Lock()
	s.moveOut = s.moveOut.with([]string{"/volume1/out/newest.jpg"})
	s.mu.Unlock()
	if err := s.saveState(); err != nil {
		t.Fatal(err)
	}
	damage(t, filepath.Join(dir, recordsFile))
	damage(t, filepath.Join(dir, stateFile))
	s2 := reload(dir)
	if !hasRecords(s2) {
		t.Fatal("the previous generation's records must be used")
	}
	if s2.moveOut["/volume1/out/newest.jpg"] {
		t.Fatal("the previous generation cannot hold the newest move")
	}
	if !strings.Contains(s2.recordsWarn, "used the one saved before it") {
		t.Fatalf("the user must be told the records are one move behind, got %q", s2.recordsWarn)
	}
}

// Nothing readable at all: the loss is announced, the announcement survives
// restarts, and only the user's acknowledgement ends it.
func TestLostRecordsWarnUntilAcknowledged(t *testing.T) {
	dir, s := recordsServer(t)
	if err := s.saveState(); err != nil { // a second save, so a previous generation exists
		t.Fatal(err)
	}
	for _, f := range []string{recordsFile, recordsPrev, stateFile} {
		damage(t, filepath.Join(dir, f))
	}
	s2 := reload(dir)
	if !strings.Contains(s2.recordsWarn, "could not read its record") {
		t.Fatalf("a total loss must be announced, got %q", s2.recordsWarn)
	}
	st := getJSON(t, s2.handleState, "GET", "/api/state", "")
	if w, _ := st["recordsWarning"].(string); w == "" {
		t.Fatal("/api/state must carry the warning")
	}
	s3 := reload(dir)
	if s3.recordsWarn == "" {
		t.Fatal("the warning must survive a restart until it is acknowledged")
	}
	rec := httptest.NewRecorder()
	s3.handleRecordsAck(rec, httptest.NewRequest("POST", "/api/records/ack", strings.NewReader("{}")))
	if rec.Code != 200 || s3.recordsWarn != "" {
		t.Fatalf("ack: code %d warn %q", rec.Code, s3.recordsWarn)
	}
	if s4 := reload(dir); s4.recordsWarn != "" {
		t.Fatalf("an acknowledged warning must stay gone, got %q", s4.recordsWarn)
	}
}

// The previous generation is only ever a file that read back whole: a
// damaged newest file is not copied over a good fallback.
func TestPreviousGenerationIsNeverADamagedFile(t *testing.T) {
	dir, s := recordsServer(t)
	if err := s.saveState(); err != nil { // a good previous generation now exists
		t.Fatal(err)
	}
	damage(t, filepath.Join(dir, recordsFile))
	if err := s.saveState(); err != nil {
		t.Fatal(err)
	}
	if r, _, err := readRecords(filepath.Join(dir, recordsPrev)); err != nil || r == nil || len(r.MoveOut) != 1 {
		t.Fatalf("a damaged file was rotated into the fallback: %v", err)
	}
}

// A fresh install has nothing to lose and nothing to say.
func TestFreshInstallHasNoWarning(t *testing.T) {
	s := reload(t.TempDir())
	if s.recordsWarn != "" {
		t.Fatalf("got %q", s.recordsWarn)
	}
}
