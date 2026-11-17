// Folders another service keeps in step with copies elsewhere — a cloud
// account, a computer running a sync client, another NAS. Moving a file out
// of one is not a local tidy-up: the service carries the removal to every
// other copy, so the file vanishes from the user's cloud or laptop too. A
// move that would do that is answered with the folders and services
// involved, and goes ahead only once the user has said so (handleMove).
//
// Everything here is asked of DSM with the caller's session, never read
// from the services' own files: each service's list is an API a DSM
// administrator may call, and a service that is not installed simply has
// no API to answer.
package main

import (
	"errors"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// syncedFolder is one folder a service keeps in step, in File Station's
// share space (/Share/sub).
type syncedFolder struct {
	Path string
	By   string
}

// syncHit is what the move tells the user: which folder, which service, and
// how many of the requested files would leave it. An empty Folder with
// Unchecked set means the service is installed but did not answer, so any
// of the files might be in one of its folders.
type syncHit struct {
	Folder    string `json:"folder"`
	By        string `json:"by"`
	Files     int    `json:"files"`
	Unchecked bool   `json:"unchecked,omitempty"`
}

const (
	byHybridShare = "Hybrid Share"
	byShareSync   = "Synology Drive ShareSync"
	byCloudSync   = "Cloud Sync"
	byDrive       = "Synology Drive"
)

// syncAPIs are the services' list APIs. SYNO.API.Info names the ones that
// exist on this NAS; a service that is not installed is not asked. Synology
// Drive has two that list its team folders; either one answering is enough.
var syncAPIs = []string{"SYNO.Core.Share", "SYNO.CloudSync", "SYNO.SynologyDrive.Share", "SYNO.SynologyDrive.TeamFolders"}

// syncedFolders lists every synced folder the services report. unchecked
// names the services that exist but could not be read: the caller must
// treat every file as possibly inside one of their folders.
func (s *fsSession) syncedFolders() (folders []syncedFolder, unchecked []string) {
	var info map[string]apiInfo
	if err := s.rawCallTo("/webapi/query.cgi", "SYNO.API.Info", 1, "query",
		url.Values{"query": {strings.Join(syncAPIs, ",")}}, &info); err != nil {
		// Without the list of installed services nothing can be ruled out.
		return nil, []string{"the NAS's sync services"}
	}
	has := func(api string) (int, bool) {
		ai, ok := info[api]
		return ai.Max, ok && ai.Max > 0
	}

	// Whole shared folders: Hybrid Share keeps a share in Synology C2 and
	// on every NAS linked to it; ShareSync keeps one in step with another
	// NAS. DSM flags both on the share itself.
	if _, ok := has("SYNO.Core.Share"); ok {
		var data struct {
			Shares []struct {
				Name      string `json:"name"`
				C2        bool   `json:"is_c2_share"`
				ShareSync bool   `json:"is_sync_share"`
			} `json:"shares"`
		}
		err := s.rawCall("SYNO.Core.Share", 1, "list", url.Values{
			"additional": {jsonArr("is_c2_share", "is_sync_share")},
		}, &data)
		if err != nil {
			unchecked = append(unchecked, "shared folder settings")
		}
		for _, sh := range data.Shares {
			switch {
			case sh.Name == "":
			case sh.C2:
				folders = append(folders, syncedFolder{"/" + sh.Name, byHybridShare})
			case sh.ShareSync:
				folders = append(folders, syncedFolder{"/" + sh.Name, byShareSync})
			}
		}
	}

	// Cloud Sync: every task of every user's connection names the NAS
	// folder it keeps in step with a cloud account.
	if _, ok := has("SYNO.CloudSync"); ok {
		fs, err := s.cloudSyncFolders(1)
		if err != nil {
			unchecked = append(unchecked, byCloudSync)
		}
		folders = append(folders, fs...)
	}

	// Synology Drive: every team folder it has been enabled on — "homes"
	// among them, which is each user's My Drive.
	var driveAPIs []string
	var driveVers []int
	for _, api := range []string{"SYNO.SynologyDrive.Share", "SYNO.SynologyDrive.TeamFolders"} {
		if v, ok := has(api); ok {
			driveAPIs, driveVers = append(driveAPIs, api), append(driveVers, v)
		}
	}
	if len(driveAPIs) > 0 {
		var fs []syncedFolder
		err := errors.New("not asked")
		for i := 0; i < len(driveAPIs) && err != nil; i++ {
			fs, err = s.driveFolders(driveAPIs[i], driveVers[i])
		}
		if err != nil {
			unchecked = append(unchecked, byDrive)
		}
		folders = append(folders, fs...)
	}
	return folders, unchecked
}

func (s *fsSession) cloudSyncFolders(v int) ([]syncedFolder, error) {
	var conns struct {
		Conn []struct {
			ID int `json:"id"`
		} `json:"conn"`
	}
	if err := s.rawCall("SYNO.CloudSync", v, "list_conn", url.Values{
		"is_tray":  {"false"},
		"group_by": {jsonQuote("group_by_user")},
	}, &conns); err != nil {
		return nil, err
	}
	var out []syncedFolder
	for _, c := range conns.Conn {
		var sess struct {
			Sess []struct {
				Local string `json:"local_sync_path"`
			} `json:"sess"`
		}
		if err := s.rawCall("SYNO.CloudSync", v, "list_sess", url.Values{
			"connection_id": {strconv.Itoa(c.ID)},
		}, &sess); err != nil {
			return out, err
		}
		for _, t := range sess.Sess {
			if p := cleanSharePath(t.Local); p != "" {
				out = append(out, syncedFolder{p, byCloudSync})
			}
		}
	}
	return out, nil
}

// driveFolders reads Synology Drive's team folders from one of its two
// list APIs. An entry that cannot be understood is an error, never a folder
// quietly left out: the caller then reports Drive as unchecked. An entry
// that does not say whether Drive is switched on for it counts as on.
func (s *fsSession) driveFolders(api string, v int) ([]syncedFolder, error) {
	var data struct {
		Items []map[string]any `json:"items"`
	}
	if err := s.rawCall(api, v, "list", url.Values{
		"action": {jsonQuote("list")},
		"offset": {"0"},
		"limit":  {"1000"},
	}, &data); err != nil {
		return nil, err
	}
	var out []syncedFolder
	for _, t := range data.Items {
		name, _ := t["share_name"].(string)
		if name == "" {
			return nil, errors.New("a Synology Drive team folder was listed without its shared folder's name")
		}
		if on, said := t["share_enable"].(bool); said && !on {
			continue
		}
		out = append(out, syncedFolder{"/" + name, byDrive})
	}
	return out, nil
}

// cleanSharePath normalises a share-space path a service reported: rooted,
// no trailing slash, no dot segments. "" for anything that names no share.
func cleanSharePath(p string) string {
	if p == "" {
		return ""
	}
	p = path.Clean("/" + p)
	if p == "/" {
		return ""
	}
	return p
}

// syncedArea is a synced folder located on the volumes, so that a file is
// matched by where it really is — never by which of a share's names its
// path happens to be written with ("home" and "homes" reach the same files).
type syncedArea struct {
	syncedFolder
	// roots are the folder's volume paths, in every form the share table
	// gives its share.
	roots []string
	// A folder a service reported under "home" is a different folder for
	// each user, and the service does not say whose: it is matched under
	// every user's home. homes are the volume paths holding the users'
	// homes, homeRest the folder's path inside a home ("" for all of it).
	homes    []string
	homeRest string
}

// locate maps each synced folder onto the volumes through the session's
// share table. A folder in a share the table does not list is left out:
// File Station cannot address that share, so nothing in it can be moved.
func locate(folders []syncedFolder, tab []shareEntry) []syncedArea {
	rootsOf := func(share string) []string {
		var out []string
		for _, e := range tab {
			if !strings.EqualFold(e.name, share) {
				continue
			}
			out = append(out, e.real)
			if e.resolved != "" {
				out = append(out, e.resolved)
			}
		}
		return out
	}
	var areas []syncedArea
	for _, f := range folders {
		share, rest := splitShare(f.Path)
		a := syncedArea{syncedFolder: f}
		if strings.EqualFold(share, "home") {
			a.homes, a.homeRest = rootsOf("homes"), rest
			if len(a.homes) == 0 {
				for _, r := range rootsOf("home") {
					a.homes = append(a.homes, filepath.Dir(r))
				}
			}
		} else {
			for _, r := range rootsOf(share) {
				a.roots = append(a.roots, r+rest)
			}
		}
		if len(a.roots) > 0 || len(a.homes) > 0 {
			areas = append(areas, a)
		}
	}
	return areas
}

func splitShare(p string) (share, rest string) {
	p = strings.TrimPrefix(p, "/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i], p[i:]
	}
	return p, ""
}

// inHome splits volume path p, somewhere inside one user's home under
// homes root h, into its path within that home ("" for the home itself).
func inHome(p, h string) (rest string, ok bool) {
	if !strings.HasPrefix(p, h+"/") {
		return "", false
	}
	p = p[len(h)+1:]
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[i:], true
	}
	return "", true
}

// holds reports whether volume path p is the synced folder or inside it.
func (a syncedArea) holds(p string) bool {
	for _, r := range a.roots {
		if p == r || strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	for _, h := range a.homes {
		if rest, ok := inHome(p, h); ok &&
			(a.homeRest == "" || rest == a.homeRest || strings.HasPrefix(rest, a.homeRest+"/")) {
			return true
		}
	}
	return false
}

// keeps reports whether moving src to dest leaves it inside the same
// synced folder — the service then sees a move and keeps the file. For a
// folder under "home" that is the same user's copy of it: one user's
// Dropbox is not another's, though both match holds.
func (a syncedArea) keeps(src, dest string) bool {
	for _, r := range a.roots {
		if (src == r || strings.HasPrefix(src, r+"/")) && (dest == r || strings.HasPrefix(dest, r+"/")) {
			return true
		}
	}
	for _, h := range a.homes {
		su, sok := homeUser(src, h)
		du, dok := homeUser(dest, h)
		if !sok || !dok || su != du {
			continue
		}
		root := h + "/" + su
		if a.homeRest != "" {
			root += a.homeRest
		}
		if (src == root || strings.HasPrefix(src, root+"/")) && (dest == root || strings.HasPrefix(dest, root+"/")) {
			return true
		}
	}
	return false
}

// homeUser is the user whose home under h holds p.
func homeUser(p, h string) (string, bool) {
	if !strings.HasPrefix(p, h+"/") {
		return "", false
	}
	u := p[len(h)+1:]
	if i := strings.IndexByte(u, '/'); i >= 0 {
		u = u[:i]
	}
	return u, u != ""
}

// within reports whether the synced folder is somewhere inside folder p,
// so that moving p carries the whole synced folder away with it.
func (a syncedArea) within(p string) bool {
	for _, r := range a.roots {
		if strings.HasPrefix(r, p+"/") {
			return true
		}
	}
	for _, h := range a.homes {
		if rest, ok := inHome(p, h); ok && strings.HasPrefix(a.homeRest, rest+"/") {
			return true
		}
	}
	return false
}

// syncHits says which of the files, given by their volume paths, would
// leave a synced folder by moving to dest. A file whose destination is
// inside the same synced folder is not counted: the service sees a move and
// keeps the file. A folder that has a synced folder inside it is counted:
// moving it takes that folder along.
func syncHits(areas []syncedArea, unchecked []string, files []string, dest string) []syncHit {
	var hits []syncHit
	idx := map[syncedFolder]int{}
	for _, src := range files {
		for _, a := range areas {
			if !a.within(src) && (!a.holds(src) || a.keeps(src, dest)) {
				continue
			}
			i, ok := idx[a.syncedFolder]
			if !ok {
				i = len(hits)
				idx[a.syncedFolder] = i
				hits = append(hits, syncHit{Folder: a.Path, By: a.By})
			}
			hits[i].Files++
		}
	}
	for _, by := range unchecked {
		hits = append(hits, syncHit{By: by, Files: len(files), Unchecked: true})
	}
	return hits
}

// moveSyncHits asks which of the request's files, given by their canonical
// volume paths, would leave a synced folder by moving to destCanon. Without
// the share table the folders cannot be located — and File Station can
// then move nothing either, so the per-file checks refuse every file.
func moveSyncHits(sess *fsSession, files []string, destCanon string) []syncHit {
	folders, unchecked := sess.syncedFolders()
	if len(folders) == 0 && len(unchecked) == 0 {
		return nil
	}
	tab, err := sess.shareTable()
	if err != nil {
		return nil
	}
	return syncHits(locate(folders, tab), unchecked, files, destCanon)
}

// syncHitsMessage is the refusal's text for a caller that is not the app,
// which draws its own dialog from the hits.
func syncHitsMessage(hits []syncHit) string {
	var parts []string
	for _, h := range hits {
		if h.Unchecked {
			parts = append(parts, h.By+" could not be checked")
		} else {
			parts = append(parts, h.Folder+" ("+h.By+")")
		}
	}
	return "some of these files are in folders kept in step elsewhere, and moving them out removes them there too: " +
		strings.Join(parts, ", ") + " — confirm to move them anyway"
}
