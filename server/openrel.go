//go:build linux || darwin

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// openRelAt opens rel — a clean, slash-separated path relative to the
// directory open at rootFD — for reading, refusing to traverse any symlink:
// every component is opened via openat(2) with O_NOFOLLOW (intermediate
// components additionally O_DIRECTORY), so a directory swapped for a
// symlink after enumeration fails the open (ELOOP/ENOTDIR) instead of
// redirecting the read outside the pinned tree. rootFD is not consumed.
// Works on the DS916+'s 3.10 kernel, which predates openat2(2).
func openRelAt(rootFD int, rel string) (*os.File, error) {
	parts := strings.Split(filepath.Clean(rel), "/")
	fd := rootFD
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			if fd != rootFD {
				unix.Close(fd)
			}
			return nil, fmt.Errorf("invalid path component %q in %q", part, rel)
		}
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if i < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}
		nfd, err := unix.Openat(fd, part, flags, 0)
		if fd != rootFD {
			unix.Close(fd)
		}
		if err != nil {
			return nil, &os.PathError{Op: "openat", Path: part, Err: err}
		}
		fd = nfd
	}
	if fd == rootFD {
		return nil, fmt.Errorf("empty relative path")
	}
	return os.NewFile(uintptr(fd), rel), nil
}
