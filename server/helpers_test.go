package main

import "os"

// Test-only helpers that production code no longer needs.

// newKeyCounter builds a counter entitled to the whole memory budget.
func newKeyCounter() *keyCounter { return newKeyCounterShare(0) }

func pathExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
