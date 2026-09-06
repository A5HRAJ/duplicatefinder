package main

// Tests for the File Station delegation: the daemon's orchestration (task
// polling, the " (n)" collision staging, upload retry) driven against a
// scripted fake of DSM's entry.cgi. The overwrite parameter must never be
// sent — the fakes fail the test if they see it.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fakeEnv(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{"success": code == 0}
	if code != 0 {
		resp["error"] = map[string]any{"code": code}
	}
	if data != nil {
		resp["data"] = data
	}
	json.NewEncoder(w).Encode(resp)
}

func fakeInfo(w http.ResponseWriter, query string) {
	out := map[string]any{}
	for _, q := range strings.Split(query, ",") {
		out[q] = map[string]int{"minVersion": 1, "maxVersion": 3}
	}
	fakeEnv(w, 0, out)
}

// fakeGetInfo answers the existence probe the collision paths use: taken
// lists the share paths that exist, everything else comes back with DSM's
// per-file 408. Asking about specific names is what keeps the probe's cost
// independent of how many entries the destination folder holds.
func fakeGetInfo(w http.ResponseWriter, pathArg string, taken map[string]bool) {
	var paths []string
	json.Unmarshal([]byte(pathArg), &paths)
	files := make([]map[string]any, 0, len(paths))
	for _, p := range paths {
		if taken[p] {
			files = append(files, map[string]any{"path": p, "isdir": false})
		} else {
			files = append(files, map[string]any{"path": p, "code": 408})
		}
	}
	fakeEnv(w, 0, map[string]any{"files": files})
}

func firstArr(v string) string {
	var out []string
	if json.Unmarshal([]byte(v), &out) == nil && len(out) > 0 {
		return out[0]
	}
	var s string
	if json.Unmarshal([]byte(v), &s) == nil {
		return s
	}
	return v
}

func TestMoveViaFSHappyPath(t *testing.T) {
	var mu sync.Mutex
	var moves []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.FormValue("overwrite") != "" {
			t.Error("overwrite parameter must never be sent")
		}
		switch r.FormValue("api") {
		case "SYNO.API.Info":
			fakeInfo(w, r.FormValue("query"))
		case "SYNO.FileStation.List":
			fakeGetInfo(w, r.FormValue("path"), nil)
		case "SYNO.FileStation.CopyMove":
			if r.FormValue("method") == "start" {
				mu.Lock()
				moves = append(moves, firstArr(r.FormValue("path"))+" -> "+firstArr(r.FormValue("dest_folder_path")))
				mu.Unlock()
				fakeEnv(w, 0, map[string]string{"taskid": "t1"})
				return
			}
			fakeEnv(w, 0, map[string]any{"finished": true})
		default:
			t.Errorf("unexpected api %s", r.FormValue("api"))
		}
	}))
	defer ts.Close()
	sess := &fsSession{base: ts.URL}
	if final, err := moveViaFS(sess, "/Backups/a.txt", "/Moved", entIdent{}); err != nil {
		t.Fatal(err)
	} else if final != "a.txt" && final != "a (1).txt" {
		t.Fatalf("unexpected final name %q", final)
	}
	if len(moves) != 1 || moves[0] != "/Backups/a.txt -> /Moved" {
		t.Fatalf("want one direct move, got %v", moves)
	}
}

// A name collision must stage through a temp folder inside the destination,
// rename to the next free " (n)" name, place it, and clean up — never
// sending overwrite, never losing the file. The collision here is detected
// by the existence pre-probe (the target name exists), so no doomed direct
// attempt is made at all.
func TestMoveViaFSCollisionStagesAndRenames(t *testing.T) {
	var mu sync.Mutex
	var seq []string
	renamed := ""
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.FormValue("overwrite") != "" {
			t.Error("overwrite parameter must never be sent")
		}
		api, method := r.FormValue("api"), r.FormValue("method")
		switch api {
		case "SYNO.API.Info":
			fakeInfo(w, r.FormValue("query"))
		case "SYNO.FileStation.List":
			// The destination already holds a.txt — the pre-probe sees it.
			fakeGetInfo(w, r.FormValue("path"), map[string]bool{"/Moved/a.txt": true})
		case "SYNO.FileStation.CopyMove":
			if method == "start" {
				src, dst := firstArr(r.FormValue("path")), firstArr(r.FormValue("dest_folder_path"))
				mu.Lock()
				seq = append(seq, "mv "+src+" -> "+dst)
				mu.Unlock()
				// Direct move into /Moved collides; moves into the temp
				// folder or of the renamed file succeed.
				id := "collide"
				if strings.Contains(dst, ".dupfinder-tmp-") || strings.Contains(src, " (1)") {
					id = "ok"
				}
				fakeEnv(w, 0, map[string]string{"taskid": id})
				return
			}
			if r.FormValue("taskid") == "ok" {
				fakeEnv(w, 0, map[string]any{"finished": true})
			} else {
				fakeEnv(w, 1003, nil)
			}
		case "SYNO.FileStation.CreateFolder":
			mu.Lock()
			seq = append(seq, "mkdir "+firstArr(r.FormValue("name")))
			mu.Unlock()
			fakeEnv(w, 0, nil)
		case "SYNO.FileStation.Rename":
			mu.Lock()
			renamed = firstArr(r.FormValue("name"))
			seq = append(seq, "rename -> "+renamed)
			mu.Unlock()
			fakeEnv(w, 0, nil)
		case "SYNO.FileStation.Delete":
			mu.Lock()
			seq = append(seq, "rmdir "+firstArr(r.FormValue("path")))
			mu.Unlock()
			fakeEnv(w, 0, nil)
		default:
			t.Errorf("unexpected api %s", api)
		}
	}))
	defer ts.Close()
	sess := &fsSession{base: ts.URL}
	final, err := moveViaFS(sess, "/Backups/a.txt", "/Moved", entIdent{})
	if err != nil {
		t.Fatal(err)
	}
	if final != "a (1).txt" {
		t.Fatalf("moveViaFS must report the collision-renamed final name, got %q", final)
	}
	if renamed != "a (1).txt" {
		t.Fatalf("want rename to 'a (1).txt', got %q (seq %v)", renamed, seq)
	}
	if len(seq) < 4 || !strings.HasPrefix(seq[0], "mkdir .dupfinder-tmp-") ||
		!strings.Contains(seq[1], ".dupfinder-tmp-") ||
		!strings.HasPrefix(seq[len(seq)-1], "rmdir") {
		t.Fatalf("unexpected sequence (pre-probe should skip the direct attempt): %v", seq)
	}
}

// A race collision the pre-probe missed: live DSM reports it as generic
// 1001 with the 1003 detail nested — the staging recovery must still fire.
func TestMoveViaFSNested1001Collision(t *testing.T) {
	var mu sync.Mutex
	var seq []string
	listCalls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		api, method := r.FormValue("api"), r.FormValue("method")
		switch api {
		case "SYNO.API.Info":
			fakeInfo(w, r.FormValue("query"))
		case "SYNO.FileStation.List":
			// The race: free at probe time, taken on every later look.
			mu.Lock()
			listCalls++
			n := listCalls
			mu.Unlock()
			if n == 1 {
				fakeGetInfo(w, r.FormValue("path"), nil)
				return
			}
			fakeGetInfo(w, r.FormValue("path"), map[string]bool{"/Moved/a.txt": true})
		case "SYNO.FileStation.CopyMove":
			if method == "start" {
				src, dst := firstArr(r.FormValue("path")), firstArr(r.FormValue("dest_folder_path"))
				mu.Lock()
				seq = append(seq, "mv "+src+" -> "+dst)
				mu.Unlock()
				id := "ok"
				if src == "/Backups/a.txt" && dst == "/Moved" {
					id = "collide" // the race: taken between probe and move
				}
				fakeEnv(w, 0, map[string]string{"taskid": id})
				return
			}
			if r.FormValue("taskid") == "ok" {
				fakeEnv(w, 0, map[string]any{"finished": true})
				return
			}
			// Live DSM's shape: generic code, detail nested.
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"success":false,"error":{"code":1001,"errors":[{"code":1003,"path":"/Backups/a.txt"}]}}`))
		case "SYNO.FileStation.CreateFolder", "SYNO.FileStation.Rename", "SYNO.FileStation.Delete":
			fakeEnv(w, 0, nil)
		default:
			t.Errorf("unexpected api %s", api)
		}
	}))
	defer ts.Close()
	sess := &fsSession{base: ts.URL}
	if final, err := moveViaFS(sess, "/Backups/a.txt", "/Moved", entIdent{}); err != nil {
		t.Fatal(err)
	} else if final != "a.txt" && final != "a (1).txt" {
		t.Fatalf("unexpected final name %q", final)
	}
	if len(seq) < 3 || !strings.HasPrefix(seq[0], "mv /Backups/a.txt -> /Moved") ||
		!strings.Contains(seq[1], ".dupfinder-tmp-") {
		t.Fatalf("staging did not follow the nested-1001 collision: %v", seq)
	}
}

// A genuine (non-collision) failure reported as bare 1001 must not spin:
// the staging move fails with the same cause and is reported, with the
// file back at its source untouched conceptually (nothing was staged).
func TestMoveViaFSGenuine1001Reported(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch r.FormValue("api") {
		case "SYNO.API.Info":
			fakeInfo(w, r.FormValue("query"))
		case "SYNO.FileStation.List":
			fakeGetInfo(w, r.FormValue("path"), nil)
		case "SYNO.FileStation.CopyMove":
			if r.FormValue("method") == "start" {
				fakeEnv(w, 0, map[string]string{"taskid": "t"})
				return
			}
			fakeEnv(w, 1001, nil) // every move fails: e.g. permissions
		case "SYNO.FileStation.CreateFolder", "SYNO.FileStation.Delete":
			fakeEnv(w, 0, nil)
		default:
			t.Errorf("unexpected api %s", r.FormValue("api"))
		}
	}))
	defer ts.Close()
	sess := &fsSession{base: ts.URL}
	_, err := moveViaFS(sess, "/Backups/a.txt", "/Moved", entIdent{})
	if err == nil || !strings.Contains(err.Error(), "error 1001") {
		t.Fatalf("want the genuine 1001 surfaced, got %v", err)
	}
}

// fetchCrtimes maps File Station's crtime values back onto volume paths.
func TestFetchCrtimesShares(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch r.FormValue("api") {
		case "SYNO.API.Info":
			fakeInfo(w, r.FormValue("query"))
		case "SYNO.FileStation.List":
			if r.FormValue("method") != "getinfo" {
				t.Errorf("unexpected method %s", r.FormValue("method"))
			}
			fakeEnv(w, 0, map[string]any{"files": []map[string]any{
				{"path": "/Backups/a.txt", "additional": map[string]any{
					"time": map[string]any{"crtime": 1722173315}}},
			}})
		default:
			t.Errorf("unexpected api %s", r.FormValue("api"))
		}
	}))
	defer ts.Close()
	sess := &fsSession{base: ts.URL}
	got := sess.fetchCrtimesShares([]string{"/Backups/a.txt"},
		map[string]string{"/Backups/a.txt": "/volume1/Backups/a.txt"}, nil, nil)
	if len(got) != 1 || got["/volume1/Backups/a.txt"].Unix() != 1722173315 {
		t.Fatalf("crtime not mapped: %v", got)
	}
}

// Creation-time batches fan out to a small worker pool:
// verify the batches genuinely overlap, results stay complete across many
// batches, progress is monotonic and reaches the end, and a cancelled scan
// stops the fetch.
func TestFetchCrtimesParallel(t *testing.T) {
	var inFlight, maxInFlight int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch r.FormValue("api") {
		case "SYNO.API.Info":
			fakeInfo(w, r.FormValue("query"))
		case "SYNO.FileStation.List":
			cur := atomic.AddInt64(&inFlight, 1)
			for {
				old := atomic.LoadInt64(&maxInFlight)
				if cur <= old || atomic.CompareAndSwapInt64(&maxInFlight, old, cur) {
					break
				}
			}
			time.Sleep(30 * time.Millisecond) // force overlap between batches
			var paths []string
			json.Unmarshal([]byte(r.FormValue("path")), &paths)
			files := make([]map[string]any, 0, len(paths))
			for _, p := range paths {
				files = append(files, map[string]any{"path": p,
					"additional": map[string]any{"time": map[string]any{"crtime": 1722173315}}})
			}
			atomic.AddInt64(&inFlight, -1)
			fakeEnv(w, 0, map[string]any{"files": files})
		default:
			t.Errorf("unexpected api %s", r.FormValue("api"))
		}
	}))
	defer ts.Close()
	sess := &fsSession{base: ts.URL}
	var shares []string
	byShare := map[string]string{}
	for i := 0; i < 800; i++ { // 8 batches of 100
		sp := fmt.Sprintf("/Backups/f%d", i)
		shares = append(shares, sp)
		byShare[sp] = "/volume1" + sp
	}
	lastDone := 0
	got := sess.fetchCrtimesShares(shares, byShare, nil, func(done, total int) {
		if done <= lastDone {
			t.Errorf("progress not monotonic: %d after %d", done, lastDone)
		}
		lastDone = done
		if total != 800 {
			t.Errorf("total = %d, want 800", total)
		}
	})
	if len(got) != 800 {
		t.Fatalf("want 800 crtimes, got %d", len(got))
	}
	if lastDone != 800 {
		t.Fatalf("progress never reached completion: %d", lastDone)
	}
	if atomic.LoadInt64(&maxInFlight) < 2 {
		t.Fatalf("batches never overlapped (max in-flight %d)", maxInFlight)
	}

	// A closed cancel channel stops the fetch with (at most) partial results.
	closed := make(chan struct{})
	close(closed)
	if got := sess.fetchCrtimesShares(shares, byShare, closed, nil); len(got) != 0 {
		t.Fatalf("cancelled fetch should return nothing, got %d", len(got))
	}
}

// An upload collision (1805) steps to the next free " (n)" report name.
func TestExportViaFSCollisionRenames(t *testing.T) {
	var uploads []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			r.ParseMultipartForm(1 << 20)
		} else {
			r.ParseForm()
		}
		switch r.FormValue("api") {
		case "SYNO.API.Info":
			fakeInfo(w, r.FormValue("query"))
		case "SYNO.FileStation.List":
			fakeGetInfo(w, r.FormValue("path"), map[string]bool{"/Moved/duplicate_report.csv": true})
		case "SYNO.FileStation.Upload":
			if r.FormValue("overwrite") != "" {
				t.Error("overwrite parameter must never be sent")
			}
			_, hdr, err := r.FormFile("file")
			if err != nil {
				t.Errorf("missing file part: %v", err)
				fakeEnv(w, 1800, nil)
				return
			}
			uploads = append(uploads, hdr.Filename)
			if hdr.Filename == "duplicate_report.csv" {
				fakeEnv(w, 1805, nil)
				return
			}
			fakeEnv(w, 0, nil)
		default:
			t.Errorf("unexpected api %s", r.FormValue("api"))
		}
	}))
	defer ts.Close()
	sess := &fsSession{base: ts.URL}
	name, err := exportViaFS(sess, "/Moved", "duplicate_report.csv", []byte("csv"))
	if err != nil {
		t.Fatal(err)
	}
	if name != "duplicate_report (1).csv" {
		t.Fatalf("want ' (1)' name, got %q", name)
	}
	if len(uploads) != 2 {
		t.Fatalf("want exactly two upload attempts, got %v", uploads)
	}
}

// The CSV renderer produces the header row and one line per file.
func TestExportCSVContent(t *testing.T) {
	res := &toolResult{Files: []FileEnt{{Name: "big.bin", Dir: "/v", Size: 5}}}
	name, content, err := exportCSV("temp_files", res)
	if err != nil || name != "temp_files_report.csv" {
		t.Fatalf("name %q err %v", name, err)
	}
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "Name,") || !strings.HasPrefix(lines[1], "big.bin,") {
		t.Fatalf("unexpected csv: %q", content)
	}
	if _, _, err := exportCSV("nope", res); err == nil {
		t.Fatal("unknown tool must error")
	}
	// The read-only tool keeps the id corrupted_files — persisted state and the
	// readOnlyTools/groupedTools maps are keyed on it — but everything the user
	// reads says "conflicting", and the export filename is one of
	// those things. The id and the name are deliberately allowed to differ here;
	// this pins the name so a later reader does not "fix" it back to the id.
	name, _, err = exportCSV("corrupted_files", &toolResult{Tool: "corrupted_files"})
	if err != nil || name != "conflicting_files_report.csv" {
		t.Fatalf("conflicting-files export name %q err %v", name, err)
	}
}

// csvCell must neutralize spreadsheet formula markers, including markers
// hidden behind leading whitespace or control characters, while leaving
// ordinary names untouched.
func TestCSVCell(t *testing.T) {
	cases := map[string]string{
		"":                    "",
		"report.txt":          "report.txt",
		" holiday.jpg":        " holiday.jpg",
		"a=b.txt":             "a=b.txt",
		"=1+1":                "'=1+1",
		"+SUM(A1:A9)":         "'+SUM(A1:A9)",
		"-x":                  "'-x",
		"@cmd":                "'@cmd",
		"\t=2":                "'\t=2",
		"\n=HYPERLINK(\"x\")": "'\n=HYPERLINK(\"x\")",
		" \t-2":               "' \t-2",
		"\r\n@x":              "'\r\n@x",
		" \r\n ":              " \r\n ",
	}
	for in, want := range cases {
		if got := csvCell(in); got != want {
			t.Errorf("csvCell(%q) = %q, want %q", in, got, want)
		}
	}
}

// Session extraction: the forwarded header wins, and without it the request
// is refused. The DUPFINDER_DSM_URL hook is dev-tag-only — a release build
// (this test binary) must ignore it, so scans and mutations can only ride a
// real CGI-forwarded DSM session.
func TestFSSessionFrom(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/move", nil)
	r.Header.Set(dsmBaseHeader, "https://127.0.0.1:5001")
	r.Header.Set("Cookie", "id=abc")
	r.Header.Set("X-Syno-Token", "tok")
	s, err := fsSessionFrom(r)
	if err != nil || s.base != "https://127.0.0.1:5001" || s.cookie != "id=abc" || s.token != "tok" {
		t.Fatalf("session not extracted: %+v %v", s, err)
	}
	r2 := httptest.NewRequest("POST", "/api/move", nil)
	if _, err := fsSessionFrom(r2); err == nil {
		t.Fatal("no session must be refused")
	}
	t.Setenv("DUPFINDER_DSM_URL", "http://127.0.0.1:9807")
	if _, err := fsSessionFrom(r2); err == nil {
		t.Fatal("release build honored the dev-only DUPFINDER_DSM_URL hook")
	}
}

// ------------------------------------------------ preserve-mode batch folder

// batchFake scripts a File Station that owns a set of existing names and lets
// the test decide whether each CreateFolder succeeds. Every CreateFolder is
// recorded so the tests can assert both how many were attempted and — the
// property the whole feature rests on — that force_parent was never set.
type batchFake struct {
	mu      sync.Mutex
	taken   map[string]bool
	creates []string
	forces  []string
	// onCreate returns the DSM error code for this attempt (0 = success) and
	// may mutate taken to simulate another writer winning the race.
	onCreate func(f *batchFake, full string, attempt int) int
}

func newBatchFake(taken map[string]bool) (*batchFake, *fsSession, func()) {
	f := &batchFake{taken: map[string]bool{}}
	for k, v := range taken {
		f.taken[k] = v
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch r.FormValue("api") {
		case "SYNO.API.Info":
			fakeInfo(w, r.FormValue("query"))
		case "SYNO.FileStation.List":
			f.mu.Lock()
			snapshot := map[string]bool{}
			for k, v := range f.taken {
				snapshot[k] = v
			}
			f.mu.Unlock()
			fakeGetInfo(w, r.FormValue("path"), snapshot)
		case "SYNO.FileStation.CreateFolder":
			f.mu.Lock()
			full := firstArr(r.FormValue("folder_path")) + "/" + firstArr(r.FormValue("name"))
			attempt := len(f.creates)
			f.creates = append(f.creates, full)
			f.forces = append(f.forces, r.FormValue("force_parent"))
			code := 0
			if f.onCreate != nil {
				code = f.onCreate(f, full, attempt)
			}
			if code == 0 {
				f.taken[full] = true
			}
			f.mu.Unlock()
			fakeEnv(w, code, nil)
		default:
			panic("unexpected api " + r.FormValue("api"))
		}
	}))
	return f, &fsSession{base: ts.URL}, ts.Close
}

// The batch folder must never merge into an existing one, so a taken name is
// enumerated past — and it must be created with force_parent=false, the
// allocating form that fails on a taken name. force_parent=true would turn
// CreateFolder into mkdir -p, silently reuse the existing folder, and make
// every batch land in the first batch's tree: the change would look like it
// worked while doing nothing.
func TestAllocBatchFolderEnumeratesPastTakenNames(t *testing.T) {
	f, sess, done := newBatchFake(map[string]bool{
		"/Moved/Duplicates": true, "/Moved/Duplicates (1)": true,
	})
	defer done()
	name, err := sess.allocBatchFolder("/Moved", "Duplicates")
	if err != nil {
		t.Fatal(err)
	}
	if name != "Duplicates (2)" {
		t.Fatalf("want Duplicates (2), got %q", name)
	}
	if len(f.creates) != 1 || f.creates[0] != "/Moved/Duplicates (2)" {
		t.Fatalf("want exactly one create of the free name, got %v", f.creates)
	}
	if f.forces[0] != "false" {
		t.Fatalf("batch folder must be created with force_parent=false, got %q", f.forces[0])
	}
}

// An existing FILE (not folder) with the tool's name is still "taken": the
// allocator must step past it rather than fail the whole move. fakeGetInfo
// reports isdir:false, so this is the file case by construction.
func TestAllocBatchFolderStepsPastAFileOfTheSameName(t *testing.T) {
	_, sess, done := newBatchFake(map[string]bool{"/Moved/Empty Files": true})
	defer done()
	name, err := sess.allocBatchFolder("/Moved", "Empty Files")
	if err != nil {
		t.Fatal(err)
	}
	if name != "Empty Files (1)" {
		t.Fatalf("want Empty Files (1), got %q", name)
	}
}

// Lost race: the probe says the name is free, but someone creates it before
// our CreateFolder lands. DSM collapses the failure into a generic code, so
// the allocator cannot read "collision" from it — it re-probes, sees the name
// is now taken, and advances instead of giving up or spinning on it.
func TestAllocBatchFolderAdvancesAfterALostRace(t *testing.T) {
	f, sess, done := newBatchFake(nil)
	defer done()
	f.onCreate = func(f *batchFake, full string, attempt int) int {
		if attempt == 0 {
			// Another writer got there first: fail, and make it exist.
			f.taken[full] = true
			return 1100
		}
		return 0
	}
	name, err := sess.allocBatchFolder("/Moved", "Duplicates")
	if err != nil {
		t.Fatal(err)
	}
	if name != "Duplicates (1)" {
		t.Fatalf("want Duplicates (1) after losing the race, got %q", name)
	}
	if len(f.creates) != 2 {
		t.Fatalf("want exactly two create attempts, got %v", f.creates)
	}
}

// A genuine failure — no permission, quota, read-only share — leaves the name
// still free on the re-probe. That must be reported after ONE attempt, not
// retried 25 times, and not misread as a collision.
func TestAllocBatchFolderReportsGenuineFailureWithoutSpinning(t *testing.T) {
	f, sess, done := newBatchFake(nil)
	defer done()
	f.onCreate = func(f *batchFake, full string, attempt int) int { return 1100 }
	if _, err := sess.allocBatchFolder("/Moved", "Duplicates"); err == nil {
		t.Fatal("a create that keeps failing on a free name must be reported")
	}
	if len(f.creates) != 1 {
		t.Fatalf("want exactly one attempt on a genuine failure, got %d", len(f.creates))
	}
}

// ---------------------------------------------------- share-space mapping

// The share NAME and the directory it lives in are independent facts joined
// only by list_share: internal shares happen to agree ("Backups" at
// /volume1/Backups) but external ones do not ("usbshare1" at
// /volumeUSB1/usbshare). Deriving either side from the other by string
// surgery makes USB destinations unusable — this pins the mapping to the
// table.
func TestShareSpacePathUsesShareNamesNotDirectories(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch r.FormValue("api") {
		case "SYNO.API.Info":
			fakeInfo(w, r.FormValue("query"))
		case "SYNO.FileStation.List":
			if r.FormValue("method") != "list_share" {
				t.Errorf("unexpected List method %s", r.FormValue("method"))
			}
			fakeEnv(w, 0, map[string]any{"shares": []map[string]any{
				{"name": "Backups", "additional": map[string]any{"real_path": "/volume1/Backups"}},
				{"name": "usbshare1", "additional": map[string]any{"real_path": "/volumeUSB1/usbshare"}},
			}})
		default:
			t.Errorf("unexpected api %s", r.FormValue("api"))
		}
	}))
	defer ts.Close()
	sess := &fsSession{base: ts.URL}

	cases := []struct{ in, want string }{
		{"/volume1/Backups/sub/a.bin", "/Backups/sub/a.bin"},
		{"/volume1/Backups", "/Backups"},
		// The external share: the NAME goes in, the directory does not.
		{"/volumeUSB1/usbshare/x/y.mov", "/usbshare1/x/y.mov"},
		{"/volumeUSB1/usbshare", "/usbshare1"},
	}
	for _, c := range cases {
		got, err := sess.shareSpacePath(c.in)
		if err != nil || got != c.want {
			t.Errorf("shareSpacePath(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	// Paths File Station cannot address must stay errors: a volume root has
	// no share, and a directory that merely LOOKS like a share name must not
	// map (usbshare is the directory, usbshare1 is the share).
	for _, bad := range []string{"/volume1", "/volumeUSB1", "/volumeUSB1/usbshare1/x", "/somewhere/else"} {
		if got, err := sess.shareSpacePath(bad); err == nil {
			t.Errorf("shareSpacePath(%q) = %q, want error", bad, got)
		}
	}
}

// A session whose list_share fails must refuse to map anything (fail closed)
// rather than fall back to guessing from path prefixes.
func TestShareSpacePathFailsClosedWithoutTheTable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.FormValue("api") == "SYNO.API.Info" {
			fakeInfo(w, r.FormValue("query"))
			return
		}
		fakeEnv(w, 119, nil) // session expired
	}))
	defer ts.Close()
	sess := &fsSession{base: ts.URL}
	if got, err := sess.shareSpacePath("/volume1/Backups/a.bin"); err == nil {
		t.Fatalf("mapped %q with no share table — a native fallback has crept back in", got)
	}
}
