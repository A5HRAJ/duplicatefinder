// Moved copies: telling a copy Duplicate Finder itself moved apart from an
// original. A move destination inside the scan scope is scanned like any
// other folder, so the next duplicates scan pairs every moved copy with the
// original it was moved away from. Treated as an ordinary copy, the moved
// one could be "the copy that stays" — keep-one would then let the original
// move too, and every copy would end up in folders that exist to be emptied.
package main

import (
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

// moveOutSet is the record of where moves put things, as canonical paths: a
// preserve-mode batch folder, or — for a flat move, which lands among
// whatever the destination already held — each moved entry's own final
// path, so files that were there before are not mistaken for moved copies.
// Copy-on-write: a move publishes a NEW set under mu and never mutates a
// published one, so readers hold it without a lock.
type moveOutSet map[string]bool

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
	return inBatchLayout(p) || m.covers(p)
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
