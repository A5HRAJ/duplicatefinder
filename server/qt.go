package main

import (
	"encoding/binary"
	"os"
	"strings"
	"time"
)

// qtCaptured extracts the capture date from QuickTime/MP4 containers
// (.mov/.mp4/.m4v). Preference order:
//  1. com.apple.quicktime.creationdate (moov/meta keys+ilst) — the wall-clock
//     capture time with timezone that iPhones record;
//  2. mvhd creation_time — UTC seconds since 1904-01-01.
//
// open is the entry's pinned-handle opener (see fEnt.openContent).
func qtCaptured(open func() (*os.File, error)) string {
	f, err := open()
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	end := st.Size()

	moovStart, moovEnd, ok := findBox(f, 0, end, "moov")
	if !ok {
		return ""
	}
	if d := qtAppleCreationDate(f, moovStart, moovEnd); d != "" {
		return d
	}
	return qtMvhdDate(f, moovStart, moovEnd)
}

func qtMvhdDate(f *os.File, moovStart, moovEnd int64) string {
	s, e, ok := findBox(f, moovStart, moovEnd, "mvhd")
	if !ok || e-s < 16 {
		return ""
	}
	buf := make([]byte, 16)
	if _, err := f.ReadAt(buf, s); err != nil {
		return ""
	}
	var secs uint64
	if buf[0] == 1 {
		secs = binary.BigEndian.Uint64(buf[4:12])
	} else {
		secs = uint64(binary.BigEndian.Uint32(buf[4:8]))
	}
	if secs == 0 {
		return ""
	}
	t := time.Unix(int64(secs)-2082844800, 0).UTC() // 1904 → unix epoch
	if t.Year() < 1971 || t.Year() > 2100 {
		return ""
	}
	return t.Format("2006-01-02 15:04:05")
}

// qtAppleCreationDate reads moov/meta/{keys,ilst}. The meta box has a
// version/flags prefix in ISO files but not in classic QuickTime files, so
// both interpretations are tried.
func qtAppleCreationDate(f *os.File, moovStart, moovEnd int64) string {
	metaStart, metaEnd, ok := findBox(f, moovStart, moovEnd, "meta")
	if !ok {
		return ""
	}
	for _, base := range []int64{metaStart, metaStart + 4} {
		if base >= metaEnd {
			continue
		}
		if d := qtKeysIlst(f, base, metaEnd); d != "" {
			return d
		}
	}
	return ""
}

func qtKeysIlst(f *os.File, base, end int64) string {
	keysStart, keysEnd, ok := findBox(f, base, end, "keys")
	if n := keysEnd - keysStart; !ok || n <= 0 || n > 1<<16 {
		return ""
	}
	kb := make([]byte, keysEnd-keysStart)
	if _, err := f.ReadAt(kb, keysStart); err != nil {
		return ""
	}
	if len(kb) < 8 {
		return ""
	}
	count := int(binary.BigEndian.Uint32(kb[4:8]))
	pos, wantIdx := 8, 0
	for i := 1; i <= count && pos+8 <= len(kb); i++ {
		sz := int(binary.BigEndian.Uint32(kb[pos : pos+4]))
		if sz < 8 || pos+sz > len(kb) {
			break
		}
		ns := string(kb[pos+4 : pos+8])
		name := string(kb[pos+8 : pos+sz])
		if ns == "mdta" && name == "com.apple.quicktime.creationdate" {
			wantIdx = i
			break
		}
		pos += sz
	}
	if wantIdx == 0 {
		return ""
	}

	ilstStart, ilstEnd, ok := findBox(f, base, end, "ilst")
	if n := ilstEnd - ilstStart; !ok || n <= 0 || n > 1<<20 {
		return ""
	}
	ib := make([]byte, ilstEnd-ilstStart)
	if _, err := f.ReadAt(ib, ilstStart); err != nil {
		return ""
	}
	pos = 0
	for pos+8 <= len(ib) {
		sz := int(binary.BigEndian.Uint32(ib[pos : pos+4]))
		idx := int(binary.BigEndian.Uint32(ib[pos+4 : pos+8]))
		if sz < 8 || pos+sz > len(ib) {
			break
		}
		if idx == wantIdx {
			// child 'data' box: 4 size, 4 'data', 4 type, 4 locale, value
			inner := ib[pos+8 : pos+sz]
			if len(inner) >= 16 && string(inner[4:8]) == "data" {
				val := strings.TrimSpace(string(inner[16:]))
				return parseISODate(val)
			}
			return ""
		}
		pos += sz
	}
	return ""
}

func parseISODate(v string) string {
	for _, layout := range []string{
		"2006-01-02T15:04:05-0700",
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05.999999999Z07:00",
		"2006-01-02T15:04:05",
	} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.Format("2006-01-02 15:04:05")
		}
	}
	return ""
}
