//go:build !linux

package main

import "os"

// dropCached is a no-op off Linux: the daemon only runs on DSM, and the
// development hosts have no page-cache hint worth giving.
func dropCached(*os.File) {}
