//go:build linux || darwin

package main

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The scan's idea of a file: regular files and directories, nothing else. A
// named pipe or a socket looks like a 0-byte file to stat; listing one as an
// Empty File offered it for deletion, and verifying one before a move
// blocked forever.
func TestScannableIsRegularFilesAndDirectories(t *testing.T) {
	// Not t.TempDir(): a Unix socket path is limited to ~100 bytes, and the
	// test's name would push one past it on macOS.
	dir, err := os.MkdirTemp("", "dfw")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x"), 0o644)
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	os.Symlink("file.txt", filepath.Join(dir, "link"))
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", filepath.Join(dir, "sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"file.txt": true, "sub": true, "link": false, "pipe": false, "sock": false}
	seen := 0
	for _, e := range ents {
		w, ok := want[e.Name()]
		if !ok {
			continue
		}
		seen++
		if got := scannable(e); got != w {
			t.Errorf("scannable(%s) = %v, want %v", e.Name(), got, w)
		}
	}
	if seen != len(want) {
		t.Fatalf("fixture incomplete: saw %d of %d entries", seen, len(want))
	}
}

// physOf reads what a file holds on disk: a sparse file holds less than its
// size, an ordinary file exactly its size (never more, block rounding aside).
func TestPhysOfSparseAndDense(t *testing.T) {
	dir := t.TempDir()
	sparse := filepath.Join(dir, "sparse.bin")
	f, err := os.Create(sparse)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(64 << 20); err != nil {
		t.Fatal(err)
	}
	f.Sync()
	f.Close()
	fi, _ := os.Stat(sparse)
	if p := physOf(fi); p >= fi.Size() {
		t.Fatalf("sparse file: physOf %d, size %d — expected less", p, fi.Size())
	}
	dense := filepath.Join(dir, "dense.bin")
	buf := make([]byte, 1<<20)
	for i := range buf {
		buf[i] = byte(i * 7)
	}
	g, _ := os.Create(dense)
	g.Write(buf)
	g.Sync()
	g.Close()
	fi, _ = os.Stat(dense)
	if p := physOf(fi); p != fi.Size() {
		t.Fatalf("dense file: physOf %d, size %d — expected equal", p, fi.Size())
	}
}

func TestReclaimMeasuredOnDisk(t *testing.T) {
	const sz = 1 << 30
	dense := func() fEnt { return fEnt{size: sz} }
	sparse := func() fEnt { return fEnt{size: sz, holes: sz} }
	cases := []struct {
		name  string
		files []fEnt
		want  int64
	}{
		{"two dense copies", []fEnt{dense(), dense()}, sz},
		{"three dense copies", []fEnt{dense(), dense(), dense()}, 2 * sz},
		{"two wholly sparse copies free nothing", []fEnt{sparse(), sparse()}, 0},
		{"dense beside sparse promises only what is certain", []fEnt{dense(), sparse()}, 0},
		{"half-sparse pair", []fEnt{{size: sz, holes: sz / 2}, {size: sz, holes: sz / 2}}, sz / 2},
		{"hard links are one copy", []fEnt{
			{size: sz, link: fileLink{dev: 1, ino: 9, n: 2}}, {size: sz, link: fileLink{dev: 1, ino: 9, n: 2}}, dense()}, sz},
	}
	for _, c := range cases {
		if got := reclaimPhys(copyPhys(c.files)); got != c.want {
			t.Errorf("%s: reclaim %d, want %d", c.name, got, c.want)
		}
	}
	// The same cases through the served-row arithmetic.
	row := func(alloc int64, ino string) FileEnt {
		fe := FileEnt{Size: sz, Ino: ino}
		if alloc < sz {
			fe.Alloc = &alloc
		}
		return fe
	}
	if got := groupReclaim(sz, []FileEnt{row(sz, ""), row(sz, "")}, nil); got != sz {
		t.Errorf("rows, two dense: %d", got)
	}
	if got := groupReclaim(sz, []FileEnt{row(0, ""), row(0, "")}, nil); got != 0 {
		t.Errorf("rows, two sparse: %d", got)
	}
	if got := groupReclaim(sz, []FileEnt{row(sz, ""), row(0, "")}, nil); got != 0 {
		t.Errorf("rows, dense beside sparse: %d", got)
	}
	if got := groupReclaim(sz, []FileEnt{row(sz, "1:9"), row(sz, "1:9"), row(sz, "")}, nil); got != sz {
		t.Errorf("rows, hard links: %d", got)
	}
	// A protected copy is the one kept, whatever its size.
	prot := func(f *FileEnt) bool { return f.Prot }
	kept := row(0, "")
	kept.Prot = true
	if got := groupReclaim(sz, []FileEnt{kept, row(sz, ""), row(sz, "")}, prot); got != 2*sz {
		t.Errorf("rows, protected sparse kept, two dense go: %d", got)
	}
	linkA, linkB := row(sz, "1:9"), row(sz, "1:9")
	linkB.Prot = true // protecting either name keeps the one copy
	if got := groupReclaim(sz, []FileEnt{linkA, linkB, row(sz, "")}, prot); got != sz {
		t.Errorf("rows, protected hard link: %d", got)
	}
}
