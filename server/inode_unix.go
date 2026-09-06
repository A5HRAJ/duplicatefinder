//go:build linux || darwin

package main

import (
	"os"
	"syscall"
)

// linkOf reads the identity of the data behind a directory entry: the device
// and inode, and how many names point at it. Two names with the same device
// and inode are one file — a hard link — and moving or removing one of them
// frees nothing while the other remains. The integer widths of Stat_t differ
// between architectures (Nlink is 64-bit on amd64, 32-bit on arm64 and arm,
// 16-bit on Darwin), hence the conversions.
func linkOf(fi os.FileInfo) fileLink {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileLink{}
	}
	return fileLink{dev: uint64(st.Dev), ino: uint64(st.Ino), n: uint32(st.Nlink)}
}
