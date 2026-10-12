// File names are bytes; JSON is Unicode. Linux lets a name hold any byte
// but NUL and "/", and a share copied from an old Windows or Linux machine
// commonly holds Latin-1 or Shift-JIS names that are not UTF-8. Go's JSON
// encoder replaces every such byte with U+FFFD, so a row for one of these
// files reached the app with a name that no longer named it: the app could
// list the file, count it, and never act on it — every move came back
// "not part of the current scan results", which rescanning does not fix —
// and two files whose names differed only in such bytes were shown as the
// same row twice. The persisted state file went through the same encoder
// and lost the bytes across a restart.
//
// The daemon keeps paths as the bytes the filesystem gave it and converts
// only at its JSON boundaries. A byte that is not part of a valid UTF-8
// sequence travels as the rune escBase+byte — U+10FE80…U+10FEFF, inside
// Supplementary Private Use Area-B (U+100000…U+10FFFD), a range no writing
// system and no vendor assigns. Deliberately not the very top of the plane:
// U+10FFFE and U+10FFFF are Unicode NONCHARACTERS, which are legal to encode
// but which a strict JSON validator or a normalizing proxy may reject or
// replace — and 0xFE and 0xFF are exactly the bytes a Latin-1 name is likely
// to hold. The byte comes back unchanged. The app carries these runes
// untouched (they are ordinary characters to it) and draws each as U+FFFD
// where it shows a name (esc in DuplicateFinder.js), so the user sees the
// same replacement glyph as before, while the string it sends back still
// names the file. Every path field, in every message, in both directions,
// gets the same treatment, so no field can be forgotten: the conversion
// walks the value being encoded or decoded.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
)

const escBase = 0x10FE00

// escapeBytes returns s with every byte of every invalid UTF-8 sequence
// replaced by its escape rune. A valid string is returned as is, without
// allocating: this runs over every string of every response on the slow
// path, and almost all of them are plain UTF-8.
func escapeBytes(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	b := make([]byte, 0, len(s)+8)
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n == 1 {
			b = utf8.AppendRune(b, rune(escBase)+rune(s[i]))
			i++
			continue
		}
		b = append(b, s[i:i+n]...)
		i += n
	}
	return string(b)
}

// escapeMarker is the two-byte UTF-8 prefix every escape rune shares: the
// range U+10FE80…U+10FEFF encodes as F4 8F BA 80 … F4 8F BB BF.
const escapeMarker = "\xf4\x8f"

// unescapeBytes is the inverse of escapeBytes: escape runes become the bytes
// they stood for, everything else — including bytes that are already
// invalid UTF-8 — is copied through unchanged.
func unescapeBytes(s string) string {
	if !strings.Contains(s, escapeMarker) {
		return s
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r >= escBase+0x80 && r <= escBase+0xFF {
			b = append(b, byte(r-escBase))
		} else {
			b = append(b, s[i:i+n]...)
		}
		i += n
	}
	return string(b)
}

// rewriteStrings returns rv with fn applied to every string reachable
// through exported fields, slices, arrays, maps (keys and values), pointers
// and interfaces. Nothing is modified in place: wherever fn changes
// something, that container is copied on the way back up, and the boolean
// says whether anything changed at all — a value fn leaves alone is returned
// as it was, so the common case costs a walk and no allocation.
func rewriteStrings(rv reflect.Value, fn func(string) string) (reflect.Value, bool) {
	switch rv.Kind() {
	case reflect.String:
		s := rv.String()
		if t := fn(s); t != s {
			nv := reflect.New(rv.Type()).Elem()
			nv.SetString(t)
			return nv, true
		}
	case reflect.Pointer:
		if rv.IsNil() {
			break
		}
		if ev, ok := rewriteStrings(rv.Elem(), fn); ok {
			np := reflect.New(rv.Type().Elem())
			np.Elem().Set(ev)
			return np, true
		}
	case reflect.Interface:
		if rv.IsNil() {
			break
		}
		if ev, ok := rewriteStrings(rv.Elem(), fn); ok {
			nv := reflect.New(rv.Type()).Elem()
			nv.Set(ev)
			return nv, true
		}
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			break
		}
		var out reflect.Value
		for i := 0; i < rv.Len(); i++ {
			ev, ok := rewriteStrings(rv.Index(i), fn)
			if !ok {
				continue
			}
			if !out.IsValid() {
				if rv.Kind() == reflect.Slice {
					out = reflect.MakeSlice(rv.Type(), rv.Len(), rv.Len())
					reflect.Copy(out, rv)
				} else {
					out = reflect.New(rv.Type()).Elem()
					out.Set(rv)
				}
			}
			out.Index(i).Set(ev)
		}
		if out.IsValid() {
			return out, true
		}
	case reflect.Map:
		if rv.IsNil() {
			break
		}
		var out reflect.Value
		it := rv.MapRange()
		for it.Next() {
			k, kok := rewriteStrings(it.Key(), fn)
			v, vok := rewriteStrings(it.Value(), fn)
			if !kok && !vok {
				continue
			}
			if !out.IsValid() {
				out = reflect.MakeMapWithSize(rv.Type(), rv.Len())
				it2 := rv.MapRange()
				for it2.Next() {
					out.SetMapIndex(it2.Key(), it2.Value())
				}
			}
			if kok {
				out.SetMapIndex(it.Key(), reflect.Value{}) // drop the old key
			} else {
				k = it.Key()
			}
			if !vok {
				v = it.Value()
			}
			out.SetMapIndex(k, v)
		}
		if out.IsValid() {
			return out, true
		}
	case reflect.Struct:
		var out reflect.Value
		t := rv.Type()
		for i := 0; i < rv.NumField(); i++ {
			if !t.Field(i).IsExported() {
				continue
			}
			fv, ok := rewriteStrings(rv.Field(i), fn)
			if !ok {
				continue
			}
			if !out.IsValid() {
				out = reflect.New(t).Elem()
				out.Set(rv)
			}
			out.Field(i).Set(fv)
		}
		if out.IsValid() {
			return out, true
		}
	}
	return rv, false
}

// rewriteInPlace applies fn to every string reachable from *ptr, storing the
// result back through the pointer.
func rewriteInPlace(ptr any, fn func(string) string) {
	rv := reflect.ValueOf(ptr)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return
	}
	if nv, ok := rewriteStrings(rv.Elem(), fn); ok {
		rv.Elem().Set(nv)
	}
}

// marshalBytesafe is json.Marshal with the daemon's byte escaping. The plain
// encoding is kept whenever it lost nothing; the encoder marks every byte it
// replaced with the six-character escape \ufffd (a genuine U+FFFD is written
// raw, never escaped), so its absence proves the fast path safe, and its
// presence sends the value through escapeBytes and a second encoding.
func marshalBytesafe(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil || !bytes.Contains(b, []byte(`\ufffd`)) {
		return b, err
	}
	nv, changed := rewriteStrings(reflect.ValueOf(v), escapeBytes)
	if !changed {
		return b, nil
	}
	return json.Marshal(nv.Interface())
}

// decodeBytesafe decodes one JSON value from r into ptr and turns escape
// runes back into the bytes they stood for, so a path the app sends back is
// the path the daemon gave it.
func decodeBytesafe(r io.Reader, ptr any) error {
	if err := json.NewDecoder(r).Decode(ptr); err != nil {
		return err
	}
	rewriteInPlace(ptr, unescapeBytes)
	return nil
}

// unmarshalBytesafe is decodeBytesafe over a byte slice.
func unmarshalBytesafe(b []byte, ptr any) error {
	if err := json.Unmarshal(b, ptr); err != nil {
		return err
	}
	rewriteInPlace(ptr, unescapeBytes)
	return nil
}

// jsonQuote writes s as a JSON string literal WITHOUT validating its bytes:
// the standard encoder would turn a non-UTF-8 name into U+FFFD, and a File
// Station call carrying that would name a file that does not exist. Control
// characters, quotes and backslashes are escaped as the grammar requires;
// every other byte is passed through as it is, which is how the name reaches
// DSM from its own CGI form decoding as well.
func jsonQuote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < 0x20:
			const hex = "0123456789abcdef"
			b.WriteString(`\u00`)
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// jsonArr is a JSON array of strings for a File Station parameter, quoted
// byte for byte (jsonQuote).
func jsonArr(items ...string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, s := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(jsonQuote(s))
	}
	b.WriteByte(']')
	return b.String()
}
