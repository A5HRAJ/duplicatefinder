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

// physOf is the number of bytes the file's data occupies on disk, which is
// what removing it gives back. st_blocks counts 512-byte units on Linux and
// Darwin alike. It is never reported above the file's size: a dense file's
// last block is rounded up, and the reclaimable figure is meant to agree
// with the Size column for ordinary files. Below it means a sparse file
// (a VM disk image, a preallocated download) or a compressed one (btrfs),
// whose apparent size says nothing about the space it holds.
//
// Not every filesystem allocates on write: with delayed allocation a file
// written moments before the scan can still show fewer blocks than it will
// have. That understates such a file until writeback, never overstates.
func physOf(fi os.FileInfo) int64 {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fi.Size()
	}
	p := int64(st.Blocks) * 512
	if p > fi.Size() || p < 0 {
		return fi.Size()
	}
	return p
}
