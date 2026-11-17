//go:build linux

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// dropCached tells the kernel that the pages of a file just read in full
// will not be needed again. A scan reads every candidate end to end — on a
// NAS with 1 GB of memory, gigabytes of it — and left in the page cache that
// data pushed out everything else: DSM's own working set and this package's
// program file, which the web server runs once per request. Measured on a
// DS124 (DSM 7.4.1) during a 4 GB scan: the app's status replies took a
// median 3 s and up to 14 s, one start of the program 2.3 s. Only clean
// pages are dropped; the hint changes no data and an error changes nothing.
func dropCached(f *os.File) {
	_ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
}
