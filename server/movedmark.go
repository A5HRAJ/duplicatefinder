// The record a flat move leaves in its destination folder. The app's own
// record of moved copies (moveout.go) lives in the package's data folder,
// and can be lost entirely there: Package Center's Repair is reported to
// erase it, and moving the files to a new NAS leaves it behind. A copy moved
// flat would then look like an original again, and keep-one could let the
// last real copy follow it into a folder that exists to be emptied. So a
// flat move also leaves, in the folder the files landed in, a hidden folder
// holding one small record per move; it travels with the files. A move that
// keeps the folder structure needs none: its batch folder's layout says what
// it is (inBatchLayout).
package main

import (
	"bytes"
	"crypto/rand"
	"dupfinder/internal/dirhandle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// movedMarkDir is the hidden folder the records live in. Hidden, so the walk
// never lists it or its records as files of the user's.
const movedMarkDir = ".duplicate-finder-moved"

// movedMarkAbout is written into every record, for whoever opens one.
const movedMarkAbout = "Duplicate Finder moved the files listed here into the folder that holds this one. " +
	"While this record is here, Duplicate Finder never counts them as the copy to keep, even if its own records are lost. " +
	"Delete this folder if you no longer want that."

const (
	movedMarkFileMax  = 8 << 20   // bytes read from one record
	movedMarkCap      = 200000    // record entries one scan holds
	movedMarkReadMax  = 256 << 20 // record bytes one scan reads, every folder together
	movedMarkFilesMax = 1000      // records read from one folder
	movedMarkIssueMax = 100       // record problems one scan lists by folder
)

// movedMarkChunk is how many entries one record holds; a bigger move writes
// several. Small enough that a record stays far below movedMarkFileMax
// whatever the names, so a reader never has to refuse one the app wrote. A
// variable only so a test can make it small.
var movedMarkChunk = 2000

// movedMarkBytesMax bounds the names a scan holds from records, every folder
// together: a planted record full of long names must not cost a small NAS
// its memory. A variable only so a test can make it small.
var movedMarkBytesMax = 64 << 20

type movedMarkEntry struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
}

type movedMarkRecord struct {
	About string `json:"about"`
	At    string `json:"at"`
	// Folder is the folder the record was written in, as its shared folder
	// and path with the volume left off ("/Photos/cleanup", see
	// markFolderKey). A record speaks for that folder alone: copied anywhere
	// else, beside the original say, it must not mark the original as moved —
	// with every copy of a group marked, keep-one lets any copy stay. Records
	// written before this field existed carry none and are honoured where
	// they are found.
	Folder string           `json:"folder,omitempty"`
	Moved  []movedMarkEntry `json:"moved"`
}

// markFolderKey names the folder at canonical path canon the way a record
// does: without its volume root, so a record still matches after the files
// are carried to another NAS whose volume numbers differ.
func markFolderKey(canon string) string {
	if v := volumeOf(canon, volumeRootsResolved()); v != "" {
		return canon[len(v):]
	}
	return canon
}

// leaveMovedMark writes the record of what a flat move landed directly in
// the destination folder (landed holds canonical landing paths; anything not
// directly in destCanon is skipped): each regular file by its name, and for a
// folder moved flat — an Empty Folders row, which takes its junk files along
// — every regular file inside it by its path below the destination
// ("j1/Thumbs.db"), so a copy among them is still known as moved. Each move
// writes a record of its own under a fresh name, so no record is ever
// overwritten.
func leaveMovedMark(sess *fsSession, destShare, destCanon string, landed []string) error {
	var ents []movedMarkEntry
	add := func(rel string, fi os.FileInfo) {
		if _, _, ok := markName(rel); ok && utf8.ValidString(rel) {
			ents = append(ents, movedMarkEntry{Name: rel, Size: fi.Size(), MTime: fi.ModTime().Unix()})
		}
	}
	for _, at := range landed {
		if filepath.Dir(at) != destCanon {
			continue
		}
		fi, err := os.Lstat(at)
		if err != nil {
			continue // not landed yet (an unsettled move)
		}
		switch {
		case fi.Mode().IsRegular():
			add(filepath.Base(at), fi)
		case fi.IsDir():
			n := 0
			filepath.WalkDir(at, func(p string, d fs.DirEntry, werr error) error {
				if werr != nil {
					return nil
				}
				if d.IsDir() {
					if d.Name() == "@eaDir" || d.Name() == movedMarkDir {
						return fs.SkipDir
					}
					return nil
				}
				if !d.Type().IsRegular() {
					return nil
				}
				if n >= maxManifest {
					return fs.SkipAll
				}
				if info, ierr := d.Info(); ierr == nil {
					if rel, rerr := filepath.Rel(destCanon, p); rerr == nil {
						add(filepath.ToSlash(rel), info)
						n++
					}
				}
				return nil
			})
		}
	}
	if len(ents) == 0 {
		return nil
	}
	dir := destShare + "/" + movedMarkDir
	there, isDir, err := sess.statShare(dir)
	if err != nil {
		return err
	}
	if there && !isDir {
		return errors.New("a file named " + movedMarkDir + " is in the way")
	}
	if !there {
		if cerr := sess.createFolder(destShare, movedMarkDir, false); cerr != nil {
			// Another move, or a reply lost on the way, may have made it.
			if ok, d, serr := sess.statShare(dir); serr != nil || !ok || !d {
				return cerr
			}
		}
	}
	// One record per movedMarkChunk entries, so none comes near the size a
	// reader refuses. HTML escaping is off: "&" in a name would otherwise
	// take six bytes.
	stamp := time.Now().UTC().Format("20060102-150405")
	for start := 0; start < len(ents); start += movedMarkChunk {
		end := start + movedMarkChunk
		if end > len(ents) {
			end = len(ents)
		}
		var body bytes.Buffer
		enc := json.NewEncoder(&body)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", " ")
		if err := enc.Encode(movedMarkRecord{About: movedMarkAbout, At: time.Now().UTC().Format(time.RFC3339), Folder: markFolderKey(destCanon), Moved: ents[start:end]}); err != nil {
			return err
		}
		buf := make([]byte, 4)
		if _, err := rand.Read(buf); err != nil {
			return err
		}
		if err := sess.upload(dir, stamp+"-"+hex.EncodeToString(buf)+".json", body.Bytes()); err != nil {
			return err
		}
	}
	return nil
}

// movedMarks is what one scan read from the record folders it met, keyed by
// the display path of the folder holding each.
type movedMarks struct {
	dirs   map[string]*markedDir
	n      int   // entries held
	bytes  int   // name bytes held
	read   int64 // record bytes read
	over   bool  // a scan-wide bound was reached
	issues []string
	more   int // problems past movedMarkIssueMax
	cancel chan struct{}
}

type markedDir struct {
	canon string
	ents  map[string]movedMarkEntry
}

func newMovedMarks(cancel chan struct{}) *movedMarks {
	return &movedMarks{dirs: map[string]*markedDir{}, cancel: cancel}
}

// problem notes a record that could not be used. Never silent: a record that
// is skipped is protection lost, and the user is told where to look.
func (m *movedMarks) problem(display, why string) {
	if len(m.issues) >= movedMarkIssueMax {
		m.more++
		return
	}
	m.issues = append(m.issues, display+": a record of files Duplicate Finder moved here could not be read ("+why+"), so they may not be recognised as moved — check this folder before deleting anything")
}

// load is the walk's enterDir hook: it reads the record folder inside the
// directory rel (under the pinned root h; display path display, canonical
// path canon), if it holds one. Records are opened without following any
// symlink. One that cannot be opened, read or parsed, or that is larger than
// any record the app writes, is reported (problem), never silently passed
// over. What a scan reads and holds from records is bounded, and Stop is
// honoured between records.
func (m *movedMarks) load(h *dirhandle.Handle, rel, display, canon string) {
	recRel := filepath.Join(rel, movedMarkDir)
	fi, err := os.Lstat(filepath.Join(h.Path(), recRel))
	if err != nil || !fi.IsDir() {
		return
	}
	names, err := os.ReadDir(filepath.Join(h.Path(), recRel))
	if err != nil {
		m.problem(display, err.Error())
		return
	}
	files := 0
	for _, e := range names {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if cancelled(m.cancel) || m.over {
			return
		}
		if files++; files > movedMarkFilesMax {
			m.problem(display, "it holds more than "+strconv.Itoa(movedMarkFilesMax)+" records; the rest were not read")
			return
		}
		if m.read >= movedMarkReadMax {
			m.over = true
			return
		}
		f, err := h.OpenRel(filepath.Join(recRel, e.Name()))
		if err != nil {
			m.problem(display, e.Name()+": "+err.Error())
			continue
		}
		b, err := io.ReadAll(io.LimitReader(f, movedMarkFileMax+1))
		f.Close()
		m.read += int64(len(b))
		if err != nil {
			m.problem(display, e.Name()+": "+err.Error())
			continue
		}
		if len(b) > movedMarkFileMax {
			m.problem(display, e.Name()+" is larger than any record Duplicate Finder writes")
			continue
		}
		var rec movedMarkRecord
		if jerr := json.Unmarshal(b, &rec); jerr != nil {
			m.problem(display, e.Name()+" is damaged")
			continue
		}
		if rec.Folder != "" && rec.Folder != markFolderKey(canon) {
			continue // written for another folder, and copied here
		}
		for _, ent := range rec.Moved {
			sub, base, ok := markName(ent.Name)
			if !ok {
				continue
			}
			if m.n >= movedMarkCap || m.bytes+len(ent.Name) > movedMarkBytesMax {
				m.over = true
				return
			}
			// A file inside a folder the move landed is registered under
			// that folder, which the walk enters after this one.
			key, c := display, canon
			if sub != "" {
				key, c = filepath.Join(display, sub), filepath.Join(canon, sub)
			}
			md := m.dirs[key]
			if md == nil {
				md = &markedDir{canon: c, ents: map[string]movedMarkEntry{}}
				m.dirs[key] = md
			}
			ent.Name = base
			md.ents[base] = ent
			m.n++
			m.bytes += len(key) + len(base)
		}
	}
}

// markName splits a record entry's name — a file name, or a path below the
// record's folder for a file inside a folder the move landed — into that
// path's folders and its base name. It refuses anything that could reach
// outside the record's folder or that no filesystem would hold: an absolute
// path, an empty, "." or ".." component, a component over 255 bytes.
func markName(name string) (sub, base string, ok bool) {
	if name == "" || strings.HasPrefix(name, "/") || len(name) > 4096 {
		return "", "", false
	}
	parts := strings.Split(name, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || len(p) > 255 {
			return "", "", false
		}
	}
	return strings.Join(parts[:len(parts)-1], "/"), parts[len(parts)-1], true
}

// match reports the canonical path of the file at display path dir/name when
// a record in dir lists it with this size and modified time.
func (m *movedMarks) match(dir, name string, size int64, mod time.Time) (string, bool) {
	md := m.dirs[dir]
	if md == nil {
		return "", false
	}
	ent, ok := md.ents[name]
	if !ok || ent.Size != size || ent.MTime != mod.Unix() {
		return "", false
	}
	return filepath.Join(md.canon, name), true
}

// report is the issue lines a scan adds for its records: every one that
// could not be used, and one line when a scan-wide bound was reached.
func (m *movedMarks) report() []string {
	out := append([]string(nil), m.issues...)
	if m.more > 0 {
		out = append(out, "… and "+strconv.Itoa(m.more)+" more records of moved files could not be read")
	}
	if m.over {
		out = append(out, "more is recorded in "+movedMarkDir+" folders than one scan reads; files listed past that point were not recognised as ones Duplicate Finder moved")
	}
	return out
}

// outKeys returns the move-record entries for files a record identified:
// each canonical path, plus its data key so the record follows the file if
// its folder is later renamed (moveout.go).
func outKeys(paths []string) []string {
	out := make([]string, 0, 2*len(paths))
	for _, p := range paths {
		out = append(out, p)
		if fi, err := os.Lstat(p); err == nil {
			if k := dataKeyOf(p, fi); k != "" {
				out = append(out, k)
			}
		}
	}
	return out
}

// errMovedMark wraps a failure to leave the record, for the move report.
func errMovedMark(dest string, err error) string {
	return fmt.Sprintf("the files were moved, but the record of them could not be left in %s (%v): if Duplicate Finder's own records are ever lost, it will not know it moved them", dest, err)
}
