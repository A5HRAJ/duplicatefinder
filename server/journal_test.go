package main

// The move journal (journal.go): every step that can put a file somewhere is
// on disk before File Station is asked, and a batch the daemon did not live
// to end is finished at the next start — its records, its pruned rows and a
// report of what was in flight.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func journalLines(t *testing.T, dir string) []journalLine {
	t.Helper()
	lines, _, _ := readJournal(filepath.Join(dir, journalFile))
	return lines
}

// tracked attaches a journal in dir to sess for one entry, as handleMove does.
func tracked(t *testing.T, f *moveVerifyFake, sess *fsSession, dir, src string) *moveJournal {
	t.Helper()
	j, err := openJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.f.Close() })
	sess.track = &fileTrack{j: j, src: src, cp: src, destShare: "/Backups/dest", destCanon: filepath.Join(f.root, "dest")}
	return j
}

func TestMoveRecordsWhereItWillLandBeforeFileStationMovesIt(t *testing.T) {
	f, sess := newMoveVerifyFake(t)
	dir := t.TempDir()
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	p, want, _ := mkMoveFile(t, f, "a.bin")
	tracked(t, f, sess, dir, p)
	var atMove []journalLine
	f.afterMove = func(string) { atMove = journalLines(t, dir) }
	if err := execMoveFS(sess, p, p, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	land := filepath.Join(f.root, "dest", "a.bin")
	for _, l := range atMove {
		if l.Ahead == land && len(l.From) == 2 && l.From[0] == p {
			return
		}
	}
	t.Fatalf("when File Station moved the file, the journal did not say where it would land: %+v", atMove)
}

func TestTransitRecordsTheStagingCopyAndWhereItWasPlaced(t *testing.T) {
	forceTransit(t)
	f, sess := newMoveVerifyFake(t)
	dir := t.TempDir()
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	p, want, _ := mkMoveFile(t, f, "a.bin")
	tracked(t, f, sess, dir, p)
	var atCopy []journalLine
	f.afterCopy = func(string) { atCopy = journalLines(t, dir) }
	at, err := execMoveFSAt(sess, p, p, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(atCopy) == 0 || atCopy[len(atCopy)-1].Staging == "" {
		t.Fatalf("the copy into the staging folder was not recorded before it was made: %+v", atCopy)
	}
	placed := false
	for _, l := range journalLines(t, dir) {
		placed = placed || l.Placed == at
	}
	if !placed {
		t.Fatalf("where the verified copy was placed (%s) is not in the journal: %+v", at, journalLines(t, dir))
	}
}

// cutShortJournal writes what a batch of three leaves when the daemon stops
// during its second file: a moved, b on its way, c never reached.
func cutShortJournal(t *testing.T, dir, a, b, c, dest string) {
	t.Helper()
	j, err := openJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer j.f.Close()
	destA, destB := filepath.Join(dest, filepath.Base(a)), filepath.Join(dest, filepath.Base(b))
	for _, l := range []journalLine{
		{Begin: &journalBegin{ID: "m1", Tool: "duplicates", Dest: dest, Files: []string{a, b, c}}},
		{Start: a},
		{Ahead: destA, From: []string{a, a}, Where: "/share/dest/a"},
		{Done: a, Moved: true, MoveOut: []string{destA, fromKey + a}, Gone: []string{a, a}},
		{Start: b},
		{Ahead: destB, From: []string{b, b}, Where: "/share/dest/b.txt"},
	} {
		if err := j.write(l, true); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReplayFinishesABatchCutShortByAStop(t *testing.T) {
	dir := t.TempDir()
	root, _ := filepath.EvalSymlinks(t.TempDir()) // keepers are kept canonical
	a, b, c, x := filepath.Join(root, "a.txt"), filepath.Join(root, "b.txt"), filepath.Join(root, "c.txt"), filepath.Join(root, "x.txt")
	dest := filepath.Join(root, "dest")
	s := &Server{varDir: dir, results: map[string]*toolResult{"duplicates": {Groups: []Group{{Hash: "h",
		Files: []FileEnt{{Dir: root, Name: "a.txt", Pfx: "p"}, {Dir: root, Name: "x.txt", Pfx: "p"}}}}}}}
	if err := s.saveState(); err != nil {
		t.Fatal(err)
	}
	cutShortJournal(t, dir, a, b, c, dest)

	s = reload(dir)
	s.replayMoveJournal()
	destA, destB := filepath.Join(dest, "a.txt"), filepath.Join(dest, "b.txt")
	for _, k := range []string{destA, fromKey + a, destB, fromKey + b} {
		if !s.moveOut[k] {
			t.Errorf("move record %q missing after the replay", k)
		}
	}
	if g := s.results["duplicates"].Groups; len(g) != 0 {
		t.Errorf("the moved file's group was not pruned: %+v", g)
	}
	if !s.keepers[x] {
		t.Errorf("the dissolved group's survivor is not held back: %v", s.keepers)
	}
	r := s.lastMove
	if r == nil || r.ID != "m1" || !r.Interrupted {
		t.Fatalf("no interrupted report for the batch: %+v", r)
	}
	if len(r.Moved) != 1 || r.Moved[0] != a {
		t.Errorf("moved list: %v", r.Moved)
	}
	if len(r.Errors) != 2 || r.Errors[0]["path"] != b || !strings.Contains(r.Errors[0]["error"], "/share/dest/b.txt") ||
		r.Errors[1]["path"] != c || !strings.Contains(r.Errors[1]["error"], "not attempted") {
		t.Errorf("errors: %v", r.Errors)
	}
	if _, err := os.Stat(filepath.Join(dir, journalFile)); !os.IsNotExist(err) {
		t.Errorf("the replayed journal is still there: %v", err)
	}
	// And all of it was saved: another restart, with no journal, has it.
	s2 := reload(dir)
	s2.loadMoveReport()
	if !s2.moveOut[destA] || !s2.moveOut[destB] || s2.lastMove == nil || s2.lastMove.ID != "m1" {
		t.Errorf("the replay's outcome did not survive the next restart")
	}
}

func TestReplayOfAnEndedBatchAddsRecordsButNoReport(t *testing.T) {
	dir := t.TempDir()
	j, _ := openJournal(dir)
	j.write(journalLine{Begin: &journalBegin{ID: "m2", Files: []string{"/v/a"}}}, true)
	j.write(journalLine{Start: "/v/a"}, false)
	j.write(journalLine{Done: "/v/a", Moved: true, MoveOut: []string{"/v/out/a"}}, true)
	j.finish(false) // the batch's save failed: the journal is kept, marked ended
	s := reload(dir)
	s.replayMoveJournal()
	if !s.moveOut["/v/out/a"] {
		t.Error("the ended batch's records were not applied")
	}
	if s.lastMove != nil {
		t.Errorf("an ended batch got an interrupted report: %+v", s.lastMove)
	}
}

func TestReplayKeepsTheReportTheBatchAlreadyPublished(t *testing.T) {
	dir, root := t.TempDir(), t.TempDir()
	s := &Server{varDir: dir}
	s.publishMoveReport(&moveReport{ID: "m1", Moved: []string{"done"}})
	cutShortJournal(t, dir, filepath.Join(root, "a"), filepath.Join(root, "b"), filepath.Join(root, "c"), root)
	s = reload(dir)
	s.loadMoveReport()
	s.replayMoveJournal()
	if s.lastMove == nil || s.lastMove.Interrupted || len(s.lastMove.Moved) != 1 {
		t.Errorf("the batch's own report was replaced: %+v", s.lastMove)
	}
}

func TestDamagedJournalWarnsAndIsKept(t *testing.T) {
	dir := t.TempDir()
	body := `{"begin":{"id":"m3","files":["/v/a","/v/b"]}}` + "\n" +
		"\x00\x01 garbage\n" +
		`{"done":"/v/a","moved":true,"moveOut":["/v/out/a"]}` + "\n"
	os.WriteFile(filepath.Join(dir, journalFile), []byte(body), 0o600)
	s := reload(dir)
	s.replayMoveJournal()
	if !s.moveOut["/v/out/a"] {
		t.Error("the readable lines of a damaged journal were not applied")
	}
	if s.recordsWarn == "" {
		t.Error("a damaged journal was not announced")
	}
	if _, err := os.Stat(filepath.Join(dir, journalFile+damagedSuffix)); err != nil {
		t.Errorf("the damaged journal was not set aside: %v", err)
	}
}

func TestJournalCutOffMidLineIsNotDamage(t *testing.T) {
	dir := t.TempDir()
	body := `{"begin":{"id":"m4","files":["/v/a"]}}` + "\n" +
		`{"done":"/v/a","moved":true,"moveOut":["/v/out/a"]}` + "\n" + `{"start":"/v/`
	os.WriteFile(filepath.Join(dir, journalFile), []byte(body), 0o600)
	s := reload(dir)
	s.replayMoveJournal()
	if s.recordsWarn != "" {
		t.Errorf("a line cut off by the stop was reported as damage: %s", s.recordsWarn)
	}
	if !s.moveOut["/v/out/a"] {
		t.Error("the complete lines were not applied")
	}
}
