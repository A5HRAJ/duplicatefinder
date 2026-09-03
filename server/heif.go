package main

import (
	"bytes"
	"encoding/binary"
	"os"
)

// heifCaptured extracts the EXIF DateTimeOriginal from a HEIF/HEIC/AVIF
// container. HEIF stores EXIF as a metadata item: the 'meta' box's 'iinf'
// lists an item of type "Exif" and 'iloc' gives its position in the file.
// The item payload is a 4-byte TIFF-header offset followed by the classic
// EXIF block, which is handed to the same TIFF parser used for JPEG.
// open is the entry's pinned-handle opener (see fEnt.openContent).
func heifCaptured(open func() (*os.File, error)) string {
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

	metaStart, metaEnd, ok := findBox(f, 0, end, "meta")
	if !ok {
		return ""
	}
	metaStart += 4 // meta is a FullBox: skip version/flags

	exifID, ok := heifExifItemID(f, metaStart, metaEnd)
	if !ok {
		return ""
	}
	off, length, ok := heifItemLocation(f, metaStart, metaEnd, exifID)
	if !ok || length < 8 || off <= 0 || off+length > end {
		return ""
	}
	if length > 1<<20 {
		length = 1 << 20
	}
	payload := make([]byte, length)
	if _, err := f.ReadAt(payload, off); err != nil {
		return ""
	}
	return parseTiffDate(tiffFromExifItem(payload))
}

// heifBoxMax bounds the metadata boxes this parser will buffer. Box sizes
// come from the file itself, so a corrupt or hostile media file could
// otherwise declare a multi-gigabyte iinf/iloc and have the scanner allocate
// it. Real ones are kilobytes.
const heifBoxMax = 8 << 20

// findBox scans [start,end) for a top-level box of the given type and
// returns its payload bounds.
func findBox(f *os.File, start, end int64, typ string) (int64, int64, bool) {
	off := start
	var hdr [16]byte
	for off+8 <= end {
		if _, err := f.ReadAt(hdr[:8], off); err != nil {
			return 0, 0, false
		}
		size := int64(binary.BigEndian.Uint32(hdr[0:4]))
		name := string(hdr[4:8])
		payload := off + 8
		if size == 1 {
			if _, err := f.ReadAt(hdr[8:16], off+8); err != nil {
				return 0, 0, false
			}
			size = int64(binary.BigEndian.Uint64(hdr[8:16]))
			payload = off + 16
		} else if size == 0 {
			size = end - off
		}
		// Box sizes come from the file. A hostile or corrupt one can declare
		// a 64-bit size smaller than the 16-byte header it just used, or one
		// whose int64 form is negative — either way the payload would start
		// AFTER the box ends, and every caller computes a length by
		// subtracting the two. A negative length passes an upper-bound check
		// and then panics in make([]byte, n), taking the scan down with it.
		if size < 8 || off+size > end || payload > off+size {
			return 0, 0, false
		}
		if name == typ {
			return payload, off + size, true
		}
		off += size
	}
	return 0, 0, false
}

// heifExifItemID finds the item ID of the "Exif" item in the iinf box.
func heifExifItemID(f *os.File, metaStart, metaEnd int64) (uint32, bool) {
	iinfStart, iinfEnd, ok := findBox(f, metaStart, metaEnd, "iinf")
	if !ok {
		return 0, false
	}
	if n := iinfEnd - iinfStart; n <= 0 || n > heifBoxMax {
		return 0, false
	}
	buf := make([]byte, iinfEnd-iinfStart)
	if _, err := f.ReadAt(buf, iinfStart); err != nil {
		return 0, false
	}
	if len(buf) < 6 {
		return 0, false
	}
	ver := buf[0]
	pos := 4
	if ver == 0 {
		pos += 2 // uint16 entry_count
	} else {
		pos += 4
	}
	// entries are 'infe' child boxes
	for pos+8 <= len(buf) {
		size := int(binary.BigEndian.Uint32(buf[pos : pos+4]))
		name := string(buf[pos+4 : pos+8])
		if size < 8 || pos+size > len(buf) {
			return 0, false
		}
		if name == "infe" && size >= 16 {
			b := buf[pos+8 : pos+size]
			iv := b[0]
			var itemID uint32
			var typOff int
			switch {
			case iv == 2 && len(b) >= 12:
				itemID = uint32(binary.BigEndian.Uint16(b[4:6]))
				typOff = 8
			case iv >= 3 && len(b) >= 14:
				itemID = binary.BigEndian.Uint32(b[4:8])
				typOff = 10
			default:
				typOff = -1
			}
			if typOff > 0 && len(b) >= typOff+4 &&
				string(b[typOff:typOff+4]) == "Exif" {
				return itemID, true
			}
		}
		pos += size
	}
	return 0, false
}

// heifItemLocation returns the absolute file offset and length of an item's
// first extent from the iloc box (construction method 0 / file offsets).
func heifItemLocation(f *os.File, metaStart, metaEnd int64, wantID uint32) (int64, int64, bool) {
	ilocStart, ilocEnd, ok := findBox(f, metaStart, metaEnd, "iloc")
	if !ok {
		return 0, 0, false
	}
	if n := ilocEnd - ilocStart; n <= 0 || n > heifBoxMax {
		return 0, 0, false
	}
	buf := make([]byte, ilocEnd-ilocStart)
	if _, err := f.ReadAt(buf, ilocStart); err != nil {
		return 0, 0, false
	}
	if len(buf) < 8 {
		return 0, 0, false
	}
	ver := buf[0]
	offSize := int(buf[4] >> 4)
	lenSize := int(buf[4] & 0x0F)
	baseSize := int(buf[5] >> 4)
	idxSize := 0
	if ver == 1 || ver == 2 {
		idxSize = int(buf[5] & 0x0F)
	}
	pos := 6
	var count uint32
	if ver < 2 {
		if len(buf) < pos+2 {
			return 0, 0, false
		}
		count = uint32(binary.BigEndian.Uint16(buf[pos : pos+2]))
		pos += 2
	} else {
		if len(buf) < pos+4 {
			return 0, 0, false
		}
		count = binary.BigEndian.Uint32(buf[pos : pos+4])
		pos += 4
	}
	readUint := func(n int) (uint64, bool) {
		if n == 0 {
			return 0, true
		}
		if pos+n > len(buf) {
			return 0, false
		}
		var v uint64
		for i := 0; i < n; i++ {
			v = v<<8 | uint64(buf[pos+i])
		}
		pos += n
		return v, true
	}
	for i := uint32(0); i < count; i++ {
		var itemID uint32
		if ver < 2 {
			v, ok2 := readUint(2)
			if !ok2 {
				return 0, 0, false
			}
			itemID = uint32(v)
		} else {
			v, ok2 := readUint(4)
			if !ok2 {
				return 0, 0, false
			}
			itemID = uint32(v)
		}
		method := uint64(0)
		if ver == 1 || ver == 2 {
			m, ok2 := readUint(2)
			if !ok2 {
				return 0, 0, false
			}
			method = m & 0x0F
		}
		if _, ok2 := readUint(2); !ok2 { // data_reference_index
			return 0, 0, false
		}
		base, ok2 := readUint(baseSize)
		if !ok2 {
			return 0, 0, false
		}
		extents, ok2 := readUint(2)
		if !ok2 {
			return 0, 0, false
		}
		var firstOff, firstLen uint64
		for e := uint64(0); e < extents; e++ {
			if _, ok3 := readUint(idxSize); !ok3 {
				return 0, 0, false
			}
			eo, ok3 := readUint(offSize)
			if !ok3 {
				return 0, 0, false
			}
			el, ok3 := readUint(lenSize)
			if !ok3 {
				return 0, 0, false
			}
			if e == 0 {
				firstOff, firstLen = eo, el
			}
		}
		if itemID == wantID && method == 0 && extents > 0 {
			return int64(base + firstOff), int64(firstLen), true
		}
	}
	return 0, 0, false
}

// tiffFromExifItem locates the TIFF header inside a HEIF Exif item payload
// (4-byte header offset, then usually "Exif\0\0" + TIFF). Scanning for the
// byte-order mark tolerates both spec interpretations seen in the wild.
func tiffFromExifItem(p []byte) []byte {
	limit := len(p)
	if limit > 64 {
		limit = 64
	}
	for i := 0; i+4 <= limit; i++ {
		if (p[i] == 'M' && p[i+1] == 'M' && p[i+2] == 0 && p[i+3] == 42) ||
			(p[i] == 'I' && p[i+1] == 'I' && p[i+2] == 42 && p[i+3] == 0) {
			return p[i:]
		}
	}
	// fall back to the declared offset field
	if len(p) > 4 {
		off := int(binary.BigEndian.Uint32(p[0:4]))
		// off < len(p)-4, never 4+off < len(p): on the 32-bit armv7 build
		// `4+off` wraps negative for offsets just under 2^31, which passes
		// the comparison and then indexes with a negative bound. The
		// enclosing len(p) > 4 keeps the subtraction positive.
		if off >= 0 && off < len(p)-4 {
			rest := p[4+off:]
			if i := bytes.Index(rest[:min(len(rest), 32)], []byte("MM\x00\x2a")); i >= 0 {
				return rest[i:]
			}
			if i := bytes.Index(rest[:min(len(rest), 32)], []byte("II\x2a\x00")); i >= 0 {
				return rest[i:]
			}
		}
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
