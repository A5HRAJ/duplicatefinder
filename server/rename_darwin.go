//go:build darwin

package main

// renameNoReplace on macOS: x/sys v0.15.0 does not expose Renameatx_np /
// RENAME_EXCL, so use the Lstat+rename emulation. macOS builds are dev-only;
// release binaries are Linux and get the kernel-atomic version.
func renameNoReplace(src, dst string) error {
	return renameNoReplaceFallback(src, dst)
}
