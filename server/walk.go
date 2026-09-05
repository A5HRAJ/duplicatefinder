// The streaming directory walk every scan consumes: pinned roots, symlinks
// skipped, unreadable locations reported, nothing materialized.
package main

import (
	"dupfinder/internal/dirhandle"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// walkStream enumerates entries under each root, handing every one to visit
// instead of materializing the tree in memory: what a scan keeps is decided
// by its accumulator, so daemon memory does not grow with the volume. When
// recurse is false only the direct children of each
// root are visited; directories are visited only when withDirs is true
// (empty-folder scan). The unreadable result lists paths whose contents
// could not be read (permission denied etc.) — such directories have
// unknown contents and must never be treated as empty. The returned handles
// are the pinned roots (rootOf holds each handle's raw root path, and visit
// receives the handle's index); they stay open so later content reads go
// through them, and the caller closes them when the scan is done. Roots
// must already be de-overlapped (runScan) — there is no seen-set here.
func (s *Server) walkStream(roots []string, recurse, withDirs bool, cancel chan struct{}, visit func(rootIdx int, f fEnt), unreadable func(path string)) (errs []string, handles []*dirhandle.Handle, rootOf []string) {
	count := 0
	// The error list is capped so a volume full of permission-denied
	// subtrees cannot grow it without bound — but no cap here is silent, so
	// the overflow is counted and reported as one closing line.
	dropped := 0
	addErr := func(e string) {
		if len(errs) < 50 {
			errs = append(errs, e)
		} else {
			dropped++
		}
	}
	defer func() {
		if dropped > 0 {
			errs = append(errs, fmt.Sprintf("… and %d more locations could not be read", dropped))
		}
	}()
	// Only the empty-folder scan takes the unreadable callback (a directory
	// whose contents cannot be read must never be called empty), and it takes
	// it INTERLEAVED with the entries: its accumulator tracks where the walk
	// currently is, so an unreadable path has to arrive in walk order rather
	// than in a list afterwards.
	noteUnreadable := func(p string) {
		if unreadable != nil {
			unreadable(p)
		}
	}
	// path is the user-facing location stored in results; rel/rh address the
	// same entry through its pinned root for later content reads. created is
	// left zero on purpose: Created Dates come from File Station only (see
	// applyCrtimes) — never from a native statx/ctime approximation.
	emit := func(rootIdx int, path, rel string, rh *dirhandle.Handle, d fs.DirEntry) {
		info, err := d.Info()
		if err != nil {
			return
		}
		visit(rootIdx, fEnt{
			path: path, name: d.Name(), dir: filepath.Dir(path),
			size: info.Size(), mod: info.ModTime(),
			isDir: d.IsDir(), rel: rel, rh: rh,
		})
		count++
		if count%2000 == 0 {
			// The total is unknown until the walk ends, so the bar approaches
			// the end of this phase asymptotically rather than sitting still:
			// a bar that has not moved in an hour reads as a hung scan.
			frac := float64(count) / float64(count+2000000)
			s.setProgress(2+13*frac, "Collecting file metadata… ("+humanCount(count)+" items)")
		}
	}

	vols := volumeRootsResolved()
	for _, root := range roots {
		if cancelled(cancel) {
			break
		}
		// Label only (-1 keeps the current percentage): the 2–15 band belongs
		// to emit's count-based tick, which is monotonic across all roots.
		// Writing a root-proportional percentage here as well put two
		// formulas on one band — each new root jumped the bar forward and the
		// next tick pulled it back, the same "scan restarted itself" reading
		// the finalize path is careful to avoid.
		s.setProgress(-1, "Collecting file metadata…")
		// Pin the root and re-check containment from the pinned object: the
		// enumeration below goes through the handle, so a root swapped for
		// an outside-pointing symlink after validation is never followed.
		h, err := dirhandle.Open(root)
		if err != nil {
			addErr(root + ": " + err.Error())
			noteUnreadable(root)
			continue
		}
		canon, cerr := h.Canon()
		if cerr != nil || !isUnder(canon, vols) {
			h.Close()
			addErr(root + ": no longer inside the allowed volumes")
			noteUnreadable(root)
			continue
		}
		// The handle stays open past enumeration: content reads (hashing,
		// EXIF) open entries through it, so it is closed by the caller
		// only when the whole scan is finished.
		handles = append(handles, h)
		rootOf = append(rootOf, root)
		idx := len(handles) - 1
		scrub := map[string]string{h.Path(): root}
		if !recurse {
			ents, err := os.ReadDir(h.Path())
			if err != nil {
				addErr(root + ": " + scrubErr(err, scrub).Error())
				noteUnreadable(root)
				continue
			}
			for _, e := range ents {
				if e.Type()&fs.ModeSymlink != 0 {
					continue
				}
				if e.IsDir() {
					if withDirs && !skipName(e.Name()) {
						emit(idx, filepath.Join(root, e.Name()), e.Name(), h, e)
					}
					continue
				}
				emit(idx, filepath.Join(root, e.Name()), e.Name(), h, e)
			}
			continue
		}
		// Walk through the pinned root (DirFS: WalkDir itself would Lstat
		// the proc magic link), storing display paths under the raw root.
		walkErr := fs.WalkDir(os.DirFS(h.Path()), ".", func(rel string, d fs.DirEntry, err error) error {
			if cancelled(cancel) {
				return fs.SkipAll
			}
			display := root
			if rel != "." {
				display = filepath.Join(root, rel)
			}
			if err != nil {
				addErr(display + ": " + scrubErr(err, scrub).Error())
				noteUnreadable(display)
				return nil
			}
			if d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			if d.IsDir() {
				if rel != "." && skipName(d.Name()) {
					return fs.SkipDir
				}
				if withDirs && rel != "." {
					emit(idx, display, rel, h, d)
				}
				return nil
			}
			emit(idx, display, rel, h, d)
			return nil
		})
		if walkErr != nil {
			addErr(root + ": " + scrubErr(walkErr, scrub).Error())
			noteUnreadable(root)
		}
	}
	return errs, handles, rootOf
}
