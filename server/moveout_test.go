package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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

// A moved copy is recognised by the file itself, so renaming or reorganising
// the folder it was moved into does not make it an "original" again — while
// a moved file put back where it came from, a file edited since, a file with
// a second name and an unrelated file of the same content are not mistaken
// for one.
func TestMovedCopyIsRecognisedByItsData(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "dest")
	os.Mkdir(dest, 0o755)
	moved := filepath.Join(dest, "x.jpg")
	os.WriteFile(moved, []byte("same content"), 0o644)
	orig := filepath.Join(root, "x.jpg") // where it was moved from
	m := moveOutSet{}.with(append([]string{moved, fromKey + orig}, landedData(moved)...))
	if !m.isOut(moved) {
		t.Fatal("a just-moved copy must be out")
	}

	renamed := filepath.Join(root, "To review")
	if err := os.Rename(dest, renamed); err != nil {
		t.Fatal(err)
	}
	if !m.isOut(filepath.Join(renamed, "x.jpg")) {
		t.Fatal("renaming the folder made the moved copy look like an original")
	}
	deeper := filepath.Join(root, "Sorted", "2020")
	os.MkdirAll(deeper, 0o755)
	if err := os.Rename(filepath.Join(renamed, "x.jpg"), filepath.Join(deeper, "x.jpg")); err != nil {
		t.Fatal(err)
	}
	if !m.isOut(filepath.Join(deeper, "x.jpg")) {
		t.Fatal("moving the copy elsewhere in its shared folder made it look like an original")
	}

	other := filepath.Join(root, "other.jpg") // same bytes, same time, its own data
	os.WriteFile(other, []byte("same content"), 0o644)
	fi, _ := os.Stat(filepath.Join(deeper, "x.jpg"))
	os.Chtimes(other, fi.ModTime(), fi.ModTime())
	if m.isOut(other) {
		t.Fatal("an unrelated file of the same content was taken for the moved copy")
	}

	link := filepath.Join(root, "hardlink.jpg")
	if err := os.Link(filepath.Join(deeper, "x.jpg"), link); err == nil {
		if m.isOut(link) {
			t.Fatal("a second name of the data is not a copy this app moved")
		}
		os.Remove(link)
	}

	if err := os.Rename(filepath.Join(deeper, "x.jpg"), orig); err != nil {
		t.Fatal(err)
	}
	if m.isOut(orig) {
		t.Fatal("a moved file put back where it came from is an original again")
	}
	os.Rename(orig, filepath.Join(deeper, "x.jpg"))
	later := fi.ModTime().Add(time.Hour)
	os.Chtimes(filepath.Join(deeper, "x.jpg"), later, later)
	if m.isOut(filepath.Join(deeper, "x.jpg")) {
		t.Fatal("a moved copy the user has since edited is theirs, not a moved copy")
	}
}

// A moved folder's files are recorded one by one.
func TestLandedDataCoversAMovedFolder(t *testing.T) {
	root := t.TempDir()
	d := filepath.Join(root, "junk")
	os.Mkdir(d, 0o755)
	os.WriteFile(filepath.Join(d, "a.tmp"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(d, "b.bak"), []byte("b"), 0o644)
	m := moveOutSet{}.with(landedData(d))
	os.Rename(d, filepath.Join(root, "elsewhere"))
	if !m.isOut(filepath.Join(root, "elsewhere", "b.bak")) || !m.isOut(filepath.Join(root, "elsewhere", "a.tmp")) {
		t.Fatal("the files of a moved folder must stay recognised after the folder is renamed")
	}
}
