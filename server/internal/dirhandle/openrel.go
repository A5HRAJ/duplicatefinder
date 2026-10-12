//go:build linux || darwin

package dirhandle

import (
	"errors"
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
// Works on DSM's 3.10 kernels, which predate openat2(2).
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
		} else {
			flags |= unix.O_NONBLOCK // see regularFile
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
	return regularFile(fd, rel)
}

// ErrNotRegular is returned for an entry that is not a regular file: a named
// pipe, a socket or a device node. None has content a scan could compare.
var ErrNotRegular = errors.New("not a regular file")

// regularFile wraps an fd opened with O_NONBLOCK as a *os.File, refusing
// anything but a regular file. The non-blocking open is what makes the
// refusal possible: opening a FIFO for reading blocks until a writer opens
// it, so a plain open of one never returns and nothing after it can run —
// a move that verified one held the move lock until the package was
// restarted. With O_NONBLOCK the open returns at once, the fd can be
// examined, and a regular file has the flag cleared again before use, so
// it reads exactly as a blocking open would have. fd is always consumed.
func regularFile(fd int, name string) (*os.File, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, &os.PathError{Op: "fstat", Path: name, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(fd)
		return nil, &os.PathError{Op: "open", Path: name, Err: ErrNotRegular}
	}
	fl, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err == nil {
		_, err = unix.FcntlInt(uintptr(fd), unix.F_SETFL, fl&^unix.O_NONBLOCK)
	}
	if err != nil {
		unix.Close(fd)
		return nil, &os.PathError{Op: "fcntl", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), name), nil
}

// OpenFile opens an absolute path for reading with the same guarantee as
// OpenRel — a regular file or an error, never a blocking open — following
// symlinks as os.Open would. For the paths the move flow reads after it has
// vetted them.
func OpenFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return regularFile(fd, path)
}
