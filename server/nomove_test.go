package main

// Rows File Station cannot address (fsCannotAddress), pinned against DSM 7.4
// as measured on a DS916+. The .DS_Store cases were measured directly (the
// file uploads, the scanner reads it, list omits it and getinfo answers 418).
// The "._" cases were established indirectly — File Station answers 418 when
// asked to CREATE such a name, by upload or by rename, so no probe file could
// be made through the API. See fsCannotAddress for the full distinction. If a
// DSM upgrade changes the behaviour, re-run those probes and change these
// tests first.

import "testing"

func TestFsCannotAddressMatchesMeasuredDSMBehaviour(t *testing.T) {
	cases := []struct {
		name   string
		hidden bool
		why    string
	}{
		{".DS_Store", true, "the exact name DSM filters"},
		{"._x", true, "AppleDouble prefix: File Station 418s on the name"},
		{"._UPPER", true, "prefix match, whatever follows"},
		{"._", true, "the bare prefix counts"},
		{".__notapple", true, "starts ._ even though the rest is not an AppleDouble name"},
		{"._dirform", true, "directories match the same rule"},

		// The half that matters most: these ARE movable, so disabling their
		// checkbox would break working rows.
		{".ds_store", false, "DSM's filter is CASE-SENSITIVE — this one is visible"},
		{".Ds_StOrE", false, "same, mixed case is visible"},
		{".DS_Store_dir", false, "only the exact name is filtered, not a prefix of it"},
		{"x._y", false, "._ counts only as a prefix"},
		{"Thumbs.db", false, "junk to temp_files, but perfectly visible to File Station"},
		{".apdisk", false, "a dotfile, and visible — dotfiles are NOT hidden as a class"},
		{".gitkeep", false, "ordinary dotfile"},
		{"", false, "empty name is not special"},
	}
	for _, c := range cases {
		if got := fsCannotAddress(c.name); got != c.hidden {
			t.Errorf("fsCannotAddress(%q) = %v, want %v (%s)", c.name, got, c.hidden, c.why)
		}
	}
}

// The case-sensitivity clause is the one a later reader is most likely to
// "tidy up" into a ToLower, so state the consequence on its own: these names
// are junk to temp_files AND movable, and both facts must survive together.
func TestCaseVariantDSStoreIsJunkButStillMovable(t *testing.T) {
	for _, n := range []string{".ds_store", ".Ds_StOrE"} {
		if !isTempName(n) {
			t.Errorf("%q should still be temp junk (isTempName lowercases)", n)
		}
		if fsCannotAddress(n) {
			t.Errorf("%q is visible to File Station and must stay movable", n)
		}
	}
	// ...while the exact-case name is both.
	if !isTempName(".DS_Store") || !fsCannotAddress(".DS_Store") {
		t.Error(".DS_Store must be both temp junk and unaddressable")
	}
}

// A path File Station cannot address for the OTHER reason: bytes that are
// not valid UTF-8. Its parameters are JSON, and its own encoder rewrites such
// a name on the way out, so the daemon refuses the move with that reason
// rather than letting File Station answer "no such file" for a file the scan
// has just read. Unlike the name clauses this one covers the whole path.
func TestFsCannotAddressPathCoversInvalidUTF8(t *testing.T) {
	cases := []struct {
		path   string
		hidden bool
		why    string
	}{
		{"/volume1/music/caf\xe9.txt", true, "Latin-1 name: not valid UTF-8"},
		{"/volume1/jp/\x83\x5c.txt", true, "Shift-JIS name"},
		{"/volume1/m\xfcsic/ok.txt", true, "a valid name inside such a folder is just as unreachable"},
		{"/volume1/music/caf\u00e9.txt", false, "the same name in UTF-8 is ordinary"},
		{"/volume1/music/résumé.txt", false, "accents are fine when they are UTF-8"},
		{"/volume1/a/.DS_Store", true, "the name clause still applies"},
		{"/volume1/a/holiday.jpg", false, "an ordinary file"},
	}
	for _, c := range cases {
		if got := fsCannotAddressPath(c.path); got != c.hidden {
			t.Errorf("fsCannotAddressPath(%q) = %v, want %v (%s)", c.path, got, c.hidden, c.why)
		}
	}
}

// fileEnt stamps NoMove from the row's own path, so every tool's rows carry
// it without each scan case remembering to.
func TestFileEntStampsNoMove(t *testing.T) {
	s := &Server{}
	if fe := s.fileEnt(fEnt{name: ".DS_Store", dir: "/v/a"}); !fe.NoMove {
		t.Error("a .DS_Store row must be marked NoMove")
	}
	// Stamped from Dir and Name, not from fEnt.path, which only the walk fills.
	if fe := s.fileEnt(fEnt{name: "caf\xe9.txt", dir: "/v/a"}); !fe.NoMove {
		t.Error("a row whose name is not valid UTF-8 must be marked NoMove")
	}
	if fe := s.fileEnt(fEnt{name: "ok.txt", dir: "/v/m\xfcsic"}); !fe.NoMove {
		t.Error("a row inside a folder whose name is not valid UTF-8 must be marked NoMove")
	}
	if fe := s.fileEnt(fEnt{name: "holiday.jpg", dir: "/v/a"}); fe.NoMove {
		t.Error("an ordinary file must not be marked NoMove")
	}
	// A FOLDER is unmovable only by its own name — not because of what it
	// holds. That is what lets a junk-only folder move with its junk inside.
	if fe := s.fileEnt(fEnt{name: "photos", dir: "/v/a", isDir: true}); fe.NoMove {
		t.Error("a folder holding junk is still movable itself")
	}
}
