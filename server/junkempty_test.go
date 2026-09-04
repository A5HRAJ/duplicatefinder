package main

// Junk-only folders count as empty (maintainer directive, 2026-08-10): a
// folder holding nothing but the Temporary Files tool's own junk names — or
// Synology's regenerable @eaDir thumbnail cache — is as disposable as a bare
// one. These tests pin the three halves of that: the walk-side candidacy
// (visit must not let junk mark a folder non-empty), the File Station
// confirmation (folderHoldsOnlyJunk's entry rules and paging), and the
// pruning of rows UNDER a moved directory (the junk rides along, and any
// temp_files rows it held must not survive as phantoms).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// driveEF replays an entry stream through emptyFolderScan and returns the
// candidate paths it OFFERS to the confirm callback plus what finish reports
// when the callback answers `answer`.
func driveEF(t *testing.T, ents []fEnt, answer func(string) bool) (offered, reported []string) {
	t.Helper()
	sc := newEmptyFolderScan()
	for _, f := range ents {
		sc.visit(0, f)
	}
	files, _, _ := sc.finish(&Server{}, nil, func(p string) (bool, error) {
		offered = append(offered, p)
		return answer(p), nil
	})
	for _, f := range files {
		reported = append(reported, f.Dir+"/"+f.Name)
	}
	return offered, reported
}

// Both helpers must populate name and dir exactly as the walk's own emit()
// does (name: d.Name(), dir: filepath.Dir(path)) — visit classifies junk by
// NAME, so a hand-built entry that leaves it blank makes isTempName("")
// answer false for everything. That does not just weaken the junk tests, it
// silently turns TestZeroByteFileIsNotJunk green for the wrong reason: with
// no name, .gitkeep is not junk because NOTHING is.
func efDir(p string) fEnt {
	return fEnt{path: p, name: filepath.Base(p), dir: filepath.Dir(p), isDir: true, mod: time.Now()}
}
func efFile(p string) fEnt {
	return fEnt{path: p, name: filepath.Base(p), dir: filepath.Dir(p), isDir: false, mod: time.Now(), size: 10}
}

func TestJunkOnlyFolderIsACandidate(t *testing.T) {
	// junkonly holds two junk names (one FS-visible, one FS-invisible on real
	// DSM — identical here, the walk sees the raw directory); mixed holds
	// junk BESIDE a real file; real holds only a real file.
	offered, reported := driveEF(t, []fEnt{
		efDir("/r/junkonly"),
		efFile("/r/junkonly/Thumbs.db"),
		efFile("/r/junkonly/.DS_Store"),
		efDir("/r/mixed"),
		efFile("/r/mixed/Thumbs.db"),
		efFile("/r/mixed/keep.txt"),
		efDir("/r/real"),
		efFile("/r/real/data.bin"),
	}, func(string) bool { return true })
	if len(offered) != 1 || offered[0] != "/r/junkonly" {
		t.Fatalf("want exactly /r/junkonly offered for confirmation, got %v", offered)
	}
	if len(reported) != 1 || reported[0] != "/r/junkonly" {
		t.Fatalf("want /r/junkonly reported, got %v", reported)
	}
}

func TestZeroByteFileIsNotJunk(t *testing.T) {
	// A .gitkeep exists precisely to keep its folder non-empty: size must
	// never make a file junk, only its name may.
	ents := []fEnt{
		efDir("/r/kept"),
		{path: "/r/kept/.gitkeep", isDir: false, mod: time.Now(), size: 0},
	}
	offered, _ := driveEF(t, ents, func(string) bool { return true })
	if len(offered) != 0 {
		t.Fatalf(".gitkeep-only folder must not be a candidate, got %v", offered)
	}
}

// fakeJunkList serves SYNO.FileStation.List method=list from a canned entry
// slice, honouring offset/limit so the paging loop is exercised for real.
type junkEnt struct {
	name  string
	isDir bool
}

func newJunkListFake(t *testing.T, ents []junkEnt, page int) (*fsSession, func()) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch r.FormValue("api") {
		case "SYNO.API.Info":
			fakeInfo(w, r.FormValue("query"))
		case "SYNO.FileStation.List":
			var off, lim int
			json.Unmarshal([]byte(r.FormValue("offset")), &off)
			json.Unmarshal([]byte(r.FormValue("limit")), &lim)
			if lim <= 0 || lim > page {
				lim = page // the fake's page is what paginates, like a small DSM limit
			}
			end := off + lim
			if end > len(ents) {
				end = len(ents)
			}
			files := []map[string]any{}
			for _, e := range ents[min(off, len(ents)):end] {
				files = append(files, map[string]any{"name": e.name, "isdir": e.isDir})
			}
			fakeEnv(w, 0, map[string]any{"files": files, "total": len(ents), "offset": off})
		default:
			t.Errorf("unexpected api %s", r.FormValue("api"))
		}
	}))
	return &fsSession{base: ts.URL}, ts.Close
}

func TestFolderHoldsOnlyJunk(t *testing.T) {
	cases := []struct {
		name string
		ents []junkEnt
		want bool
	}{
		{"empty listing", nil, true},
		{"all junk files", []junkEnt{{"Thumbs.db", false}, {"desktop.ini", false}, {"note.tmp", false}}, true},
		{"junk plus a real file", []junkEnt{{"Thumbs.db", false}, {"keep.txt", false}}, false},
		{"@eaDir alone", []junkEnt{{"@eaDir", true}}, true},
		{"@eaDir beside junk", []junkEnt{{"@eaDir", true}, {"Thumbs.db", false}}, true},
		// A dot-directory with content is a .git shape: never junk. The name
		// being junk-like as a FILE must not carry over to directories.
		{"dot directory", []junkEnt{{".git", true}}, false},
		{"junk-NAMED directory", []junkEnt{{"Thumbs.db", true}}, false},
	}
	for _, c := range cases {
		sess, done := newJunkListFake(t, c.ents, 500)
		got, err := sess.folderHoldsOnlyJunk("/share/x")
		done()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: want %v, got %v", c.name, c.want, got)
		}
	}
}

func TestFolderHoldsOnlyJunkPagesThroughLargeListings(t *testing.T) {
	// 1,203 junk entries across pages of 500: the loop must reach the end
	// before answering yes — and a single real file on the LAST page must
	// still flip the answer, proving no page is skipped.
	many := make([]junkEnt, 0, 1203)
	for i := 0; i < 1203; i++ {
		many = append(many, junkEnt{"junk.tmp", false})
	}
	sess, done := newJunkListFake(t, many, 500)
	got, err := sess.folderHoldsOnlyJunk("/share/big")
	done()
	if err != nil || !got {
		t.Fatalf("all-junk multi-page listing: want true, got %v err %v", got, err)
	}
	many[1202] = junkEnt{"real.dat", false}
	sess, done = newJunkListFake(t, many, 500)
	got, err = sess.folderHoldsOnlyJunk("/share/big")
	done()
	if err != nil || got {
		t.Fatalf("real file on the last page: want false, got %v err %v", got, err)
	}
}

func TestPruneMovedDropsRowsUnderAMovedFolder(t *testing.T) {
	s := &Server{results: map[string]*toolResult{
		"temp_files": {Tool: "temp_files", Files: []FileEnt{
			{ID: "t1", Dir: "/v/photo/junkonly", Name: "Thumbs.db"},
			{ID: "t2", Dir: "/v/photo/elsewhere", Name: "Thumbs.db"},
		}},
		"empty_folders": {Tool: "empty_folders", Files: []FileEnt{
			{ID: "e1", Dir: "/v/photo", Name: "junkonly", IsDir: true},
			{ID: "e2", Dir: "/v/photo", Name: "other", IsDir: true},
		}},
	}}
	s.pruneMoved([]string{"/v/photo/junkonly"}, []string{"/v/photo/junkonly"}, nil)
	var tempLeft, efLeft []string
	for _, f := range s.results["temp_files"].Files {
		tempLeft = append(tempLeft, f.ID)
	}
	for _, f := range s.results["empty_folders"].Files {
		efLeft = append(efLeft, f.ID)
	}
	if len(tempLeft) != 1 || tempLeft[0] != "t2" {
		t.Fatalf("temp rows under the moved folder must prune, kept %v", tempLeft)
	}
	if len(efLeft) != 1 || efLeft[0] != "e2" {
		t.Fatalf("the moved folder's own row must prune, kept %v", efLeft)
	}
}
