//go:build linux

package main

import (
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// createdTime returns the file's creation (birth) time via statx, falling
// back to the inode change time. Used ONLY by the dev mock DSM (dev.go) as
// its crtime data source — the mock stands in for DSM itself, so a native
// read is the point. The scanner never calls this: Created Dates come from
// File Station's API alone, with no native fallback.
func createdTime(path string, fi os.FileInfo) time.Time {
	var stx unix.Statx_t
	err := unix.Statx(unix.AT_FDCWD, path,
		unix.AT_SYMLINK_NOFOLLOW|unix.AT_STATX_DONT_SYNC, unix.STATX_BTIME, &stx)
	if err == nil && stx.Mask&unix.STATX_BTIME != 0 && stx.Btime.Sec != 0 {
		return time.Unix(stx.Btime.Sec, int64(stx.Btime.Nsec))
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		sec, nsec := st.Ctim.Unix()
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
