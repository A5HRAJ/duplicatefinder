package main

// Move-time full verification (the dialog's "Verify file contents after
// moving", daemon side). The fake File Station here maps one share onto a
// real temp directory and really renames on CopyMove, so the daemon's
// verification reads run against real bytes — and an afterMove hook lets a
// test corrupt or hide the destination in exactly the window verification
// exists to police: after the move, before the read-back.

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type moveVerifyFake struct {
	root      string            // real path of the "Backups" share
	afterMove func(dest string) // called with the real destination path after a rename
	created   []string          // CreateFolder calls, share-space
	// breakStatus makes the NEXT CopyMove status reply die on the wire after
	// the move itself really happened — the lost-answer case the staging
	// safety guards exist for.
	breakStatus bool
}

func (f *moveVerifyFake) real(share string) string {
	return filepath.Join(f.root, strings.TrimPrefix(share, "/Backups"))
}

func newMoveVerifyFake(t *testing.T) (*moveVerifyFake, *fsSession) {
	t.Helper()
	f := &moveVerifyFake{root: t.TempDir()}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch r.FormValue("api") {
		case "SYNO.API.Info":
			fakeInfo(w, r.FormValue("query"))
		case "SYNO.FileStation.List":
			switch r.FormValue("method") {
			case "list_share":
				fakeEnv(w, 0, map[string]any{"shares": []map[string]any{
					{"name": "Backups", "additional": map[string]any{"real_path": f.root}},
				}})
			case "getinfo":
				var paths []string
				json.Unmarshal([]byte(r.FormValue("path")), &paths)
				files := make([]map[string]any, 0, len(paths))
				for _, p := range paths {
					st, err := os.Stat(f.real(p))
					if err != nil {
						files = append(files, map[string]any{"path": p, "code": 408})
						continue
					}
					files = append(files, map[string]any{
						"path": p, "isdir": st.IsDir(),
						"additional": map[string]any{
							"size": st.Size(),
							"time": map[string]any{"mtime": st.ModTime().Unix()},
						},
					})
				}
				fakeEnv(w, 0, map[string]any{"files": files})
			default:
				t.Errorf("unexpected List method %s", r.FormValue("method"))
			}
		case "SYNO.FileStation.CreateFolder":
			var dirs, names []string
			json.Unmarshal([]byte(r.FormValue("folder_path")), &dirs)
			json.Unmarshal([]byte(r.FormValue("name")), &names)
			for i := range dirs {
				full := dirs[i] + "/" + names[i]
				f.created = append(f.created, full)
				if r.FormValue("force_parent") == "true" {
					os.MkdirAll(f.real(full), 0o755)
				} else {
					os.Mkdir(f.real(full), 0o755)
				}
			}
			fakeEnv(w, 0, map[string]any{"folders": []any{}})
		case "SYNO.FileStation.CopyMove":
			if r.FormValue("method") == "start" {
				var srcs []string
				json.Unmarshal([]byte(r.FormValue("path")), &srcs)
				// dest_folder_path arrives as a JSON-quoted string (matching
				// copyMoveOnce); tolerate an array form too.
				destDir := r.FormValue("dest_folder_path")
				var ds string
				var dd []string
				if json.Unmarshal([]byte(destDir), &ds) == nil {
					destDir = ds
				} else if json.Unmarshal([]byte(destDir), &dd) == nil && len(dd) > 0 {
					destDir = dd[0]
				}
				for _, sp := range srcs {
					dst := f.real(destDir + "/" + filepath.Base(sp))
					if err := os.Rename(f.real(sp), dst); err != nil {
						fakeEnv(w, 1001, nil)
						return
					}
					if f.afterMove != nil {
						f.afterMove(dst)
					}
				}
				fakeEnv(w, 0, map[string]string{"taskid": "t1"})
				return
			}
			if f.breakStatus {
				f.breakStatus = false
				if hj, ok := w.(http.Hijacker); ok {
					if conn, _, err := hj.Hijack(); err == nil {
						conn.Close() // the reply dies on the wire
						return
					}
				}
			}
			fakeEnv(w, 0, map[string]any{"finished": true})
		case "SYNO.FileStation.Rename":
			var ps, ns []string
			json.Unmarshal([]byte(r.FormValue("path")), &ps)
			json.Unmarshal([]byte(r.FormValue("name")), &ns)
			for i := range ps {
				os.Rename(f.real(ps[i]), filepath.Join(filepath.Dir(f.real(ps[i])), ns[i]))
			}
			fakeEnv(w, 0, nil)
		case "SYNO.FileStation.Delete":
			var dp []string
			json.Unmarshal([]byte(r.FormValue("path")), &dp)
			for _, p := range dp {
				os.RemoveAll(f.real(p))
			}
			fakeEnv(w, 0, map[string]any{"finished": true})
		default:
			t.Errorf("unexpected api %s", r.FormValue("api"))
		}
	}))
	t.Cleanup(ts.Close)
	return f, &fsSession{base: ts.URL}
}

// mkMoveFile writes an 80 KiB file (big enough that rot can hide past the
// 64 KiB prefix) and returns its path plus the identity a duplicates scan
// would have recorded: size, mtime, prefix fingerprint and FULL hash.
func mkMoveFile(t *testing.T, f *moveVerifyFake, name string) (string, entIdent, []byte) {
	t.Helper()
	body := bytes.Repeat([]byte{0x42}, 80*1024)
	p := filepath.Join(f.root, name)
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	pfx, err := hashFile(func() (*os.File, error) { return os.Open(p) }, 64*1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	full, err := hashFile(func() (*os.File, error) { return os.Open(p) }, -1, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p, entIdent{size: fi.Size(), mod: fmtTime(fi.ModTime()), pfx: pfx[:32], hash: full}, body
}

func TestMoveVerifyHappyPath(t *testing.T) {
	f, sess := newMoveVerifyFake(t)
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	p, want, body := mkMoveFile(t, f, "a.bin")
	err := execMoveFS(sess, p, p, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, nil, true, nil)
	if err != nil {
		t.Fatalf("verified move failed: %v", err)
	}
	got, rerr := os.ReadFile(filepath.Join(f.root, "dest", "a.bin"))
	if rerr != nil || !bytes.Equal(got, body) {
		t.Fatalf("file not intact at destination: %v", rerr)
	}
}

// Rot past the 64 KiB prefix under a restored mtime slips the fingerprint
// check — only the full pre-move hash can refuse it. The refusal must land
// BEFORE the move, while the source still exists, and before any batch
// folder is allocated.
func TestMoveVerifyRefusesDeepRotBeforeMoving(t *testing.T) {
	f, sess := newMoveVerifyFake(t)
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	p, want, body := mkMoveFile(t, f, "a.bin")
	fi, _ := os.Stat(p)
	rotted := append([]byte(nil), body...)
	rotted[70*1024] ^= 0xff
	if err := os.WriteFile(p, rotted, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	batchCalls := 0
	batch := func() (string, error) { batchCalls++; return "/Backups/dest", nil }
	err := execMoveFS(sess, p, p, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, batch, true, nil)
	if err == nil || !strings.Contains(err.Error(), "file contents changed since the scan") {
		t.Fatalf("deep source rot must refuse the move, got %v", err)
	}
	if batchCalls != 0 {
		t.Fatal("a verify refusal allocated the batch folder — refusals must precede the first side effect")
	}
	if _, serr := os.Stat(p); serr != nil {
		t.Fatal("the refused file must still be at its source")
	}
	// The same move WITHOUT verification goes through: that is exactly the
	// trade the checkbox puts in the user's hands.
	if err := execMoveFS(sess, p, p, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, nil, false, nil); err != nil {
		t.Fatalf("unverified move should not notice deep rot: %v", err)
	}
}

// Corruption in transit: the destination copy differs from what left the
// source. The move HAS happened, so the error must be the movedButError kind
// (the row prunes; the message carries the truth) and must say the content
// does not match.
func TestMoveVerifyCatchesTransitCorruption(t *testing.T) {
	f, sess := newMoveVerifyFake(t)
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	p, want, _ := mkMoveFile(t, f, "a.bin")
	f.afterMove = func(dest string) {
		fh, _ := os.OpenFile(dest, os.O_WRONLY, 0)
		fh.WriteAt([]byte{0x00}, 70*1024)
		fh.Close()
	}
	err := execMoveFS(sess, p, p, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, nil, true, nil)
	var mbe movedButError
	if !errors.As(err, &mbe) || !strings.Contains(err.Error(), "does not match the original content") {
		t.Fatalf("transit corruption must surface as movedButError, got %v", err)
	}
	// And with verification off the same damage passes silently — the
	// pre-checkbox behavior, preserved for the user who is deleting anyway.
	p2, want2, _ := mkMoveFile(t, f, "b.bin")
	if err := execMoveFS(sess, p2, p2, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want2}, nil, false, nil); err != nil {
		t.Fatalf("unverified move must not read the destination: %v", err)
	}
}

// A destination the daemon cannot READ is not a destination that does not
// MATCH: the first is almost always a permissions gap and must say so,
// never cry corruption.
func TestMoveVerifyDistinguishesUnreadableDestination(t *testing.T) {
	f, sess := newMoveVerifyFake(t)
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	p, want, _ := mkMoveFile(t, f, "a.bin")
	var moved string
	f.afterMove = func(dest string) {
		moved = dest
		os.Chmod(dest, 0)
	}
	defer func() {
		if moved != "" {
			os.Chmod(moved, 0o644)
		}
	}()
	err := execMoveFS(sess, p, p, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, nil, true, nil)
	var mbe movedButError
	if !errors.As(err, &mbe) || !strings.Contains(err.Error(), "could not be read back") {
		t.Fatalf("unreadable destination must be its own message, got %v", err)
	}
	if strings.Contains(err.Error(), "does not match") {
		t.Fatalf("an unreadable destination must not be reported as corruption: %v", err)
	}
}

// Rows without a scan-recorded hash (temp and empty files) still get real
// verification: the fresh pre-move hash becomes the reference the
// destination must reproduce — the move itself is verified even where the
// scan recorded no content identity.
func TestMoveVerifyHashlessRowUsesFreshReference(t *testing.T) {
	f, sess := newMoveVerifyFake(t)
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	p, want, body := mkMoveFile(t, f, "a.tmp")
	want.pfx, want.hash = "", "" // a temp_files row: identity is size+mtime only
	if err := execMoveFS(sess, p, p, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, nil, true, nil); err != nil {
		t.Fatalf("verified hashless move failed: %v", err)
	}
	if got, rerr := os.ReadFile(filepath.Join(f.root, "dest", "a.tmp")); rerr != nil || !bytes.Equal(got, body) {
		t.Fatalf("file not intact at destination: %v", rerr)
	}
	// And transit corruption is still caught — against the fresh reference.
	p2, want2, _ := mkMoveFile(t, f, "b.tmp")
	want2.pfx, want2.hash = "", ""
	f.afterMove = func(dest string) {
		fh, _ := os.OpenFile(dest, os.O_WRONLY, 0)
		fh.WriteAt([]byte{0x00}, 70*1024)
		fh.Close()
	}
	err := execMoveFS(sess, p2, p2, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want2}, nil, true, nil)
	var mbe movedButError
	if !errors.As(err, &mbe) || !strings.Contains(err.Error(), "does not match the original content") {
		t.Fatalf("hashless transit corruption must still be caught, got %v", err)
	}
}

// A name collision forces the staged " (n)" rename; the read-back must
// verify the file under the name it actually landed with, not the original.
func TestMoveVerifyReadsBackTheCollisionRename(t *testing.T) {
	f, sess := newMoveVerifyFake(t)
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	p, want, body := mkMoveFile(t, f, "a.bin")
	if err := os.WriteFile(filepath.Join(f.root, "dest", "a.bin"), []byte("squatter"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := execMoveFS(sess, p, p, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, nil, true, nil); err != nil {
		t.Fatalf("verified collision move failed: %v", err)
	}
	got, rerr := os.ReadFile(filepath.Join(f.root, "dest", "a (1).bin"))
	if rerr != nil || !bytes.Equal(got, body) {
		t.Fatalf("renamed copy not intact at destination: %v", rerr)
	}
}

// A source the daemon cannot read refuses BEFORE anything moves. A row with
// a scan fingerprint hits the (older) prefix gate first and refuses with the
// changed-since-scan wording; a hashless row's first content read is the
// verification pre-read, which refuses with its own message. Both keep the
// file where it is.
func TestMoveVerifyRefusesUnreadableSource(t *testing.T) {
	f, sess := newMoveVerifyFake(t)
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	p, want, _ := mkMoveFile(t, f, "a.bin")
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(p, 0o644)
	err := execMoveFS(sess, p, p, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, nil, true, nil)
	if err == nil || !strings.Contains(err.Error(), "changed since the scan") {
		t.Fatalf("a fingerprinted row refuses at the prefix gate, got %v", err)
	}
	if _, serr := os.Stat(p); serr != nil {
		t.Fatal("the refused file must still be at its source")
	}
	// The hashless row reaches the verification pre-read and gets the
	// distinct could-not-verify wording.
	hashless := want
	hashless.pfx, hashless.hash = "", ""
	err = execMoveFS(sess, p, p, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{hashless}, nil, true, nil)
	if err == nil || !strings.Contains(err.Error(), "could not read the file to verify it") {
		t.Fatalf("a hashless unreadable source must refuse with the verify wording, got %v", err)
	}
	if _, serr := os.Stat(p); serr != nil {
		t.Fatal("the refused file must still be at its source")
	}
}

// The staging move's status reply dies on the wire AFTER the rename into the
// staging folder really happened: the folder now holds the ONLY copy. The
// old code deleted it on that evidence — permanent data loss. It must park
// instead, report movedButError so the row prunes, and (verification being
// on) verify the parked copy in place.
func TestStagedMoveLostReplyParksInsteadOfDeleting(t *testing.T) {
	f, sess := newMoveVerifyFake(t)
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	p, want, body := mkMoveFile(t, f, "a.bin")
	// Occupy the destination name so moveViaFS must stage…
	if err := os.WriteFile(filepath.Join(f.root, "dest", "a.bin"), []byte("squatter"), 0o644); err != nil {
		t.Fatal(err)
	}
	// …and kill the staging hop's status reply after the rename lands.
	f.breakStatus = true
	err := execMoveFS(sess, p, p, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, nil, true, nil)
	var mbe movedButError
	if !errors.As(err, &mbe) {
		t.Fatalf("a parked file must report movedButError, got %v", err)
	}
	if !strings.Contains(err.Error(), "parked in") || !strings.Contains(err.Error(), "verified intact") {
		t.Fatalf("the parked copy must be located and verified in the message, got %v", err)
	}
	// The only copy must still exist, inside the staging folder.
	ents, _ := os.ReadDir(filepath.Join(f.root, "dest"))
	found := false
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".dupfinder-tmp-") {
			got, rerr := os.ReadFile(filepath.Join(f.root, "dest", e.Name(), "a.bin"))
			if rerr == nil && bytes.Equal(got, body) {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the staging folder holding the only copy was deleted — that is data loss")
	}
	if _, serr := os.Stat(p); serr == nil {
		t.Fatal("the source should be gone — the staging move really happened")
	}
}

// identMatches must hand back the strongest identity on offer: the
// hash-carrying one beats prefix-only beats loose, whatever the order.
func TestIdentMatchesPrefersTheStrongestIdentity(t *testing.T) {
	e := fsEntry{}
	e.Additional.Size = 10
	e.Additional.Time.Mtime = 100
	mod := fmtTime(time.Unix(100, 0))
	loose := entIdent{size: 10, mod: mod}
	pfxed := entIdent{size: 10, mod: mod, pfx: "aa"}
	hashed := entIdent{size: 10, mod: mod, pfx: "aa", hash: "bb"}
	got, ok := identMatches(e, []entIdent{loose, pfxed, hashed})
	if !ok || got.hash != "bb" {
		t.Fatalf("want the hash-carrying identity, got %+v", got)
	}
	got, ok = identMatches(e, []entIdent{loose, pfxed})
	if !ok || got.pfx != "aa" || got.hash != "" {
		t.Fatalf("want the prefix-carrying identity, got %+v", got)
	}
	got, ok = identMatches(e, []entIdent{loose})
	if !ok || got.pfx != "" {
		t.Fatalf("want the loose identity, got %+v", got)
	}
}

// Preserve mode: the read-back must find the file inside the batch folder's
// mirrored chain — the derived destination path, not the picked folder.
func TestMoveVerifyReadsBackInsideTheBatchFolder(t *testing.T) {
	f, sess := newMoveVerifyFake(t)
	os.Mkdir(filepath.Join(f.root, "dest"), 0o755)
	os.MkdirAll(filepath.Join(f.root, "photos"), 0o755)
	p, want, body := mkMoveFile(t, f, "photos/a.bin")
	os.Mkdir(filepath.Join(f.root, "dest", "Duplicates"), 0o755)
	batch := func() (string, error) { return "/Backups/dest/Duplicates", nil }
	err := execMoveFS(sess, p, p, "/Backups/dest", filepath.Join(f.root, "dest"),
		[]entIdent{want}, batch, true, nil)
	if err != nil {
		t.Fatalf("verified preserve move failed: %v", err)
	}
	final := filepath.Join(f.root, "dest", "Duplicates", f.root, "photos", "a.bin")
	got, rerr := os.ReadFile(final)
	if rerr != nil || !bytes.Equal(got, body) {
		t.Fatalf("file not intact inside the batch tree at %s: %v", final, rerr)
	}
}
