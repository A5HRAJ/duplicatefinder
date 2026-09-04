package main

// Tests for the paged results delivery (scale phase 1): display-time match
// refinement, group-level search, flat sorting, group-boundary paging, the
// stored-results caps, and the aggregate totals the UI's summary and badges
// rely on.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"testing"
)

func mkGroup(id string, size int64, files ...FileEnt) Group {
	for i := range files {
		if files[i].ID == "" {
			files[i].ID = id + "f" + strconv.Itoa(i)
		}
		files[i].Size = size
	}
	return Group{ID: id, Ext: "BIN", Size: size, Hash: "h" + id, Files: files}
}

func TestRefineGroupsByName(t *testing.T) {
	g := mkGroup("g0", 10,
		FileEnt{Name: "a.bin", Dir: "/v/x", Mod: "2026-01-01 00:00:00"},
		FileEnt{Name: "a.bin", Dir: "/v/y", Mod: "2026-01-02 00:00:00"},
		FileEnt{Name: "b.bin", Dir: "/v/z", Mod: "2026-01-03 00:00:00"},
	)
	out := refineGroups([]Group{g}, MatchOpts{Name: true})
	if len(out) != 1 || len(out[0].Files) != 2 {
		t.Fatalf("name refinement should keep only the a.bin pair: %+v", out)
	}
	for _, f := range out[0].Files {
		if f.Name != "a.bin" {
			t.Fatalf("wrong file kept: %+v", f)
		}
	}
	// Refining by modified splits everything below pair size — no groups.
	if out := refineGroups([]Group{g}, MatchOpts{Modified: true}); len(out) != 0 {
		t.Fatalf("modified refinement should dissolve the group: %+v", out)
	}
}

// A file with no Created value never groups under created-date refinement —
// the scan's own rule (an unknown date is not a confirmed match) applies to
// display-time refinement too.
func TestRefineGroupsCreatedUnknownNeverGroups(t *testing.T) {
	g := mkGroup("g0", 10,
		FileEnt{Name: "a", Dir: "/v", Created: ""},
		FileEnt{Name: "b", Dir: "/v", Created: ""},
		FileEnt{Name: "c", Dir: "/v", Created: "2026-01-01 00:00:00"},
		FileEnt{Name: "d", Dir: "/v", Created: "2026-01-01 00:00:00"},
	)
	out := refineGroups([]Group{g}, MatchOpts{Created: true})
	if len(out) != 1 || len(out[0].Files) != 2 {
		t.Fatalf("only the known-equal pair may group: %+v", out)
	}
	for _, f := range out[0].Files {
		if f.Created == "" {
			t.Fatalf("blank created date grouped: %+v", f)
		}
	}
}

// Refinement and the no-refinement copy path must never alias the cached
// slices — pruneMoved rewrites those in place.
func TestRefineGroupsNeverAliases(t *testing.T) {
	g := mkGroup("g0", 10,
		FileEnt{Name: "a", Dir: "/v"},
		FileEnt{Name: "b", Dir: "/v"},
	)
	cached := []Group{g}
	out := refineGroups(cached, MatchOpts{})
	out[0].Files[0].Name = "mutated"
	if cached[0].Files[0].Name == "mutated" {
		t.Fatal("refineGroups aliased the cached Files slice")
	}
}

func TestFilterGroupsKeepsWholeGroup(t *testing.T) {
	groups := []Group{
		mkGroup("g0", 10, FileEnt{Name: "IMG_0001.JPG", Dir: "/v/a"}, FileEnt{Name: "other.jpg", Dir: "/v/b"}),
		mkGroup("g1", 10, FileEnt{Name: "x", Dir: "/v/c"}, FileEnt{Name: "y", Dir: "/v/c"}),
	}
	out := filterGroups(groups, compileSearch(&resultsQuery{Query: "img_0001"}))
	if len(out) != 1 || len(out[0].Files) != 2 {
		t.Fatalf("a matching group must be kept whole (peers included): %+v", out)
	}
}

func TestScanDateRanges(t *testing.T) {
	res := &toolResult{Tool: "duplicates", Groups: []Group{
		mkGroup("g0", 10,
			FileEnt{Name: "a", Dir: "/v", Mod: "2019-05-26 04:40:00",
				Created: "2023-06-22 01:08:15", Captured: "2019-05-25 19:17:00"},
			FileEnt{Name: "b", Dir: "/v", Mod: "2024-01-02 00:00:00",
				Created: "", Captured: ""}), // unknown dates must not bound anything
		mkGroup("g1", 10,
			FileEnt{Name: "c", Dir: "/v", Mod: "2015-03-01 12:00:00",
				Created: "2026-07-31 23:59:59", Captured: "2030-01-01 00:00:00"},
			FileEnt{Name: "d", Dir: "/v", Mod: "2020-01-01 00:00:00"}),
	}}
	r := scanDateRanges(res)
	if r.Modified.Min != "2015-03-01 12:00:00" || r.Modified.Max != "2024-01-02 00:00:00" {
		t.Errorf("modified span: %+v", r.Modified)
	}
	// a future Created must be honoured — a file may legitimately carry one,
	// so the range is the data's, never "today"
	if r.Created.Min != "2023-06-22 01:08:15" || r.Created.Max != "2026-07-31 23:59:59" {
		t.Errorf("created span: %+v", r.Created)
	}
	if r.Captured.Min != "2019-05-25 19:17:00" || r.Captured.Max != "2030-01-01 00:00:00" {
		t.Errorf("captured span: %+v", r.Captured)
	}
	// a field no file carries stays empty, so the UI can fall back
	empty := scanDateRanges(&toolResult{Tool: "temp_files", Files: []FileEnt{{Name: "x", Dir: "/v"}}})
	if empty.Modified.Min != "" || empty.Captured.Max != "" {
		t.Errorf("absent fields must produce an empty span: %+v", empty)
	}
	// flat tools span their Files slice
	flat := scanDateRanges(&toolResult{Tool: "temp_files", Files: []FileEnt{
		{Name: "x", Dir: "/v", Mod: "2021-01-01 00:00:00"},
		{Name: "y", Dir: "/v", Mod: "2022-01-01 00:00:00"},
	}})
	if flat.Modified.Min != "2021-01-01 00:00:00" || flat.Modified.Max != "2022-01-01 00:00:00" {
		t.Errorf("flat span: %+v", flat.Modified)
	}
}

func TestDateRangeCacheInvalidatedWithResults(t *testing.T) {
	s := &Server{results: map[string]*toolResult{}}
	res := &toolResult{Tool: "temp_files", Files: []FileEnt{{Name: "a", Dir: "/v", Mod: "2020-01-01 00:00:00"}}}
	s.results["temp_files"] = res
	if got := s.dateRangeLocked("temp_files", res).Modified.Max; got != "2020-01-01 00:00:00" {
		t.Fatalf("first computation: %q", got)
	}
	// mutate the stored result the way pruneMoved does, then invalidate
	res.Files = append(res.Files, FileEnt{Name: "b", Dir: "/v", Mod: "2031-09-09 00:00:00"})
	if got := s.dateRangeLocked("temp_files", res).Modified.Max; got != "2020-01-01 00:00:00" {
		t.Fatalf("cache should still be serving the old span until invalidated, got %q", got)
	}
	s.invalidateDerivedLocked()
	if s.view != nil || s.dateRange != nil {
		t.Fatal("invalidateDerivedLocked must clear both derived caches")
	}
	if got := s.dateRangeLocked("temp_files", res).Modified.Max; got != "2031-09-09 00:00:00" {
		t.Fatalf("after invalidation: %q", got)
	}
}

func TestCompileSearchNilWhenEmpty(t *testing.T) {
	if compileSearch(&resultsQuery{}) != nil {
		t.Fatal("no criteria must compile to a nil predicate (grand==total path)")
	}
	// extension mode without an extension filters nothing
	if compileSearch(&resultsQuery{FType: "ext"}) != nil {
		t.Fatal("ftype=ext with empty extq must be a nil predicate")
	}
	// a date range without dates filters nothing
	if compileSearch(&resultsQuery{DateBy: "modified"}) != nil {
		t.Fatal("dateBy without from/to must be a nil predicate")
	}
}

func TestSearchPredOptions(t *testing.T) {
	f := FileEnt{
		Name: "IMG_0001.JPG", Dir: "/volume1/Backups/Ashwin", Size: 5 << 20,
		Mod: "2023-06-22 01:08:15", Created: "2019-05-26 04:40:00",
		Captured: "2019-05-25 19:17:00", Ext: "JPG",
	}
	cases := []struct {
		name string
		q    resultsQuery
		want bool
	}{
		{"loc subtree hit", resultsQuery{Loc: "/volume1/Backups"}, true},
		{"loc exact dir hit", resultsQuery{Loc: "/volume1/Backups/Ashwin"}, true},
		{"loc prefix is path-segment safe", resultsQuery{Loc: "/volume1/Back"}, false},
		{"loc miss", resultsQuery{Loc: "/volume1/Test"}, false},
		{"category hit", resultsQuery{FType: "pic"}, true},
		{"category miss", resultsQuery{FType: "video"}, false},
		{"extension hit is case-insensitive", resultsQuery{FType: "ext", ExtQ: ".jpg"}, true},
		{"extension miss", resultsQuery{FType: "ext", ExtQ: "mov"}, false},
		{"file vs dir", resultsQuery{FType: "dir"}, false},
		{"modified in range", resultsQuery{DateBy: "modified", DateFrom: "2023-06-22", DateTo: "2023-06-22"}, true},
		{"modified out of range", resultsQuery{DateBy: "modified", DateFrom: "2024-01-01"}, false},
		{"created in range", resultsQuery{DateBy: "created", DateTo: "2019-05-26"}, true},
		{"captured from only", resultsQuery{DateBy: "captured", DateFrom: "2019-05-25"}, true},
		{"size eq MB bucket", resultsQuery{SizeOp: "eq", SizeMB: 5}, true},
		{"size gt miss", resultsQuery{SizeOp: "gt", SizeMB: 5}, false},
		{"size lt hit", resultsQuery{SizeOp: "lt", SizeMB: 6}, true},
		{"conjunction: loc AND wrong type", resultsQuery{Loc: "/volume1/Backups", FType: "video"}, false},
		{"keyword AND option", resultsQuery{Query: "img_0001", FType: "pic"}, true},
	}
	for _, c := range cases {
		p := compileSearch(&c.q)
		if p == nil {
			t.Fatalf("%s: predicate unexpectedly nil", c.name)
		}
		if got := p.match(&f); got != c.want {
			t.Errorf("%s: match=%v want %v", c.name, got, c.want)
		}
	}
	// an absent date never matches an active date filter (the scan's rule)
	blank := f
	blank.Captured = ""
	p := compileSearch(&resultsQuery{DateBy: "captured", DateFrom: "2000-01-01"})
	if p.match(&blank) {
		t.Error("blank captured date must not match an active captured filter")
	}
}

func TestBuildViewSearchOptsTotalsAndGrand(t *testing.T) {
	res := &toolResult{Tool: "duplicates", Groups: []Group{
		mkGroup("g0", 10,
			FileEnt{Name: "a.jpg", Dir: "/v/pics", Ext: "JPG", Size: 10},
			FileEnt{Name: "b.jpg", Dir: "/v/other", Ext: "JPG", Size: 10}),
		mkGroup("g1", 10,
			FileEnt{Name: "c.mov", Dir: "/v/vids", Ext: "MOV", Size: 10},
			FileEnt{Name: "d.mov", Dir: "/v/vids", Ext: "MOV", Size: 10}),
	}}
	q := &resultsQuery{Tool: "duplicates", FType: "pic"}
	v := buildView(res, q, q.viewKey(), newRefMatcher(nil, nil))
	if v.total.Groups != 1 || v.grand.Groups != 2 {
		t.Fatalf("ftype filter: total=%+v grand=%+v", v.total, v.grand)
	}
	if len(v.groups) != 1 || len(v.groups[0].Files) != 2 {
		t.Fatalf("matching group must be whole: %+v", v.groups)
	}
	// distinct option sets must produce distinct view keys (cache slot)
	q2 := &resultsQuery{Tool: "duplicates", FType: "video"}
	if q.viewKey() == q2.viewKey() {
		t.Fatal("viewKey must include search options")
	}
}

func TestDupTotalsReclaimable(t *testing.T) {
	groups := []Group{
		// 3 copies of 10 bytes, none protected: 2 reclaimable
		mkGroup("g0", 10, FileEnt{Name: "a", Dir: "/v/x"}, FileEnt{Name: "a", Dir: "/v/y"}, FileEnt{Name: "a", Dir: "/v/z"}),
		// 2 copies, one protected under /v/ref: 1 reclaimable
		mkGroup("g1", 7, FileEnt{Name: "b", Dir: "/v/ref/sub"}, FileEnt{Name: "b", Dir: "/v/q"}),
	}
	tot := dupTotals(groups, newRefMatcher([]string{"/v/ref"}, nil))
	if tot.Groups != 2 || tot.Files != 5 {
		t.Fatalf("bad counts: %+v", tot)
	}
	if want := int64(2*10 + 1*7); tot.Reclaimable != want {
		t.Fatalf("reclaimable = %d, want %d", tot.Reclaimable, want)
	}
	// Both copies protected: the whole group is kept, nothing reclaimable.
	tot = dupTotals(groups[1:], newRefMatcher([]string{"/v/ref", "/v/q"}, nil))
	if tot.Reclaimable != 0 {
		t.Fatalf("fully protected group must reclaim nothing: %+v", tot)
	}
}

func TestSortFilesFieldsAndTies(t *testing.T) {
	files := []FileEnt{
		{Name: "b", Dir: "/v/2", Size: 5, Mod: "2026-01-02 00:00:00"},
		{Name: "a", Dir: "/v/1", Size: 9, Mod: "2026-01-01 00:00:00"},
		{Name: "c", Dir: "/v/0", Size: 5, Mod: "2026-01-03 00:00:00"},
	}
	sortFiles(files, "size", "DESC")
	if files[0].Name != "a" || files[1].Name != "c" || files[2].Name != "b" {
		// size 9 first; the two 5s tie-break by full path ascending
		// (/v/0/c before /v/2/b)
		t.Fatalf("size DESC wrong: %+v", files)
	}
	sortFiles(files, "name", "ASC")
	if files[0].Name != "a" || files[2].Name != "c" {
		t.Fatalf("name ASC wrong: %+v", files)
	}
	before := append([]FileEnt{}, files...)
	sortFiles(files, "nonsense", "ASC")
	for i := range files {
		if files[i] != before[i] {
			t.Fatal("unknown sort field must keep the stored order")
		}
	}
}

func TestCapDuplicateGroups(t *testing.T) {
	groups := []Group{
		mkGroup("g0", 10, FileEnt{Name: "a", Dir: "/v"}, FileEnt{Name: "b", Dir: "/v"}),
		mkGroup("g1", 9, FileEnt{Name: "c", Dir: "/v"}, FileEnt{Name: "d", Dir: "/v"}, FileEnt{Name: "e", Dir: "/v"}),
		mkGroup("g2", 8, FileEnt{Name: "f", Dir: "/v"}, FileEnt{Name: "g", Dir: "/v"}),
	}
	// Budget 5 fits g0 (2) + g1 (3); g2 is cut on its group boundary.
	kept, trunc := capDuplicateGroups(groups, 5)
	if len(kept) != 2 || trunc == nil || trunc.Groups != 1 || trunc.Files != 2 || trunc.Cap != 5 {
		t.Fatalf("cap mid-list wrong: kept=%d trunc=%+v", len(kept), trunc)
	}
	// Budget 4: g1 would overflow, cut before it.
	kept, trunc = capDuplicateGroups(groups, 4)
	if len(kept) != 1 || trunc.Groups != 2 || trunc.Files != 5 {
		t.Fatalf("boundary cut wrong: kept=%d trunc=%+v", len(kept), trunc)
	}
	// A first group larger than the whole budget is still kept.
	kept, trunc = capDuplicateGroups(groups, 1)
	if len(kept) != 1 || trunc.Groups != 2 {
		t.Fatalf("giant first group must survive: kept=%d trunc=%+v", len(kept), trunc)
	}
	// Everything fits: no truncation report.
	if _, trunc = capDuplicateGroups(groups, 7); trunc != nil {
		t.Fatalf("no truncation expected: %+v", trunc)
	}
}

// boundedTop must reproduce sort-then-cap exactly: same kept set, same
// order, same truncation report — it replaced that code path (phase 3).
func TestBoundedTopMatchesSortAndCap(t *testing.T) {
	bigOrder := func(a, b *fEnt) bool {
		if a.size != b.size {
			return a.size > b.size
		}
		return a.path < b.path
	}
	var all []fEnt
	for i := 0; i < 137; i++ {
		all = append(all, fEnt{
			path: fmt.Sprintf("/v/p%03d", (i*67)%137),
			size: int64((i * 31) % 11), // plenty of size ties
		})
	}
	ref := append([]fEnt{}, all...)
	sort.Slice(ref, func(i, j int) bool { return bigOrder(&ref[i], &ref[j]) })

	top := newBoundedTop(25, bigOrder)
	for _, f := range all {
		top.add(f)
	}
	got, trunc := top.final()
	if len(got) != 25 || trunc == nil || trunc.Files != 137-25 || trunc.Cap != 25 {
		t.Fatalf("kept=%d trunc=%+v", len(got), trunc)
	}
	for i := range got {
		if got[i].path != ref[i].path || got[i].size != ref[i].size {
			t.Fatalf("order diverges at %d: got %+v want %+v", i, got[i], ref[i])
		}
	}
	// Under the limit: everything kept, no truncation report.
	small := newBoundedTop(200, bigOrder)
	for _, f := range all {
		small.add(f)
	}
	got, trunc = small.final()
	if len(got) != 137 || trunc != nil {
		t.Fatalf("under-limit wrong: kept=%d trunc=%+v", len(got), trunc)
	}
}

// ------------------------------------------------- handler-level round trips

func pagedReq(t *testing.T, s *Server, q resultsQuery) map[string]any {
	t.Helper()
	body, _ := json.Marshal(q)
	req := httptest.NewRequest(http.MethodPost, "/api/results", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.handleResults(w, req)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func testServerWithDuplicates(n int) *Server {
	groups := make([]Group, 0, n)
	for i := 0; i < n; i++ {
		id := "g" + strconv.Itoa(i)
		groups = append(groups, mkGroup(id, int64(1000-i),
			FileEnt{Name: "f" + strconv.Itoa(i) + ".bin", Dir: "/v/a"},
			FileEnt{Name: "f" + strconv.Itoa(i) + ".bin", Dir: "/v/b"},
		))
	}
	return &Server{results: map[string]*toolResult{
		"duplicates": {Tool: "duplicates", Groups: groups, Match: &MatchOpts{}, Scanned: "now"},
	}}
}

func TestPagedResultsGroupPaging(t *testing.T) {
	s := testServerWithDuplicates(7)
	out := pagedReq(t, s, resultsQuery{Tool: "duplicates", Offset: 0, Limit: 3})
	if n := len(out["groups"].([]any)); n != 3 {
		t.Fatalf("page 1 groups = %d, want 3", n)
	}
	tot := out["total"].(map[string]any)
	if tot["groups"].(float64) != 7 || tot["files"].(float64) != 14 {
		t.Fatalf("totals wrong: %+v", tot)
	}
	// Last page is short; beyond-the-end is empty, not an error.
	out = pagedReq(t, s, resultsQuery{Tool: "duplicates", Offset: 6, Limit: 3})
	if n := len(out["groups"].([]any)); n != 1 {
		t.Fatalf("last page groups = %d, want 1", n)
	}
	out = pagedReq(t, s, resultsQuery{Tool: "duplicates", Offset: 99, Limit: 3})
	if n := len(out["groups"].([]any)); n != 0 {
		t.Fatalf("past-the-end page groups = %d, want 0", n)
	}
}

func TestPagedResultsSearchTotalsVsGrand(t *testing.T) {
	s := testServerWithDuplicates(5)
	out := pagedReq(t, s, resultsQuery{Tool: "duplicates", Query: "f3.bin", Limit: 10})
	if n := len(out["groups"].([]any)); n != 1 {
		t.Fatalf("filtered groups = %d, want 1", n)
	}
	tot := out["total"].(map[string]any)
	grand := out["grand"].(map[string]any)
	if tot["files"].(float64) != 2 || grand["files"].(float64) != 10 {
		t.Fatalf("total should be filtered (2), grand unfiltered (10): %+v / %+v", tot, grand)
	}
}

func TestPagedResultsFlatSort(t *testing.T) {
	files := []FileEnt{
		{ID: "1", Name: "big.tmp", Dir: "/v/a", Size: 5000},
		{ID: "2", Name: "mid.tmp", Dir: "/v/b", Size: 3000},
		{ID: "3", Name: "small.tmp", Dir: "/v/c", Size: 100},
	}
	s := &Server{results: map[string]*toolResult{
		"temp_files": {Tool: "temp_files", Files: files, Scanned: "now"},
	}}
	out := pagedReq(t, s, resultsQuery{Tool: "temp_files", Sort: "name", Dir: "ASC", Limit: 10})
	page := out["files"].([]any)
	if len(page) != 3 {
		t.Fatalf("page carried %d rows, want 3", len(page))
	}
	if page[0].(map[string]any)["name"] != "big.tmp" {
		t.Fatalf("name ASC wrong: %+v", page)
	}
	tot := out["total"].(map[string]any)
	if tot["files"].(float64) != 3 || tot["bytes"].(float64) != 8100 {
		t.Fatalf("totals wrong: %+v", tot)
	}
	// The stored result must not have been reordered by the sorted view.
	if s.results["temp_files"].Files[0].Name != "big.tmp" || s.results["temp_files"].Files[2].Name != "small.tmp" {
		t.Fatalf("stored result reordered: %+v", s.results["temp_files"].Files)
	}
}

func TestPagedResultsViewCacheInvalidation(t *testing.T) {
	s := testServerWithDuplicates(3)
	pagedReq(t, s, resultsQuery{Tool: "duplicates", Limit: 10})
	if s.view == nil {
		t.Fatal("view not cached")
	}
	// Different parameters replace the cached slot.
	pagedReq(t, s, resultsQuery{Tool: "duplicates", Query: "f1", Limit: 10})
	if s.view == nil || s.view.total.Files != 2 {
		t.Fatalf("view not rebuilt for new params: %+v", s.view)
	}
	// pruneMoved must drop the cached view (rows may have moved).
	s.pruneMoved([]string{"/v/a/f1.bin"}, nil, nil)
	if s.view != nil {
		t.Fatal("pruneMoved left a stale view cached")
	}
	out := pagedReq(t, s, resultsQuery{Tool: "duplicates", Query: "f1", Limit: 10})
	if out["total"].(map[string]any)["files"].(float64) != 0 {
		// group g1 dissolved below two members and was pruned
		t.Fatalf("stale rows served after prune: %+v", out["total"])
	}
}

func TestPagedResultsUnscannedAndUnknownTool(t *testing.T) {
	s := &Server{results: map[string]*toolResult{}}
	out := pagedReq(t, s, resultsQuery{Tool: "duplicates"})
	if out["scanned"] != false {
		t.Fatalf("unscanned tool must answer scanned:false: %+v", out)
	}
	body, _ := json.Marshal(resultsQuery{Tool: "nonsense"})
	req := httptest.NewRequest(http.MethodPost, "/api/results", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.handleResults(w, req)
	if w.Code != 400 {
		t.Fatalf("unknown tool must 400, got %d", w.Code)
	}
}

// The legacy GET dump keeps its full shape (raw localhost callers), now with
// the truncation report when a cap applied.
func TestLegacyGetDumpUnchanged(t *testing.T) {
	s := testServerWithDuplicates(3)
	s.results["duplicates"].Truncated = &TruncInfo{Groups: 2, Files: 9, Cap: 100}
	req := httptest.NewRequest(http.MethodGet, "/api/results?tool=duplicates", nil)
	w := httptest.NewRecorder()
	s.handleResults(w, req)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if n := len(out["groups"].([]any)); n != 3 {
		t.Fatalf("GET dump must return every group, got %d", n)
	}
	tr := out["truncated"].(map[string]any)
	if tr["files"].(float64) != 9 {
		t.Fatalf("GET dump missing truncation report: %+v", out)
	}
}

// A duplicates page counts GROUPS, and a group is never split across pages —
// so without a row budget one enormous group is one enormous page. On the
// DS916+ a 25,600-file group came back as 6.7 MB of JSON and locked the
// browser for half a minute. Every requested group must still appear (the
// pager's arithmetic is in groups), but their rows are capped and any group
// that was shortened reports its true size.
func TestTrimGroupRowsBoundsAPage(t *testing.T) {
	mkBig := func(id string, n int) Group {
		g := Group{ID: id, Size: 10, Hash: id}
		for i := 0; i < n; i++ {
			g.Files = append(g.Files, FileEnt{Name: fmt.Sprintf("f%05d", i), Dir: "/v"})
		}
		return g
	}
	// One huge group on its own.
	got := trimGroupRows([]Group{mkBig("g0", 25600)})
	if len(got) != 1 {
		t.Fatalf("the group must still be on the page, got %d", len(got))
	}
	if len(got[0].Files) != maxGroupRows {
		t.Fatalf("rows should be capped at %d, got %d", maxGroupRows, len(got[0].Files))
	}
	if got[0].Count != 25600 {
		t.Fatalf("a trimmed group must report its true size, got %d", got[0].Count)
	}

	// A full page of huge groups: every group present, page rows bounded, and
	// none starved below the minimum.
	var page []Group
	for i := 0; i < defGroupLimit; i++ {
		page = append(page, mkBig(fmt.Sprintf("g%d", i), 5000))
	}
	got = trimGroupRows(page)
	if len(got) != defGroupLimit {
		t.Fatalf("every requested group must survive, got %d", len(got))
	}
	rows := 0
	for _, g := range got {
		if len(g.Files) < minGroupRows {
			t.Fatalf("group %s starved to %d rows", g.ID, len(g.Files))
		}
		if g.Count != 5000 {
			t.Fatalf("group %s should report its true size, got %d", g.ID, g.Count)
		}
		rows += len(g.Files)
	}
	if max := maxPageRows + defGroupLimit*minGroupRows; rows > max {
		t.Fatalf("page carries %d rows, past the %d budget", rows, max)
	}

	// Ordinary results are untouched, and carry no count.
	small := []Group{mkBig("a", 2), mkBig("b", 3)}
	got = trimGroupRows(small)
	if len(got[0].Files) != 2 || len(got[1].Files) != 3 || got[0].Count != 0 || got[1].Count != 0 {
		t.Fatalf("small groups must pass through untouched: %+v", got)
	}
}

// Trimming must not reach back into the cached result: the view is shared by
// every later request, and a move rewrites those slices in place.
func TestTrimGroupRowsLeavesTheViewIntact(t *testing.T) {
	g := Group{ID: "g0", Size: 10, Hash: "h"}
	for i := 0; i < maxGroupRows+50; i++ {
		g.Files = append(g.Files, FileEnt{Name: fmt.Sprintf("f%03d", i), Dir: "/v"})
	}
	view := []Group{g}
	page := trimGroupRows(slicePage(view, 0, 100))
	if len(page[0].Files) != maxGroupRows {
		t.Fatalf("page not trimmed: %d", len(page[0].Files))
	}
	if len(view[0].Files) != maxGroupRows+50 {
		t.Fatalf("the cached view was trimmed too: %d", len(view[0].Files))
	}
}
