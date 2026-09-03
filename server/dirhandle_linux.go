//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"
)

// dirHandle pins an open directory object for the read-side safety checks
// (path vetting, scan-root containment). Path() addresses the directory
// through /proc/self/fd, so checks act on the pinned object even if the
// original path is renamed or swapped for a symlink after validation.
// Callers must Close() only after every Path()-derived string has been
// used. All file mutations are delegated to DSM's File Station API and do
// not go through handles.
type dirHandle struct {
	f *os.File
}

// openDirHandle opens path as a directory, following symlinks: the caller
// decides whether the object is acceptable from Canon(), the pinned truth.
func openDirHandle(path string) (*dirHandle, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	return &dirHandle{f: f}, nil
}

func (h *dirHandle) Path() string {
	return fmt.Sprintf("/proc/self/fd/%d", h.f.Fd())
}

// Canon returns the kernel's current canonical path for the pinned directory
// — derived from the object itself, not from re-walking the user-supplied
// path, so it cannot be spoofed by concurrent renames.
func (h *dirHandle) Canon() (string, error) {
	return os.Readlink(h.Path())
}

func (h *dirHandle) Close() error { return h.f.Close() }

// OpenRel opens rel (relative to the pinned directory) for reading without
// traversing any symlink between the pinned root and the file — see
// openRelAt. Safe for concurrent use; the handle must stay open for the
// duration of the call.
func (h *dirHandle) OpenRel(rel string) (*os.File, error) {
	return openRelAt(int(h.f.Fd()), rel)
}
