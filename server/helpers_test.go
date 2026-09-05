package main

// Test-only helpers.

// newKeyCounter builds a counter entitled to the whole memory budget.
func newKeyCounter() *keyCounter { return newKeyCounterShare(0) }
