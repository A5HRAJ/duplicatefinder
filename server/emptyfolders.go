// The empty-folders pass: the streaming topmost-empty rule over the walk,
// and the File Station confirmation every candidate must pass.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// confirmEmpty reports whether a candidate folder holds nothing the user
// could miss, per File Station's own listing (see folderHoldsOnlyJunk):
// zero entries, or entries that are all junk — files the Temporary Files
// tool itself would list, plus Synology's @eaDir thumbnail cache. Any other
// entry counts as "not empty": this gate must never over-report emptiness.
// An error is returned rather than folded into "not empty", because the
// caller has to REPORT it: a File Station outage mid-confirmation must read
// as "these could not be checked", not as "these were not empty". Candidates
// always sit inside a shared folder — scan roots must, and candidates lie
// strictly below the roots — so share space can always address them.
func confirmEmpty(p string, sess *fsSession) (bool, error) {
	sp, err := sess.shareSpacePath(p)
	if err != nil {
		return false, err
	}
	if ok, err := sess.folderHoldsOnlyJunk(sp); err != nil || !ok {
		return ok, err
	}
	// Then the daemon's own reading of the directory. It sees every name
	// whatever File Station's listing chooses to show, so a hidden directory
	// such as .git — content the walk skipped over — rejects the folder even
	// on a DSM whose listing omitted it. Stricter only: a name seen here can
	// make the folder non-empty, never empty.
	return dirHoldsOnlyJunk(p)
}

// dirHoldsOnlyJunk is confirmEmpty's native half: every entry of the
// directory must be junk (isTempName) or Synology's @eaDir cache. It fails
// closed — a directory that cannot be read has unknown contents.
func dirHoldsOnlyJunk(p string) (bool, error) {
	d, err := os.Open(p)
	if err != nil {
		return false, err
	}
	defer d.Close()
	for {
		names, err := d.Readdirnames(256)
		for _, n := range names {
			if n != "@eaDir" && !isTempName(n) {
				return false, nil
			}
		}
		if err == io.EOF {
			return true, nil
		}
		if err != nil {
			return false, err
		}
	}
}

// emptyFolderScan turns the walk's entry stream into topmost-empty-folder
// candidates without ever holding the directory tree.
//
// "Empty" means the folder holds nothing the user could miss: zero entries,
// or nothing but junk files — the Temporary Files tool's own name list
// (visit skips those when marking hasFile; the junk rides along when the
// folder is moved). The walk finds the TOPMOST directory whose subtree
// holds no real file, and only that one is offered; a candidate is reported
// only after the confirm callback (File Station's own listing in production
// — see confirmEmpty) verifies every entry it can see is junk too. The
// walker skips hidden/system DIRECTORIES (".*", "@eaDir", "#recycle", …),
// so a candidate may hold one — confirmation accepts @eaDir (Synology's
// thumbnail cache, regenerated on demand) and rejects every other
// directory: a folder whose only child is .git is a repository, not
// clutter, and a folder holding just an empty folder reports neither —
// data-safe for a tool that must never cause loss. Directories whose
// contents could not be read have unknown contents
// — e.g. a Hyper Backup .hbk vault the package user cannot open — so they
// and their ancestors count as non-empty rather than being offered for
// moving.
//
// Maps of every directory, every file-holding directory and every ancestor
// mark would grow with the filesystem — millions of entries on a large
// volume. This keeps a frame per OPEN directory instead — tree depth, not
// tree size — because fs.WalkDir hands entries out depth-first: a
// directory's fate is decided when the walk leaves it, and leaving is
// visible as the first entry that is no longer underneath it.
type emptyFolderScan struct {
	stack   []efFrame
	top     *boundedTop // topmost-empty candidates, first emptyFolderCap by path
	dropped int         // candidates dropped by a frame's own cap
	// Which scan root the frames on the stack belong to. runScan only
	// de-overlaps nested roots when the scan is RECURSIVE, so a
	// non-recursive scope legitimately walks both /R and /R/D — and a
	// reference folder inside a scanned folder is exactly that shape,
	// since handleScan appends refDirs to the roots.
	curRoot int
}

// efFrame is one directory the walk is currently inside. empties holds its
// child directories whose subtrees turned out to hold no file: whether they
// are TOPMOST is not known until this directory's own fate is (if this one
// also holds no file, it supersedes them), so they wait here. They are
// flushed the moment a file shows up, which is why the list is normally
// empty — it only ever holds what was seen before the first file.
type efFrame struct {
	path    string
	size    int64
	mod     time.Time
	isRoot  bool
	hasFile bool
	empties []dirEnt
	dropped int
}

func newEmptyFolderScan() *emptyFolderScan {
	return &emptyFolderScan{
		top:     newBoundedTop(emptyFolderCap, func(a, b *fEnt) bool { return a.path < b.path }),
		curRoot: -1,
	}
}

func (e *emptyFolderScan) emit(d dirEnt) {
	e.top.add(fEnt{
		path: d.path, name: filepath.Base(d.path), dir: filepath.Dir(d.path),
		size: d.size, mod: d.mod, isDir: true,
	})
}

// markHasFile records that a directory's subtree holds content, and releases
// the empty children it was holding back — they are topmost now.
func (e *emptyFolderScan) markHasFile(f *efFrame) {
	if f.hasFile {
		return
	}
	f.hasFile = true
	for _, d := range f.empties {
		e.emit(d)
	}
	e.dropped += f.dropped
	f.empties, f.dropped = nil, 0
}

// addEmpty hands an empty directory to its parent: emitted straight away if
// the parent already has content (or is a scan root, whose children are
// always topmost), held otherwise.
func (e *emptyFolderScan) addEmpty(p *efFrame, d dirEnt) {
	if p.hasFile || p.isRoot {
		e.emit(d)
		return
	}
	if len(p.empties) < emptyFolderCap {
		p.empties = append(p.empties, d)
		return
	}
	p.dropped++
}

// popTo leaves every directory the walk is no longer inside. A frame stays
// while p is itself that directory or sits underneath it.
func (e *emptyFolderScan) popTo(p string) {
	for len(e.stack) > 0 {
		f := &e.stack[len(e.stack)-1]
		if f.path == p || strings.HasPrefix(p, f.path+"/") {
			return
		}
		e.pop()
	}
}

func (e *emptyFolderScan) pop() {
	f := e.stack[len(e.stack)-1]
	e.stack = e.stack[:len(e.stack)-1]
	var parent *efFrame
	if len(e.stack) > 0 {
		parent = &e.stack[len(e.stack)-1]
	}
	if f.hasFile || f.isRoot {
		for _, d := range f.empties {
			e.emit(d)
		}
		e.dropped += f.dropped
		if f.hasFile && parent != nil {
			e.markHasFile(parent)
		}
		return
	}
	// Nothing but (possibly) empty directories below: this one supersedes
	// them as the topmost empty folder, and they are dropped with it.
	if parent != nil {
		e.addEmpty(parent, dirEnt{path: f.path, size: f.size, mod: f.mod})
	}
}

func (e *emptyFolderScan) visit(rootIdx int, f fEnt) {
	// Crossing into a new scan root closes the previous root's frames. Without
	// this, a nested root inherits the OUTER walk's still-open frame for its
	// own directory: popTo keeps that frame (the new entries sit under it),
	// so the `len(e.stack) == 0` branch below never runs, the nested root
	// never gets the isRoot frame that makes its children topmost, and pop()
	// then treats it as an ordinary childless directory — superseding and
	// DISCARDING the genuinely empty folders inside it, while offering the
	// root itself as a candidate, which it must never be. Whether that
	// happened depended on nothing more than where the nested root sorted in
	// its parent's listing.
	if rootIdx != e.curRoot {
		for len(e.stack) > 0 {
			e.pop()
		}
		e.curRoot = rootIdx
	}
	e.popTo(f.path)
	if len(e.stack) == 0 {
		// First entry under a scan root: the root is this entry's parent.
		// A root is never a candidate itself, and its children are always
		// topmost — that is what isRoot says.
		e.stack = append(e.stack, efFrame{path: filepath.Dir(f.path), isRoot: true})
	}
	if f.isDir {
		e.stack = append(e.stack, efFrame{path: f.path, size: f.size, mod: f.mod})
		return
	}
	// A junk file does not make its folder non-empty: a folder holding
	// nothing but the Temporary Files tool's own junk — .DS_Store,
	// Thumbs.db, desktop.ini, editor droppings — is as
	// disposable as a bare one, and the junk simply rides along when the
	// folder is moved. The definition is shared with that tool on purpose:
	// one list decides what "useless" means everywhere.
	//
	// Deliberately NAMES only, never sizes: a zero-byte file is not junk
	// here. A .gitkeep exists precisely to keep its folder non-empty, and a
	// zero-byte placeholder someone created is theirs — the Empty Files tool
	// lists those individually instead.
	if isTempName(f.name) {
		return
	}
	e.markHasFile(&e.stack[len(e.stack)-1])
}

// noteUnreadable marks the directory the walk could not read — and with it
// every ancestor, once they pop — as holding content. Unknown contents must
// never read as empty.
func (e *emptyFolderScan) noteUnreadable(p string) {
	e.popTo(p)
	if len(e.stack) == 0 {
		return // a scan root the walk never entered: it has no children here
	}
	e.markHasFile(&e.stack[len(e.stack)-1])
}

// finish closes the remaining frames, then confirms the candidates through
// File Station. Confirmation happens only for the capped set, so the round
// trips are bounded along with the result.
func (e *emptyFolderScan) finish(s *Server, cancel chan struct{}, confirm func(string) (bool, error)) ([]FileEnt, *TruncInfo, []string) {
	for len(e.stack) > 0 {
		e.pop()
	}
	cands, trunc := e.top.final()
	if e.dropped > 0 {
		if trunc == nil {
			trunc = &TruncInfo{Cap: emptyFolderCap}
		}
		trunc.Files += e.dropped
	}
	out := make([]FileEnt, 0, len(cands))
	failed, firstErr := 0, ""
	for i, c := range cands {
		// One File Station round trip per candidate, up to the cap: this
		// loop has to answer Stop and move the bar, or twenty thousand
		// sequential calls read as a hung scan that cannot be stopped.
		if cancelled(cancel) {
			break // runScan discards the result; nothing left to confirm
		}
		if i%25 == 0 {
			s.setProgress(15+81*float64(i)/float64(max(len(cands), 1)),
				"Confirming empty folders… ("+humanCount(i)+" of "+humanCount(len(cands))+")")
		}
		// Hard confirmation: re-check the directory itself. Any entry at all
		// (hidden, system) means it is not safely empty. This also closes the
		// gap for entries whose Info() failed during the walk and shrinks the
		// scan→move staleness window.
		empty, err := confirm(c.path)
		if err != nil {
			// Not empty as far as this scan is concerned — the gate never
			// over-reports — but counted and reported: an expired session
			// mid-confirmation must not read as "700 empty folders" when the
			// truth is "700 confirmed, 3,300 never checked".
			failed++
			if firstErr == "" {
				firstErr = err.Error()
			}
			continue
		}
		if !empty {
			continue
		}
		fe := s.fileEnt(c)
		fe.IsDir = true
		fe.Ext = "DIR"
		out = append(out, fe)
	}
	var errs []string
	if failed > 0 {
		errs = append(errs, fmt.Sprintf("%d empty-folder candidates could not be confirmed through File Station and were left out (%s)", failed, firstErr))
	}
	return out, trunc, errs
}
