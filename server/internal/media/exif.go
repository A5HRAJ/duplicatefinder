package media

import (
	"encoding/binary"
	"os"
	"strings"
	"time"
)

// Captured returns the capture date ("YYYY-MM-DD HH:MM:SS") recorded in a
// media file, or "" when there is none: EXIF DateTimeOriginal for JPEG,
// the TIFF-family raws and HEIF/HEIC/AVIF, the QuickTime creation date for
// MOV/MP4/M4V. The format is chosen by the file's extension; the EXIF walk
// is deliberately minimal (IFD0 → Exif IFD, tags 0x9003/0x9004/0x0132). open
// is the caller's opener for the file — the scanner passes its
// pinned-handle opener, so metadata is read from inside the vetted tree,
// never through a raw path.
func Captured(open func() (*os.File, error), name string) string {
	l := strings.ToLower(name)
	isJpeg := strings.HasSuffix(l, ".jpg") || strings.HasSuffix(l, ".jpeg")
	isTiff := strings.HasSuffix(l, ".tif") || strings.HasSuffix(l, ".tiff") ||
		strings.HasSuffix(l, ".dng") || strings.HasSuffix(l, ".nef") ||
		strings.HasSuffix(l, ".cr2") || strings.HasSuffix(l, ".arw")
	if strings.HasSuffix(l, ".heic") || strings.HasSuffix(l, ".heif") ||
		strings.HasSuffix(l, ".hif") || strings.HasSuffix(l, ".avif") {
		return heifCaptured(open)
	}
	if strings.HasSuffix(l, ".mov") || strings.HasSuffix(l, ".mp4") ||
		strings.HasSuffix(l, ".m4v") {
		return qtCaptured(open)
	}
	if !isJpeg && !isTiff {
		return ""
	}
	f, err := open()
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 256*1024)
	n, _ := f.Read(head)
	head = head[:n]

	var tiff []byte
	if isTiff {
		tiff = head
	} else {
		tiff = findExifInJpeg(head)
	}
	if tiff == nil {
		return ""
	}
	return parseTiffDate(tiff)
}

// findExifInJpeg locates the TIFF block inside a JPEG APP1 "Exif" segment.
func findExifInJpeg(b []byte) []byte {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return nil
	}
	i := 2
	for i+4 <= len(b) {
		if b[i] != 0xFF {
			return nil
		}
		marker := b[i+1]
		if marker == 0xD8 || (marker >= 0xD0 && marker <= 0xD9) {
			i += 2
			continue
		}
		if i+4 > len(b) {
			return nil
		}
		segLen := int(binary.BigEndian.Uint16(b[i+2 : i+4]))
		if segLen < 2 {
			return nil
		}
		if marker == 0xE1 && i+4+6 <= len(b) && string(b[i+4:i+10]) == "Exif\x00\x00" {
			// The segment must be long enough to hold the signature just
			// matched: segLen counts its own 2 bytes plus the 6 of
			// "Exif\0\0", so anything below 8 ends BEFORE the low bound
			// below. `end` is only ever clamped downwards, so a truncated
			// or byte-flipped length would make b[i+10:end] a low>high
			// slice — a panic that runScan's recover would turn into a
			// whole scan's results being discarded, on every rescan.
			if segLen < 8 {
				return nil
			}
			end := i + 2 + segLen
			if end > len(b) {
				end = len(b)
			}
			return b[i+10 : end]
		}
		if marker == 0xDA { // start of scan — no EXIF before image data
			return nil
		}
		i += 2 + segLen
	}
	return nil
}

func parseTiffDate(t []byte) string {
	if len(t) < 8 {
		return ""
	}
	var bo binary.ByteOrder
	switch string(t[0:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return ""
	}
	if bo.Uint16(t[2:4]) != 42 {
		return ""
	}
	ifd0 := int(bo.Uint32(t[4:8]))
	dateTime := "" // tag 0x0132 fallback
	exifOff := 0

	// Every bound below is written as a SUBTRACTION from len(t), never as an
	// addition compared against it. build.sh ships a GOARCH=arm binary, where
	// int is 32 bits: file-supplied offsets and counts are near 2^31 for free,
	// and `off+n > len(t)` wraps negative on exactly the values an attacker (or
	// a corrupt sensor dump) would pick, passing the guard and then panicking
	// in the slice expression. len(t) >= 8 is guaranteed above.
	readIFD := func(off int, want map[uint16]bool, out map[uint16]string) {
		if off < 0 || off > len(t)-2 {
			return
		}
		n := int(bo.Uint16(t[off : off+2]))
		if n > 512 {
			return
		}
		for i := 0; i < n; i++ {
			e := off + 2 + i*12
			if e+12 > len(t) {
				return
			}
			tag := bo.Uint16(t[e : e+2])
			if !want[tag] {
				continue
			}
			typ := bo.Uint16(t[e+2 : e+4])
			cnt := int(bo.Uint32(t[e+4 : e+8]))
			if tag == 0x8769 { // Exif IFD pointer
				exifOff = int(bo.Uint32(t[e+8 : e+12]))
				continue
			}
			if typ != 2 || cnt < 10 { // ASCII date "YYYY:MM:DD HH:MM:SS"
				continue
			}
			valOff := e + 8
			if cnt > 4 {
				valOff = int(bo.Uint32(t[e+8 : e+12]))
			}
			if valOff < 0 || cnt < 0 || valOff > len(t) || cnt > len(t)-valOff {
				continue
			}
			out[tag] = strings.TrimRight(string(t[valOff:valOff+cnt]), "\x00")
		}
	}

	vals := map[uint16]string{}
	readIFD(ifd0, map[uint16]bool{0x8769: true, 0x0132: true}, vals)
	dateTime = vals[0x0132]
	if exifOff > 0 {
		readIFD(exifOff, map[uint16]bool{0x9003: true, 0x9004: true}, vals)
	}
	raw := vals[0x9003]
	if raw == "" {
		raw = vals[0x9004]
	}
	if raw == "" {
		raw = dateTime
	}
	if raw == "" {
		return ""
	}
	ts, err := time.Parse("2006:01:02 15:04:05", strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return captureDate(ts)
}
