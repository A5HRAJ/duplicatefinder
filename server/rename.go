package main

import (
	"os"
	"syscall"
)

// renameNoReplaceFallback emulates no-replace rename semantics with an Lstat
// check followed by os.Rename. Unlike the Linux implementation this leaves a
// window between check and rename; it is used only where the kernel primitive
// is unavailable (non-Linux dev builds, filesystems without renameat2 flags).
func renameNoReplaceFallback(src, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		return syscall.EEXIST
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(src, dst)
}
