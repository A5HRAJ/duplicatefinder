//go:build darwin && dev

// Native stat helpers for the dev mock DSM (dev.go); see stat_linux.go.
package main

import (
	"os"
	"syscall"
	"time"
)

// createdTime returns the file's birth time (macOS exposes it in stat).
// The path parameter is only needed by the Linux statx implementation.
func createdTime(_ string, fi os.FileInfo) time.Time {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		sec, nsec := st.Birthtimespec.Unix()
		return time.Unix(sec, nsec)
	}
	return fi.ModTime()
}

// diskUsage returns the size and used bytes of the filesystem holding path.
func diskUsage(path string) (total, used int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	total = int64(st.Blocks) * int64(st.Bsize)
	used = total - int64(st.Bavail)*int64(st.Bsize)
	return total, used
}
