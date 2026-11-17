package main

// Junk-only folders count as empty: a folder holding nothing but the
// Temporary Files tool's own junk names — or
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
	"os"
	"path/filepath"
	"strings"
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
		got, err := sess.folderHoldsOnlyJunk("/share/x", nil)
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
	got, err := sess.folderHoldsOnlyJunk("/share/big", nil)
	done()
	if err != nil || !got {
		t.Fatalf("all-junk multi-page listing: want true, got %v err %v", got, err)
	}
	many[1202] = junkEnt{"real.dat", false}
	sess, done = newJunkListFake(t, many, 500)
	got, err = sess.folderHoldsOnlyJunk("/share/big", nil)
	done()
	if err != nil || got {
		t.Fatalf("real file on the last page: want false, got %v err %v", got, err)
	}
	// A DSM that answers in pieces smaller than asked for: the next page
	// must start after what arrived, or the real file in the gap is missed.
	many[1202] = junkEnt{"junk.tmp", false}
	many[300] = junkEnt{"real.dat", false}
	sess, done = newJunkListFake(t, many, 200)
	got, err = sess.folderHoldsOnlyJunk("/share/big", nil)
	done()
	if err != nil || got {
		t.Fatalf("real file inside a listing answered in smaller pieces: want false, got %v err %v", got, err)
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

// A folder offered as empty because everything in it has a junk name must
// say what it holds: junk is decided by NAME, so "film.part" qualifies
// whatever its size, and a row reading "Empty" would hide gigabytes. Size is
// the junk's total bytes, Junk the count, JunkEx a few names; a nested
// junk-only folder's junk counts toward the topmost folder that supersedes
// it. A truly empty folder reports nothing.
func TestEmptyFolderRowReportsTheJunkItHolds(t *testing.T) {
	sized := func(p string, n int64) fEnt { f := efFile(p); f.size = n; return f }
	sc := newEmptyFolderScan()
	for _, f := range []fEnt{
		efDir("/r/backups"),
		sized("/r/backups/film.part", 4<<30),
		efDir("/r/backups/old"),
		sized("/r/backups/old/setup.tmp", 100),
		efDir("/r/bare"),
		efDir("/r/mixed"),
		sized("/r/mixed/a.tmp", 7),
		efFile("/r/mixed/keep.txt"),
	} {
		sc.visit(0, f)
	}
	files, _, _ := sc.finish(&Server{}, nil, func(string) (bool, error) { return true, nil })
	got := map[string]FileEnt{}
	for _, f := range files {
		got[f.Dir+"/"+f.Name] = f
	}
	if len(got) != 2 {
		t.Fatalf("want /r/backups and /r/bare, got %v", got)
	}
	b := got["/r/backups"]
	if b.Size != 4<<30+100 || b.Junk != 2 {
		t.Fatalf("backups row: size %d junk %d, want %d and 2", b.Size, b.Junk, int64(4<<30+100))
	}
	if len(b.JunkEx) != 2 || b.JunkEx[0] != "film.part" || b.JunkEx[1] != "setup.tmp" {
		t.Fatalf("backups row names: %v", b.JunkEx)
	}
	if e := got["/r/bare"]; e.Size != 0 || e.Junk != 0 || e.JunkEx != nil {
		t.Fatalf("a bare folder must report nothing inside it: %+v", e)
	}
}

// The native half of the confirmation reads the directory itself, and a
// DIRECTORY whose name happens to be junk-shaped is not junk: the walk never
// went inside it (".DS_Store", "._x" and every other dot-name are skipped as
// hidden), and File Station's listing omits exactly those names, so this
// check is the only one that sees it at all. Whatever such a folder holds,
// the parent is not empty — only Synology's @eaDir cache is accepted.
func TestNativeJunkCheckRejectsJunkNamedDirectories(t *testing.T) {
	for _, sub := range []string{".DS_Store", "._photos", "old.tmp", "~$draft"} {
		d := t.TempDir()
		mkfile(t, filepath.Join(d, "Thumbs.db"), "x")
		mkfile(t, filepath.Join(d, sub, "precious.jpg"), "real data")
		if ok, err := dirHoldsOnlyJunk(d); err != nil || ok {
			t.Errorf("a folder holding a %q directory was confirmed junk-only (ok=%v err=%v)", sub, ok, err)
		}
	}
	d := t.TempDir()
	mkfile(t, filepath.Join(d, "Thumbs.db"), "x")
	mkfile(t, filepath.Join(d, "@eaDir", "SYNOPHOTO_THUMB_S.jpg"), "t")
	if ok, err := dirHoldsOnlyJunk(d); err != nil || !ok {
		t.Errorf("junk plus @eaDir must still confirm (ok=%v err=%v)", ok, err)
	}
}

func mkfile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Names that may be the one copy of something are not junk: a backup
// ("accounts.bak", "settings.old", an editor's "notes.txt~") or an editor's
// swap file, which holds edits that were never saved. The Temporary Files
// tool does not list them, and a folder holding nothing else is not empty —
// neither to the scan nor to the move-time confirmation.
func TestBackupNamesAreNotJunk(t *testing.T) {
	keep := []string{"accounts.bak", "ACCOUNTS.BAK", "settings.old", "notes.txt~", ".notes.txt.swp", ".notes.txt.swo"}
	for _, n := range keep {
		if isTempName(n) {
			t.Errorf("%q is counted as junk", n)
		}
	}
	for _, n := range []string{"Thumbs.db", "x.tmp", "film.part", "~$book.xlsx"} {
		if !isTempName(n) {
			t.Errorf("%q must still count as junk", n)
		}
	}
	sc := newEmptyFolderScan()
	for _, f := range []fEnt{
		efDir("/r/backups"),
		efFile("/r/backups/accounts.bak"),
		efDir("/r/editor"),
		efFile("/r/editor/notes.txt~"),
		efFile("/r/editor/x.tmp"),
	} {
		sc.visit(0, f)
	}
	files, _, _ := sc.finish(&Server{}, nil, func(string) (bool, error) { return true, nil })
	if len(files) != 0 {
		t.Fatalf("a folder holding a backup was offered as empty: %v", files)
	}
	for _, n := range keep {
		d := t.TempDir()
		mkfile(t, filepath.Join(d, "Thumbs.db"), "x")
		mkfile(t, filepath.Join(d, n), "the only copy")
		if ok, err := dirHoldsOnlyJunk(d); err != nil || ok {
			t.Errorf("a folder holding %q was confirmed junk-only (ok=%v err=%v)", n, ok, err)
		}
	}
}

// The move judges a Temporary Files row by the rule in force at move time,
// so a backup name an older scan listed is refused before anything happens.
func TestMoveRefusesATempRowNoLongerJunk(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "accounts.bak")
	mkfile(t, p, "the only copy")
	canon := newDirResolver()
	cp := canon.path(p)
	v := &moveVet{tool: "temp_files", canon: canon, allowed: map[string]bool{cp: true},
		idents: map[string][]entIdent{}, keepers: map[string]bool{}, drops: map[string]bool{},
		outHeld: map[string]bool{}, staleHeld: map[string]bool{}, vols: []string{canon.dir(d)}}
	if err := v.refuse(cp); err == nil || !strings.Contains(err.Error(), "no longer counted as a temporary file") {
		t.Fatalf("want a refusal naming the rule, got %v", err)
	}
	// The same vetting lets real junk through to the move itself.
	j := filepath.Join(d, "x.tmp")
	mkfile(t, j, "junk")
	v.allowed[canon.path(j)] = true
	if err := v.refuse(canon.path(j)); err != nil {
		t.Fatalf("real junk must pass the vetting, got %v", err)
	}
}

// A folder whose only content is more folders — empty, or holding nothing
// but junk — is empty. The walk offers only the topmost such folder, so the
// confirmation has to look inside the folders it holds rather than refuse
// them on sight: a refusal there lists neither the folder nor anything
// inside it. What the walk never went into is still unknown
// content at any depth — a hidden folder, a folder with a junk name, a
// Synology @-folder other than @eaDir, the recycle bin — and so is a
// symlink; any of them, or any real file, keeps the whole folder off the
// list. Run against the real confirmEmpty and against each half alone,
// which must agree.
func TestConfirmEmptyLooksInsideFolders(t *testing.T) {
	f, sess := newMoveVerifyFake(t)
	elsewhere := t.TempDir()
	cases := []struct {
		name  string
		build func(d string)
		want  bool
	}{
		{"bare", func(d string) {}, true},
		{"one empty folder", func(d string) { mkdirs(t, d, "inner") }, true},
		{"a chain of empty folders", func(d string) { mkdirs(t, d, "a/b/c") }, true},
		{"several empty folders beside junk", func(d string) {
			mkdirs(t, d, "one", "two/three")
			mkfile(t, filepath.Join(d, "x.tmp"), "junk")
		}, true},
		{"junk and @eaDir deep down", func(d string) {
			mkfile(t, filepath.Join(d, "a/b/Thumbs.db"), "x")
			mkfile(t, filepath.Join(d, "a/@eaDir/SYNOPHOTO_THUMB_S.jpg"), "t")
		}, true},
		{"a real file deep down", func(d string) { mkfile(t, filepath.Join(d, "a/b/keep.txt"), "real") }, false},
		{"a hidden folder deep down", func(d string) { mkfile(t, filepath.Join(d, "a/.git/config"), "repo") }, false},
		{"an empty hidden folder", func(d string) { mkdirs(t, d, "a/.hidden") }, false},
		{"a junk-named hidden folder", func(d string) { mkfile(t, filepath.Join(d, "a/.DS_Store/precious.jpg"), "real") }, false},
		{"an empty junk-named folder", func(d string) { mkdirs(t, d, "a/old.tmp") }, false},
		{"the recycle bin", func(d string) { mkdirs(t, d, "a/#recycle") }, false},
		{"a Synology folder other than @eaDir", func(d string) { mkdirs(t, d, "a/@tmp") }, false},
		{"a FILE named @eaDir", func(d string) { mkfile(t, filepath.Join(d, "@eaDir"), "real") }, false},
		{"a symlink to a folder", func(d string) {
			mkdirs(t, d, "a")
			if err := os.Symlink(elsewhere, filepath.Join(d, "a", "link")); err != nil {
				t.Fatal(err)
			}
		}, false},
	}
	for i, c := range cases {
		rel := "case" + strings.Repeat("x", i)
		d := filepath.Join(f.root, rel)
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		c.build(d)
		got, err := confirmEmpty(d, sess, nil)
		if err != nil || got != c.want {
			t.Errorf("%s: confirmEmpty = %v (err %v), want %v", c.name, got, err, c.want)
		}
		if got, err := dirHoldsOnlyJunk(d); err != nil || got != c.want {
			t.Errorf("%s: the native half = %v (err %v), want %v", c.name, got, err, c.want)
		}
		if got, err := sess.folderHoldsOnlyJunk("/Backups/"+rel, nil); err != nil || got != c.want {
			t.Errorf("%s: File Station's half = %v (err %v), want %v", c.name, got, err, c.want)
		}
	}
}

func mkdirs(t *testing.T, base string, rels ...string) {
	t.Helper()
	for _, r := range rels {
		if err := os.MkdirAll(filepath.Join(base, r), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// Looking inside folders is bounded, and the bound fails closed: a folder
// holding more than the limit is reported as one that could not be checked
// — an error, so the scan says so — never as empty. So does a folder deep
// inside that cannot be read, and a Stop ends the File Station reading at
// once.
func TestConfirmEmptyIsBoundedAndFailsClosed(t *testing.T) {
	f, sess := newMoveVerifyFake(t)
	old := emptyCheckMax
	emptyCheckMax = 5
	t.Cleanup(func() { emptyCheckMax = old })

	d := filepath.Join(f.root, "many")
	mkdirs(t, d, "a/b/c", "d/e/f") // six folders: one past the limit
	ok, err := confirmEmpty(d, sess, nil)
	if ok || err == nil || !strings.Contains(err.Error(), "more than 5") {
		t.Fatalf("a folder past the limit: want an error naming it, got %v %v", ok, err)
	}
	if ok, err := dirHoldsOnlyJunk(d); ok || err == nil {
		t.Fatalf("the native half past the limit: want an error, got %v %v", ok, err)
	}
	if ok, err := sess.folderHoldsOnlyJunk("/Backups/many", nil); ok || err == nil {
		t.Fatalf("File Station's half past the limit: want an error, got %v %v", ok, err)
	}
	emptyCheckMax = old

	stop := make(chan struct{})
	close(stop)
	if ok, err := sess.folderHoldsOnlyJunk("/Backups/many", stop); ok || err == nil {
		t.Fatalf("a stopped check must not answer empty, got %v %v", ok, err)
	}

	if os.Geteuid() == 0 {
		return // root reads every folder; the unreadable case cannot be built
	}
	l := filepath.Join(f.root, "locked")
	mkdirs(t, l, "a/vault")
	os.Chmod(filepath.Join(l, "a", "vault"), 0o000)
	t.Cleanup(func() { os.Chmod(filepath.Join(l, "a", "vault"), 0o755) })
	if ok, err := confirmEmpty(l, sess, nil); ok || err == nil {
		t.Fatalf("an unreadable folder deep inside must fail closed with an error, got %v %v", ok, err)
	}
	if ok, err := dirHoldsOnlyJunk(l); ok || err == nil {
		t.Fatalf("the native half on an unreadable folder: want an error, got %v %v", ok, err)
	}
}

// The confirmation refuses a folder holding a folder with a junk name, so the
// walk has to count one as content for the folder around it. Otherwise that
// folder is offered as the topmost candidate, refused, and the genuinely
// empty folders beside it — the junk-named one included — are never offered.
func TestJunkNamedFolderIsContentForItsParent(t *testing.T) {
	offered, reported := driveEF(t, []fEnt{
		efDir("/r/ef"),
		efDir("/r/ef/a"),
		efDir("/r/ef/cache.tmp"),
	}, func(string) bool { return true })
	want := []string{"/r/ef/a", "/r/ef/cache.tmp"}
	if strings.Join(offered, " ") != strings.Join(want, " ") {
		t.Fatalf("want %v offered, got %v", want, offered)
	}
	if strings.Join(reported, " ") != strings.Join(want, " ") {
		t.Fatalf("want %v reported, got %v", want, reported)
	}
}

// What the walk passes over without visiting is content for the folder
// holding it exactly when the confirmation would refuse that folder: every
// folder the walk does not enter except @eaDir, and every symlink, pipe,
// socket or device whose name is not junk.
func TestPassedOverIsContentMatchesTheConfirmation(t *testing.T) {
	cases := []struct {
		name  string
		isDir bool
		want  bool
	}{
		{"@eaDir", true, false},
		{".git", true, true},
		{".DS_Store", true, true},
		{"@tmp", true, true},
		{"#recycle", true, true},
		{"#snapshot", true, true},
		{"lost+found", true, true},
		{"photos", false, true}, // a symlink, pipe or socket named like content
		{"x.tmp", false, false}, // the same under a junk name: junk, as the confirmation reads it
	}
	for _, c := range cases {
		if got := passedOverIsContent(c.name, c.isDir); got != c.want {
			t.Errorf("%q (dir %v): got %v, want %v", c.name, c.isDir, got, c.want)
		}
	}
}
