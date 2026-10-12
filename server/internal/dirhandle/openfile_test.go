//go:build linux || darwin

package dirhandle

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Opening a FIFO for reading blocks until something opens it for writing —
// a plain open of one never returned, and a move that tried to verify one
// held the daemon's move lock until the package was restarted. Both openers
// must refuse it at once instead.
func TestOpenRefusesFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for name, open := range map[string]func() (*os.File, error){
		"OpenFile": func() (*os.File, error) { return OpenFile(fifo) },
		"OpenRel":  func() (*os.File, error) { return h.OpenRel("pipe") },
	} {
		done := make(chan error, 1)
		go func() {
			f, err := open()
			if f != nil {
				f.Close()
			}
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, ErrNotRegular) {
				t.Errorf("%s(fifo): err = %v, want ErrNotRegular", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s(fifo) blocked — the open must never wait for a writer", name)
		}
	}
}

func TestOpenRefusesSocket(t *testing.T) {
	dir := t.TempDir()
	l, err := net.Listen("unix", filepath.Join(dir, "sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if f, err := OpenFile(filepath.Join(dir, "sock")); err == nil {
		f.Close()
		t.Fatal("a socket opened as a file")
	}
}

// A regular file opens as it always did: readable end to end, and handed
// over as a blocking descriptor — the O_NONBLOCK the open needed is cleared.
func TestOpenFileRegularReadsWhole(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.bin")
	want := make([]byte, 3<<20)
	for i := range want {
		want[i] = byte(i)
	}
	os.WriteFile(p, want, 0o644)
	h, _ := Open(dir)
	defer h.Close()
	for name, open := range map[string]func() (*os.File, error){
		"OpenFile": func() (*os.File, error) { return OpenFile(p) },
		"OpenRel":  func() (*os.File, error) { return h.OpenRel("f.bin") },
	} {
		f, err := open()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		fl, err := unix.FcntlInt(f.Fd(), unix.F_GETFL, 0)
		if err != nil || fl&unix.O_NONBLOCK != 0 {
			t.Errorf("%s: descriptor left non-blocking (flags %#x, %v)", name, fl, err)
		}
		got, err := io.ReadAll(f)
		f.Close()
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s: read %d bytes, err %v", name, len(got), err)
		}
	}
}
