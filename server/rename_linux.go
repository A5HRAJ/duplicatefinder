//go:build linux

package main

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames src to dst, failing with syscall.EEXIST if dst
// already exists. The no-overwrite guarantee is kernel-enforced
// (RENAME_NOREPLACE), so there is no window in which a concurrently created
// dst could be clobbered. Cross-volume renames fail with syscall.EXDEV.
// Filesystems that do not implement renameat2 flags report EINVAL/ENOSYS/
// ENOTSUP; those fall back to the best-effort emulation.
func renameNoReplace(src, dst string) error {
	err := unix.Renameat2(unix.AT_FDCWD, src, unix.AT_FDCWD, dst, unix.RENAME_NOREPLACE)
	if err == syscall.EINVAL || err == syscall.ENOSYS || err == syscall.ENOTSUP {
		return renameNoReplaceFallback(src, dst)
	}
	return err
}
