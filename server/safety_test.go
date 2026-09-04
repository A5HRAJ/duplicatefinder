package main

// Unit tests for the data-safety and auth-hardening logic. These cover what
// the HTTP smoke suite cannot reach: the no-replace rename used by token
// creation, the empty-folder hard confirmation, duplicate matching rules,
// the keep-one-per-group rule and its canonicalization, pinned-handle
// vetting, and the token middleware. Mutation execution lives in
// fsapi_test.go.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ------------------------------------------------- 1.1 atomic no-overwrite

func TestRenameNoReplaceRefusesExisting(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := renameNoReplace(src, dst); !errors.Is(err, syscall.EEXIST) {
		t.Fatalf("want EEXIST, got %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "precious" {
		t.Fatalf("existing file clobbered: %q", b)
	}
	if _, err := os.Lstat(src); err != nil {
		t.Fatalf("source vanished: %v", err)
	}
}

// efWalk replays entries the way fs.WalkDir hands them to the scan: parents
// before children, depth-first, each path a display path under a scan root.
// The empty-folder accumulator decides a directory's fate when the walk
// leaves it, so the ORDER is the thing under test as much as the contents.
type efStep struct {
	path       string
	isDir      bool
	unreadable bool
}

func efRun(t *testing.T, steps []efStep, confirm func(string) bool) ([]FileEnt, *TruncInfo) {
	t.Helper()
	ef := newEmptyFolderScan()
	for _, st := range steps {
		if st.unreadable {
			ef.noteUnreadable(st.path)
			continue
		}
		ef.visit(0, fEnt{path: st.path, name: filepath.Base(st.path),
			dir: filepath.Dir(st.path), isDir: st.isDir})
	}
	files, trunc, _ := ef.finish(&Server{}, nil, func(p string) (bool, error) { return confirm(p), nil })
	return files, trunc
}

func efNames(ents []FileEnt) []string {
	var out []string
	for _, e := range ents {
		out = append(out, e.Name)
	}
	return out
}

func TestScanEmptyFoldersHardConfirm(t *testing.T) {
	root := t.TempDir()
	empty := filepath.Join(root, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	hiddenOnly := filepath.Join(root, "hiddenonly")
	if err := os.MkdirAll(filepath.Join(hiddenOnly, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hiddenOnly, ".hidden", "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Both directories look childless to the walk: hidden names are skipped,
	// so the walker never descends into .hidden at all.
	// The confirm stub reads the fixture directly; production confirms
	// through File Station (confirmEmpty), which the smoke suite covers
	// end-to-end. This test owns the candidate logic: the hard confirm
	// must veto folders whose only content the walker skipped.
	confirm := func(p string) bool {
		ents, err := os.ReadDir(p)
		return err == nil && len(ents) == 0
	}
	got, trunc := efRun(t, []efStep{
		{path: empty, isDir: true},
		{path: hiddenOnly, isDir: true},
	}, confirm)
	if len(got) != 1 || got[0].Name != "empty" {
		t.Fatalf("want only the truly empty folder, got %+v", got)
	}
	if trunc != nil {
		t.Fatalf("two candidates must not report truncation: %+v", trunc)
	}
}

// Only the TOPMOST directory whose subtree holds no file is offered, and a
// directory holding content releases the empty children it was holding back.
// These are the rules the old map-based pass encoded in hasFile/isDir; the
// streaming form has to reproduce them exactly, including when the file that
// settles a directory's fate arrives AFTER its empty child.
func TestScanEmptyFoldersTopmostRule(t *testing.T) {
	yes := func(string) bool { return true }
	cases := []struct {
		name  string
		steps []efStep
		want  []string
	}{
		{
			name: "child of a root is topmost",
			steps: []efStep{
				{path: "/r/a", isDir: true},
			},
			want: []string{"a"},
		},
		{
			name: "empty parent supersedes its empty child",
			steps: []efStep{
				{path: "/r/c", isDir: true},
				{path: "/r/c/d", isDir: true},
			},
			want: []string{"c"},
		},
		{
			name: "a directory with a file releases its empty child",
			steps: []efStep{
				{path: "/r/a", isDir: true},
				{path: "/r/a/b", isDir: true},
				{path: "/r/a/f.txt"},
			},
			want: []string{"b"},
		},
		{
			name: "same, with the file seen before the empty child",
			steps: []efStep{
				{path: "/r/a", isDir: true},
				{path: "/r/a/f.txt"},
				{path: "/r/a/b", isDir: true},
			},
			want: []string{"b"},
		},
		{
			name: "a file deep below keeps every ancestor out of the report",
			steps: []efStep{
				{path: "/r/a", isDir: true},
				{path: "/r/a/b", isDir: true},
				{path: "/r/a/b/c", isDir: true},
				{path: "/r/a/b/c/f.txt"},
			},
			want: nil,
		},
		{
			name: "an unreadable directory is never empty, nor are its ancestors",
			steps: []efStep{
				{path: "/r/a", isDir: true},
				{path: "/r/a/vault", isDir: true},
				{path: "/r/a/vault", unreadable: true},
			},
			want: nil,
		},
		{
			name: "an unreadable sibling does not protect a genuinely empty one",
			steps: []efStep{
				{path: "/r/a", isDir: true},
				{path: "/r/a/vault", isDir: true},
				{path: "/r/a/vault", unreadable: true},
				{path: "/r/a/gone", isDir: true},
			},
			want: []string{"gone"},
		},
		{
			name: "several roots each report their own children",
			steps: []efStep{
				{path: "/r1/a", isDir: true},
				{path: "/r2/b", isDir: true},
			},
			want: []string{"a", "b"},
		},
		{
			// The stack is popped by path prefix, so a sibling whose name
			// merely STARTS with the open directory's name must not be
			// mistaken for a child — that would inherit its parent's state
			// and could report a folder with content as empty.
			name: "a sibling sharing a name prefix is not a child",
			steps: []efStep{
				{path: "/r/foo", isDir: true},
				{path: "/r/foo/f.txt"},
				{path: "/r/foo2", isDir: true},
			},
			want: []string{"foo2"},
		},
		{
			name: "and the reverse order, where the empty one comes first",
			steps: []efStep{
				{path: "/r/foo", isDir: true},
				{path: "/r/foo2", isDir: true},
				{path: "/r/foo2/f.txt"},
			},
			want: []string{"foo"},
		},
		{
			// A root the walk never entered reports unreadable with nothing
			// on the stack: it must be ignored, not crash and not affect a
			// later root.
			name: "a root that could not be opened at all",
			steps: []efStep{
				{path: "/r1", unreadable: true},
				{path: "/r2/a", isDir: true},
			},
			want: []string{"a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := efRun(t, tc.steps, yes)
			names := efNames(got)
			if strings.Join(names, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("want %v, got %v", tc.want, names)
			}
		})
	}
}

// The scan holds a frame per OPEN directory, not one per directory: a deep
// tree with a million siblings must not grow the stack past its depth.
func TestScanEmptyFoldersHoldsOnlyTheOpenPath(t *testing.T) {
	ef := newEmptyFolderScan()
	for i := 0; i < 5000; i++ {
		ef.visit(0, fEnt{path: fmt.Sprintf("/r/sib%05d", i), isDir: true})
		ef.visit(0, fEnt{path: fmt.Sprintf("/r/sib%05d/f.txt", i)})
	}
	if len(ef.stack) > 2 {
		t.Fatalf("stack should hold the root and the open directory only, holds %d", len(ef.stack))
	}
}

// The empty-folder scan was the one tool with no cap: on a volume with more
// empty folders than the stored-results budget it must keep the first
// emptyFolderCap by path, report the rest, and — because every confirmation
// is a File Station round trip — never confirm beyond the cap.
func TestScanEmptyFoldersCapsCandidates(t *testing.T) {
	n := emptyFolderCap + 25
	steps := make([]efStep, 0, n)
	for i := 0; i < n; i++ {
		steps = append(steps, efStep{path: fmt.Sprintf("/vol/root/d%06d", i), isDir: true})
	}
	confirms := 0
	got, trunc := efRun(t, steps, func(string) bool { confirms++; return true })
	if len(got) != emptyFolderCap {
		t.Fatalf("want the cap kept, got %d", len(got))
	}
	if confirms != emptyFolderCap {
		t.Fatalf("confirmation calls must stop at the cap, made %d", confirms)
	}
	if trunc == nil || trunc.Files != 25 || trunc.Cap != emptyFolderCap {
		t.Fatalf("truncation must be reported: %+v", trunc)
	}
	if got[0].Name != "d000000" {
		t.Fatalf("cap must keep the first by path, got %s", got[0].Name)
	}
}

// The move's identity check has to catch a file rewritten in place with
// content of the same length and its modification time restored — File
// Station's size and mtime both still agree, so only the content
// fingerprint the duplicates scan recorded can tell.
func TestMoveIdentityCatchesSameSizeSameMtimeRewrite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "photo.bin")
	if err := os.WriteFile(p, []byte("original-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	pfx, err := hashFile(func() (*os.File, error) { return os.Open(p) }, 64*1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := pfx[:32]
	if !contentPrefixUnchanged(p, want) {
		t.Fatal("an untouched file must pass its own fingerprint")
	}
	// Same length, mtime restored — indistinguishable to File Station.
	fi, _ := os.Stat(p)
	if err := os.WriteFile(p, []byte("REPLACED-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	if contentPrefixUnchanged(p, want) {
		t.Fatal("a rewritten file must fail the fingerprint — it would have been moved as a duplicate")
	}
	// A vanished file is a refusal, never a pass.
	os.Remove(p)
	if contentPrefixUnchanged(p, want) {
		t.Fatal("an unreadable file must not pass")
	}
	// Results from an older build carry no fingerprint: the check is skipped
	// rather than blocking every move.
	if !contentPrefixUnchanged(p, "") {
		t.Fatal("no recorded fingerprint means no content check")
	}
}

// identMatches must hand back the identity it matched, preferring one that
// carries a fingerprint so the caller can make the stronger check.
func TestIdentMatchesPrefersFingerprintedIdentity(t *testing.T) {
	e := fsEntry{}
	e.Additional.Size = 10
	e.Additional.Time.Mtime = 1722173315
	mod := fmtTime(time.Unix(1722173315, 0))
	got, ok := identMatches(e, []entIdent{
		{size: 10, mod: mod},
		{size: 10, mod: mod, pfx: "abc"},
	})
	if !ok || got.pfx != "abc" {
		t.Fatalf("want the fingerprinted identity, got %+v (ok=%v)", got, ok)
	}
	if _, ok := identMatches(e, []entIdent{{size: 11, mod: mod}}); ok {
		t.Fatal("a different size must not match")
	}
}

// Content reads go through the pinned root handle and must refuse to
// traverse a symlink swapped in after enumeration — the scanner-TOCTOU
// guard. OpenRel must also reject relative paths that try to walk up.
func TestOpenRelRefusesSwappedSymlink(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "f.txt"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := openDirHandle(root)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	f, err := h.OpenRel("sub/f.txt")
	if err != nil {
		t.Fatalf("legitimate open through the handle failed: %v", err)
	}
	b := make([]byte, 6)
	if _, err := io.ReadFull(f, b); err != nil || string(b) != "inside" {
		t.Fatalf("read through handle: %q %v", b, err)
	}
	f.Close()

	// Swap: sub is renamed away and replaced by a symlink to an outside
	// directory holding a same-named file. The open must fail, never
	// follow the link.
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "f.txt"), []byte("OUTSIDE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "sub"), filepath.Join(root, "gone")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "sub")); err != nil {
		t.Fatal(err)
	}
	if f, err := h.OpenRel("sub/f.txt"); err == nil {
		f.Close()
		t.Fatal("open followed a swapped-in symlink outside the root")
	}

	// A symlinked FILE must be refused too, not just directories.
	if err := os.Symlink(filepath.Join(outside, "f.txt"), filepath.Join(root, "gone", "link.txt")); err != nil {
		t.Fatal(err)
	}
	if f, err := h.OpenRel("gone/link.txt"); err == nil {
		f.Close()
		t.Fatal("open followed a symlinked file")
	}

	// Upward traversal is rejected outright.
	if f, err := h.OpenRel("../outside/f.txt"); err == nil {
		f.Close()
		t.Fatal("open accepted an upward-traversing relative path")
	}
}

// Files whose creation time File Station could not answer must never group
// under created-date matching — an unknown value is not a confirmed match.
func TestCreatedMatchUnknownNeverGroups(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.bin"), filepath.Join(dir, "b.bin")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("same"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{}
	files := []fEnt{
		{path: a, name: "a.bin", dir: dir, size: 4},
		{path: b, name: "b.bin", dir: dir, size: 4},
	}
	if g, _ := s.scanDuplicates(files, MatchOpts{Created: true}, nil, make(chan struct{})); len(g) != 0 {
		t.Fatalf("unknown created dates must not group: %+v", g)
	}
	// With known equal creation times the same pair groups.
	ct := time.Now()
	files[0].created, files[1].created = ct, ct
	if g, _ := s.scanDuplicates(files, MatchOpts{Created: true}, nil, make(chan struct{})); len(g) != 1 {
		t.Fatalf("known equal created dates must group: %+v", g)
	}
}

// --------------------------------------------- 2.2 keep one copy per group

func TestKeepOneDrops(t *testing.T) {
	g := Group{Files: []FileEnt{
		{Dir: "/v/a", Name: "f"},
		{Dir: "/v/b", Name: "f"},
	}}
	all := map[string]bool{"/v/a/f": true, "/v/b/f": true}
	bothExist := map[string]bool{"/v/a/f": true, "/v/b/f": true}

	drops := keepOneDrops([]Group{g}, all, nil, newDirResolver(), bothExist)
	if len(drops) != 1 || !drops["/v/b/f"] {
		t.Fatalf("whole group requested: want the last file held back, got %v", drops)
	}
	if d := keepOneDrops([]Group{g}, map[string]bool{"/v/a/f": true}, nil, newDirResolver(), bothExist); len(d) != 0 {
		t.Fatalf("one unrequested peer survives, nothing to hold back: got %v", d)
	}
	// A protected reference file cannot move, so it always survives.
	if d := keepOneDrops([]Group{g}, all, []string{"/v/a"}, newDirResolver(), bothExist); len(d) != 0 {
		t.Fatalf("reference file survives, nothing to hold back: got %v", d)
	}
	// Files outside any group are never affected.
	if d := keepOneDrops(nil, all, nil, newDirResolver(), bothExist); len(d) != 0 {
		t.Fatalf("no groups: got %v", d)
	}

	// Staleness: an unrequested peer that vanished since the scan is no
	// survivor — the requested copy must be held back.
	onlyA := map[string]bool{"/v/a/f": true}
	if d := keepOneDrops([]Group{g}, onlyA, nil, newDirResolver(), onlyA); len(d) != 1 || !d["/v/a/f"] {
		t.Fatalf("vanished peer: want the requested copy held back, got %v", d)
	}
	// Whole group requested but one copy vanished: hold back the copy that
	// still exists, never the missing one.
	if d := keepOneDrops([]Group{g}, all, nil, newDirResolver(), onlyA); len(d) != 1 || !d["/v/a/f"] {
		t.Fatalf("half-vanished group: want the existing copy held back, got %v", d)
	}
	// File Station unavailable (empty exists map): fail closed — hold one
	// back rather than trusting the stale snapshot.
	if d := keepOneDrops([]Group{g}, all, nil, newDirResolver(), map[string]bool{}); len(d) != 1 {
		t.Fatalf("unknown existence: want one held back, got %v", d)
	}
}

// -------------------------------------------- symlink-alias canonicalization

func TestDirResolverCanonicalizesAliases(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	canon := newDirResolver()
	pa, err := canon.strictPath(filepath.Join(alias, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	pr, err := canon.strictPath(filepath.Join(real, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if pa != pr {
		t.Fatalf("alias and real path did not canonicalize equal: %q vs %q", pa, pr)
	}
	// A reference dir given as (or matched through) an alias still protects.
	if !isUnder(pa, []string{canon.dir(alias)}) {
		t.Fatal("canonical path not under the canonical alias ref dir")
	}
	// Unresolvable parent: strict fails, lenient falls back to the clean path.
	if _, err := canon.strictPath(filepath.Join(dir, "gone", "f.txt")); err == nil {
		t.Fatal("strictPath should fail for a missing parent")
	}
	if p := canon.path("/no/such/dir/f"); p != "/no/such/dir/f" {
		t.Fatalf("lenient path fallback: got %q", p)
	}
}

func TestKeepOneDropsAlias(t *testing.T) {
	dir := t.TempDir()
	realA := filepath.Join(dir, "A")
	realB := filepath.Join(dir, "B")
	for _, d := range []string{realA, realB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "f"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	aliasB := filepath.Join(dir, "Balias")
	if err := os.Symlink(realB, aliasB); err != nil {
		t.Fatal(err)
	}
	g := Group{Files: []FileEnt{
		{Dir: realA, Name: "f"},
		{Dir: realB, Name: "f"},
	}}
	// The whole group is requested, but one copy is named through the alias.
	// v0058 compared raw strings, saw B/f as an unrequested survivor, and let
	// every copy move.
	canon := newDirResolver()
	requested := map[string]bool{}
	for _, p := range []string{filepath.Join(realA, "f"), filepath.Join(aliasB, "f")} {
		cp, err := canon.strictPath(p)
		if err != nil {
			t.Fatal(err)
		}
		requested[cp] = true
	}
	exists := map[string]bool{
		canon.path(filepath.Join(realA, "f")): true,
		canon.path(filepath.Join(realB, "f")): true,
	}
	drops := keepOneDrops([]Group{g}, requested, nil, canon, exists)
	if len(drops) != 1 || !drops[canon.path(filepath.Join(realB, "f"))] {
		t.Fatalf("aliased group drain: want the last file held back, got %v", drops)
	}
}

func TestDirHandleBasics(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(filepath.Join(real, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(real, "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openDirHandle(file); err == nil {
		t.Fatal("openDirHandle must refuse a regular file")
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	h, err := openDirHandle(alias)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	want, _ := filepath.EvalSymlinks(real)
	if got, err := h.Canon(); err != nil || got != want {
		t.Fatalf("Canon through alias: got %q (%v), want %q", got, err, want)
	}
}

func TestScrubErr(t *testing.T) {
	err := errors.New("rename /proc/self/fd/7/a.txt /proc/self/fd/9/a (1).txt: file exists")
	got := scrubErr(err, map[string]string{"/proc/self/fd/7": "/vol/src", "/proc/self/fd/9": "/vol/dst"})
	want := "rename /vol/src/a.txt /vol/dst/a (1).txt: file exists"
	if got.Error() != want {
		t.Fatalf("scrubErr: got %q, want %q", got, want)
	}
	// Unknown handle addresses are masked rather than leaked.
	got = scrubErr(errors.New("open /proc/self/fd/12/x: permission denied"), nil)
	if strings.Contains(got.Error(), "/proc/self/fd") {
		t.Fatalf("proc path leaked: %q", got)
	}
	// Clean errors pass through typed and unchanged.
	if scrubErr(nil, nil) != nil {
		t.Fatal("scrubErr(nil) != nil")
	}
	typed := os.ErrNotExist
	if scrubErr(typed, nil) != typed {
		t.Fatal("clean error was rewrapped")
	}
}

// Scan roots are re-resolved inside the scan goroutine: with no allowed
// volumes configured (unit-test environment), every root must be refused at
// scan time rather than enumerated.
func TestRunScanRevalidatesRoots(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "big.bin"), make([]byte, 2<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{results: map[string]*toolResult{}}
	cancel := make(chan struct{})
	s.job = jobState{Running: true, Tool: "temp_files", cancel: cancel}
	s.runScan(ScanReq{Tool: "temp_files"}, []string{root}, nil, cancel, 0)
	res := s.results["temp_files"]
	if res == nil {
		t.Fatal("no result stored")
	}
	if len(res.Files) != 0 {
		t.Fatalf("disallowed root was enumerated: %d files", len(res.Files))
	}
	found := false
	for _, e := range res.Errors {
		if strings.Contains(e, "no longer inside the allowed volumes") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing root-revalidation error, got %v", res.Errors)
	}
}

// ------------------------------------------------- 3.1 token middleware

func TestAuthMiddleware(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	s := &Server{authToken: "sekrit"}
	h := s.withAuth(ok)
	for _, tc := range []struct {
		token string
		want  int
	}{
		{"", 401},
		{"wrong", 401},
		{"sekrit", 200},
	} {
		r := httptest.NewRequest("GET", "/api/state", nil)
		if tc.token != "" {
			r.Header.Set(authTokenHeader, tc.token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("token %q: want %d, got %d", tc.token, tc.want, w.Code)
		}
	}
	// No token configured (dev/tests without -var) → passthrough.
	w := httptest.NewRecorder()
	(&Server{}).withAuth(ok).ServeHTTP(w, httptest.NewRequest("GET", "/api/state", nil))
	if w.Code != 200 {
		t.Fatalf("no-token passthrough: want 200, got %d", w.Code)
	}
}

func TestLoadOrCreateToken(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, authTokenFile)
	t1, err := loadOrCreateToken(p)
	if err != nil || len(t1) != 64 {
		t.Fatalf("want 32-byte hex token, got %q (%v)", t1, err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file must be 0600, got %v", fi.Mode().Perm())
	}
	if t2, _ := loadOrCreateToken(p); t2 != t1 {
		t.Fatal("token not stable across daemon restarts")
	}
	// Idempotent and tidy: exactly the token file, no temp residue (R2.4).
	if ents, _ := os.ReadDir(dir); len(ents) != 1 || ents[0].Name() != authTokenFile {
		t.Fatalf("unexpected files next to the token: %v", ents)
	}
}

// R2.4 — an existing token is never overwritten (O_EXCL-style create), and a
// corrupt empty token file fails closed instead of being silently rotated.
func TestLoadOrCreateTokenNeverOverwrites(t *testing.T) {
	p := filepath.Join(t.TempDir(), authTokenFile)
	if err := os.WriteFile(p, []byte("existing-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, err := loadOrCreateToken(p); err != nil || tok != "existing-token" {
		t.Fatalf("existing token must be reused: %q (%v)", tok, err)
	}
	p2 := filepath.Join(t.TempDir(), authTokenFile)
	if err := os.WriteFile(p2, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, err := loadOrCreateToken(p2); err == nil {
		t.Fatalf("empty token file must fail closed, got token %q", tok)
	}
	if b, _ := os.ReadFile(p2); len(b) != 0 {
		t.Fatal("corrupt token file was overwritten")
	}
}

// ------------------------------------------------ 3.3 export no-overwrite

// ---------------------------------------- preserve-mode batch folder naming

// The tool id in a /move request is client-controlled and, under preserve,
// decides a folder that gets CREATED at the destination. The entire defence
// is that the client's string is only ever a map KEY and never reaches a
// path: the names below are compile-time literals. This test pins the
// properties that makes safe, so a future name cannot quietly break it.
func TestMoveFolderNamesAreSafePathComponents(t *testing.T) {
	want := map[string]string{
		"duplicates": "Duplicates", "empty_folders": "Empty Folders",
		"empty_files": "Empty Files", "temp_files": "Temporary Files",
	}
	for id, name := range want {
		if got := moveFolderNames[id]; got != name {
			t.Errorf("%s: want folder %q, got %q", id, name, got)
		}
	}
	for id, name := range moveFolderNames {
		switch {
		case name == "":
			t.Errorf("%s: empty folder name", id)
		case strings.ContainsRune(name, '/'), strings.ContainsRune(name, filepath.Separator):
			t.Errorf("%s: folder name %q contains a separator", id, name)
		case name == "." || name == "..":
			t.Errorf("%s: folder name %q is a traversal component", id, name)
		case strings.HasPrefix(name, "."):
			t.Errorf("%s: folder name %q is hidden", id, name)
		case name != filepath.Clean(name):
			t.Errorf("%s: folder name %q is not already clean", id, name)
		// firstFreeName splits on the extension to build " (n)" variants, so
		// a dotted name would enumerate as "Empty (1).Files".
		case filepath.Ext(name) != "":
			t.Errorf("%s: folder name %q has an extension, which breaks enumeration", id, name)
		}
		for _, r := range name {
			if r < 0x20 || r == 0x7f {
				t.Errorf("%s: folder name %q contains a control character", id, name)
				break
			}
		}
	}
}

// A hostile or unknown tool id must miss the map entirely — the lookup is the
// only thing standing between a client string and a created path.
func TestMoveFolderNamesRejectHostileToolIDs(t *testing.T) {
	for _, id := range []string{
		"", "..", ".", "a/b", "../../etc", "Duplicates/../..", "/absolute",
		"duplicates ", "DUPLICATES", "dup\x00licates", strings.Repeat("x", 300),
	} {
		if name, ok := moveFolderNames[id]; ok {
			t.Errorf("tool id %q must not resolve, got %q", id, name)
		}
	}
}

// validTools is derived from moveFolderNames plus readOnlyTools, so every tool
// is exactly one of "can be moved from, and therefore has a destination folder
// name" or "is a report and must never have one". Assert the derivation rather
// than trusting it: a movable tool with no folder name would 400 every
// preserve move, and a read-only tool that acquired one would be silently
// movable.
func TestMoveFolderNamesCoverValidTools(t *testing.T) {
	for id := range validTools {
		_, movable := moveFolderNames[id]
		if movable == readOnlyTools[id] {
			t.Errorf("tool %q must be exactly one of movable or read-only (movable=%v, readOnly=%v)",
				id, movable, readOnlyTools[id])
		}
	}
	for id := range moveFolderNames {
		if !validTools[id] {
			t.Errorf("tool %q has a move folder name but cannot scan", id)
		}
	}
	for id := range readOnlyTools {
		if !validTools[id] {
			t.Errorf("read-only tool %q is not a valid tool", id)
		}
	}
	// A literal count, so adding or retiring a tool has to be a deliberate
	// edit here too: retiring one means its persisted results must be dropped
	// on load (loadState) or its rows stay in the move allowlist forever.
	if len(validTools) != 5 {
		t.Errorf("want 5 tools, got %d", len(validTools))
	}
}

// The read-only rule is the daemon's, not the UI's: hiding the Move button is
// presentation, and a raw API caller must be refused too.
func TestMoveRefusesReadOnlyTool(t *testing.T) {
	// Refused before any state is consulted, so a bare server is enough — and
	// that it needs no results is itself the point.
	s := &Server{}
	body, _ := json.Marshal(MoveReq{
		Files: []string{"/volume1/anywhere/file.jpg"}, Dest: "/volume1/dest", Tool: "corrupted_files",
	})
	rec := httptest.NewRecorder()
	s.handleMove(rec, httptest.NewRequest("POST", "/api/move", strings.NewReader(string(body))))
	if rec.Code != 400 {
		t.Fatalf("move from a read-only tool: want 400, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "report") {
		t.Errorf("refusal should say the tool is a report, got %s", rec.Body.String())
	}
}

// Scanning a read-only tool directly must be refused before a job starts. The
// switch in runScan has no arm for it, and without this the request would run
// nothing and store an empty result over the real one.
func TestScanRefusesReadOnlyTool(t *testing.T) {
	s := &Server{}
	body, _ := json.Marshal(ScanReq{Tool: "corrupted_files", Dirs: []string{"/volume1/x"}})
	rec := httptest.NewRecorder()
	s.handleScan(rec, httptest.NewRequest("POST", "/api/scan", strings.NewReader(string(body))))
	if rec.Code != 400 {
		t.Fatalf("scan of a read-only tool: want 400, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// ------------------------------------------------------- move progress

// decodeState runs handleState and returns the parsed body.
func decodeState(t *testing.T, s *Server) map[string]any {
	t.Helper()
	out, err := stateBody(s)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// stateBody is the error-returning form. Anything calling this off the test
// goroutine must use it rather than decodeState: t.Fatalf runs Goexit on
// whichever goroutine calls it, so a Fatalf in a spawned one kills that
// goroutine without recording the failure — and here that would silently
// turn "state body was not JSON" into the timeout below, i.e. a bogus
// "handleState blocked" diagnosis.
func stateBody(s *Server) (map[string]any, error) {
	w := httptest.NewRecorder()
	s.handleState(w, httptest.NewRequest("GET", "/api/state", nil))
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("state body not JSON: %v (%s)", err, w.Body.String())
	}
	return out, nil
}

// The premise of move progress is that /api/state can be answered WHILE a move
// is running — and a move holds moveMu for its entire run, which for a
// cross-volume batch is minutes. If the status path ever came to depend on
// moveMu, the poll would simply block until the move finished and the progress
// bar would jump from empty to done, with nothing in between and no error to
// show for it.
func TestMoveProgressReadableWhileMoveMuHeld(t *testing.T) {
	s := &Server{results: map[string]*toolResult{}}
	s.moveMu.Lock()
	defer s.moveMu.Unlock()
	s.setMoveProgress(true, 3, 10, "clip.mov")

	type stateRes struct {
		out map[string]any
		err error
	}
	done := make(chan stateRes, 1)
	go func() {
		out, err := stateBody(s)
		done <- stateRes{out, err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		mv, ok := got.out["move"].(map[string]any)
		if !ok {
			t.Fatalf("state carried no move progress: %v", got.out)
		}
		if mv["done"] != float64(3) || mv["total"] != float64(10) || mv["name"] != "clip.mov" {
			t.Fatalf("wrong progress reported: %v", mv)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handleState blocked while moveMu was held — progress polling would hang")
	}
}

// The absence of the key is what stops the UI's poller, so an idle daemon must
// not report one — and a finished move must not leave one behind.
func TestStateOmitsMoveProgressWhenIdle(t *testing.T) {
	s := &Server{results: map[string]*toolResult{}}
	if _, present := decodeState(t, s)["move"]; present {
		t.Fatal("idle daemon reported a move in progress")
	}
	s.setMoveProgress(true, 1, 4, "a.bin")
	if _, present := decodeState(t, s)["move"]; !present {
		t.Fatal("running move was not reported")
	}
	s.setMoveProgress(false, 0, 0, "") // what handleMove's defer does
	if _, present := decodeState(t, s)["move"]; present {
		t.Fatal("finished move still reported as running — the UI would poll forever")
	}
}

// ------------------------------------------- scan/move mutual exclusion

// Scans and moves must not overlap in EITHER direction. A move relocating files
// under a running scan makes the scan under-report: the moved path fails its
// pinned re-open and is dropped, and a duplicate group that falls below two
// members is discarded whole, taking its surviving member out of the results.
// The move-during-scan direction has been guarded since 0095; these cover the
// reverse, and the TOCTOU in the original guard.
func TestScanRefusedWhileMoveActive(t *testing.T) {
	s := &Server{results: map[string]*toolResult{}}
	if why := s.scanAdmissionLocked(); why != "" {
		t.Fatalf("idle daemon refused a scan: %q", why)
	}
	s.moveMu.Lock()
	if !s.beginMove() {
		s.moveMu.Unlock()
		t.Fatal("beginMove refused on an idle daemon")
	}
	why := s.scanAdmissionLocked()
	if why == "" {
		t.Fatal("a scan was admitted while a move was in flight — it would enumerate a tree the move is mutating")
	}
	if !strings.Contains(why, "move") {
		t.Fatalf("refusal does not tell the user a move is running: %q", why)
	}
	s.endMove()
	s.moveMu.Unlock()
	if why := s.scanAdmissionLocked(); why != "" {
		t.Fatalf("scan still refused after the move finished: %q", why)
	}
}

// The bug the post-lock check exists for: handleMove's cheap pre-lock check
// passes, then the request spends a File Station round trip (and, for a queued
// second move, however long the first one runs) before taking moveMu. A scan
// starting in that window used to go unnoticed because nothing re-read the flag.
func TestBeginMoveRefusesAScanThatStartedAfterThePrecheck(t *testing.T) {
	s := &Server{results: map[string]*toolResult{}}

	s.mu.Lock() // handleMove's pre-lock check, on an idle daemon
	precheckSawScan := s.job.Running
	s.mu.Unlock()
	if precheckSawScan {
		t.Fatal("precondition: no scan should be running yet")
	}

	// …the destination round trips happen here, and a scan starts meanwhile
	s.mu.Lock()
	s.job = jobState{Running: true, Tool: "duplicates"}
	s.mu.Unlock()

	s.moveMu.Lock()
	defer s.moveMu.Unlock()
	if s.beginMove() {
		t.Fatal("move proceeded against results a scan was already replacing")
	}
	s.mu.Lock()
	claimed := s.moveActive
	s.mu.Unlock()
	if claimed {
		t.Fatal("a refused move left the claim set — every later scan would be refused forever")
	}
}

// handleScan must never wait on moveMu. Taking mu and then blocking on moveMu
// inverts the moveMu -> mu order used by beginMove, setMoveProgress, the move
// snapshot, pruneMoved and saveState, and would deadlock; it would also hang
// /api/scan for the whole duration of a large batch. This fails on a timeout if
// the admission check ever grows a moveMu dependency.
func TestScanAdmissionNeverWaitsOnMoveMu(t *testing.T) {
	s := &Server{results: map[string]*toolResult{}}
	s.moveMu.Lock()
	defer s.moveMu.Unlock()
	if !s.beginMove() {
		t.Fatal("beginMove refused on an idle daemon")
	}
	defer s.endMove()

	done := make(chan string, 1)
	go func() {
		s.mu.Lock()
		why := s.scanAdmissionLocked()
		s.mu.Unlock()
		done <- why
	}()
	select {
	case why := <-done:
		if why == "" {
			t.Fatal("scan admitted while a move was claimed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scan admission blocked while moveMu was held — /api/scan would hang for the length of a move")
	}
}
