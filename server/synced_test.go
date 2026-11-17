package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSyncDSM answers SYNO.API.Info for the services in installed and
// their lists from the arguments; broken services answer every list with
// an error. Any other API fails the test: the check must ask nothing else.
func fakeSyncDSM(t *testing.T, installed []string, broken map[string]bool, shares []map[string]any, cloud []string, drive []map[string]any) (*fsSession, *[]string, func()) {
	var asked []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		api, method := r.FormValue("api"), r.FormValue("method")
		asked = append(asked, api+"."+method)
		if broken[api] {
			fakeEnv(w, 400, nil)
			return
		}
		switch api {
		case "SYNO.API.Info":
			out := map[string]any{}
			for _, q := range strings.Split(r.FormValue("query"), ",") {
				for _, have := range installed {
					if q == have {
						out[q] = map[string]int{"minVersion": 1, "maxVersion": 2}
					}
				}
			}
			fakeEnv(w, 0, out)
		case "SYNO.Core.Share":
			fakeEnv(w, 0, map[string]any{"shares": shares})
		case "SYNO.CloudSync":
			switch method {
			case "list_conn":
				fakeEnv(w, 0, map[string]any{"conn": []map[string]any{{"id": 7}}})
			case "list_sess":
				if r.FormValue("connection_id") != "7" {
					t.Errorf("list_sess for connection %q, want 7", r.FormValue("connection_id"))
				}
				var sess []map[string]any
				for _, p := range cloud {
					sess = append(sess, map[string]any{"local_sync_path": p})
				}
				fakeEnv(w, 0, map[string]any{"sess": sess})
			}
		case "SYNO.SynologyDrive.Share", "SYNO.SynologyDrive.TeamFolders":
			fakeEnv(w, 0, map[string]any{"items": drive})
		default:
			t.Errorf("unexpected api %s", api)
			fakeEnv(w, 102, nil)
		}
	}))
	return &fsSession{base: ts.URL}, &asked, ts.Close
}

func TestSyncedFoldersReadsEveryInstalledService(t *testing.T) {
	sess, _, done := fakeSyncDSM(t, syncAPIs, nil,
		[]map[string]any{
			{"name": "Plain"},
			{"name": "Cloudy", "is_c2_share": true},
			{"name": "Mirror", "is_sync_share": true},
		},
		[]string{"/Photos/phone/", "Docs"},
		[]map[string]any{
			{"share_name": "Team", "share_enable": true},
			{"share_name": "Off", "share_enable": false},
		})
	defer done()
	folders, unchecked := sess.syncedFolders()
	if len(unchecked) != 0 {
		t.Fatalf("unchecked = %v, want none", unchecked)
	}
	want := map[syncedFolder]bool{
		{"/Cloudy", byHybridShare}:     true,
		{"/Mirror", byShareSync}:       true,
		{"/Photos/phone", byCloudSync}: true,
		{"/Docs", byCloudSync}:         true,
		{"/Team", byDrive}:             true,
	}
	for _, f := range folders {
		if !want[f] {
			t.Errorf("unexpected synced folder %+v", f)
		}
		delete(want, f)
	}
	for f := range want {
		t.Errorf("missing synced folder %+v", f)
	}
}

// A NAS with no sync service installed is asked only which services exist
// — never a list it does not have — and nothing is reported.
func TestSyncedFoldersAsksNothingOfServicesNotInstalled(t *testing.T) {
	sess, asked, done := fakeSyncDSM(t, nil, nil, nil, nil, nil)
	defer done()
	folders, unchecked := sess.syncedFolders()
	if len(folders) != 0 || len(unchecked) != 0 {
		t.Fatalf("folders %v, unchecked %v; want none", folders, unchecked)
	}
	if len(*asked) != 1 || (*asked)[0] != "SYNO.API.Info.query" {
		t.Fatalf("asked %v, want only the SYNO.API.Info query", *asked)
	}
}

// An installed service that does not answer cannot be ruled out: it is
// reported as unchecked, never read as "no synced folders".
func TestSyncedFoldersReportsAServiceThatDidNotAnswer(t *testing.T) {
	sess, _, done := fakeSyncDSM(t, syncAPIs, map[string]bool{"SYNO.CloudSync": true},
		nil, nil, []map[string]any{{"share_name": "Team", "share_enable": true}})
	defer done()
	folders, unchecked := sess.syncedFolders()
	if len(unchecked) != 1 || unchecked[0] != byCloudSync {
		t.Fatalf("unchecked = %v, want [%s]", unchecked, byCloudSync)
	}
	if len(folders) != 1 || folders[0] != (syncedFolder{"/Team", byDrive}) {
		t.Fatalf("folders = %v, want the Drive team folder still reported", folders)
	}
	// And with no list of installed services at all, nothing is ruled out.
	sess2, _, done2 := fakeSyncDSM(t, nil, map[string]bool{"SYNO.API.Info": true}, nil, nil, nil)
	defer done2()
	if _, un := sess2.syncedFolders(); len(un) != 1 {
		t.Fatalf("a failed service query left unchecked = %v, want one entry", un)
	}
}

// Drive's answer is read so that a mistake errs towards asking: an entry
// that does not say whether Drive is on counts as on, and an entry with no
// shared-folder name makes Drive unchecked rather than quietly absent.
func TestDriveAnswerThatCannotBeReadIsNeverNoFolders(t *testing.T) {
	sess, _, done := fakeSyncDSM(t, syncAPIs, nil, nil, nil,
		[]map[string]any{{"share_name": "Team"}, {"share_name": "Off", "share_enable": false}})
	defer done()
	folders, unchecked := sess.syncedFolders()
	if len(unchecked) != 0 || len(folders) != 1 || folders[0] != (syncedFolder{"/Team", byDrive}) {
		t.Fatalf("folders %v, unchecked %v; want only /Team", folders, unchecked)
	}
	sess2, _, done2 := fakeSyncDSM(t, syncAPIs, nil, nil, nil,
		[]map[string]any{{"name": "Team", "enabled": true}})
	defer done2()
	folders, unchecked = sess2.syncedFolders()
	if len(folders) != 0 || len(unchecked) != 1 || unchecked[0] != byDrive {
		t.Fatalf("folders %v, unchecked %v; want Drive unchecked", folders, unchecked)
	}
}

// Drive has two list APIs. The first that answers is used, and the second
// is asked only when the first is missing or fails.
func TestDriveFallsBackToItsOtherList(t *testing.T) {
	team := []map[string]any{{"share_name": "Team", "share_enable": true}}
	sess, asked, done := fakeSyncDSM(t, syncAPIs, nil, nil, nil, team)
	defer done()
	if f, un := sess.syncedFolders(); len(f) != 1 || len(un) != 0 {
		t.Fatalf("folders %v, unchecked %v", f, un)
	}
	for _, a := range *asked {
		if a == "SYNO.SynologyDrive.TeamFolders.list" {
			t.Errorf("the second list was asked although the first answered: %v", *asked)
		}
	}
	sess2, _, done2 := fakeSyncDSM(t, syncAPIs, map[string]bool{"SYNO.SynologyDrive.Share": true}, nil, nil, team)
	defer done2()
	if f, un := sess2.syncedFolders(); len(f) != 1 || len(un) != 0 {
		t.Fatalf("with the first list failing: folders %v, unchecked %v; want /Team from the second", f, un)
	}
	sess3, _, done3 := fakeSyncDSM(t, []string{"SYNO.SynologyDrive.TeamFolders"}, nil, nil, nil, team)
	defer done3()
	if f, un := sess3.syncedFolders(); len(f) != 1 || len(un) != 0 {
		t.Fatalf("with only the second list installed: folders %v, unchecked %v", f, un)
	}
}

var syncTestShares = []shareEntry{
	{name: "Photos", real: "/volume1/Photos"},
	{name: "Team", real: "/volume1/Team"},
	{name: "Plain", real: "/volume2/Plain", resolved: "/volume1/@plain"},
	{name: "home", real: "/volume1/homes/ash"},
	{name: "homes", real: "/volume1/homes"},
}

func TestSyncHitsCountsOnlyFilesLeavingTheFolder(t *testing.T) {
	areas := locate([]syncedFolder{{"/photos/phone", byCloudSync}, {"/Team", byDrive}, {"/Gone/x", byCloudSync}}, syncTestShares)
	if len(areas) != 2 {
		t.Fatalf("areas = %+v, want the folder in an unlisted share left out", areas)
	}
	srcs := []string{
		"/volume1/Photos/phone/a.jpg",      // leaves Cloud Sync's folder (its share named in another case)
		"/volume1/Photos/phone2/c.jpg",     // a sibling whose name only starts the same
		"/volume1/Photos/d.jpg",            // above the synced folder
		"/volume1/Team/sub/e.doc",          // leaves the Drive team folder
		"/volume2/Plain/f.txt",             // not synced
		"/volume1/Photos/phone/deep/g.jpg", // inside, deeper
		"/volume1/Photos/phone",            // the synced folder itself
	}
	hits := syncHits(areas, nil, srcs, "/volume2/Plain/Moved")
	got := map[string]int{}
	for _, h := range hits {
		got[h.Folder+"|"+h.By] = h.Files
	}
	want := map[string]int{"/photos/phone|" + byCloudSync: 3, "/Team|" + byDrive: 1}
	if len(got) != len(want) {
		t.Fatalf("hits = %+v, want %v", hits, want)
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("%s: %d files, want %d", k, got[k], n)
		}
	}
	// Moving within the same synced folder is a move the service keeps.
	if h := syncHits(areas, nil, []string{"/volume1/Team/a.doc"}, "/volume1/Team/Duplicates"); len(h) != 0 {
		t.Errorf("a move inside one synced folder was flagged: %+v", h)
	}
	// An unchecked service covers every file.
	h := syncHits(nil, []string{byDrive}, []string{"/volume1/A/x", "/volume1/B/y"}, "/volume1/C")
	if len(h) != 1 || !h[0].Unchecked || h[0].Files != 2 {
		t.Errorf("unchecked hit = %+v, want one covering both files", h)
	}
}

// Moving a folder that has a synced folder somewhere inside it takes the
// synced folder away too, wherever the folder is going.
func TestSyncHitsCountsAFolderHoldingASyncedFolder(t *testing.T) {
	areas := locate([]syncedFolder{{"/Photos/2019/phone", byCloudSync}}, syncTestShares)
	h := syncHits(areas, nil, []string{"/volume1/Photos/2019", "/volume1/Photos/2019b", "/volume1/Photos/2018"}, "/volume1/Team")
	if len(h) != 1 || h[0].Files != 1 || h[0].Folder != "/Photos/2019/phone" {
		t.Fatalf("hits = %+v, want the one folder above the synced folder", h)
	}
}

// A share reached by a second volume path (a symlinked share root) is
// matched by either form.
func TestSyncHitsMatchesAShareByEveryPathItHas(t *testing.T) {
	areas := locate([]syncedFolder{{"/Plain", byHybridShare}}, syncTestShares)
	h := syncHits(areas, nil, []string{"/volume1/@plain/a", "/volume2/Plain/b", "/volume1/Photos/c"}, "/volume1/Team")
	if len(h) != 1 || h[0].Files != 2 {
		t.Fatalf("hits = %+v, want both forms of the share matched", h)
	}
}

// "home" and "homes" are two names for the same files. Drive syncing
// "homes" covers a file however its path is written, and a folder a
// service reports under "home" — whose home, it does not say — is matched
// in every user's home.
func TestSyncHitsSeesThroughTheTwoNamesOfAHome(t *testing.T) {
	areas := locate([]syncedFolder{{"/homes", byDrive}}, syncTestShares)
	if h := syncHits(areas, nil, []string{"/volume1/homes/ash/doc.txt"}, "/volume1/Team"); len(h) != 1 || h[0].Files != 1 {
		t.Fatalf("a file in a home was not matched to Drive's homes folder: %+v", h)
	}
	areas = locate([]syncedFolder{{"/home/Dropbox", byCloudSync}}, syncTestShares)
	srcs := []string{
		"/volume1/homes/ash/Dropbox/a",  // the caller's own home
		"/volume1/homes/bob/Dropbox/b",  // someone else's
		"/volume1/homes/bob/Dropbox",    // the synced folder itself
		"/volume1/homes/bob",            // a folder holding it
		"/volume1/homes/bob/Dropbox2/c", // a sibling
		"/volume1/homes/bob/other/d",    // elsewhere in a home
		"/volume1/Photos/Dropbox/e",     // not a home
	}
	h := syncHits(areas, nil, srcs, "/volume1/Team")
	if len(h) != 1 || h[0].Files != 4 {
		t.Fatalf("hits = %+v, want the four paths in or around a home's Dropbox", h)
	}
	// Tidying inside the same synced home folder is not a removal.
	if h := syncHits(areas, nil, []string{"/volume1/homes/ash/Dropbox/a"}, "/volume1/homes/ash/Dropbox/Duplicates"); len(h) != 0 {
		t.Errorf("a move inside the synced home folder was flagged: %+v", h)
	}
	// But one user's synced folder is not another's: moving from Ash's
	// Dropbox into Bob's removes it from Ash's, and is warned about.
	if h := syncHits(areas, nil, []string{"/volume1/homes/ash/Dropbox/a"}, "/volume1/homes/bob/Dropbox/Duplicates"); len(h) != 1 {
		t.Errorf("a move between two users' synced home folders was not flagged: %+v", h)
	}
	// Without a "homes" share listed, the homes are found beside "home".
	areas = locate([]syncedFolder{{"/home", byCloudSync}}, []shareEntry{{name: "home", real: "/volume1/homes/ash"}})
	if h := syncHits(areas, nil, []string{"/volume1/homes/ash/x", "/volume1/homes/bob/y"}, "/volume1/Team"); len(h) != 1 || h[0].Files != 2 {
		t.Errorf("hits = %+v, want both homes matched", h)
	}
}

// Agreeing to move out of the synced folders shown covers those folders
// only: one that is synced now and was not shown — by another service, or
// newly — must be asked about first.
func TestSyncConsentCoversOnlyWhatWasShown(t *testing.T) {
	shown := []syncHit{{Folder: "/Photos", By: byCloudSync, Files: 2}}
	if !syncAgreedCovers(shown, []syncHit{{Folder: "/Photos", By: byCloudSync, Files: 5}}) {
		t.Fatal("the folder that was shown is not covered (file counts may change)")
	}
	if syncAgreedCovers(shown, []syncHit{{Folder: "/Photos", By: byCloudSync}, {Folder: "/Photos", By: byDrive}}) {
		t.Fatal("a second service on the same folder was covered without being shown")
	}
	if syncAgreedCovers(shown, []syncHit{{Folder: "/Work", By: byCloudSync}}) {
		t.Fatal("another folder was covered without being shown")
	}
	if syncAgreedCovers(nil, []syncHit{{Folder: "/Photos", By: byCloudSync}}) {
		t.Fatal("agreeing to nothing covered a synced folder")
	}
	if syncAgreedCovers(shown, []syncHit{{By: byDrive, Unchecked: true}}) {
		t.Fatal("a service that could not be checked was covered without being shown")
	}
}
