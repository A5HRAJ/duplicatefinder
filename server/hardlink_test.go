package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Hard links are one copy under several names. A group holding two links to
// one file and one real copy has two physical copies: the rows say which
// names are links, the group says how many copies there are, and the
// reclaimable figure counts copies rather than names — removing one link of
// a file frees nothing.
func TestHardLinksCountAsOneCopy(t *testing.T) {
	dir := t.TempDir()
	body := []byte("the same twelve bytes, many times over ")
	when := time.Date(2024, 5, 6, 7, 8, 9, 0, time.Local)
	write := func(rel string) string {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a := write("a/file.bin")
	c := write("c/file.bin")
	b := filepath.Join(dir, "b", "file.bin")
	if err := os.MkdirAll(filepath.Dir(b), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(a, b); err != nil {
		t.Skipf("hard links unavailable here: %v", err)
	}
	var files []fEnt
	for _, p := range []string{a, b, c} {
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(dir, p)
		files = append(files, fEnt{path: p, name: fi.Name(), dir: filepath.Dir(p), size: fi.Size(),
			mod: fi.ModTime(), rel: rel, link: linkOf(fi)})
	}
	if files[0].link.n != 2 || files[0].link != files[1].link || files[2].link.n != 1 {
		t.Fatalf("link identity not read: %+v %+v %+v", files[0].link, files[1].link, files[2].link)
	}
	s := &Server{}
	acc := newGroupTop(dupFileCap)
	cancel := make(chan struct{})
	if !s.dupWindow(files, MatchOpts{}, &hashCache{ents: map[uint64]hcEnt{}, gen: 1}, cancel, acc, 0, 1) {
		t.Fatal("cancelled")
	}
	groups, _ := acc.final(s, dupFileCap, cancel)
	if len(groups) != 1 || len(groups[0].Files) != 3 {
		t.Fatalf("want one group of three names, got %+v", groups)
	}
	g := groups[0]
	if g.Copies != 2 {
		t.Errorf("two links and one copy are 2 physical copies, got %d", g.Copies)
	}
	linked := 0
	for _, f := range g.Files {
		if f.Links == 2 && f.Ino != "" {
			linked++
		} else if f.Links != 0 || f.Ino != "" {
			t.Errorf("the plain copy must carry no link fields: %+v", f)
		}
	}
	if linked != 2 {
		t.Errorf("want both link names marked, got %d", linked)
	}
	tot := dupTotals(groups, newRefMatcher(nil, nil))
	if tot.Reclaimable != g.Size {
		t.Errorf("reclaimable must count copies, not names: got %d, want %d", tot.Reclaimable, g.Size)
	}
	if w := acc.gs[0].weight(); w != g.Size {
		t.Errorf("group weight must count copies, not names: got %d, want %d", w, g.Size)
	}
}

// Names that are all hard links to one file are one copy, not duplicates:
// moving any of them frees nothing, so they form no group — while a real
// copy beside them still does.
func TestHardLinksAloneAreNotDuplicates(t *testing.T) {
	dir := t.TempDir()
	when := time.Date(2024, 5, 6, 7, 8, 9, 0, time.Local)
	write := func(rel, body string) string {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	link := func(from, rel string) string {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(from, p); err != nil {
			t.Skipf("hard links unavailable here: %v", err)
		}
		return p
	}
	solo := write("a/solo.bin", "one file, three names, no copies at all")
	paths := []string{solo, link(solo, "b/solo.bin"), link(solo, "c/solo.bin")}
	pair := write("a/pair.bin", "one file with a link and a real copy!!")
	paths = append(paths, link(pair, "b/pair.bin"), pair, write("c/pair.bin", "one file with a link and a real copy!!"))
	var files []fEnt
	for _, p := range paths {
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(dir, p)
		files = append(files, fEnt{path: p, name: fi.Name(), dir: filepath.Dir(p), size: fi.Size(),
			mod: fi.ModTime(), rel: rel, link: linkOf(fi)})
	}
	s := &Server{}
	acc := newGroupTop(dupFileCap)
	cancel := make(chan struct{})
	if !s.dupWindow(files, MatchOpts{}, &hashCache{ents: map[uint64]hcEnt{}, gen: 1}, cancel, acc, 0, 1) {
		t.Fatal("cancelled")
	}
	groups, _ := acc.final(s, dupFileCap, cancel)
	if len(groups) != 1 {
		t.Fatalf("want only the group with a real copy, got %d groups: %+v", len(groups), groups)
	}
	if n := len(groups[0].Files); n != 3 || groups[0].Files[0].Name != "pair.bin" || groups[0].Copies != 2 {
		t.Fatalf("want the three pair.bin names as two copies, got %+v", groups[0])
	}
}
