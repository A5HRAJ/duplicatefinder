//go:build linux && dev

// Native stat helpers for the dev mock DSM (dev.go), which stands in for File
// Station and so needs a native source for creation times and disk usage.
// Release builds never read either natively: Created Dates come from File
// Station's API alone, and the volume overview from its share listing.
package main

import (
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// createdTime returns the file's creation (birth) time via statx, falling
// back to the inode change time.
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
