// Moved copies: telling a copy Duplicate Finder itself moved apart from an
// original. A move destination inside the scan scope is scanned like any
// other folder, so the next duplicates scan pairs every moved copy with the
// original it was moved away from. Treated as an ordinary copy, the moved
// one could be "the copy that stays" — keep-one would then let the original
// move too, and every copy would end up in folders that exist to be emptied.
package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// outFolderRe matches the name of a preserve-mode batch folder ("Duplicates",
// "Duplicates (2)", "Temporary Files" …), built from moveFolderNames so the
// two can never disagree.
var outFolderRe = func() *regexp.Regexp {
	var names []string
	for _, n := range moveFolderNames {
		names = append(names, regexp.QuoteMeta(n))
	}
	sort.Strings(names)
	return regexp.MustCompile(`^(` + strings.Join(names, "|") + `)( \(\d+\))?$`)
}()

// volumeCompRe matches a volume root's own name as preserve mode mirrors it
// inside a batch folder: Duplicates/volume1/…, Duplicates/volumeUSB1/….
var volumeCompRe = regexp.MustCompile(`^volume(USB|SATA)?[0-9]+$`)

// inBatchLayout reports whether p lies inside a preserve-mode batch folder,
// recognised by the layout every such folder has: a batch-folder name
// immediately followed by a volume name, with the moved entry somewhere
// below. This is what recognises the batch folders moves made before the
// daemon started recording them; a user's own folder that merely shares the
// name does not have a volume name inside it.
func inBatchLayout(p string) bool {
	parts := strings.Split(p, "/")
	for i := 0; i+2 < len(parts); i++ {
		if outFolderRe.MatchString(parts[i]) && volumeCompRe.MatchString(parts[i+1]) {
			return true
		}
	}
	return false
}

// moveOutSet is the record of where moves put things. Three kinds of entry
// share the one set, told apart by their form:
//
//   - a canonical path: a preserve-mode batch folder, or — for a flat move,
//     which lands among whatever the destination already held — each moved
//     entry's own final path, so files that were there before are not
//     mistaken for moved copies;
//   - "data:" + share + inode + size + modified time (dataKeyOf): every
//     regular file a move landed, recognised by the file itself, so renaming
//     or moving the folder it sits in — anywhere within its shared folder —
//     does not turn the copy back into an "original";
//   - "from:" + path: a path an entry was moved AWAY from. Whatever stands
//     there later — the user's own restore of the moved file, or something
//     new — is not a copy in a cleanup folder.
//
// Copy-on-write: a move publishes a NEW set under mu and never mutates a
// published one, so readers hold it without a lock.
type moveOutSet map[string]bool

const (
	dataKey = "data:"
	fromKey = "from:"
)

// shareRoot is the shared folder a canonical path lies in, "/volume1/Photos"
// for "/volume1/Photos/2020/a.jpg": the namespace an inode is unique within
// on Btrfs, where every shared folder is a subvolume of its own. "" for a
// path too short to name one.
func shareRoot(p string) string {
	parts := strings.SplitN(p, "/", 4)
	if len(parts) < 4 || parts[0] != "" || parts[1] == "" || parts[2] == "" {
		return ""
	}
	return "/" + parts[1] + "/" + parts[2]
}

// dataKeyOf identifies the regular file at canonical path p by its data: the
// shared folder, inode, size and modified time. A rename or a move within
// the shared folder keeps all four; an unrelated file matching all four by
// chance after the moved one was deleted is not a practical event. "" for
// anything but a regular file with a readable inode.
func dataKeyOf(p string, fi os.FileInfo) string {
	root := shareRoot(p)
	if root == "" || !fi.Mode().IsRegular() {
		return ""
	}
	l := linkOf(fi)
	if l.ino == 0 {
		return ""
	}
	return fmt.Sprintf("%s%s|%d|%d|%d", dataKey, root, l.ino, fi.Size(), fi.ModTime().Unix())
}

// landedData returns the data keys of the regular files a move just put at
// the canonical path at: the file itself, or every file of a moved folder.
func landedData(at string) []string {
	var out []string
	fi, err := os.Lstat(at)
	if err != nil {
		return nil
	}
	if !fi.IsDir() {
		if k := dataKeyOf(at, fi); k != "" {
			out = append(out, k)
		}
		return out
	}
	filepath.WalkDir(at, func(p string, d fs.DirEntry, err error) error {
		if err != nil || len(out) >= maxManifest {
			return nil
		}
		if d.Type().IsRegular() {
			if fi, ierr := d.Info(); ierr == nil {
				if k := dataKeyOf(p, fi); k != "" {
					out = append(out, k)
				}
			}
		}
		return nil
	})
	return out
}

// covers reports whether p is a recorded entry or lies inside one. It walks
// p's ancestors — tree depth, not the size of the record.
func (m moveOutSet) covers(p string) bool {
	if len(m) == 0 {
		return false
	}
	for q := p; q != "/" && q != "."; q = filepath.Dir(q) {
		if m[q] {
			return true
		}
	}
	return false
}

// isOut reports whether the canonical path p is a copy Duplicate Finder moved.
func (m moveOutSet) isOut(p string) bool {
	return inBatchLayout(p) || m.covers(p) || m.holdsMovedData(p)
}

// holdsMovedData reports whether the file at p is one a move landed, by its
// data rather than its path. Not at a path something was moved away from —
// a moved file put back where it came from is an original again — and not
// for a file with several names: its other names share the inode, and one
// of them may be the very place the moved name was taken from.
func (m moveOutSet) holdsMovedData(p string) bool {
	if len(m) == 0 || m[fromKey+p] {
		return false
	}
	fi, err := os.Lstat(p)
	if err != nil || linkOf(fi).n > 1 {
		return false
	}
	k := dataKeyOf(p, fi)
	return k != "" && m[k]
}

// with returns a new set holding m's entries plus add.
func (m moveOutSet) with(add []string) moveOutSet {
	out := make(moveOutSet, len(m)+len(add))
	for p := range m {
		out[p] = true
	}
	for _, p := range add {
		out[p] = true
	}
	return out
}

// rowIsOut is isOut for a result row, whose stored path is its display form:
// mapped into canonical space through the result's root table the same way
// refMatcher.protects does, and the raw form checked too.
func rowIsOut(m moveOutSet, roots []RootMap, dir, name string) bool {
	p := filepath.Join(dir, name)
	if m.isOut(p) {
		return true
	}
	for _, rt := range roots {
		if p == rt.Raw || strings.HasPrefix(p, rt.Raw+"/") {
			return m.isOut(rt.Canon + p[len(rt.Raw):])
		}
	}
	return false
}

// markGroupOut flags each duplicates row that is a moved copy, so the grid's
// Keep Newest / Keep Oldest never keeps one over an original. Rows are copied
// before flagging: the page's groups share their Files with the cached view.
func markGroupOut(groups []Group, m moveOutSet, roots []RootMap) {
	for i := range groups {
		files := append([]FileEnt(nil), groups[i].Files...)
		for j := range files {
			if rowIsOut(m, roots, files[j].Dir, files[j].Name) {
				files[j].Out = true
			}
		}
		groups[i].Files = files
	}
}
