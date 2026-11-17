package main

import (
	"dupfinder/internal/dirhandle"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readMarks returns every record in dir's record folder.
func readMarks(t *testing.T, dir string) []movedMarkRecord {
	t.Helper()
	ents, _ := os.ReadDir(filepath.Join(dir, movedMarkDir))
	var out []movedMarkRecord
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(dir, movedMarkDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var rec movedMarkRecord
		if err := json.Unmarshal(b, &rec); err != nil {
			t.Fatalf("%s is not a record: %v", e.Name(), err)
		}
		out = append(out, rec)
	}
	return out
}

// A flat move's record lists the files it landed directly in the
// destination, by name, size and modified time; each move writes a record of
// its own, so nothing is ever overwritten.
func TestFlatMoveLeavesARecordInItsDestination(t *testing.T) {
	f, sess := newMoveVerifyFake(t)
	dest := filepath.Join(f.root, "dest")
	os.MkdirAll(filepath.Join(dest, "sub"), 0o755)
	when := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
	put := func(rel, body string) string {
		p := filepath.Join(dest, rel)
		os.WriteFile(p, []byte(body), 0o644)
		os.Chtimes(p, when, when)
		return p
	}
	a := put("a.bin", "first")
	b := put("b (1).bin", "second!")
	deeper := put("sub/c.bin", "not directly in the destination")
	// A folder the move landed (an Empty Folders row) takes its junk along.
	os.MkdirAll(filepath.Join(dest, "j1", "inner"), 0o755)
	put("j1/Thumbs.db", "cache")
	put("j1/inner/x.part", "partial")
	if err := leaveMovedMark(sess, "/Backups/dest", dest, []string{a, b, deeper, filepath.Join(dest, "j1"), filepath.Join(dest, "never-landed.bin")}); err != nil {
		t.Fatal(err)
	}
	recs := readMarks(t, dest)
	if len(recs) != 1 {
		t.Fatalf("want one record, got %d", len(recs))
	}
	got := recs[0].Moved
	want := []movedMarkEntry{{"a.bin", 5, when.Unix()}, {"b (1).bin", 7, when.Unix()},
		{"j1/Thumbs.db", 5, when.Unix()}, {"j1/inner/x.part", 7, when.Unix()}}
	if len(got) != len(want) {
		t.Fatalf("record lists %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("record lists %+v, want %+v", got, want)
		}
	}
	if recs[0].Folder != markFolderKey(dest) {
		t.Fatalf("the record names folder %q, want %q", recs[0].Folder, markFolderKey(dest))
	}
	if !strings.Contains(recs[0].About, "Duplicate Finder moved") {
		t.Fatalf("the record does not say what it is: %q", recs[0].About)
	}
	// A second move into the same folder adds a record beside the first.
	if err := leaveMovedMark(sess, "/Backups/dest", dest, []string{put("d.bin", "fourth")}); err != nil {
		t.Fatal(err)
	}
	if n := len(readMarks(t, dest)); n != 2 {
		t.Fatalf("a second move left %d records, want 2", n)
	}
	// Nothing to record writes nothing, not even the folder.
	empty := filepath.Join(f.root, "empty")
	os.Mkdir(empty, 0o755)
	if err := leaveMovedMark(sess, "/Backups/empty", empty, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(empty, movedMarkDir)); err == nil {
		t.Fatal("a move that landed nothing made a record folder")
	}
}

// A scan recognises exactly the files a record lists with their size and
// modified time; anything else in the folder, a file whose size or time has
// changed since, and entries a record could use to point elsewhere are not.
func TestRecordedFilesAreRecognisedAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	recDir := filepath.Join(dir, movedMarkDir)
	os.Mkdir(recDir, 0o755)
	mtime := int64(1700000000)
	rec := movedMarkRecord{Moved: []movedMarkEntry{
		{"a.bin", 5, mtime}, {"b.bin", 7, mtime}, {"../escape.bin", 1, mtime}, {"sub/x.bin", 1, mtime},
		{"sub/../../out.bin", 1, mtime}, {"/abs.bin", 1, mtime}, {"a//b.bin", 1, mtime}, {strings.Repeat("n", 256), 1, mtime},
	}}
	body, _ := json.Marshal(rec)
	os.WriteFile(filepath.Join(recDir, "r1.json"), body, 0o644)
	os.WriteFile(filepath.Join(recDir, "broken.json"), []byte("{not json"), 0o644)
	os.WriteFile(filepath.Join(recDir, "notes.txt"), body, 0o644) // not a record name
	// A record reached through a shortcut is not read.
	other := filepath.Join(t.TempDir(), "elsewhere.json")
	body2, _ := json.Marshal(movedMarkRecord{Moved: []movedMarkEntry{{"c.bin", 3, mtime}}})
	os.WriteFile(other, body2, 0o644)
	os.Symlink(other, filepath.Join(recDir, "linked.json"))

	h, err := dirhandle.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	m := newMovedMarks(nil)
	m.load(h, ".", "/volume1/share/dest", "/canon/share/dest")
	at := time.Unix(mtime, 0)
	for _, tc := range []struct {
		name string
		size int64
		mod  time.Time
		want bool
	}{
		{"a.bin", 5, at, true},
		{"b.bin", 7, at, true},
		{"b.bin", 8, at, false},                  // changed size
		{"a.bin", 5, at.Add(time.Second), false}, // changed time
		{"c.bin", 3, at, false},                  // only in the record behind a shortcut
		{"other.bin", 5, at, false},              // the user's own file
		{"escape.bin", 1, at, false},
	} {
		c, ok := m.match("/volume1/share/dest", tc.name, tc.size, tc.mod)
		if ok != tc.want {
			t.Errorf("%s (%d bytes): recognised=%v, want %v", tc.name, tc.size, ok, tc.want)
		}
		if ok && c != "/canon/share/dest/"+tc.name {
			t.Errorf("%s: canonical path %q", tc.name, c)
		}
	}
	if _, ok := m.match("/volume1/share/elsewhere", "a.bin", 5, at); ok {
		t.Error("a record recognised a file in another folder")
	}
	// A file inside a folder the move landed is recognised in that folder.
	if c, ok := m.match("/volume1/share/dest/sub", "x.bin", 1, at); !ok || c != "/canon/share/dest/sub/x.bin" {
		t.Errorf("the file inside a moved folder was not recognised there: %q %v", c, ok)
	}
	if _, ok := m.match("/volume1/share/dest", "sub/x.bin", 1, at); ok {
		t.Error("a nested entry was matched by its whole path in the record's folder")
	}
	if m.n != 3 {
		t.Errorf("held %d entries, want 3 (the escapes, the absolute path, the empty component and the over-long name refused)", m.n)
	}
}

// The entries a scan can hold are bounded, and reaching the bound is
// reported rather than silent.
func TestRecordEntriesAreBoundedAndTheBoundReported(t *testing.T) {
	m := newMovedMarks(nil)
	m.n = movedMarkCap
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, movedMarkDir), 0o755)
	body, _ := json.Marshal(movedMarkRecord{Moved: []movedMarkEntry{{"a.bin", 1, 1}}})
	os.WriteFile(filepath.Join(dir, movedMarkDir, "r.json"), body, 0o644)
	h, _ := dirhandle.Open(dir)
	defer h.Close()
	m.load(h, ".", "/d", "/d")
	if !m.over || !strings.Contains(strings.Join(m.report(), "\n"), "were not recognised") {
		t.Fatalf("reaching the bound was not reported: over=%v %q", m.over, m.report())
	}
	if len(newMovedMarks(nil).report()) != 0 {
		t.Fatal("a scan that read no records reported something")
	}
}

// A record speaks for the folder it was written in. One whose folder is
// another folder's — copied here, beside an original — is ignored; one
// written for this folder is honoured; one from before records named their
// folder is honoured where it lies.
func TestARecordSpeaksOnlyForItsOwnFolder(t *testing.T) {
	mtime := int64(1700000000)
	at := time.Unix(mtime, 0)
	for _, tc := range []struct {
		name, folder string
		want         bool
	}{
		{"written for this folder", "/canon/share/lib", true},
		{"copied from another folder", "/canon/share/clean", false},
		{"written before records named their folder", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			os.Mkdir(filepath.Join(dir, movedMarkDir), 0o755)
			body, _ := json.Marshal(movedMarkRecord{Folder: tc.folder, Moved: []movedMarkEntry{{"p.jpg", 9, mtime}}})
			os.WriteFile(filepath.Join(dir, movedMarkDir, "r.json"), body, 0o644)
			h, err := dirhandle.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			m := newMovedMarks(nil)
			m.load(h, ".", "/volume1/share/lib", "/canon/share/lib")
			if _, ok := m.match("/volume1/share/lib", "p.jpg", 9, at); ok != tc.want {
				t.Fatalf("recognised=%v, want %v", ok, tc.want)
			}
		})
	}
}

// A big move writes several records, each one readable in full: a reader
// never refuses a record the app itself wrote.
func TestABigMoveIsRecordedInSeveralRecords(t *testing.T) {
	old := movedMarkChunk
	movedMarkChunk = 3
	t.Cleanup(func() { movedMarkChunk = old })
	f, sess := newMoveVerifyFake(t)
	dest := filepath.Join(f.root, "dest")
	os.Mkdir(dest, 0o755)
	var landed []string
	for i := 0; i < 7; i++ {
		p := filepath.Join(dest, fmt.Sprintf("f%d & more.bin", i))
		os.WriteFile(p, []byte("x"), 0o644)
		landed = append(landed, p)
	}
	if err := leaveMovedMark(sess, "/Backups/dest", dest, landed); err != nil {
		t.Fatal(err)
	}
	recs := readMarks(t, dest)
	if len(recs) != 3 {
		t.Fatalf("7 files in records of 3: want 3 records, got %d", len(recs))
	}
	raw, _ := os.ReadDir(filepath.Join(dest, movedMarkDir))
	b, _ := os.ReadFile(filepath.Join(dest, movedMarkDir, raw[0].Name()))
	if strings.Contains(string(b), `\u0026`) {
		t.Fatal("names are HTML-escaped, inflating the record")
	}
	h, _ := dirhandle.Open(dest)
	defer h.Close()
	m := newMovedMarks(nil)
	m.load(h, ".", "/d", dest) // the same path the writer was given
	if m.n != 7 || len(m.report()) != 0 {
		t.Fatalf("read back %d of 7 entries, problems %v", m.n, m.report())
	}
}

// A record that cannot be used is reported by folder, never skipped silently.
func TestAnUnusableRecordIsReported(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, movedMarkDir), 0o755)
	os.WriteFile(filepath.Join(dir, movedMarkDir, "damaged.json"), []byte("{not json"), 0o644)
	big := make([]byte, movedMarkFileMax+10)
	os.WriteFile(filepath.Join(dir, movedMarkDir, "huge.json"), big, 0o644)
	h, _ := dirhandle.Open(dir)
	defer h.Close()
	m := newMovedMarks(nil)
	m.load(h, ".", "/volume1/share/clean", "/canon/share/clean")
	rep := strings.Join(m.report(), "\n")
	if !strings.Contains(rep, "damaged.json is damaged") || !strings.Contains(rep, "huge.json is larger") || !strings.Contains(rep, "/volume1/share/clean") {
		t.Fatalf("unusable records not reported by folder: %q", rep)
	}
}

// The names a scan holds from records are bounded in bytes, not only in
// count, and reaching the bound is reported.
func TestRecordNamesAreBoundedInBytes(t *testing.T) {
	old := movedMarkBytesMax
	movedMarkBytesMax = 300
	t.Cleanup(func() { movedMarkBytesMax = old })
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, movedMarkDir), 0o755)
	var ents []movedMarkEntry
	for i := 0; i < 20; i++ {
		ents = append(ents, movedMarkEntry{Name: fmt.Sprintf("%s-%d", strings.Repeat("n", 200), i), Size: 1, MTime: 1})
	}
	body, _ := json.Marshal(movedMarkRecord{Moved: ents})
	os.WriteFile(filepath.Join(dir, movedMarkDir, "r.json"), body, 0o644)
	h, _ := dirhandle.Open(dir)
	defer h.Close()
	m := newMovedMarks(nil)
	m.load(h, ".", "/d", "/d")
	if !m.over || m.bytes > 300 || m.n >= 20 {
		t.Fatalf("held %d entries, %d bytes, over=%v", m.n, m.bytes, m.over)
	}
}
