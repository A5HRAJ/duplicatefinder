package main

// Paged results delivery. The daemon can cache result sets far larger than a
// browser — or an ExtJS 3.4 grid — can hold (a large NAS may carry millions
// of duplicates), so the UI reads results through POST /api/results in pages
// and reveals them progressively. Search, match refinement and column sorting
// are applied here, server-side: the client only ever holds a window into the
// result set, so any client-side filter or sort would silently act on a
// fraction of the data. The GET dump remains for raw localhost API callers.

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Page-size clamps. The UI always sends an explicit limit — 100 groups or
// 1000 rows per page (PAGE_GROUPS / PAGE_FILES in the UI) — so the def*
// values below apply only to a raw caller that sends none. The max* clamps
// bound what one response may carry (a "refresh in place" after a move
// re-requests everything already on screen in one call).
const (
	maxGroupLimit = 5000
	maxFileLimit  = 20000
	defGroupLimit = 100
	defFileLimit  = 500
)

// Row budget for a duplicates page. Paging counts GROUPS, and a group is
// never split across pages, so without this a single huge group is a single
// huge page: 25,600 identical files measured 6.7 MB of JSON and locked the
// browser up for half a minute rendering it, and the stored-results cap
// allows four times that. So a page also trims: every group still appears
// (the pager's arithmetic is in groups and must stay exact), but the files
// inside them are capped, and each trimmed group reports its true size.
//
// Trimming is display-only and safe: the totals are computed from the whole
// view before paging, and keep-one is enforced by the daemon against the
// stored group, never by what the grid happens to be showing.
const (
	maxPageRows  = 2000 // whole page
	maxGroupRows = 500  // any one group
	minGroupRows = 2    // ... but never so few that a group stops looking like one
)

// refMatcher decides whether a result row is a read-only reference copy. It
// compares CANONICAL paths — the reference folders symlink-resolved through
// the same dirResolver the move uses, and each row mapped into canonical
// space through the result's own root table (a row's canonical path is its
// root's canonical path plus the same relative tail, because the walk never
// follows symlinks below a root) — so it needs no syscall per row, and the
// padlock the grid draws can never disagree with the refusal the move
// issues about a folder given through an alias. The raw forms are matched
// too, for results an older build persisted without a root table.
type refMatcher struct {
	raw   []string
	canon []string
	roots []RootMap
}

func newRefMatcher(refs []string, roots []RootMap) *refMatcher {
	m := &refMatcher{roots: roots}
	if len(refs) == 0 {
		return m
	}
	m.raw = uniquePaths(refs)
	r := newDirResolver()
	for _, d := range m.raw {
		m.canon = append(m.canon, r.dir(d))
	}
	return m
}

func (m *refMatcher) empty() bool { return m == nil || len(m.raw) == 0 }

// protects reports whether the row at dir/name lies under a reference folder.
func (m *refMatcher) protects(dir, name string) bool {
	if m.empty() {
		return false
	}
	p := filepath.Join(dir, name)
	if isUnder(p, m.raw) || isUnder(p, m.canon) {
		return true
	}
	for _, rt := range m.roots {
		if p == rt.Raw || strings.HasPrefix(p, rt.Raw+"/") {
			return isUnder(rt.Canon+p[len(rt.Raw):], m.canon)
		}
	}
	return false
}

// markGroupProt records each group's protected-reference count over its WHOLE
// membership, and flags each protected row, so the client draws the padlock
// from the daemon's decision instead of re-deriving it. It must run BEFORE
// trimGroupRows: the client cannot recount protection from a shortened page,
// and pairing a partial protected count with the true member count overstates
// the group's reclaimable space — the header then advertises bytes the move
// will refuse to free, and disagrees with the toolbar summary, which
// dupTotals computes over the whole group.
//
// Safe to mutate: the caller passes slicePage's freshly copied Group headers,
// never the cached view's — and the rows are copied here before flagging.
func markGroupProt(groups []Group, m *refMatcher) {
	if m.empty() {
		return // nothing protected
	}
	for i := range groups {
		files := append([]FileEnt(nil), groups[i].Files...)
		n := 0
		for j := range files {
			if m.protects(files[j].Dir, files[j].Name) {
				files[j].Prot = true
				n++
			}
		}
		groups[i].Files = files
		groups[i].Prot = n
	}
}

// trimGroupRows caps the rows a page carries, marking every group it shortens
// with its true member count.
func trimGroupRows(groups []Group) []Group {
	budget := maxPageRows
	for i := range groups {
		full := len(groups[i].Files)
		room := budget
		if room > maxGroupRows {
			room = maxGroupRows
		}
		if room < minGroupRows {
			room = minGroupRows
		}
		if full > room {
			groups[i].Files = groups[i].Files[:room:room]
			groups[i].Count = full
		}
		budget -= len(groups[i].Files)
		if budget < 0 {
			budget = 0
		}
	}
	return groups
}

// resultsQuery is the body of POST /api/results. Offset and limit count
// duplicate GROUPS for the duplicates tool — groups are never split across
// pages, because the UI's keep-one-per-group selection logic needs every
// loaded group whole — and FILES (rows) for the flat tools.
type resultsQuery struct {
	Tool   string    `json:"tool"`
	Offset int       `json:"offset"`
	Limit  int       `json:"limit"`
	Query  string    `json:"q"`     // case-insensitive substring of the full path
	Match  MatchOpts `json:"match"` // duplicates: display-time refinement
	// RefDirs is accepted for compatibility and IGNORED: reference protection
	// is decided from the folders the stored results were scanned with,
	// which the daemon holds — a client-supplied list would let the padlocks
	// and the move's refusals describe two different sets.
	Sort    string   `json:"sort"`    // flat tools: column to order by
	Dir     string   `json:"dir"`     // ASC | DESC
	RefDirs []string `json:"refDirs"` // reference dirs, for the reclaimable figure

	// Search options (the magnifier menu, mirroring File Station's advanced
	// search). Like q, they narrow `total` but never `grand`: rail badges keep
	// describing the scan. All are ANDed with q; for duplicates a group stays
	// when ANY member passes the whole conjunction, and stays WHOLE (the
	// keep-one selection rules need loaded groups complete, same as q).
	Loc      string `json:"loc"`      // folder subtree ("" = everywhere)
	FType    string `json:"ftype"`    // "", dir, file, docs, video, pic, audio, web, exe, disk, zip, ext
	ExtQ     string `json:"extq"`     // extension to match when FType == "ext"
	DateBy   string `json:"dateBy"`   // "", modified, created, captured
	DateFrom string `json:"dateFrom"` // YYYY-MM-DD, inclusive
	DateTo   string `json:"dateTo"`   // YYYY-MM-DD, inclusive
	SizeOp   string `json:"sizeOp"`   // "", eq, gt, lt
	SizeMB   int64  `json:"sizeMB"`
}

// typeExts maps the File Station file-type categories the magnifier menu
// offers onto extension sets, in FileEnt.Ext's normalized UPPERCASE form.
var typeExts = map[string]map[string]bool{
	"docs":  extSet("DOC DOCX XLS XLSX PPT PPTX PDF TXT RTF ODT ODS ODP CSV MD PAGES NUMBERS KEY"),
	"video": extSet("MP4 MOV M4V AVI MKV WMV FLV WEBM MPG MPEG M2TS MTS 3GP"),
	"pic":   extSet("JPG JPEG PNG GIF BMP TIFF TIF HEIC HEIF WEBP SVG PSD AI RAW ARW CR2 CR3 NEF ORF RW2 DNG"),
	"audio": extSet("MP3 AAC M4A FLAC WAV OGG OGA OPUS WMA AIFF AIF DSF"),
	"web":   extSet("HTML HTM CSS JS JSON XML PHP ASP ASPX JSP"),
	"exe":   extSet("EXE MSI COM BAT SH BIN RUN APK DEB RPM PKG"),
	"disk":  extSet("ISO IMG DMG VHD VHDX VMDK QCOW2"),
	"zip":   extSet("ZIP RAR 7Z GZ BZ2 XZ TAR TGZ TBZ Z LZ4 ZST"),
}

func extSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, e := range strings.Fields(s) {
		m[e] = true
	}
	return m
}

// searchPred is the compiled row predicate for one query's search options
// (keyword included). nil means "no filtering active".
type searchPred struct {
	needle   string          // "" = no keyword
	loc      string          // "" = everywhere
	ftype    string          // "" = any
	exts     map[string]bool // non-nil only for category ftypes
	extq     string
	dateBy   string
	dateFrom string // "" = open-ended
	dateTo   string // upper bound with "\xff" appended, or ""
	sizeOp   string
	sizeMB   int64
}

func compileSearch(q *resultsQuery) *searchPred {
	p := &searchPred{
		needle: strings.ToLower(strings.TrimSpace(q.Query)),
		loc:    strings.TrimRight(strings.TrimSpace(q.Loc), "/"),
		ftype:  q.FType,
		extq:   strings.ToUpper(strings.TrimPrefix(strings.TrimSpace(q.ExtQ), ".")),
		dateBy: q.DateBy,
		sizeOp: q.SizeOp,
		sizeMB: q.SizeMB,
	}
	p.exts = typeExts[p.ftype]
	if p.ftype == "ext" && p.extq == "" {
		p.ftype = "" // extension mode without an extension filters nothing
	}
	if q.DateFrom != "" || q.DateTo != "" {
		p.dateFrom = q.DateFrom
		if q.DateTo != "" {
			// values carry a time of day; make the To day inclusive
			p.dateTo = q.DateTo + "\xff"
		}
	} else {
		p.dateBy = ""
	}
	if p.sizeOp != "eq" && p.sizeOp != "gt" && p.sizeOp != "lt" {
		p.sizeOp = ""
	}
	if p.needle == "" && p.loc == "" && p.ftype == "" && p.dateBy == "" && p.sizeOp == "" {
		return nil
	}
	return p
}

func (p *searchPred) match(f *FileEnt) bool {
	// The verdict joins the free-text needle so "corrupted" or "intact" in the
	// search box narrows a Conflicting Files listing to the rows that matter. It
	// matches the WORDS THE USER CAN SEE, not the token stored on the row —
	// nobody types "unknown" at a column reading "Undetermined", and typing
	// the label they are looking at must not come back empty. Empty on every
	// other tool's rows, so those search exactly as before.
	if p.needle != "" &&
		!strings.Contains(strings.ToLower(filepath.Join(f.Dir, f.Name)), p.needle) &&
		!strings.Contains(verdictSearchText(f.Verdict), p.needle) {
		return false
	}
	if p.loc != "" && f.Dir != p.loc && !strings.HasPrefix(f.Dir, p.loc+"/") {
		return false
	}
	switch p.ftype {
	case "":
	case "dir":
		if !f.IsDir {
			return false
		}
	case "file":
		if f.IsDir {
			return false
		}
	case "ext":
		if !strings.EqualFold(f.Ext, p.extq) {
			return false
		}
	default:
		if p.exts == nil || !p.exts[f.Ext] {
			return false
		}
	}
	if p.dateBy != "" {
		v := f.Mod
		switch p.dateBy {
		case "created":
			v = f.Created
		case "captured":
			v = f.Captured
		}
		// an absent date is not a confirmed match (the scan's own rule)
		if v == "" || (p.dateFrom != "" && v < p.dateFrom) || (p.dateTo != "" && v > p.dateTo) {
			return false
		}
	}
	if p.sizeOp != "" {
		const mib = int64(1) << 20
		switch p.sizeOp {
		case "eq":
			if f.Size/mib != p.sizeMB {
				return false
			}
		case "gt":
			if f.Size <= p.sizeMB*mib {
				return false
			}
		case "lt":
			if f.Size >= p.sizeMB*mib {
				return false
			}
		}
	}
	return true
}

// dateSpan is the oldest and newest value of one date field across a whole
// result set, "YYYY-MM-DD HH:MM:SS" as stored (empty when nothing carries the
// field). The UI bounds its date pickers with these so a range can never be
// asked for outside the data — note the newest file, not "today": a file may
// legitimately carry a future timestamp.
type dateSpan struct {
	Min string `json:"min,omitempty"`
	Max string `json:"max,omitempty"`
}

// dateRanges covers every field the search menu can filter on.
type dateRanges struct {
	Modified dateSpan `json:"modified"`
	Created  dateSpan `json:"created"`
	Captured dateSpan `json:"captured"`
}

func (d *dateSpan) add(v string) {
	if v == "" { // unknown dates never bound the range (the scan's own rule)
		return
	}
	if d.Min == "" || v < d.Min {
		d.Min = v
	}
	if d.Max == "" || v > d.Max {
		d.Max = v
	}
}

func (r *dateRanges) addFile(f *FileEnt) {
	r.Modified.add(f.Mod)
	r.Created.add(f.Created)
	r.Captured.add(f.Captured)
}

// invalidateDerivedLocked drops everything derived from s.results. Every site
// that replaces or rewrites a stored result must call this, or a cached view /
// date span will describe rows that no longer exist. Caller holds s.mu.
func (s *Server) invalidateDerivedLocked() {
	s.view = nil
	s.dateRange = nil
}

// dateRangeLocked returns the tool's whole-result date span, computing it once
// per stored result — it is O(files), so it must not be redone per request.
// Caller holds s.mu.
func (s *Server) dateRangeLocked(tool string, res *toolResult) dateRanges {
	if r, ok := s.dateRange[tool]; ok {
		return r
	}
	r := scanDateRanges(res)
	if s.dateRange == nil {
		s.dateRange = map[string]dateRanges{}
	}
	s.dateRange[tool] = r
	return r
}

// scanDateRanges spans the tool's ENTIRE stored result, deliberately ignoring
// the active search: the pickers should always reach any file the scan found,
// rather than collapsing as other filters narrow things.
func scanDateRanges(res *toolResult) dateRanges {
	var r dateRanges
	for i := range res.Groups {
		for j := range res.Groups[i].Files {
			r.addFile(&res.Groups[i].Files[j])
		}
	}
	for i := range res.Files {
		r.addFile(&res.Files[i])
	}
	return r
}

// pageTotals aggregates a whole (filtered) result set, so the UI can show
// accurate summaries while holding only the loaded pages.
type pageTotals struct {
	Groups      int   `json:"groups"`
	Files       int   `json:"files"`
	Bytes       int64 `json:"bytes"`
	Reclaimable int64 `json:"reclaimable"`
	// Corrupted-files only: how many rows the scan could actually convict.
	// It is the honest headline for that tool — "48 files in 21 sets, 12
	// identified as damaged" says what the other 36 are, where a bare file
	// count would imply all of them are bad.
	Corrupt int `json:"corrupt,omitempty"`
}

// resultView is one fully refined/filtered/sorted result set, derived from
// the cached scan result for one combination of display parameters; pages
// are slices of it. It is built under s.mu and immutable afterwards, and its
// slices never alias the cached result's (pruneMoved rewrites those in
// place). A single slot is cached: the UI pages sequentially through one
// parameter set, so consecutive requests differ only in offset.
type resultView struct {
	key    string
	groups []Group
	files  []FileEnt
	total  pageTotals // after the q filter — what the summary describes
	grand  pageTotals // before q — badges describe the scan, not the search box
}

func (q *resultsQuery) viewKey() string {
	parts := []string{
		q.Tool,
		strings.ToLower(strings.TrimSpace(q.Query)),
		strconv.FormatBool(q.Match.Name) + "|" + strconv.FormatBool(q.Match.Modified) + "|" + strconv.FormatBool(q.Match.Created),
		q.Sort,
		strings.ToUpper(q.Dir),
		// search options — every field that changes the derived view must be
		// in the key, or the single-slot cache serves a stale view for it
		q.Loc, q.FType, q.ExtQ, q.DateBy, q.DateFrom, q.DateTo,
		q.SizeOp, strconv.FormatInt(q.SizeMB, 10),
	}
	return strings.Join(append(parts, uniquePaths(q.RefDirs)...), "\x00")
}

func clampPage(tool string, off, lim int) (int, int) {
	maxL, defL := maxFileLimit, defFileLimit
	if groupedTools[tool] {
		maxL, defL = maxGroupLimit, defGroupLimit
	}
	if lim <= 0 {
		lim = defL
	}
	if lim > maxL {
		lim = maxL
	}
	if off < 0 {
		off = 0
	}
	return off, lim
}

// slicePage returns in[off:off+lim] without ever going out of bounds, always
// non-nil so the page serializes as [] rather than null.
func slicePage[T any](in []T, off, lim int) []T {
	if off > len(in) {
		off = len(in)
	}
	end := off + lim
	if end > len(in) {
		end = len(in)
	}
	out := make([]T, end-off)
	copy(out, in[off:end])
	return out
}

// handlePagedResults serves one page of the (possibly refined) result set
// plus whole-set totals. Like the legacy GET, it reads cached state only and
// needs no DSM session.
func (s *Server) handlePagedResults(w http.ResponseWriter, r *http.Request) {
	var q resultsQuery
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		writeErr(w, 400, "bad request body")
		return
	}
	if !validTools[q.Tool] {
		writeErr(w, 400, "unknown tool")
		return
	}
	s.mu.Lock()
	res := s.results[q.Tool]
	if res == nil {
		s.mu.Unlock()
		writeJSON(w, 200, map[string]any{"tool": q.Tool, "scanned": false})
		return
	}
	key := q.viewKey()
	view := s.view
	// The matcher resolves the (few) reference folders once per request; the
	// view it feeds is cached until the results — and with them s.refDirs —
	// change.
	refs := newRefMatcher(s.refDirs, res.Roots)
	if view == nil || view.key != key {
		view = buildView(res, &q, key, refs)
		s.view = view
	}
	// Metadata is copied under the lock; the view itself is immutable.
	dates := s.dateRangeLocked(q.Tool, res)
	scannedAt := res.Scanned
	var match *MatchOpts
	if res.Match != nil {
		m := *res.Match
		match = &m
	}
	errs := append([]string{}, res.Errors...)
	trunc := res.Truncated
	s.mu.Unlock()

	off, lim := clampPage(q.Tool, q.Offset, q.Limit)
	resp := map[string]any{
		"tool": q.Tool, "scanned": true, "scannedAt": scannedAt,
		"match": match, "errors": errs,
		"offset": off, "limit": lim,
		"total": view.total, "grand": view.grand,
		// whole-scan span per date field, so the UI can bound its pickers
		"dateRange": dates,
	}
	if trunc != nil {
		resp["truncated"] = trunc
	}
	if groupedTools[q.Tool] {
		page := slicePage(view.groups, off, lim)
		// Before trimming, while the group is still whole.
		if q.Tool == "duplicates" {
			markGroupProt(page, refs)
		}
		resp["groups"] = trimGroupRows(page)
	} else {
		resp["files"] = slicePage(view.files, off, lim)
	}
	writeJSON(w, 200, resp)
}

// buildView derives the filtered/refined/sorted view for one parameter set.
// Caller holds s.mu. Every slice in the view is freshly allocated.
func buildView(res *toolResult, q *resultsQuery, key string, refs *refMatcher) *resultView {
	v := &resultView{key: key}
	pred := compileSearch(q)
	if res.Tool == "corrupted_files" {
		// No refineGroups here, deliberately. Refinement re-buckets a group by
		// the match criteria and drops sub-buckets below two members — which
		// for a corrupted set would split the very comparison that defines it
		// and make members disappear from a report about missing data. The
		// set's criteria are fixed at scan time (size and modified time) and
		// are not the user's to narrow.
		// Deep copy, not append([]Group(nil), …): that copies the Group headers
		// but leaves every Files slice pointing at the cached result's backing
		// array, which pruneMoved rewrites IN PLACE under s.mu — while this
		// view is being serialized outside it. The contract above ("its slices
		// never alias the cached result's") is what makes the lock-free
		// serialization safe, and the duplicates branch gets it from
		// refineGroups' own copies.
		groups := make([]Group, 0, len(res.Groups))
		for _, g := range res.Groups {
			g.Files = append([]FileEnt(nil), g.Files...)
			groups = append(groups, g)
		}
		v.grand = corruptedTotals(groups)
		if pred != nil {
			groups = filterGroups(groups, pred)
		}
		v.groups = groups
		v.total = corruptedTotals(groups)
		if pred == nil {
			v.total = v.grand
		}
		return v
	}
	if res.Tool == "duplicates" {
		groups := refineGroups(res.Groups, q.Match)
		v.grand = dupTotals(groups, refs)
		if pred != nil {
			groups = filterGroups(groups, pred)
		}
		v.groups = groups
		v.total = dupTotals(groups, refs)
		if pred == nil {
			v.total = v.grand
		}
		return v
	}
	// One copy, not two: the view must never alias the cached slices (a move
	// rewrites those in place), but building the unfiltered set first and
	// then filtering it would hold both at once — a needless doubling on a
	// capped-but-large result.
	files := make([]FileEnt, 0, len(res.Files))
	for i := range res.Files {
		f := &res.Files[i]
		v.grand.Files++
		v.grand.Bytes += f.Size
		if pred != nil && !pred.match(f) {
			continue
		}
		files = append(files, *f)
	}
	sortFiles(files, q.Sort, q.Dir)
	v.files = files
	v.total = flatTotals(files)
	return v
}

// refineGroups applies display-time match refinement to scan-time groups —
// the same narrowing the scan performs when criteria are requested up front
// (candKey), applied to an already-stored result. Buckets preserve each
// group's file order and sub-groups the parent group's position, so
// refinement never reorders the reclaimable-first listing. Under
// created-date matching a file with no Created value never groups (an
// unknown date is not a confirmed match — the scan's own rule). The result
// never aliases the cached slices.
func refineGroups(groups []Group, m MatchOpts) []Group {
	out := make([]Group, 0, len(groups))
	if !m.Name && !m.Modified && !m.Created {
		// One backing array for every group's files rather than an
		// allocation per group: at the stored-results cap that is 100k rows
		// arriving as one slice instead of tens of thousands of small ones.
		n := 0
		for _, g := range groups {
			n += len(g.Files)
		}
		arena := make([]FileEnt, 0, n)
		for _, g := range groups {
			off := len(arena)
			arena = append(arena, g.Files...)
			g.Files = arena[off:len(arena):len(arena)]
			out = append(out, g)
		}
		return out
	}
	for _, g := range groups {
		var keys []string
		buckets := map[string][]FileEnt{}
		for _, f := range g.Files {
			k := ""
			if m.Name {
				k = f.Name
			}
			k += "\x00"
			if m.Modified {
				k += f.Mod
			}
			k += "\x00"
			if m.Created {
				if f.Created == "" {
					k += "?" + f.ID // unknown creation time: can never match
				} else {
					k += f.Created
				}
			}
			if _, ok := buckets[k]; !ok {
				keys = append(keys, k)
			}
			buckets[k] = append(buckets[k], f)
		}
		n := 0
		for _, k := range keys {
			b := buckets[k]
			if len(b) < 2 {
				continue
			}
			sub := g
			sub.ID = g.ID + "-" + strconv.Itoa(n)
			sub.Files = append([]FileEnt{}, b...)
			out = append(out, sub)
			n++
		}
	}
	return out
}

// filterGroups keeps every group in which ANY file passes the whole search
// predicate — and keeps the whole group, matching peers and all: the
// keep-one-per-group selection rules only work when the loaded groups are
// complete.
func filterGroups(groups []Group, pred *searchPred) []Group {
	out := make([]Group, 0, len(groups))
	for _, g := range groups {
		for i := range g.Files {
			if pred.match(&g.Files[i]) {
				out = append(out, g)
				break
			}
		}
	}
	return out
}

// dupTotals aggregates duplicate groups. Reclaimable mirrors the UI's own
// arithmetic: per group, every copy beyond the kept one may go — protected
// reference copies count as kept. With no reference folders the per-row
// test is skipped entirely: it runs under s.mu, and joining and comparing
// 100k paths against an empty list on every keystroke would be pure cost.
func dupTotals(groups []Group, refs *refMatcher) pageTotals {
	t := pageTotals{Groups: len(groups)}
	for _, g := range groups {
		t.Files += len(g.Files)
		t.Bytes += g.Size * int64(len(g.Files))
		// Measured in physical copies: hard links to one file are one copy,
		// and a protected name protects the copy behind it.
		copies := physicalRows(g.Files, nil)
		keep := 0
		if !refs.empty() {
			keep = physicalRows(g.Files, func(f *FileEnt) bool { return refs.protects(f.Dir, f.Name) })
		}
		if keep < 1 {
			keep = 1
		}
		if n := copies - keep; n > 0 {
			t.Reclaimable += g.Size * int64(n)
		}
	}
	return t
}

// physicalRows counts the distinct pieces of data among the rows only
// accepts (all rows when only is nil): rows sharing an inode are one copy,
// rows without link information are one copy each.
func physicalRows(files []FileEnt, only func(*FileEnt) bool) int {
	n := 0
	seen := map[string]bool{}
	for i := range files {
		f := &files[i]
		if only != nil && !only(f) {
			continue
		}
		if f.Ino == "" {
			n++
			continue
		}
		if !seen[f.Ino] {
			seen[f.Ino] = true
			n++
		}
	}
	return n
}

// corruptedTotals aggregates corrupted sets. Reclaimable stays zero: nothing
// here is reclaimable — the tool never proposes deleting anything, and a
// number in that field would be read as an invitation to.
func corruptedTotals(groups []Group) pageTotals {
	t := pageTotals{Groups: len(groups)}
	for _, g := range groups {
		t.Files += len(g.Files)
		t.Bytes += g.Size * int64(len(g.Files))
		for _, f := range g.Files {
			if f.Verdict == verdictCorrupt {
				t.Corrupt++
			}
		}
	}
	return t
}

func flatTotals(files []FileEnt) pageTotals {
	t := pageTotals{Files: len(files)}
	for _, f := range files {
		t.Bytes += f.Size
	}
	return t
}

// sortFiles orders a flat view by one column (the grid's sortable columns),
// ties broken by full path so the order is deterministic across pages. An
// unknown field keeps the stored order (the scan's own: size-first for big
// files, path for the rest).
func sortFiles(files []FileEnt, field, dir string) {
	var cmp func(a, b FileEnt) int
	switch field {
	case "name":
		cmp = func(a, b FileEnt) int { return strings.Compare(a.Name, b.Name) }
	case "path":
		cmp = func(a, b FileEnt) int { return strings.Compare(a.Dir, b.Dir) }
	case "size":
		cmp = func(a, b FileEnt) int {
			switch {
			case a.Size < b.Size:
				return -1
			case a.Size > b.Size:
				return 1
			}
			return 0
		}
	case "date":
		cmp = func(a, b FileEnt) int { return strings.Compare(a.Mod, b.Mod) }
	case "created":
		cmp = func(a, b FileEnt) int { return strings.Compare(a.Created, b.Created) }
	case "captured":
		cmp = func(a, b FileEnt) int { return strings.Compare(a.Captured, b.Captured) }
	default:
		return
	}
	desc := strings.EqualFold(dir, "DESC")
	sort.SliceStable(files, func(i, j int) bool {
		c := cmp(files[i], files[j])
		if c == 0 {
			return filepath.Join(files[i].Dir, files[i].Name) < filepath.Join(files[j].Dir, files[j].Name)
		}
		if desc {
			return c > 0
		}
		return c < 0
	})
}
