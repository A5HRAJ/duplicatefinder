// The result model: what a scan stores per tool and what the API serves —
// groups, rows, the truncation report and the match criteria. FileEnt's
// JSON tags are the wire format the UI reads and the persisted state file
// holds; fields carry omitempty so rows written by an earlier build still
// load.
package main

type toolResult struct {
	Tool string `json:"tool"`
	// Roots maps each scan root's display form to its canonical (symlink-
	// resolved) path. The walk skips symlinks below a root, so a row's
	// canonical path is its root's canonical path plus the same relative
	// tail — which lets the reference-folder protection be decided on
	// canonical paths without a syscall per row (results.go refMatcher).
	Roots     []RootMap  `json:"roots,omitempty"`
	Groups    []Group    `json:"groups,omitempty"`
	Files     []FileEnt  `json:"files,omitempty"`
	Errors    []string   `json:"errors,omitempty"`
	Match     *MatchOpts `json:"match,omitempty"`     // criteria applied at scan time
	Truncated *TruncInfo `json:"truncated,omitempty"` // results found beyond the stored cap
	Scanned   string     `json:"scannedAt"`
}

// RootMap is one scan root: Raw as the user gave it (and as rows display it),
// Canon as the filesystem resolves it.
type RootMap struct {
	Raw   string `json:"raw"`
	Canon string `json:"canon"`
}

// TruncInfo reports what a scan found but did not keep, so no results cap is
// ever silent: the UI tells the user how much more exists and that narrowing
// the scan scope will surface it. Immutable once stored.
type TruncInfo struct {
	Groups int `json:"groups,omitempty"` // duplicates: whole groups dropped
	Files  int `json:"files"`            // files (rows) dropped
	Cap    int `json:"cap"`              // the stored-results cap that applied
}

// FileEnt is one row in the results table. Dir is the containing directory
// (the "Location" column); the full path is Dir + "/" + Name.
type FileEnt struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Dir  string `json:"path"`
	Size int64  `json:"size"`
	Mod  string `json:"date"`
	// ModUnix is the same instant as Mod as an epoch value, and is what the
	// move-time identity check compares: Mod is a zone-less local-time
	// string, so a NAS whose time zone changed between scan and move would
	// otherwise refuse every file as "changed". Absent from results written
	// by older builds, which fall back to comparing Mod.
	ModUnix  int64  `json:"modUnix,omitempty"`
	Created  string `json:"created"`
	Captured string `json:"captured,omitempty"`
	Hash     string `json:"hash,omitempty"`
	// Pfx is the first half of the file's 64 KiB-prefix BLAKE3, recorded by
	// the duplicates scan only — it is the one tool whose result asserts
	// something about a file's CONTENT, and the move re-checks it before
	// touching anything. Results written by an older build carry none, and
	// the check is simply skipped for them.
	Pfx   string `json:"pfx,omitempty"`
	Ext   string `json:"ext"`
	IsDir bool   `json:"isDir,omitempty"`
	// Verdict and Evidence are written by the corrupted-files scan only:
	// "corrupt", "intact" or "unknown", plus the sentence explaining why.
	// Both are omitempty, so every other tool's rows — and every result
	// written by an older build — serialize exactly as they did before.
	Verdict  string `json:"verdict,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	// NoMove marks a row File Station cannot address, so no move can ever
	// succeed for it (see fsCannotAddress). The grid draws these with DSM's
	// DISABLED checkbox — the same sprite the last-copy-of-a-group case uses,
	// because it says the same thing: this row cannot be acted on. Derived
	// from the name at scan time and sent to the client so the box is right
	// before anyone clicks it; the daemon refuses these independently, so the
	// disabled box is presentation, not the enforcement.
	NoMove bool `json:"nomove,omitempty"`
	// Prot marks a read-only reference copy. Decided by the daemon per page
	// (results.go) on CANONICAL paths — the same comparison the move refuses
	// with — so the padlock the grid draws and the refusal the move issues
	// can never disagree about a folder given through a symlink alias.
	Prot bool `json:"prot,omitempty"`
}

type Group struct {
	ID    string    `json:"id"`
	Ext   string    `json:"ext"`
	Size  int64     `json:"size"`
	Hash  string    `json:"hash"`
	Files []FileEnt `json:"files"`
	// Corrupted-files sets only. A set is defined by its members DISAGREEING,
	// so Hash above is necessarily empty for one and cannot identify it; Mod
	// is the modified time the whole set shares, and Variants counts the
	// distinct contents found under it. SameName records that every member
	// also shares a filename — the difference between two copies of one file
	// and two unrelated files that happen to match on size and timestamp.
	Mod      string `json:"mod,omitempty"`
	Variants int    `json:"variants,omitempty"`
	SameName bool   `json:"sameName,omitempty"`
	// Count is the group's TRUE member count, set only when Files carries
	// fewer than that — a page trims very large groups so one of them cannot
	// bury a browser. Zero means Files is the whole group.
	Count int `json:"count,omitempty"`
	// Prot is how many of the group's members are protected reference copies,
	// counted over the WHOLE group before any trimming. The client draws the
	// group header's reclaimable figure from it: recounting protection over a
	// trimmed page mixes a partial protected count with the true member count
	// and overstates what a move could free. Duplicates only, and omitted
	// when no reference folder is set.
	Prot int `json:"prot,omitempty"`
}

// MatchOpts are the optional extra duplicate criteria. When set they join
// the pre-hash candidate key, so files unique in (size + criteria) are never
// read or hashed at all.
type MatchOpts struct {
	Name     bool `json:"name"`
	Modified bool `json:"modified"`
	Created  bool `json:"created"`
}
