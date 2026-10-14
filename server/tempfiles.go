// What counts as a temporary or junk file. One list serves the Temporary
// Files tool and the empty-folder rule alike, so "junk" means the same
// thing everywhere.
package main

import "strings"

// Junk by exact name, by suffix and by prefix. Compared case-insensitively
// (isTempName lowercases), which is why the move-time fsCannotAddress check
// keeps its own, case-sensitive .DS_Store rule.
//
// Nothing on this list may be a file somebody keeps on purpose. Backup names
// — *.bak, *.old, an editor's "name~" copy — and an editor's swap file, which
// holds the only copy of edits that were never saved, are therefore NOT
// junk: they are often the one copy of something, and a folder holding one
// is not empty.
var tempExact = map[string]bool{
	"thumbs.db": true, ".ds_store": true, "desktop.ini": true,
	"ehthumbs.db": true, ".apdisk": true, "npm-debug.log": true,
}
var tempSuffix = []string{".tmp", ".temp", ".crdownload", ".part", ".partial"}
var tempPrefix = []string{"~$", ".~lock.", "._"}

func isTempName(name string) bool {
	l := strings.ToLower(name)
	if tempExact[l] {
		return true
	}
	for _, suf := range tempSuffix {
		if strings.HasSuffix(l, suf) {
			return true
		}
	}
	for _, pre := range tempPrefix {
		if strings.HasPrefix(l, pre) {
			return true
		}
	}
	return false
}
