//go:build darwin

package main

import (
	"os"
	"syscall"
	"time"
)

// createdTime returns the file's birth time (macOS exposes it in stat).
// The path parameter is only needed by the Linux statx implementation.
// Used ONLY by the dev mock DSM (dev.go) as its crtime data source — the
// scanner never calls this: Created Dates come from File Station's API
// alone, with no native fallback.
func createdTime(_ string, fi os.FileInfo) time.Time {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		sec, nsec := st.Birthtimespec.Unix()
		return time.Unix(sec, nsec)
	}
	return fi.ModTime()
}

func diskUsage(path string) (total, used int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	total = int64(st.Blocks) * int64(st.Bsize)
	used = total - int64(st.Bavail)*int64(st.Bsize)
	return total, used
}
