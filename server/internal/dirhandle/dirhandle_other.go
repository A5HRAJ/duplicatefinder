//go:build !linux

package dirhandle

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Handle on non-Linux: no fd pinning (release binaries are Linux; dev
// builds get best-effort semantics). Path() is the original path and
// Canon() re-resolves it — dev-only, acceptable. Open follows symlinks like
// the Linux form.
type Handle struct {
	path string
}

func Open(path string) (*Handle, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", path)
	}
	return &Handle{path: filepath.Clean(path)}, nil
}

func (h *Handle) Path() string { return h.path }

func (h *Handle) Canon() (string, error) { return filepath.EvalSymlinks(h.path) }

func (h *Handle) Close() error { return nil }

// OpenRel opens rel for reading without traversing symlinks below the root
// (see openRelAt). The root itself is reopened by path — following symlinks,
// like openDirHandle — so root pinning stays best-effort here; dev-only,
// acceptable (release binaries are Linux and pin the root fd).
func (h *Handle) OpenRel(rel string) (*os.File, error) {
	rootFD, err := unix.Open(h.path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(rootFD)
	return openRelAt(rootFD, rel)
}
