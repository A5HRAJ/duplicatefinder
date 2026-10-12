// Verified moves between filesystems: copy, prove the copy, and only then
// remove the original. File Station moves an entry to another filesystem by
// copying it and deleting the source in one task, so verification run after
// that task can only REPORT damage done in transit — by then the original is
// gone and the damaged copy is all there is. This flow keeps the original in
// place until a copy at the destination has been read back and matched, so
// verification decides whether the original is removed at all.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// crossesFS reports whether moving an entry out of the folder srcDir into the
// folder destDir leaves one filesystem for another: another volume, a USB
// disk, a remote folder mounted inside a share, or — on Btrfs, where every
// shared folder is its own subvolume — another shared folder. That is the
// move File Station performs as copy-then-delete. A path that cannot be
// examined counts as crossing: the careful flow is correct on one filesystem
// too, only slower. A variable so the tests can pick either flow.
var crossesFS = func(srcDir, destDir string) bool {
	vols := volumeRootsResolved()
	if volumeOf(srcDir, vols) != volumeOf(destDir, vols) {
		return true
	}
	a, err := os.Stat(srcDir)
	if err != nil {
		return true
	}
	b, err := os.Stat(destDir)
	if err != nil {
		return true
	}
	return linkOf(a).dev != linkOf(b).dev
}

// transitRef is what the copy at the destination must reproduce: a file's
// content hash, or a folder's manifest.
type transitRef struct {
	isDir bool
	hash  string
	man   manifest
}

// manEnt is one entry of a folder manifest. Directories never appear: the
// folders this flow moves are junk-only empty-folder rows, which hold files
// and Synology's @eaDir cache and nothing else.
type manEnt struct {
	typ   fs.FileMode // the type bits only
	size  int64
	mtime int64
	hash  string // regular files, when hashed
	link  string // symlinks
}

type manifest map[string]manEnt

// maxManifest bounds a folder manifest, as folderHoldsOnlyJunk bounds its
// listing: past it the folder is refused rather than half-checked.
const maxManifest = 10000

// takeManifest reads every entry of dir except @eaDir — Synology's thumbnail
// cache, regenerated on demand and not reliably carried by a copy — with each
// regular file's content hash when hashed is set. A subfolder is refused: a
// junk-only folder holds none, so one appearing means the folder gained
// content since it was confirmed empty.
func takeManifest(dir string, hashed bool) (manifest, error) {
	d, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	m := manifest{}
	for {
		ents, rerr := d.ReadDir(256)
		for _, e := range ents {
			if e.Name() == "@eaDir" {
				continue
			}
			if len(m) >= maxManifest {
				return nil, fmt.Errorf("the folder holds more than %d entries", maxManifest)
			}
			p := filepath.Join(dir, e.Name())
			fi, err := os.Lstat(p)
			if err != nil {
				return nil, err
			}
			me := manEnt{typ: fi.Mode().Type(), size: fi.Size(), mtime: fi.ModTime().Unix()}
			switch {
			case fi.IsDir():
				return nil, errors.New("the folder is no longer empty — it now holds a folder")
			case fi.Mode().IsRegular() && hashed:
				if me.hash, err = verifyRead(p); err != nil {
					return nil, err
				}
			case fi.Mode()&fs.ModeSymlink != 0:
				if me.link, err = os.Readlink(p); err != nil {
					return nil, err
				}
			}
			m[e.Name()] = me
		}
		if rerr == io.EOF {
			return m, nil
		}
		if rerr != nil {
			return nil, rerr
		}
	}
}

// copyMatches reports whether a copy's manifest reproduces the original's:
// the same names, and for each the same type and the same content (or link
// target). Modified times are not compared — a copy's are its own business.
func copyMatches(orig, cp manifest) bool {
	if len(orig) != len(cp) {
		return false
	}
	for name, o := range orig {
		c, ok := cp[name]
		if !ok || c.typ != o.typ || c.hash != o.hash || c.link != o.link {
			return false
		}
		if o.typ.IsRegular() && c.size != o.size {
			return false
		}
	}
	return true
}

// unchanged reports whether a folder still holds exactly what it held when
// its manifest was taken: the same names, types, sizes and modified times.
func unchanged(orig, now manifest) bool {
	if len(orig) != len(now) {
		return false
	}
	for name, o := range orig {
		n, ok := now[name]
		if !ok || n.typ != o.typ || n.size != o.size || n.mtime != o.mtime || n.link != o.link {
			return false
		}
	}
	return true
}

// hiddenName is a fresh dot-name for a folder or entry this flow parks out of
// sight: the scanner skips dot-names, so nothing parked is ever offered as a
// result.
func hiddenName(prefix string) (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(buf), nil
}

// untouched is the ending of every failure before the original was touched:
// whatever went wrong, the original is exactly where it was.
func untouched(why string) error {
	return errors.New(why + " — nothing was moved; the original is untouched")
}

// transitViaFS moves the entry at srcShare (filesystem path cp) into outShare
// (filesystem path outFS) across filesystems with verification:
//
//  1. COPY it into a fresh hidden staging folder at the destination.
//  2. READ THE COPY BACK and require ref — the content that left. A mismatch
//     discards the copy and leaves the original alone.
//  3. Put the copy in place (claimFromStaging: a rename on the destination's
//     filesystem, " (n)" on a name clash, never an overwrite).
//  4. Retire the original (retireOriginal).
//
// It returns the name the copy landed under whenever step 3 completed, even
// when step 4 then failed — the copy IS at the destination, and the caller
// records it as one this app put there. An error from steps 1–3 is a plain
// error and the original is untouched; from step 4 it is plain when the
// original is still at its path (both copies exist) and movedButError when it
// is not.
func transitViaFS(s *fsSession, cp, srcShare, outShare, outFS string, want entIdent, ref transitRef, note func(string)) (string, error) {
	say := func(stage string) {
		if note != nil {
			note(stage)
		}
	}
	base := filepath.Base(srcShare)
	tmp, err := hiddenName(".dupfinder-tmp-")
	if err != nil {
		return "", err
	}
	if err := s.createFolder(outShare, tmp, false); err != nil {
		return "", err
	}
	tmpShare := outShare + "/" + tmp
	tmpFS := filepath.Join(outFS, tmp)

	say(" — copying")
	if err := s.copyOnce(srcShare, tmpShare); err != nil {
		return "", s.discardCopy(tmpShare, srcShare, want, untouched("File Station could not copy it to the destination ("+err.Error()+")"))
	}
	say(" — verifying the copy")
	staged := filepath.Join(tmpFS, base)
	if ref.isDir {
		got, merr := takeManifest(staged, true)
		if merr != nil {
			return "", s.discardCopy(tmpShare, srcShare, want, untouched("the copy at the destination could not be read back to verify it"))
		}
		if !copyMatches(ref.man, got) {
			return "", s.discardCopy(tmpShare, srcShare, want, untouched("the copy at the destination does not match the original — the data was damaged in transit"))
		}
	} else {
		h, herr := verifyRead(staged)
		if herr != nil {
			return "", s.discardCopy(tmpShare, srcShare, want, untouched("the copy at the destination could not be read back to verify it — grant the package user read access to the destination share"))
		}
		if h != ref.hash {
			return "", s.discardCopy(tmpShare, srcShare, want, untouched("the copy at the destination does not match the original — the data was damaged in transit"))
		}
	}
	say("")

	// The copy's own identity is what the final hop looks for if its answer
	// is lost: a copied folder's modified time is the copy's, not the
	// original's, so the scan's record cannot recognise it.
	stagedWant := want
	if info, ierr := s.getInfo([]string{tmpShare + "/" + base}, []string{"size", "time"}); ierr == nil {
		if e, ok := info[tmpShare+"/"+base]; ok && e.exists() {
			stagedWant = entIdent{isDir: e.IsDir, size: e.Additional.Size, modUnix: e.Additional.Time.Mtime}
		}
	}
	name, err := s.claimFromStaging(tmpShare, base, outShare, stagedWant)
	if err != nil {
		var pe parkedError
		if errors.As(err, &pe) {
			return "", s.discardCopy(tmpShare, srcShare, want, untouched("the verified copy could not be put in place ("+pe.cause.Error()+")"))
		}
		// The final hop's answer was lost and the copy is in neither place
		// File Station was asked about. The original was never touched.
		return "", untouched("File Station's answer was lost while putting the verified copy in place — check " + outShare + " for it")
	}
	return name, s.retireOriginal(srcShare, cp, want, ref)
}

// discardCopy removes a staging folder whose copy is not going to be used,
// and returns why (the error to report). Only once the original is confirmed
// still in place exactly as the scan recorded it: the staging folder then
// holds nothing that exists nowhere else. Otherwise it is left — visible by
// path in the report — because it may be the only copy there is.
func (s *fsSession) discardCopy(tmpShare, srcShare string, want entIdent, why error) error {
	info, err := s.getInfo([]string{srcShare}, []string{"size", "time"})
	if err == nil {
		if e, ok := info[srcShare]; ok && e.exists() {
			if _, same := identMatches(e, []entIdent{want}); same {
				if derr := s.removeTree(tmpShare); derr == nil {
					return why
				}
				return fmt.Errorf("%v (the unused copy could not be removed from %s)", why, tmpShare)
			}
		}
	}
	return fmt.Errorf("%v (a copy was left in %s — the original could not be confirmed in place, so it was kept)", why, tmpShare)
}

// retireOriginal removes the original once its verified copy is in place.
// The original is first RENAMED to a hidden name beside itself — one atomic
// step that takes it out of reach of anything still addressing it by path,
// so nothing written there afterwards can be deleted with it — and only then
// re-checked: it must still be what was verified (a file: the scan's type,
// size and modified time; a folder: the same, plus every entry unchanged).
// Anything else puts it back under its own name and leaves both copies.
func (s *fsSession) retireOriginal(srcShare, cp string, want entIdent, ref transitRef) error {
	q, err := hiddenName(".dupfinder-del-")
	if err != nil {
		return keptBoth("a name to retire it under could not be made")
	}
	qShare := filepath.Dir(srcShare) + "/" + q
	qFS := filepath.Join(filepath.Dir(cp), q)
	if rerr := s.rename(srcShare, q); rerr != nil {
		// A lost answer may hide a rename that happened.
		there, _, serr := s.statShare(qShare)
		if !moveOutcomeUnknown(rerr) || serr != nil || !there {
			return keptBoth("File Station could not rename it (" + rerr.Error() + ")")
		}
	}
	same := false
	if info, ierr := s.getInfo([]string{qShare}, []string{"size", "time"}); ierr == nil {
		if e, ok := info[qShare]; ok && e.exists() {
			_, same = identMatches(e, []entIdent{want})
		}
	}
	if same && ref.isDir {
		now, merr := takeManifest(qFS, false)
		same = merr == nil && unchanged(ref.man, now)
	}
	if !same {
		return s.restoreOriginal(qShare, srcShare, "it changed while it was being moved")
	}
	var derr error
	if ref.isDir {
		derr = s.removeTree(qShare)
	} else {
		derr = s.removeEmptyFolder(qShare) // on a file: deletes the file
	}
	if derr != nil {
		if there, _, serr := s.statShare(qShare); serr == nil && !there {
			return nil // removed; only the answer was lost
		}
		return s.restoreOriginal(qShare, srcShare, "File Station could not delete it ("+derr.Error()+")")
	}
	return nil
}

// restoreOriginal puts a retired original back under its own name after the
// removal was called off. When even that fails, the original sits at its
// hidden name: the source path is empty, so the report says where it is and
// is the movedButError kind.
func (s *fsSession) restoreOriginal(qShare, srcShare, why string) error {
	if err := s.rename(qShare, filepath.Base(srcShare)); err == nil {
		return keptBoth(why)
	}
	if there, _, serr := s.statShare(srcShare); serr == nil && there {
		if gone, _, qerr := s.statShare(qShare); qerr == nil && !gone {
			return keptBoth(why) // renamed back; only the answer was lost
		}
	}
	return movedButError{"copied and verified, but the original was not removed (" + why + ") and could not be put back under its own name — it is at " + qShare + "; rename it back, or delete it once you have checked the copy"}
}

// keptBoth reports a verified copy in place with its original still where it
// was: nothing is lost, but there are now two copies.
func keptBoth(why string) error {
	return errors.New("copied and verified, but the original was left in place (" + why + ") — both copies now exist; delete one once you have checked them")
}
