//go:build dev

// Development-only build (go build -tags dev). Lets the daemon run and be
// exercised off-NAS: it serves the packaged UI as static files, takes its
// volume roots from an env var, and mocks the DSM File Station Web API the
// daemon delegates mutations to (run.sh points DUPFINDER_DSM_URL back at
// the daemon itself). None of this is compiled into release builds.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// devPkgPrefix is the /webman/3rdparty/<id> prefix DSM mounts a package's UI
// under. Any id: the UI derives its API base from its own script tag, so a
// dev page can load it under either build's id, or (as the harness does)
// straight from the root.
var devPkgPrefix = regexp.MustCompile(`^/webman/3rdparty/[^/]+`)

// DUPFINDER_UI: serve this directory's files alongside the API, translating
// the "…/api.cgi/…" paths the packaged UI uses into /api routes.
func devServeUI(api http.Handler) http.Handler {
	uiDir := os.Getenv("DUPFINDER_UI")
	if uiDir == "" {
		return api
	}
	fs := http.FileServer(http.Dir(uiDir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/webapi/entry.cgi" || r.URL.Path == "/webapi/query.cgi" {
			mockWebAPI(w, r)
			return
		}
		r.URL.Path = devPkgPrefix.ReplaceAllString(r.URL.Path, "")
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
		if i := strings.Index(r.URL.Path, "api.cgi"); i >= 0 {
			r.URL.Path = "/api" + strings.TrimSuffix(r.URL.Path[i+len("api.cgi"):], "/")
			api.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			api.ServeHTTP(w, r)
			return
		}
		fs.ServeHTTP(w, r)
	})
}

// ------------------------------------------------- mock File Station API
// A minimal stand-in for DSM's entry.cgi implementing the subset the daemon
// uses, with File Station's documented semantics (collision → 1003/1805,
// task-based CopyMove) against the fixture volume. The smoke suite drives
// the real orchestration through it.

var (
	mockTaskMu sync.Mutex
	mockTasks  = map[string]int{} // taskid → FS error code, 0 = success
)

// devShareEntry mirrors the production shareEntry: the mock, like real DSM,
// owns an authoritative name↔path table and the daemon may never rebuild one
// side from the other. Shares on roots[0] keep name == directory (internal
// shares behave that way); every LATER root advertises each directory with a
// "1" suffixed to the share name — the exact shape of a USB share
// ("usbshare1" at /volumeUSB1/usbshare) — so the suites exercise the
// mismatch on every run.
type devShareEntry struct {
	name string
	real string
}

func devShareTable() []devShareEntry {
	var out []devShareEntry
	for i, root := range devVolumeRoots() {
		ents, _ := os.ReadDir(root)
		for _, e := range ents {
			if !e.IsDir() || skipName(e.Name()) {
				continue
			}
			name := e.Name()
			if i > 0 {
				name += "1"
			}
			out = append(out, devShareEntry{name: name, real: filepath.Join(root, e.Name())})
		}
	}
	return out
}

func devShareToPath(share string) (string, error) {
	if !strings.HasPrefix(share, "/") {
		return "", fmt.Errorf("bad share path %q", share)
	}
	seg := strings.SplitN(share[1:], "/", 2)
	for _, e := range devShareTable() {
		if seg[0] == e.name {
			if len(seg) == 2 {
				return filepath.Join(e.real, seg[1]), nil
			}
			return e.real, nil
		}
	}
	return "", fmt.Errorf("no such share %q", share)
}

func parseArr(v string) []string {
	if strings.HasPrefix(v, "[") {
		var out []string
		if json.Unmarshal([]byte(v), &out) == nil {
			return out
		}
	}
	if strings.HasPrefix(v, `"`) { // singular JSON-quoted string params
		var s string
		if json.Unmarshal([]byte(v), &s) == nil {
			return []string{s}
		}
	}
	return []string{v}
}

func mockEnv(w http.ResponseWriter, code int, data any) {
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

func mockCopyMove(srcShare, dstShare string) int {
	src, err := devShareToPath(srcShare)
	if err != nil {
		return 1001
	}
	dst, err := devShareToPath(dstShare)
	if err != nil {
		return 1002
	}
	if fi, err := os.Stat(dst); err != nil || !fi.IsDir() {
		return 1002
	}
	target := filepath.Join(dst, filepath.Base(src))
	if _, err := os.Lstat(target); err == nil {
		return 1001 // collision — reported like live DSM: the generic code
	}
	if err := os.Rename(src, target); err != nil {
		return 1001
	}
	// Test hook: a file whose NAME carries this marker is corrupted after the
	// move lands, simulating damage in transit — the one case move-time
	// verification exists to catch and the only way the smoke suite can
	// exercise the moved-but-failed-verification contract end to end. Dev
	// builds only (this whole file is behind the dev tag), and inert for any
	// file not deliberately named for it.
	if strings.Contains(filepath.Base(src), "corrupt-transit") {
		if fh, err := os.OpenFile(target, os.O_RDWR, 0); err == nil {
			b := make([]byte, 1)
			if _, rerr := fh.ReadAt(b, 0); rerr == nil {
				b[0] ^= 0xff
				fh.WriteAt(b, 0)
			}
			fh.Close()
		}
	}
	return 0
}

func mockWebAPI(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		r.ParseMultipartForm(32 << 20)
	} else {
		r.ParseForm()
	}
	api, method := r.FormValue("api"), r.FormValue("method")
	switch api {
	case "SYNO.API.Info":
		out := map[string]any{}
		for _, q := range strings.Split(r.FormValue("query"), ",") {
			out[q] = map[string]int{"minVersion": 1, "maxVersion": 3}
		}
		mockEnv(w, 0, out)
	case "SYNO.FileStation.CopyMove":
		switch method {
		case "start":
			code := mockCopyMove(parseArr(r.FormValue("path"))[0], parseArr(r.FormValue("dest_folder_path"))[0])
			id := fmt.Sprintf("t%d", time.Now().UnixNano())
			mockTaskMu.Lock()
			mockTasks[id] = code
			mockTaskMu.Unlock()
			mockEnv(w, 0, map[string]string{"taskid": id})
		case "status":
			mockTaskMu.Lock()
			code := mockTasks[r.FormValue("taskid")]
			mockTaskMu.Unlock()
			if code != 0 {
				mockEnv(w, code, nil)
				return
			}
			mockEnv(w, 0, map[string]any{"finished": true})
		default:
			mockEnv(w, 0, nil)
		}
	case "SYNO.FileStation.List":
		switch method {
		case "getinfo":
			files := []map[string]any{}
			for _, sp := range parseArr(r.FormValue("path")) {
				p, err := devShareToPath(sp)
				if err != nil {
					continue
				}
				fi, err := os.Lstat(p)
				if err != nil {
					files = append(files, map[string]any{"path": sp, "code": 408})
					continue
				}
				files = append(files, map[string]any{
					"path":  sp,
					"isdir": fi.IsDir(),
					"additional": map[string]any{
						"size": fi.Size(),
						"time": map[string]any{
							"crtime": createdTime(p, fi).Unix(),
							"mtime":  fi.ModTime().Unix(),
						},
					},
				})
			}
			mockEnv(w, 0, map[string]any{"files": files})
		case "list":
			dir, err := devShareToPath(parseArr(r.FormValue("folder_path"))[0])
			if err != nil {
				mockEnv(w, 408, nil)
				return
			}
			ents, err := os.ReadDir(dir)
			if err != nil {
				mockEnv(w, 408, nil)
				return
			}
			files := []map[string]any{}
			for _, e := range ents {
				files = append(files, map[string]any{"name": e.Name(), "isdir": e.IsDir()})
			}
			mockEnv(w, 0, map[string]any{"files": files, "total": len(files), "offset": 0})
		case "list_share":
			shares := []map[string]any{}
			var total, used int64
			if roots := devVolumeRoots(); len(roots) > 0 {
				total, used = diskUsage(roots[0])
			}
			for _, e := range devShareTable() {
				shares = append(shares, map[string]any{
					"name": e.name, "path": "/" + e.name,
					"additional": map[string]any{
						"real_path": e.real,
						"volume_status": map[string]any{
							"totalspace": total, "freespace": total - used,
						},
					},
				})
			}
			mockEnv(w, 0, map[string]any{"shares": shares, "total": len(shares)})
		default:
			mockEnv(w, 0, nil)
		}
	case "SYNO.FileStation.Rename":
		src, err := devShareToPath(parseArr(r.FormValue("path"))[0])
		if err != nil {
			mockEnv(w, 1200, nil)
			return
		}
		target := filepath.Join(filepath.Dir(src), parseArr(r.FormValue("name"))[0])
		if _, err := os.Lstat(target); err == nil {
			mockEnv(w, 1200, nil)
			return
		}
		if err := os.Rename(src, target); err != nil {
			mockEnv(w, 1200, nil)
			return
		}
		mockEnv(w, 0, nil)
	case "SYNO.FileStation.CreateFolder":
		parent, err := devShareToPath(parseArr(r.FormValue("folder_path"))[0])
		if err != nil {
			mockEnv(w, 1100, nil)
			return
		}
		full := filepath.Join(parent, parseArr(r.FormValue("name"))[0])
		mk := os.Mkdir
		if r.FormValue("force_parent") == "true" {
			mk = func(p string, m os.FileMode) error { return os.MkdirAll(p, m) }
		}
		if err := mk(full, 0o755); err != nil {
			mockEnv(w, 1100, nil)
			return
		}
		mockEnv(w, 0, nil)
	case "SYNO.FileStation.Delete":
		p, err := devShareToPath(parseArr(r.FormValue("path"))[0])
		if err != nil {
			mockEnv(w, 900, nil)
			return
		}
		if err := os.RemoveAll(p); err != nil {
			mockEnv(w, 900, nil)
			return
		}
		mockEnv(w, 0, nil)
	case "SYNO.FileStation.Upload":
		f, hdr, err := r.FormFile("file")
		if err != nil {
			mockEnv(w, 1800, nil)
			return
		}
		defer f.Close()
		destDir, derr := devShareToPath(r.FormValue("path"))
		if derr != nil {
			mockEnv(w, 1800, nil)
			return
		}
		out, err := os.OpenFile(filepath.Join(destDir, hdr.Filename),
			os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			if os.IsExist(err) {
				mockEnv(w, 1805, nil)
				return
			}
			mockEnv(w, 1800, nil)
			return
		}
		defer out.Close()
		if _, err := io.Copy(out, f); err != nil {
			mockEnv(w, 1800, nil)
			return
		}
		mockEnv(w, 0, nil)
	default:
		mockEnv(w, 102, nil)
	}
}

// DUPFINDER_DSM_URL points File Station calls at the mock Web API (run.sh
// sets it to the daemon's own address). Dev builds only — in release the
// DSM base comes solely from the CGI-forwarded header, so scans and
// mutations always ride a real DSM session.
func devDSMBase() string {
	return os.Getenv("DUPFINDER_DSM_URL")
}

// DUPFINDER_BIND_HOST overrides the daemon's loopback-only bind (dev builds
// only) so the ARM smoke run can reach a daemon inside a Docker container
// through published ports — Docker forwards to the container's interface,
// never to its loopback. Release builds always bind 127.0.0.1.
func devBindHost() string {
	return os.Getenv("DUPFINDER_BIND_HOST")
}

// DUPFINDER_STATE points persistence (results, hash cache, scan marker) at
// a writable directory in dev runs, which pass no -var — a var dir would
// also arm the daemon token, which the harness runs without. Release builds
// persist into the package var dir alone.
func devStateDir() string {
	return os.Getenv("DUPFINDER_STATE")
}

// DUPFINDER_ROOTS (colon-separated) substitutes for /volume* off-NAS.
func devVolumeRoots() []string {
	env := os.Getenv("DUPFINDER_ROOTS")
	if env == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(env, ":") {
		if p != "" {
			out = append(out, filepath.Clean(p))
		}
	}
	return out
}
