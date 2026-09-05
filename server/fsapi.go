package main

// File mutations are delegated to DSM's File Station Web API: moves,
// renames, folder creation, deletes and the CSV upload all execute inside
// File Station with the requesting administrator's own DSM session — so
// they behave exactly like File Station itself, Synology-private metadata
// (the real Created Date, ACL handling, instant btrfs fastcopy) included.
// The daemon keeps only read-side work native: scanning, hashing, and the
// safety vetting that decides whether a mutation may happen at all.

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// dsmBaseHeader carries the local DSM base URL (scheme://127.0.0.1:port),
// derived by the CGI shim from its own environment — the same server the
// user's session is valid against.
const dsmBaseHeader = "X-DupFinder-DSM"

// fsSession is the caller's DSM session as forwarded through api.cgi: the
// browser's cookie and SynoToken ride the proxied request untouched. Every
// File Station call acts as that logged-in administrator, with their
// permissions, exactly as if they had used File Station directly.
type fsSession struct {
	base   string
	cookie string
	token  string

	// Share table for path mapping, fetched from list_share once per session
	// object and reused for the session's lifetime (a scan holds one session
	// for its whole run, and fetchCrtimes maps millions of paths through it).
	// sync.Once so the concurrent crtime workers cannot race the fetch.
	shareOnce sync.Once
	shareTab  []shareEntry
	shareErr  error
}

// shareEntry is one share's mapping row: File Station name, real path, and
// the real path's symlink-resolved form (matched against canonical paths,
// which arrive resolved).
type shareEntry struct {
	name     string
	real     string
	resolved string
}

// shareTable returns the session's share mapping, fetching it on first use.
func (s *fsSession) shareTable() ([]shareEntry, error) {
	s.shareOnce.Do(func() {
		var data struct {
			Shares []struct {
				Name       string `json:"name"`
				Additional struct {
					RealPath string `json:"real_path"`
				} `json:"additional"`
			} `json:"shares"`
		}
		s.shareErr = s.call("SYNO.FileStation.List", "list_share", url.Values{
			"additional": {jsonArr("real_path")},
			"limit":      {"0"},
		}, &data)
		if s.shareErr != nil {
			return
		}
		for _, sh := range data.Shares {
			if sh.Name == "" || sh.Additional.RealPath == "" {
				continue
			}
			e := shareEntry{name: sh.Name, real: filepath.Clean(sh.Additional.RealPath)}
			if rp, err := filepath.EvalSymlinks(e.real); err == nil && rp != e.real {
				e.resolved = rp
			}
			s.shareTab = append(s.shareTab, e)
		}
	})
	return s.shareTab, s.shareErr
}

// shareSpacePath converts a volume path (/volume1/Backups/x) to File
// Station's share-space form (/Backups/x) through the session's share table.
// The share NAME is what File Station addresses and it is not derivable from
// the path: external shares mount under a directory that differs from their
// name ("usbshare1" at /volumeUSB1/usbshare), so stripping the volume prefix
// would build a share path that does not exist.
// A volume root itself maps to no share and stays an error, which the
// volume-root refusals elsewhere rely on. Fails closed: no table, no path.
func (s *fsSession) shareSpacePath(p string) (string, error) {
	tab, err := s.shareTable()
	if err != nil {
		return "", err
	}
	p = filepath.Clean(p)
	for _, e := range tab {
		for _, root := range []string{e.real, e.resolved} {
			if root == "" {
				continue
			}
			if p == root {
				return "/" + e.name, nil
			}
			if strings.HasPrefix(p, root+"/") {
				return "/" + e.name + p[len(root):], nil
			}
		}
	}
	return "", fmt.Errorf("no shared-folder path for %s", p)
}

// fsSessionFrom extracts the forwarded DSM session from a proxied request.
// Without one (a raw daemon call outside DSM), scans and mutations are
// refused — the daemon token alone can neither scan nor move files. The
// dev-build env hook (devDSMBase) points the harness at the mock Web API;
// release builds accept the CGI-forwarded header only.
func fsSessionFrom(r *http.Request) (*fsSession, error) {
	base := r.Header.Get(dsmBaseHeader)
	if base == "" {
		base = devDSMBase() // dev builds only; "" in release
	}
	if base == "" {
		return nil, fmt.Errorf("an active DSM session is required")
	}
	return &fsSession{
		base:   strings.TrimSuffix(base, "/"),
		cookie: r.Header.Get("Cookie"),
		token:  r.Header.Get("X-Syno-Token"),
	}, nil
}

// fsHTTP talks only to the loopback DSM instance; its certificate is the
// self-signed one DSM serves locally, hence the verification skip.
var fsHTTP = &http.Client{
	Timeout:   10 * time.Minute,
	Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
}

// fsError is a File Station API failure: the top-level code plus any nested
// per-file codes. Live DSM wraps the specific cause inside the generic
// family code (a collision surfaces as 1001 with a nested 1003), so both
// levels matter.
type fsError struct {
	API    string
	Code   int
	Nested []int
}

func (e *fsError) Error() string {
	// The guide's common (session-level) error codes get a human answer —
	// these surface mid-operation when a DSM session times out or is
	// displaced, and "error 106" helps nobody.
	switch e.Code {
	case 105:
		return "your DSM account does not have permission for this File Station operation"
	case 106, 107, 119:
		return "your DSM session has expired — please sign in to DSM again"
	}
	code := strconv.Itoa(e.Code)
	for _, n := range e.Nested {
		code += "/" + strconv.Itoa(n)
	}
	return fmt.Sprintf("File Station %s failed (error %s)", strings.TrimPrefix(e.API, "SYNO.FileStation."), code)
}

func (e *fsError) hasCode(codes ...int) bool {
	for _, c := range codes {
		if e.Code == c {
			return true
		}
		for _, n := range e.Nested {
			if n == c {
				return true
			}
		}
	}
	return false
}

// isCollision reports whether the error is File Station's "a file with this
// name already exists" family — 1003 (no overwrite parameter given) and
// 1004 (file/folder name clash) — at either error level. With the overwrite
// parameter always omitted, these are the only possible outcomes of a name
// collision: nothing is ever overwritten or silently skipped.
func isCollision(err error) bool {
	var fe *fsError
	return errors.As(err, &fe) && fe.hasCode(1003, 1004)
}

// worthStaging additionally accepts the bare generic move-failure codes
// (1000/1001): live DSM sometimes reports a collision that way without
// nested detail. Retrying through the staging flow is harmless when the
// real cause is something else — the staging move fails with the genuine
// error, which is then reported.
func worthStaging(err error) bool {
	if isCollision(err) {
		return true
	}
	var fe *fsError
	return errors.As(err, &fe) && (fe.Code == 1000 || fe.Code == 1001)
}

// jsonArr encodes path-like parameters as a JSON array, the Web API's
// unambiguous list syntax — a comma inside a filename must not split it
// into two paths.
func jsonArr(items ...string) string {
	b, _ := json.Marshal(items)
	return string(b)
}

// ---------------------------------------------------------- API versions

type apiInfo struct {
	Min int `json:"minVersion"`
	Max int `json:"maxVersion"`
}

var (
	apiInfoMu    sync.Mutex
	apiInfoCache = map[string]map[string]apiInfo{} // base → api → info
)

// docMinVersions are the guide-documented minimum versions of the methods
// this client calls — the fallback when SYNO.API.Info discovery is
// unavailable, since version 1 would fail every call with error 104.
var docMinVersions = map[string]int{
	"SYNO.FileStation.CopyMove":     3,
	"SYNO.FileStation.Rename":       2,
	"SYNO.FileStation.CreateFolder": 2,
	"SYNO.FileStation.Delete":       2,
	"SYNO.FileStation.Upload":       2,
	"SYNO.FileStation.List":         2,
}

// version resolves the API version to call: SYNO.API.Info is queried once
// per DSM base and cached on success, so the client always speaks a version
// the installed DSM actually supports (they differ across DSM releases).
// A failed discovery is NOT cached — it retries on the next call — and the
// fallback is each method's documented minimum, never version 1.
func (s *fsSession) version(api string) int {
	apiInfoMu.Lock()
	info, ok := apiInfoCache[s.base]
	apiInfoMu.Unlock()
	if !ok {
		var data map[string]apiInfo
		p := url.Values{"query": {"SYNO.FileStation.CopyMove,SYNO.FileStation.Rename," +
			"SYNO.FileStation.CreateFolder,SYNO.FileStation.Delete,SYNO.FileStation.Upload," +
			"SYNO.FileStation.List"}}
		// The guide's guaranteed home for SYNO.API.Info is query.cgi (the
		// per-API entry.cgi dispatch is only documented for the APIs it
		// itself lists).
		if err := s.rawCallTo("/webapi/query.cgi", "SYNO.API.Info", 1, "query", p, &data); err == nil && len(data) > 0 {
			info = data
			apiInfoMu.Lock()
			apiInfoCache[s.base] = info
			apiInfoMu.Unlock()
		}
	}
	if ai, ok := info[api]; ok && ai.Max > 0 {
		return ai.Max
	}
	if min, ok := docMinVersions[api]; ok {
		return min
	}
	return 1
}

// ------------------------------------------------------------- transport

type fsEnvelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Err     struct {
		Code   int `json:"code"`
		Errors []struct {
			Code int    `json:"code"`
			Path string `json:"path"`
		} `json:"errors"`
	} `json:"error"`
}

func (s *fsSession) rawCall(api string, version int, method string, params url.Values, out any) error {
	return s.rawCallTo("/webapi/entry.cgi", api, version, method, params, out)
}

func (s *fsSession) rawCallTo(endpoint, api string, version int, method string, params url.Values, out any) error {
	v := url.Values{}
	for k, vals := range params {
		v[k] = vals
	}
	v.Set("api", api)
	v.Set("version", strconv.Itoa(version))
	v.Set("method", method)
	req, err := http.NewRequest("POST", s.base+endpoint, strings.NewReader(v.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.decorate(req)
	return s.do(req, api, out)
}

func (s *fsSession) call(api, method string, params url.Values, out any) error {
	return s.rawCall(api, s.version(api), method, params, out)
}

func (s *fsSession) decorate(req *http.Request) {
	if s.cookie != "" {
		req.Header.Set("Cookie", s.cookie)
	}
	if s.token != "" {
		req.Header.Set("X-SYNO-TOKEN", s.token)
	}
}

func (s *fsSession) do(req *http.Request, api string, out any) error {
	resp, err := fsHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach DSM: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var env fsEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("unexpected DSM response (%s)", resp.Status)
	}
	if !env.Success {
		fe := &fsError{API: api, Code: env.Err.Code}
		for _, n := range env.Err.Errors {
			fe.Nested = append(fe.Nested, n.Code)
		}
		return fe
	}
	if out != nil && len(env.Data) > 0 {
		return json.Unmarshal(env.Data, out)
	}
	return nil
}

// ------------------------------------------------------------ operations

// copyMoveOnce moves one path into destShare (start + poll to completion).
// The overwrite parameter is deliberately never sent: a name collision
// fails with 1003/1004 instead of overwriting or skipping.
func (s *fsSession) copyMoveOnce(srcShare, destShare string) error {
	var started struct {
		TaskID string `json:"taskid"`
	}
	// dest_folder_path is documented as a singular String (unlike path);
	// send it JSON-quoted, matching the guide's own example.
	destJSON, _ := json.Marshal(destShare)
	err := s.call("SYNO.FileStation.CopyMove", "start", url.Values{
		"path":              {jsonArr(srcShare)},
		"dest_folder_path":  {string(destJSON)},
		"remove_src":        {"true"},
		"accurate_progress": {"false"},
	}, &started)
	if err != nil {
		return err
	}
	delay := 200 * time.Millisecond
	deadline := time.Now().Add(copyMoveMax)
	for {
		var st struct {
			Finished bool `json:"finished"`
		}
		err := s.call("SYNO.FileStation.CopyMove", "status",
			url.Values{"taskid": {started.TaskID}}, &st)
		if err != nil {
			return err // task failure surfaces here with its error code
		}
		if st.Finished {
			return nil
		}
		if time.Now().After(deadline) {
			return errMoveTimeout
		}
		time.Sleep(delay)
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}

func (s *fsSession) rename(pathShare, newName string) error {
	return s.call("SYNO.FileStation.Rename", "rename", url.Values{
		"path": {jsonArr(pathShare)},
		"name": {jsonArr(newName)},
	}, nil)
}

func (s *fsSession) createFolder(parentShare, name string, forceParent bool) error {
	return s.call("SYNO.FileStation.CreateFolder", "create", url.Values{
		"folder_path":  {jsonArr(parentShare)},
		"name":         {jsonArr(name)},
		"force_parent": {strconv.FormatBool(forceParent)},
	}, nil)
}

func (s *fsSession) deletePath(pathShare string) error {
	// The blocking variant: fine for the small temp folders this deletes.
	return s.call("SYNO.FileStation.Delete", "delete",
		url.Values{"path": {jsonArr(pathShare)}}, nil)
}

// upload writes content as a new file in destShare. Like the moves, the
// overwrite parameter is never sent, so an existing name fails (1805)
// rather than being overwritten.
func (s *fsSession) upload(destShare, filename string, content []byte) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	api := "SYNO.FileStation.Upload"
	w.WriteField("api", api)
	w.WriteField("version", strconv.Itoa(s.version(api)))
	w.WriteField("method", "upload")
	w.WriteField("path", destShare)
	w.WriteField("create_parents", "false")
	fw, err := w.CreateFormFile("file", filename)
	if err != nil {
		return err
	}
	if _, err := fw.Write(content); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	req, err := http.NewRequest("POST", s.base+"/webapi/entry.cgi", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	s.decorate(req)
	return s.do(req, api, nil)
}

// isUploadCollision is upload's flavor of "name already exists" (1805).
func isUploadCollision(err error) bool {
	var fe *fsError
	return errors.As(err, &fe) && fe.Code == 1805
}

// --------------------------------------------------------- move workflow

// parkedError says a move ended with the file inside the destination's
// staging folder: the SOURCE IS GONE — the risky transit has already
// happened — and the only copy sits at tmpShare/name. Typed so execMoveFS
// can verify the parked copy and prune the row like any moved file; the
// message keeps the exact wording the plain errors carried.
type parkedError struct {
	tmpShare string
	name     string
	cause    error
}

func (e parkedError) Error() string {
	return fmt.Sprintf("%v — the file is parked in %s", e.cause, e.tmpShare)
}

// nameClaimTries bounds every probe-then-claim loop over File Station's
// " (n)" names (moveViaFS's final hop, allocBatchFolder): a name that keeps
// being taken between the probe and the claim is another writer winning a
// race, and after this many rounds it is reported rather than spun on.
const nameClaimTries = 25

// copyMoveMax bounds one CopyMove task's status polling. A cross-volume move
// of a large tree legitimately takes hours; past this the task is treated as
// an unknown outcome — the caller probes for the entry rather than guessing
// — instead of holding the move lock for ever on a task DSM never finishes.
const copyMoveMax = 12 * time.Hour

// errMoveTimeout is deliberately NOT an fsError: it is the absence of an
// answer, and moveOutcomeUnknown must say so.
var errMoveTimeout = errors.New("File Station did not finish the move within 12 hours") //lint:ignore ST1005 File Station is a proper noun

// moveOutcomeUnknown reports that an error carries NO answer from File
// Station (connection dropped, reply mangled): the operation may well have
// completed. A coded fsError is the opposite — DSM answered, the state is
// known. The distinction decides whether it is safe to delete a staging
// folder or claim a file's location without probing first.
func moveOutcomeUnknown(err error) bool {
	var fe *fsError
	return !errors.As(err, &fe)
}

// moveViaFS executes one vetted move through File Station: srcShare into
// outShare (share-space paths). The app's " (n)" collision naming is
// preserved by staging: on a collision the file is first moved into a fresh
// temp folder inside the destination (no collision possible), renamed to
// the next free name per File Station's own listing, then moved into place
// — retrying the (instant, same-folder) final hop if another process grabs
// the name. Every call omits the overwrite parameter, so no sequence of
// races can overwrite an existing file. Failures after staging leave the
// file parked inside the destination's temp folder — visible, intact, and
// reported by path.
//
// On success it returns the name the entry finally landed under — the
// original base name, or the " (n)" variant a collision forced — which is
// what lets the caller find the moved file again, e.g. to read it back for
// verification.
func moveViaFS(s *fsSession, srcShare, outShare string) (string, error) {
	base := filepath.Base(srcShare)
	// Probe for the collision by asking File Station about that one name
	// before the first attempt: the common case goes straight to staging, and
	// no reliance is placed on how the installed DSM encodes collision errors
	// (observed live: generic 1001 with the 1003 detail nested — or absent).
	taken, _, err := s.statShare(outShare + "/" + base)
	if err != nil {
		return "", err
	}
	if !taken {
		err := s.copyMoveOnce(srcShare, outShare)
		if err == nil {
			return base, nil
		}
		// A lost reply is not a failed move: the task may have finished with
		// its answer dying on the wire. Before reporting failure, ask where
		// the file actually is — source still there means nothing happened;
		// source gone and the name present at the destination means the move
		// SUCCEEDED and must be reported as such, or the caller tells the
		// user a completed move failed (and never verifies it).
		if moveOutcomeUnknown(err) {
			if srcThere, _, serr := s.statShare(srcShare); serr == nil && !srcThere {
				if destThere, _, derr := s.statShare(outShare + "/" + base); derr == nil && destThere {
					return base, nil
				}
			}
		}
		if !worthStaging(err) {
			return "", err
		}
	}
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tmp := ".dupfinder-tmp-" + hex.EncodeToString(buf)
	if err := s.createFolder(outShare, tmp, false); err != nil {
		return "", err
	}
	tmpShare := outShare + "/" + tmp
	if err := s.copyMoveOnce(srcShare, tmpShare); err != nil {
		// The staging folder may be holding the ONLY copy — whatever the
		// error said. A coded reply does not mean the move did not happen:
		// the task had already been started, and a coded error can come
		// back from the status poll after data moved — a directory moved
		// member by member, or a transient code on a task that in fact
		// finished. Deleting on that evidence would be permanent data loss,
		// so the folder is removed only when the entry is CONFIRMED absent
		// from it; anything less certain parks, visibly and by path.
		if there, _, serr := s.statShare(tmpShare + "/" + base); serr != nil || there {
			return "", parkedError{tmpShare: tmpShare, name: base, cause: err}
		}
		s.deletePath(tmpShare)
		return "", err
	}
	cur := base
	for tries := 0; tries < nameClaimTries; tries++ {
		free, err := s.firstFreeName(outShare, base)
		if err != nil {
			return "", parkedError{tmpShare: tmpShare, name: cur, cause: err}
		}
		if free != cur {
			if err := s.rename(tmpShare+"/"+cur, free); err != nil {
				return "", parkedError{tmpShare: tmpShare, name: cur, cause: err}
			}
			cur = free
		}
		err = s.copyMoveOnce(tmpShare+"/"+cur, outShare)
		if err == nil {
			// The file is at its destination — the move SUCCEEDED. Failing to
			// remove the now-empty staging folder is untidy, not a failure:
			// returned as an error it would make handleMove leave the row
			// unpruned and tell the user a completed move had failed. Log and
			// move on.
			if derr := s.deletePath(tmpShare); derr != nil {
				log.Printf("move staging folder %s could not be removed: %v", tmpShare, derr)
			}
			return cur, nil
		}
		// The final hop's reply can be lost too. The file is then at exactly
		// one of two places; ask the staging folder FIRST — a name present at
		// the destination could be another process's win, but our copy still
		// sitting in staging is unambiguous.
		if moveOutcomeUnknown(err) {
			if inTmp, _, terr := s.statShare(tmpShare + "/" + cur); terr == nil && !inTmp {
				if atDest, _, derr := s.statShare(outShare + "/" + cur); derr == nil && atDest {
					if derr := s.deletePath(tmpShare); derr != nil {
						log.Printf("move staging folder %s could not be removed: %v", tmpShare, derr)
					}
					return cur, nil
				}
			}
		}
		if !worthStaging(err) {
			return "", parkedError{tmpShare: tmpShare, name: cur, cause: err}
		}
		// A staging-worthy error while File Station still reports the name as
		// free is not a collision — report it rather than spinning.
		if n2, lerr := s.firstFreeName(outShare, base); lerr != nil || n2 == cur {
			return "", parkedError{tmpShare: tmpShare, name: cur, cause: err}
		}
	}
	return "", parkedError{tmpShare: tmpShare, name: cur, cause: fmt.Errorf("could not find a free name for %s", base)}
}

// fsCannotAddress reports whether File Station's API refuses to see an entry
// with this name. DSM filters the macOS metadata pair out of every listing,
// and not just from list: getinfo answers code 418 for them, which is
// indistinguishable from "no such file". Since every move this package
// performs goes through File Station, such an entry can be FOUND by the
// scanner's own walk (which reads the directory directly) but never moved;
// left to File Station, the move fails with a bare "no such file or folder"
// for a file the scan has just read. The UI disables the checkbox on these
// rows and the daemon refuses them with an honest reason.
//
// Measured on DSM 7.4. The two clauses were established DIFFERENTLY, and the
// distinction is worth keeping because it says how much each one is worth:
//
//   - ".DS_Store", exactly and CASE-SENSITIVELY. Directly measured: a
//     .DS_Store uploaded fine, the scanner's own walk read it (15 bytes), and
//     File Station's list omitted it while getinfo answered code 418 — the
//     same answer it gives for a file that is not there. ".ds_store" and
//     ".Ds_StOrE" are fully visible and movable, so the comparison must stay
//     case-sensitive. Note isTempName LOWERCASES, so those variants are junk
//     to the Temporary Files tool while remaining perfectly movable; matching
//     case-insensitively here would disable the checkbox on rows that work.
//   - any name beginning "._" (AppleDouble). Established INDIRECTLY: File
//     Station refuses to produce such a name at all — upload answers 418, and
//     renaming an existing file to "._sidecar" answers 418 too — so no probe
//     file could be created through the API and list/getinfo/move were never
//     exercised against a real one. What is proven is that File Station
//     rejects the name wherever it is asked to act on it, which is the same
//     conclusion for our purposes. These files are not hypothetical: macOS
//     writes them over SMB/AFP, so a real NAS has them even though the API
//     cannot make one.
//
// Both clauses cover DIRECTORIES as well as files. The test is on the entry's
// OWN name, so a folder is not unmovable merely because it CONTAINS one of
// these — which is what lets a junk-only folder move with its junk inside.
//
// Re-derive with the same probes if a DSM upgrade seems to change this, and
// check upload's success flag before believing an absence.
func fsCannotAddress(name string) bool {
	return name == ".DS_Store" || strings.HasPrefix(name, "._")
}

// --------------------------------------------------------- share listing

// folderHoldsOnlyJunk reports whether a share-space folder holds nothing but
// disposable junk, per File Station's own listing. This is the empty-folders
// tool's definition of "empty": zero entries, or entries that are all either
// files the Temporary Files tool itself would list (isTempName — one shared
// list decides what "useless" means everywhere) or Synology's @eaDir
// thumbnail cache, which DSM regenerates on demand. Any OTHER directory
// rejects — a folder whose only child is .git is a repository, not clutter —
// and so does any error or a listing too large to finish: unknown contents
// must never read as empty.
//
// Two measured DSM behaviours this leans on: dotFILES like .keep and
// .htaccess DO come back from list, so real content cannot hide from this
// answer — the sole exceptions are .DS_Store and ._* AppleDouble files,
// which DSM filters from list AND getinfo (measured on DSM 7.4: getinfo
// answers code 418 for them). Those never appear here at all, and it
// does not matter: the native walk classifies exactly those names as junk
// upstream, so the two layers agree without seeing the same entries.
func (s *fsSession) folderHoldsOnlyJunk(folderShare string) (bool, error) {
	const page = 500
	const maxEntries = 10000 // past this, refuse rather than trust a partial read
	fp, _ := json.Marshal(folderShare)
	for off := 0; off < maxEntries; off += page {
		var data struct {
			Files []struct {
				Name  string `json:"name"`
				IsDir bool   `json:"isdir"`
			} `json:"files"`
			Total int `json:"total"`
		}
		if err := s.call("SYNO.FileStation.List", "list", url.Values{
			"folder_path": {string(fp)},
			"offset":      {strconv.Itoa(off)},
			"limit":       {strconv.Itoa(page)},
		}, &data); err != nil {
			return false, err
		}
		for _, f := range data.Files {
			if f.IsDir {
				if f.Name != "@eaDir" {
					return false, nil
				}
				continue
			}
			if !isTempName(f.Name) {
				return false, nil
			}
		}
		if len(data.Files) == 0 || off+len(data.Files) >= data.Total {
			return true, nil
		}
	}
	return false, nil
}

// firstFreeName returns base if the destination folder does not hold it,
// else the first free " (n)" variant. It asks File Station about the
// specific names it needs rather than listing the folder: a destination
// holding a million entries costs the same one or two calls as an empty one,
// and nothing about the answer depends on how large the folder is. The bound
// exists only so a pathological destination cannot spin forever: it takes a
// destination already holding base plus 10 000 " (n)" variants of it to reach.
func (s *fsSession) firstFreeName(folderShare, base string) (string, error) {
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if stem == "" {
		// A leading-dot name is ALL extension to filepath.Ext, which leaves
		// no stem and turns ".DS_Store" into " (1).DS_Store" — the original
		// name destroyed, the copy no longer hidden. temp_files hardcodes
		// ".ds_store" and ".apdisk", so a flat move of a Mac-written share
		// hits this on its second file. Append instead.
		stem, ext = base, ""
	}
	for i := 0; i < 10000; i++ {
		cand := base
		if i > 0 {
			cand = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		taken, _, err := s.statShare(folderShare + "/" + cand)
		if err != nil {
			return "", err
		}
		if !taken {
			return cand, nil
		}
	}
	return "", fmt.Errorf("could not find a free name for %s", base)
}

// allocBatchFolder creates a NEW folder named base — or the first free " (n)"
// variant — inside destShare, and returns the name it allocated. Unlike the
// origin chain mirrored beneath it, this folder must never be reused: a batch
// merging into an earlier batch's tree would put two different moves' files in
// one place, and it is precisely this folder's freshness that lets every moved
// file keep its original name. So it is created with force_parent=false, the
// allocating form that FAILS on a name already taken, never the idempotent
// mkdir -p form.
//
// firstFreeName treats any taken name as taken, file or folder alike, so an
// existing FILE called "Duplicates" is enumerated past rather than fought
// with. Its probe and the create that follows are not atomic, and DSM
// collapses CreateFolder failures into a generic code, so a failed create
// cannot be read as "collision" from its error. The name is re-probed
// instead: taken now means another writer won the race and the next candidate
// is tried; still free means the failure was genuine (permissions, quota, a
// read-only share) and it is reported rather than spun on — the same
// reasoning, and the same 25-try bound, as moveViaFS's final hop.
//
// A CreateFolder whose reply is lost leaves one stray empty folder and the
// batch takes the next name. File Station has no create-and-return-path call,
// so that residual is unavoidable; it is visible and harmless.
func (s *fsSession) allocBatchFolder(destShare, base string) (string, error) {
	for tries := 0; tries < nameClaimTries; tries++ {
		name, err := s.firstFreeName(destShare, base)
		if err != nil {
			return "", err
		}
		cerr := s.createFolder(destShare, name, false)
		if cerr == nil {
			return name, nil
		}
		if taken, _, serr := s.statShare(destShare + "/" + name); serr != nil || !taken {
			return "", cerr
		}
	}
	return "", fmt.Errorf("could not create a folder named %s in the destination", base)
}

// statShare reports whether a share path exists and is a directory,
// per File Station (getinfo returns per-file code 408 — or errors with it —
// for missing paths).
func (s *fsSession) statShare(pathShare string) (exists, isdir bool, err error) {
	info, err := s.getInfo([]string{pathShare}, nil)
	if err != nil {
		var fe *fsError
		if errors.As(err, &fe) && fe.hasCode(408) {
			return false, false, nil
		}
		return false, false, err
	}
	e, ok := info[pathShare]
	if !ok || !e.exists() {
		return false, false, nil
	}
	return true, e.IsDir, nil
}

// listShares builds the volume overview from File Station's shared-folder
// listing: shares grouped by the volume their real_path lives on, with the
// volume totals from volume_status.
func (s *fsSession) listShares() ([]volInfo, error) {
	var data struct {
		Shares []struct {
			Name       string `json:"name"`
			Additional struct {
				RealPath     string `json:"real_path"`
				VolumeStatus struct {
					Free  int64 `json:"freespace"`
					Total int64 `json:"totalspace"`
				} `json:"volume_status"`
			} `json:"additional"`
		} `json:"shares"`
	}
	if err := s.call("SYNO.FileStation.List", "list_share", url.Values{
		"additional": {jsonArr("real_path", "volume_status")},
		"limit":      {"0"},
	}, &data); err != nil {
		return nil, err
	}
	byVol := map[string]*volInfo{}
	var order []string
	for _, sh := range data.Shares {
		root := ""
		for _, v := range listVolumes() {
			if strings.HasPrefix(sh.Additional.RealPath, v+"/") {
				root = v
				break
			}
		}
		if root == "" {
			continue
		}
		vi, ok := byVol[root]
		if !ok {
			name := strings.TrimPrefix(filepath.Base(root), "volume")
			if name != filepath.Base(root) {
				name = "Volume " + name
			} else {
				name = filepath.Base(root)
			}
			vi = &volInfo{
				Name: name, Path: root,
				Total: sh.Additional.VolumeStatus.Total,
				Used:  sh.Additional.VolumeStatus.Total - sh.Additional.VolumeStatus.Free,
			}
			byVol[root] = vi
			order = append(order, root)
		}
		// The name/path PAIR ships to the UI: share space and volume space
		// are only connectable through it (external share names differ from
		// their directory), so the client must never rebuild one side from
		// the other by concatenation.
		vi.Shares = append(vi.Shares, shareRef{
			Name: sh.Name, Path: filepath.Clean(sh.Additional.RealPath),
		})
	}
	sort.Strings(order)
	out := make([]volInfo, 0, len(order))
	for _, root := range order {
		vi := byVol[root]
		sort.Slice(vi.Shares, func(i, j int) bool {
			return strings.ToLower(vi.Shares[i].Name) < strings.ToLower(vi.Shares[j].Name)
		})
		out = append(out, *vi)
	}
	return out, nil
}

// -------------------------------------------------- file info / existence

// fsEntry is one File Station getinfo result. A non-zero Code marks a path
// File Station could not answer for (408 = no such file), which for our
// purposes means "not usable/nonexistent".
type fsEntry struct {
	Path       string `json:"path"`
	Name       string `json:"name"`
	IsDir      bool   `json:"isdir"`
	Code       int    `json:"code"`
	Additional struct {
		Size int64 `json:"size"`
		Time struct {
			Crtime int64 `json:"crtime"`
			Mtime  int64 `json:"mtime"`
		} `json:"time"`
	} `json:"additional"`
}

func (e fsEntry) exists() bool { return e.Code == 0 }

// getInfo asks File Station about the given share-space paths (batched).
// additional is the guide's additional-fields list (nil for existence-only
// probes). Paths missing from the result map do not exist.
func (s *fsSession) getInfo(shares []string, additional []string) (map[string]fsEntry, error) {
	out := map[string]fsEntry{}
	const batch = 100
	for i := 0; i < len(shares); i += batch {
		j := i + batch
		if j > len(shares) {
			j = len(shares)
		}
		arr, _ := json.Marshal(shares[i:j])
		v := url.Values{"path": {string(arr)}}
		if len(additional) > 0 {
			v.Set("additional", jsonArr(additional...))
		}
		var data struct {
			Files []fsEntry `json:"files"`
		}
		if err := s.call("SYNO.FileStation.List", "getinfo", v, &data); err != nil {
			return nil, err
		}
		for _, f := range data.Files {
			out[f.Path] = f
		}
	}
	return out, nil
}

// ------------------------------------------------------- creation times

// crtimeWorkers is the fan-out for creation-time batches. The batches are
// independent getinfo calls, and on large scans sequential round trips would
// dominate created-date time; 4 matches the scanner's own hashing worker cap
// — enough to hide per-call latency without leaning on DSM's web server.
const crtimeWorkers = 4

// fetchCrtimes returns File Station's creation times for the given volume
// paths — the same value File Station's Created Date column shows,
// including on kernels where statx is unavailable and Synology-private
// otherwise. Failures are silently partial: anything missing from the
// result simply has no Created Date (there is no native fallback). cancel
// (a scan's cancel channel; nil allowed) stops the fetch early with partial
// results; prog, when set, receives monotonic (done, total) path counts.
func (s *fsSession) fetchCrtimes(volPaths []string, cancel chan struct{}, prog func(done, total int)) map[string]time.Time {
	byShare := map[string]string{}
	var shares []string
	for _, p := range volPaths {
		if sp, err := s.shareSpacePath(p); err == nil {
			byShare[sp] = p
			shares = append(shares, sp)
		}
	}
	return s.fetchCrtimesShares(shares, byShare, cancel, prog)
}

func (s *fsSession) fetchCrtimesShares(shares []string, byShare map[string]string, cancel chan struct{}, prog func(done, total int)) map[string]time.Time {
	out := map[string]time.Time{}
	const batch = 100
	if len(shares) == 0 {
		return out
	}
	nBatches := (len(shares) + batch - 1) / batch
	workers := crtimeWorkers
	if nBatches < workers {
		workers = nBatches
	}
	// done is advanced and prog invoked under mu, so progress reports never
	// run backwards even though batches finish out of order.
	var (
		mu   sync.Mutex
		done int
		wg   sync.WaitGroup
	)
	ch := make(chan []string)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range ch {
				if cancelled(cancel) {
					continue
				}
				info, err := s.getInfo(b, []string{"time"})
				mu.Lock()
				done += len(b)
				if err == nil {
					for p, f := range info {
						if vol, ok := byShare[p]; ok && f.exists() && f.Additional.Time.Crtime > 0 {
							out[vol] = time.Unix(f.Additional.Time.Crtime, 0)
						}
					}
				} // errors stay silently partial, as documented
				if prog != nil {
					prog(done, len(shares))
				}
				mu.Unlock()
			}
		}()
	}
	// Batches are cut as they are fed, not materialized up front: on a scan
	// window of millions of paths a slice of slices would be pure overhead.
	for i := 0; i < len(shares); i += batch {
		if cancelled(cancel) {
			break
		}
		j := i + batch
		if j > len(shares) {
			j = len(shares)
		}
		ch <- shares[i:j]
	}
	close(ch)
	wg.Wait()
	return out
}

// applyCrtimes patches the Created column of a finished result set with
// File Station's values, so the app displays exactly what File Station
// does. Anything the API cannot answer stays blank — Created Dates are
// File Station's alone, never a native approximation. cancel/prog pass
// through to the parallel fetch: a cancelled scan stores its results with
// whatever creation times had already arrived.
func applyCrtimes(sess *fsSession, res *toolResult, cancel chan struct{}, prog func(done, total int)) {
	// Only rows still without a value: under Created matching every duplicate
	// row already carries the answer from the per-window fetch, and asking
	// again would cost a thousand repeated round trips at the result cap.
	var paths []string
	for i := range res.Files {
		if res.Files[i].Created == "" {
			paths = append(paths, filepath.Join(res.Files[i].Dir, res.Files[i].Name))
		}
	}
	for gi := range res.Groups {
		for fi := range res.Groups[gi].Files {
			f := &res.Groups[gi].Files[fi]
			if f.Created == "" {
				paths = append(paths, filepath.Join(f.Dir, f.Name))
			}
		}
	}
	if len(paths) == 0 {
		return
	}
	ct := sess.fetchCrtimes(paths, cancel, prog)
	if len(ct) == 0 {
		return
	}
	for i := range res.Files {
		if t, ok := ct[filepath.Join(res.Files[i].Dir, res.Files[i].Name)]; ok {
			res.Files[i].Created = fmtTime(t)
		}
	}
	for gi := range res.Groups {
		for fi := range res.Groups[gi].Files {
			f := &res.Groups[gi].Files[fi]
			if t, ok := ct[filepath.Join(f.Dir, f.Name)]; ok {
				f.Created = fmtTime(t)
			}
		}
	}
}

// exportViaFS uploads the report, stepping to the next free " (n)" name
// (per File Station's listing) on a collision, and returns the name
// actually written.
func exportViaFS(s *fsSession, destShare, name string, content []byte) (string, error) {
	cur := name
	for tries := 0; tries < 100; tries++ {
		err := s.upload(destShare, cur, content)
		if err == nil {
			return cur, nil
		}
		if !isUploadCollision(err) {
			return "", err
		}
		next, lerr := s.firstFreeName(destShare, name)
		if lerr != nil {
			return "", err
		}
		if next == cur {
			return "", err // the listing says free but the upload collides
		}
		cur = next
	}
	return "", fmt.Errorf("could not find a free name for %s", name)
}
