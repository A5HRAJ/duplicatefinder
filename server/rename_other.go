//go:build !linux && !darwin

package main

// renameNoReplace best-effort fallback for platforms without a native
// no-replace rename. Release binaries are Linux; this exists only so the
// package still builds elsewhere.
func renameNoReplace(src, dst string) error {
	return renameNoReplaceFallback(src, dst)
}
