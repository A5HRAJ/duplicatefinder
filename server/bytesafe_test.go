package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The names that exercise the codec: bytes outside UTF-8 (Latin-1 "é",
// Shift-JIS lead bytes), a truncated multi-byte sequence, a genuine U+FFFD,
// which must survive untouched, and plain UTF-8.
var byteNames = []string{
	"caf\xe9.txt", "\x83\x5c.txt", "a\xc3", "\xff\xfe", "ok.txt", "résumé.txt", "lost�.txt", "",
}

// The escape range, pinned: it must stay inside Supplementary Private Use
// Area-B and clear of the two noncharacters at the top of the plane, and it
// must stay in step with BYTE_ESC in spk/ui/DuplicateFinder.js, which draws
// these as U+FFFD. Change one and this test tells you to change the other.
func TestEscapeRangeIsPrivateUseAndMatchesTheUI(t *testing.T) {
	lo, hi := rune(escBase+0x80), rune(escBase+0xFF)
	if lo < 0x100000 || hi > 0x10FFFD {
		t.Fatalf("escape range U+%04X…U+%04X leaves Supplementary Private Use Area-B", lo, hi)
	}
	// The UTF-16 surrogate pair the app's regex matches.
	const wantHigh, wantLoLow, wantLoHigh = 0xDBFF, 0xDE80, 0xDEFF
	for _, c := range []struct {
		r         rune
		high, low rune
	}{{lo, wantHigh, wantLoLow}, {hi, wantHigh, wantLoHigh}} {
		v := c.r - 0x10000
		if h, l := 0xD800+(v>>10), 0xDC00+(v&0x3FF); h != c.high || l != c.low {
			t.Fatalf("U+%04X is \\u%04X\\u%04X; the UI regex expects \\u%04X\\u%04X — update BYTE_ESC", c.r, h, l, c.high, c.low)
		}
	}
	if got := escapeBytes("\xff"); []rune(got)[0] != hi {
		t.Fatalf("byte 0xFF escaped to U+%04X, want U+%04X", []rune(got)[0], hi)
	}
}

func TestEscapeBytesRoundTrip(t *testing.T) {
	for _, s := range byteNames {
		e := escapeBytes(s)
		if !isValidUTF8(e) {
			t.Errorf("escapeBytes(%q) = %q is not valid UTF-8", s, e)
		}
		if got := unescapeBytes(e); got != s {
			t.Errorf("round trip of %q: got %q", s, got)
		}
		if isValidUTF8(s) && e != s {
			t.Errorf("escapeBytes changed a valid string %q -> %q", s, e)
		}
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' && !strings.Contains(s, "�") {
			return false
		}
	}
	return json.Valid([]byte(`"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`))
}

// A value shaped like the daemon's responses and state: rows, groups, a map
// with a path for a key, a pointer, nested slices.
type codecProbe struct {
	Groups []Group
	Files  []FileEnt
	Errors []string
	ByPath map[string]int
	Nested *codecProbe
	Any    any
	// private is unexported, so reflect cannot set it: the walker must skip
	// it rather than panic, and must leave its bytes alone.
	private string
}

func probe() codecProbe {
	return codecProbe{
		Groups:  []Group{{ID: "g1", Files: []FileEnt{{Name: "caf\xe9.txt", Dir: "/volume1/m\xfcsic"}, {Name: "ok.txt", Dir: "/volume1/x"}}}},
		Files:   []FileEnt{{Name: "\x83\x5c.txt", Dir: "/volume1/jp"}},
		Errors:  []string{"/volume1/m\xfcsic: permission denied"},
		ByPath:  map[string]int{"/volume1/m\xfcsic/caf\xe9.txt": 1},
		Nested:  &codecProbe{Files: []FileEnt{{Name: "a\xc3"}}},
		Any:     map[string]any{"path": "/volume1/\xff", "list": []any{"\xfe"}},
		private: "/volume1/\xfd",
	}
}

func TestMarshalBytesafeRoundTrip(t *testing.T) {
	in := probe()
	b, err := marshalBytesafe(in)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(`�`)) {
		t.Fatalf("encoded output still lost bytes: %s", b)
	}
	var out codecProbe
	if err := unmarshalBytesafe(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in.Groups, out.Groups) || !reflect.DeepEqual(in.Files, out.Files) ||
		!reflect.DeepEqual(in.Errors, out.Errors) || !reflect.DeepEqual(in.ByPath, out.ByPath) ||
		!reflect.DeepEqual(in.Nested.Files, out.Nested.Files) || !reflect.DeepEqual(in.Any, out.Any) {
		t.Fatalf("round trip changed the value:\n in: %+v\nout: %+v", in, out)
	}
	// The original was not modified in place.
	if in.Groups[0].Files[0].Name != "caf\xe9.txt" || in.ByPath["/volume1/m\xfcsic/caf\xe9.txt"] != 1 {
		t.Fatal("marshalBytesafe modified its input")
	}
	// An unexported field is skipped, not panicked on and not rewritten.
	if in.private != "/volume1/\xfd" {
		t.Fatal("an unexported field was rewritten")
	}
}

func TestMarshalBytesafeFastPath(t *testing.T) {
	v := map[string]any{"files": []FileEnt{{Name: "résumé.txt", Dir: "/volume1/docs"}}, "note": "lost�.txt"}
	want, _ := json.Marshal(v)
	got, err := marshalBytesafe(v)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("valid UTF-8 must encode exactly as json.Marshal does: %s vs %s (%v)", got, want, err)
	}
}

func TestDecodeBytesafeRequest(t *testing.T) {
	// What the app sends back is the escaped form the daemon gave it.
	esc := escapeBytes("/volume1/m\xfcsic/caf\xe9.txt")
	body, _ := json.Marshal(map[string]any{"files": []string{esc}, "dest": escapeBytes("/volume1/\xff"), "tool": "duplicates"})
	var req MoveReq
	if err := decodeBytesafe(bytes.NewReader(body), &req); err != nil {
		t.Fatal(err)
	}
	if req.Files[0] != "/volume1/m\xfcsic/caf\xe9.txt" || req.Dest != "/volume1/\xff" || req.Tool != "duplicates" {
		t.Fatalf("decoded request does not name the bytes on disk: %+v", req)
	}
}

func TestJSONQuoteKeepsBytes(t *testing.T) {
	got := jsonArr("/share/caf\xe9.txt", `q"uote\back`, "tab\tnew\nline\x01")
	want := "[\"/share/caf\xe9.txt\",\"q\\\"uote\\\\back\",\"tab\\tnew\\nline\\u0001\"]"
	if got != want {
		t.Fatalf("jsonArr = %q, want %q", got, want)
	}
	// Apart from the raw byte, it is exactly what the standard encoder says.
	std, _ := json.Marshal([]string{`q"uote\back`, "tab\tnew\nline"})
	if jsonArr(`q"uote\back`, "tab\tnew\nline") != string(std) {
		t.Fatalf("jsonArr differs from json.Marshal on plain input: %s vs %s", jsonArr(`q"uote\back`, "tab\tnew\nline"), std)
	}
}

func TestRewriteStringsLeavesUnchangedValuesAlone(t *testing.T) {
	v := probe()
	v.Groups[0].Files[0].Name, v.Groups[0].Files[0].Dir = "a", "/volume1/a"
	v.Files[0].Name, v.Errors[0], v.Nested.Files[0].Name = "b", "c", "d"
	v.ByPath = map[string]int{"e": 1}
	v.Any = map[string]any{"path": "f", "list": []any{"g"}}
	rv := reflect.ValueOf(v)
	if _, changed := rewriteStrings(rv, escapeBytes); changed {
		t.Fatal("a value without invalid bytes reported a change")
	}
}
