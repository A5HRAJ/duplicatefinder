package main

import "testing"

// Only the batch-folder LAYOUT marks an unrecorded moved copy: a folder name
// alone is a user's own "Duplicates" folder until it holds a volume name.
func TestInBatchLayout(t *testing.T) {
	for p, want := range map[string]bool{
		"/volume1/Test/Duplicates/volume1/Backups/a.jpg":          true,
		"/volume1/Test/Duplicates (3)/volumeUSB1/usbshare/a.jpg":  true,
		"/volume1/x/Temporary Files/volume2/s/b.tmp":              true,
		"/volume1/Photos/Duplicates/a.jpg":                        false, // the user's own folder
		"/volume1/Duplicates/volume1":                             false, // nothing below the volume name
		"/volume1/Test/Duplicatesx/volume1/a":                     false,
		"/volume1/Test/Duplicates/Volume1/a":                      false,
		"/volume1/Test/My Duplicates/volume1/a":                   false,
		"/volume1/Test/Empty Folders (12)/volume1/Test/old/thing": true,
	} {
		if got := inBatchLayout(p); got != want {
			t.Errorf("inBatchLayout(%q) = %v, want %v", p, got, want)
		}
	}
	rec := moveOutSet{}.with([]string{"/volume1/dest/b.txt", "/volume1/out/Duplicates"})
	if !rec.isOut("/volume1/dest/b.txt") || rec.isOut("/volume1/dest/c.txt") ||
		!rec.isOut("/volume1/out/Duplicates/x/y") || rec.isOut("/volume1/dest") {
		t.Error("a flat move's record must cover exactly the entries it landed, a batch folder everything inside it")
	}
}

// While an original exists only originals count as the copy that stays; with
// none left on disk, every copy counts as it always did.
func TestKeepOneNeverLetsTheLastOriginalFollowItsMovedCopy(t *testing.T) {
	g := Group{Files: []FileEnt{{Dir: "/v/a", Name: "f"}, {Dir: "/v/out", Name: "f"}}}
	isOut := func(p string) bool { return p == "/v/out/f" }
	both := map[string]bool{"/v/a/f": true, "/v/out/f": true}
	orig := map[string]bool{"/v/a/f": true}

	drops, why := keepOneDropsWhy([]Group{g}, orig, nil, newDirResolver(), both, isOut)
	if !drops["/v/a/f"] || !why["/v/a/f"] {
		t.Fatalf("the original must be held back, with the moved-copy reason: %v %v", drops, why)
	}
	if d := keepOneDrops([]Group{g}, both, nil, newDirResolver(), both, isOut); len(d) != 1 || !d["/v/a/f"] {
		t.Fatalf("with both requested the ORIGINAL stays: %v", d)
	}
	if d := keepOneDrops([]Group{g}, map[string]bool{"/v/out/f": true}, nil, newDirResolver(), both, isOut); len(d) != 0 {
		t.Fatalf("the moved copy may move on while the original stays: %v", d)
	}
	onlyOut := map[string]bool{"/v/out/f": true}
	if d := keepOneDrops([]Group{g}, both, nil, newDirResolver(), onlyOut, isOut); len(d) != 1 || !d["/v/out/f"] {
		t.Fatalf("with the original gone, the moved copy is the one to keep: %v", d)
	}
}
