// Path vetting: the volume roots that bound every scan and move, the
// string-level and canonical containment checks, and the per-request
// resolver that compares client, cached and reference paths in one
// symlink-resolved namespace.
package main

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// listVolumes returns the Synology volume roots. In dev builds devVolumeRoots
// can override this (see dev.go); it returns nil in release builds.
//
// External devices are included deliberately: USB and eSATA volumes mount at
// /volumeUSB<n> and /volumeSATA<n>, and moving unwanted files onto an
// external disk is a primary use of the move flow. These globs are the
// security walls every vetted path must stay inside, so widening them widens
// what scans may walk and moves may target — which is exactly the intent.
func listVolumes() []string {
	if roots := devVolumeRoots(); roots != nil {
		return roots
	}
	var matches []string
	for _, pat := range []string{"/volume[0-9]*", "/volumeUSB[0-9]*", "/volumeSATA[0-9]*"} {
		m, _ := filepath.Glob(pat)
		matches = append(matches, m...)
	}
	var out []string
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.IsDir() {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

func allowedPath(p string) bool {
	if p == "" || !filepath.IsAbs(p) {
		return false
	}
	// Clean FIRST, then reject ".." as a path COMPONENT — never as a
	// substring. A substring test would refuse every legitimate name
	// containing an ellipsis or a double dot ("Wait... What.mp4",
	// "Season 1..2"): such a file would be listed by a scan and then
	// refused at move time as "outside allowed volumes", and no folder
	// named that way could be a destination or a scan root. Traversal is
	// still impossible: Clean resolves interior ".." on an absolute path
	// (so "/volume1/../etc" becomes "/etc"), the volume-prefix test below
	// then rejects it, and every caller pairs this with a canonical
	// containment check taken from a pinned directory handle.
	p = filepath.Clean(p)
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return false
		}
	}
	for _, v := range listVolumes() {
		if p == v || strings.HasPrefix(p, v+"/") {
			return true
		}
	}
	return false
}

// volumeRootsResolved returns each allowed volume root in raw and
// symlink-resolved form. Canonical (resolved) paths must be compared against
// the resolved roots too: the roots themselves may sit behind symlinks (dev
// fixtures under macOS /var → /private/var).
func volumeRootsResolved() []string {
	var out []string
	for _, v := range listVolumes() {
		out = append(out, v)
		if rv, err := filepath.EvalSymlinks(v); err == nil && rv != v {
			out = append(out, rv)
		}
	}
	return out
}

// resolveAllowedRoot returns p's symlink-resolved form when p exists and
// stays inside the allowed volumes after resolution. allowedPath alone is a
// string check — a symlink under a volume could point anywhere, and the
// scanner must not follow it outside. The two failures are distinguished so
// callers report the right one: a path that cannot be resolved is "not a
// folder"; a path that resolves outside is a containment refusal.
func resolveAllowedRoot(p string) (string, error) {
	if !allowedPath(p) {
		return "", errors.New("path outside allowed volumes: " + p)
	}
	rp, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", errors.New("not a folder: " + p)
	}
	if !isUnder(rp, volumeRootsResolved()) {
		return "", errors.New("path outside allowed volumes: " + p)
	}
	return rp, nil
}

// dirResolver canonicalizes paths by symlink-resolving their directory part,
// memoized per request. Client-supplied paths, cached scan paths, and
// reference dirs are all compared in this one canonical namespace so a
// symlink alias cannot slip past the prefix-based invariant checks.
type dirResolver struct{ cache map[string]string }

func newDirResolver() *dirResolver { return &dirResolver{cache: map[string]string{}} }

func (r *dirResolver) resolveDir(d string) (string, error) {
	d = filepath.Clean(d)
	if v, ok := r.cache[d]; ok {
		return v, nil
	}
	v, err := filepath.EvalSymlinks(d)
	if err != nil {
		return "", err // failures are not cached: only real resolutions are
	}
	r.cache[d] = v
	return v, nil
}

// dir canonicalizes a directory, falling back to the cleaned input when it
// cannot be resolved, so already-moved/missing paths still compare stably.
func (r *dirResolver) dir(d string) string {
	if v, err := r.resolveDir(d); err == nil {
		return v
	}
	return filepath.Clean(d)
}

// path canonicalizes a file path leniently: resolved parent + original base.
// The base is never resolved — the entry itself may be a symlink that is
// moved as a link, not followed.
func (r *dirResolver) path(p string) string {
	p = filepath.Clean(p)
	return filepath.Join(r.dir(filepath.Dir(p)), filepath.Base(p))
}

// strictPath is path for paths that must be acted on: it fails when the
// parent directory cannot be resolved instead of falling back.
func (r *dirResolver) strictPath(p string) (string, error) {
	p = filepath.Clean(p)
	d, err := r.resolveDir(filepath.Dir(p))
	if err != nil {
		return "", err
	}
	return filepath.Join(d, filepath.Base(p)), nil
}

// skipName filters out Synology/system directories and hidden entries that
// should never be scanned or offered in pickers.
func skipName(name string) bool {
	return strings.HasPrefix(name, "@") || strings.HasPrefix(name, ".") ||
		name == "#recycle" || name == "#snapshot" || name == "lost+found"
}

func uniquePaths(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range in {
		p = filepath.Clean(p)
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func isUnder(p string, roots []string) bool {
	for _, r := range roots {
		if p == r || strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04:05")
}
