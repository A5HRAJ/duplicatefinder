//go:build !linux && !darwin

package main

import "os"

// linkOf has no inode to read on this platform: every name counts as its own
// copy, which is the conservative reading.
func linkOf(os.FileInfo) fileLink { return fileLink{} }
