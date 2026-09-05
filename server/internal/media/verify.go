// Package media reads the metadata and checks the structure of the file
// formats Duplicate Finder understands, from bytes it does not control: EXIF
// capture dates in JPEG, the TIFF-family raws, HEIF and QuickTime, and the
// container validators behind the conflicting-files verdicts. Every reader
// here bounds its allocations and loops, and expresses bounds as
// subtractions so they cannot overflow on the 32-bit ARM build.
package media

// Corruption evidence — the checks that answer "which of these copies is the
// damaged one?" once the scan has found files claiming to be the same file
// (identical size, identical modified time) whose contents disagree.
//
// Two families, in order of how much they prove:
//
//  1. Self-validating containers. PNG carries a CRC32 on every chunk, gzip a
//     CRC32 of the whole stream, and every ZIP member (so also .docx/.xlsx/
//     .odt/.jar/.apk) its own. These verify ONE file against itself, with no
//     twin needed — the only thing that can settle the common two-copy case,
//     where there is no majority to count and the hashes alone say only that
//     somebody is wrong.
//  2. Structural walks. JPEG, PDF and the ISO base-media formats (MP4/MOV/
//     HEIC/AVIF) carry no content checksum, but their framing has to tile
//     exactly to the end of the file. A box tree that runs off the end, or a
//     JPEG with no end-of-image marker, is a truncated transfer.
//
// A validator never reports "intact" on its own: a file can satisfy every
// checksum it carries and still be the wrong bytes, and most formats carry
// none at all. It reports DAMAGE, and its silence becomes evidence only
// alongside a sibling that is positively damaged.
//
// All of this runs on the scan goroutine — runScan's deferred recover is the
// daemon's only one, and parallelEach's workers have none (a malformed-input
// panic in a parser would be a daemon crash, not a scan error). It runs only
// for sets already known to differ, so the population is rare by construction
// and the second read it costs is bounded by that rarity, not by volume size.

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/bits"
	"os"
)

// Intactness is what one file's own structure says about it. Deliberately
// three-valued: "no validator covers this type" and "this file checks out"
// must never collapse together, or every unrecognised extension would read as
// a clean bill of health.
type Intactness int

const (
	Unproven Intactness = iota // no validator for this type, or it could not finish
	Proven                     // structure walked cleanly and every embedded checksum agreed
	Damaged                    // structure or a checksum is definitively wrong
)

// ErrCancelled is returned by CompareContent when the scan's cancel channel
// closes mid-read. The scanner shares this value for its own reads, so a
// single errors.Is answers "was this a cancel?" for either.
var ErrCancelled = errors.New("cancelled")

// maxVerifyBytes bounds the validators that must decompress to check a
// checksum. Past it the file is left unproven rather than spending minutes of
// a scan on one member of one set; the structural walks have no such limit
// because they only read framing.
const maxVerifyBytes = 512 << 20

// maxInflate bounds decompressed output, so a zip bomb inside a corrupted set
// cannot turn a verdict into an outage. Hitting it yields unproven — the check
// did not fail, it did not finish.
const maxInflate = 8 << 30

// minZeroBytes is the smallest run of zeros that may be read as an interrupted
// copy or a lost block. Storage does not lose data in ones and twos: a sector
// is 512 bytes, and anything smaller is a bit-level event, not a missing
// allocation. The floor exists because aAllZero/bAllZero mean only "every
// byte where the copies differ is NUL on this side" — with no floor a single
// differing byte satisfies that, and the verdict comes out BACKWARDS: rot that
// turns a 0x00 into a non-NUL byte leaves the healthy copy holding the zero,
// so the healthy copy would be convicted and the rotted one called Intact.
const minZeroBytes = 512

// VerifyContent walks one file's container and reports what its own structure
// proves. The magic bytes alone select the validator — never the file name,
// because the whole point here is a file whose contents may not match its
// name. The returned string is user-facing evidence and is empty unless the
// verdict is meaningful.
func VerifyContent(open func() (*os.File, error), size int64) (Intactness, string) {
	if size <= 0 {
		return Unproven, ""
	}
	f, err := open()
	if err != nil {
		return Unproven, ""
	}
	defer f.Close()

	var head [16]byte
	n, _ := io.ReadFull(f, head[:])
	magic := head[:n]

	switch {
	case bytes.HasPrefix(magic, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return verifyPNG(f, size)
	// CM (byte 3) must be 8 — deflate is the only method gzip defines. Every
	// other arm here matches on 3–8 bytes; matching gzip on 2 would route any
	// file that happens to begin 1F 8B into a validator that then convicts it.
	case bytes.HasPrefix(magic, []byte{0x1F, 0x8B, 0x08}):
		return verifyGzip(f, size)
	case bytes.HasPrefix(magic, []byte{'P', 'K', 0x03, 0x04}),
		bytes.HasPrefix(magic, []byte{'P', 'K', 0x05, 0x06}):
		return verifyZip(f, size)
	case bytes.HasPrefix(magic, []byte{0xFF, 0xD8, 0xFF}):
		return verifyJPEG(f, size)
	case bytes.HasPrefix(magic, []byte("%PDF-")):
		return verifyPDF(f, size)
	case len(magic) >= 12 && string(magic[4:8]) == "ftyp":
		return verifyISOBMFF(f, size)
	}
	// GIF and the TIFF family carry no checksum and no end marker worth
	// testing, so they deliberately have no arm above.
	return Unproven, ""
}

// verifyPNG walks the chunk list and recomputes every CRC32. This is the
// strongest check available anywhere in this file: a PNG proves its own
// integrity byte for byte, with no second copy to compare against.
func verifyPNG(f *os.File, size int64) (Intactness, string) {
	pos := int64(8)
	sawIHDR, sawIEND := false, false
	crcBuf := make([]byte, 64<<10)
	for pos < size {
		var hdr [8]byte
		if _, err := f.ReadAt(hdr[:], pos); err != nil {
			return Damaged, "PNG chunk list ends mid-header — the file is truncated"
		}
		clen := int64(binary.BigEndian.Uint32(hdr[0:4]))
		ctype := string(hdr[4:8])
		// Overflow-safe: on 32-bit ARM builds pos+clen could wrap, so every
		// bound is expressed as a subtraction against what is left.
		if clen < 0 || clen > size-pos-12 {
			return Damaged, fmt.Sprintf("PNG chunk %q claims %d bytes but only %d remain — the file is truncated", ctype, clen, size-pos-12)
		}
		h := crc32.NewIEEE()
		h.Write(hdr[4:8])
		rest := clen
		off := pos + 8
		for rest > 0 {
			nb := int64(len(crcBuf))
			if rest < nb {
				nb = rest
			}
			got, err := f.ReadAt(crcBuf[:nb], off)
			if got > 0 {
				h.Write(crcBuf[:got])
			}
			if err != nil {
				return Damaged, "PNG data ends early — the file is truncated"
			}
			off += int64(got)
			rest -= int64(got)
		}
		var want [4]byte
		if _, err := f.ReadAt(want[:], pos+8+clen); err != nil {
			return Damaged, "PNG chunk checksum is missing — the file is truncated"
		}
		if h.Sum32() != binary.BigEndian.Uint32(want[:]) {
			return Damaged, fmt.Sprintf("PNG chunk %q fails its own CRC32 at offset %d — the stored bytes are damaged", ctype, pos)
		}
		switch ctype {
		case "IHDR":
			sawIHDR = true
		case "IEND":
			sawIEND = true
		}
		pos += 12 + clen
		if sawIEND {
			break
		}
	}
	if !sawIHDR || !sawIEND {
		return Damaged, "PNG is missing its end-of-image chunk — the file is truncated"
	}
	return Proven, "every PNG chunk matches its own CRC32"
}

// verifyGzip inflates the stream so the trailing CRC32 and length are checked.
// gzip.Reader reports both as ErrChecksum at EOF.
func verifyGzip(f *os.File, size int64) (Intactness, string) {
	if size > maxVerifyBytes {
		return Unproven, ""
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Unproven, ""
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		// NO header failure convicts. 1F 8B 08 occurs by chance in files that
		// are not gzip at all, and such a file is indistinguishable from a
		// genuinely truncated one: junk in the FLG byte sends the reader
		// hunting an FEXTRA field that runs off the end, which surfaces as
		// ErrUnexpectedEOF and reads exactly like truncation. Since a false
		// conviction is the worst outcome this file can produce, the header
		// only ever decides whether there is a stream worth checking. Real
		// damage is still caught below, where the CRC32 and ISIZE are.
		return Unproven, ""
	}
	defer zr.Close()
	n, err := io.Copy(io.Discard, io.LimitReader(zr, maxInflate))
	switch {
	case errors.Is(err, gzip.ErrChecksum):
		return Damaged, "gzip stream fails its own CRC32 — the stored bytes are damaged"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return Damaged, "gzip stream ends early — the file is truncated"
	case err != nil:
		return Damaged, "gzip stream cannot be decompressed — the file is damaged"
	case n >= maxInflate:
		return Unproven, ""
	}
	return Proven, "gzip stream matches its own CRC32"
}

// verifyZip checks every member's CRC32. Covers .zip and everything built on
// it — .docx, .xlsx, .pptx, .odt, .jar, .apk — which between them are most of
// the non-media documents on a NAS.
func verifyZip(f *os.File, size int64) (Intactness, string) {
	if size > maxVerifyBytes {
		return Unproven, ""
	}
	// archive/zip materialises one File per central-directory record before
	// anything is checked, so a directory-only archive near maxVerifyBytes
	// would cost gigabytes of heap on the scan goroutine. Read the entry
	// count out of the end-of-central-directory record first.
	if n, ok := zipEntryCount(f, size); ok && n > maxZipEntries {
		return Unproven, ""
	}
	zr, err := zip.NewReader(f, size)
	if err != nil {
		return Damaged, "ZIP directory is unreadable — the archive is damaged or truncated"
	}
	var total int64
	for _, m := range zr.File {
		// "I cannot read this" is not "this is damaged". archive/zip implements
		// only Store and Deflate and never looks at the encryption flag, so a
		// bzip2 (12), LZMA (14), zstd, XZ, PPMd or password-protected member —
		// every one of which `unzip -t` clears — would otherwise be convicted.
		// Doubly wrong: a damaged verdict at this rung SHORT-CIRCUITS the byte
		// comparison that would find the copy actually at fault.
		if m.Flags&0x1 != 0 {
			return Unproven, "" // encrypted payload: nothing here can check it
		}
		rc, err := m.Open()
		if err != nil {
			if errors.Is(err, zip.ErrAlgorithm) {
				return Unproven, "" // compression method this reader lacks
			}
			return Damaged, fmt.Sprintf("ZIP entry %q cannot be opened — the archive is damaged", m.Name)
		}
		n, err := io.Copy(io.Discard, io.LimitReader(rc, maxInflate-total))
		rc.Close()
		total += n
		switch {
		case errors.Is(err, zip.ErrChecksum):
			return Damaged, fmt.Sprintf("ZIP entry %q fails its own CRC32 — the stored bytes are damaged", m.Name)
		case errors.Is(err, io.ErrUnexpectedEOF):
			return Damaged, fmt.Sprintf("ZIP entry %q ends early — the archive is truncated", m.Name)
		case errors.Is(err, zip.ErrAlgorithm):
			return Unproven, "" // surfaced at read time rather than at Open
		case err != nil:
			return Damaged, fmt.Sprintf("ZIP entry %q cannot be decompressed — the archive is damaged", m.Name)
		}
		if total >= maxInflate {
			return Unproven, ""
		}
	}
	if len(zr.File) == 0 {
		return Unproven, ""
	}
	return Proven, "every ZIP entry matches its own CRC32"
}

// maxZipEntries bounds the archives verifyZip will open: past it the archive
// is left unproven rather than decoded into memory.
const maxZipEntries = 1 << 18

// zipEntryCount reads the total entry count from the end-of-central-directory
// record (and the ZIP64 record it points at when the 16-bit field is
// saturated). ok is false when no record is found — that case is left to
// archive/zip, which reports it as damage.
func zipEntryCount(f *os.File, size int64) (uint64, bool) {
	// The EOCD is 22 bytes plus a comment of at most 65535 bytes.
	tail := int64(22 + 65535)
	if tail > size {
		tail = size
	}
	if tail < 22 {
		return 0, false
	}
	buf := make([]byte, tail)
	if _, err := f.ReadAt(buf, size-tail); err != nil && err != io.EOF {
		return 0, false
	}
	sig := []byte{'P', 'K', 0x05, 0x06}
	i := bytes.LastIndex(buf, sig)
	if i < 0 || i+22 > len(buf) {
		return 0, false
	}
	n := uint64(binary.LittleEndian.Uint16(buf[i+10 : i+12]))
	if n != 0xFFFF {
		return n, true
	}
	// ZIP64: the locator sits 20 bytes before the EOCD and names the offset of
	// the ZIP64 EOCD record, whose total-entries field is at +32.
	if i < 20 || string(buf[i-20:i-16]) != "PK\x06\x07" {
		return n, true
	}
	off := int64(binary.LittleEndian.Uint64(buf[i-12 : i-4]))
	if off < 0 || off+40 > size {
		return n, true
	}
	var rec [40]byte
	if _, err := f.ReadAt(rec[:], off); err != nil || string(rec[0:4]) != "PK\x06\x06" {
		return n, true
	}
	return binary.LittleEndian.Uint64(rec[32:40]), true
}

// verifyJPEG walks the marker segments to the start of scan, then confirms the
// end-of-image marker is present. JPEG carries no checksum, so this catches
// the truncation half of the problem and nothing else — which is why a clean
// walk returns proven only in the weak sense the caller treats it as.
func verifyJPEG(f *os.File, size int64) (Intactness, string) {
	pos := int64(2)
	for pos < size-1 {
		var m [4]byte
		if _, err := f.ReadAt(m[:2], pos); err != nil {
			return Damaged, "JPEG segment list ends early — the file is truncated"
		}
		if m[0] != 0xFF {
			return Damaged, fmt.Sprintf("JPEG segment framing breaks at offset %d — the file is damaged", pos)
		}
		mk := m[1]
		// 0xFF is a legal fill byte before a marker, not a marker code. Reading
		// it as one convicts a perfectly valid file.
		if mk == 0xFF {
			pos++
			continue
		}
		// Start of scan: entropy-coded data follows and is not length-framed,
		// so the walk stops and the end marker becomes the remaining test.
		if mk == 0xDA {
			break
		}
		// Standalone markers carry no length payload.
		if mk == 0x01 || (mk >= 0xD0 && mk <= 0xD9) {
			pos += 2
			continue
		}
		if _, err := f.ReadAt(m[2:4], pos+2); err != nil {
			return Damaged, "JPEG segment length is missing — the file is truncated"
		}
		seg := int64(binary.BigEndian.Uint16(m[2:4]))
		if seg < 2 || seg > size-pos-2 {
			return Damaged, fmt.Sprintf("JPEG segment at offset %d claims more bytes than the file holds — it is truncated", pos)
		}
		pos += 2 + seg
	}
	// The end-of-image marker is searched for across the WHOLE file, backwards,
	// not in a fixed tail window. Phone photos are the reason: a Google/Samsung
	// Motion Photo or an Apple Live Photo is a complete JPEG with an entire MP4
	// appended after its EOI, which on a NAS full of phone backups is an
	// ordinary file and not a rare one. A tail-window check calls every one of
	// them truncated — and because a damaged verdict here stops the ladder,
	// that false conviction would also suppress the comparison that finds the
	// genuinely damaged copy.
	const back = 1 << 20
	buf := make([]byte, back+1)
	for end := size; end > 0; {
		start := end - back
		if start < 0 {
			start = 0
		}
		n := int(end - start)
		if end < size {
			n++ // one byte of overlap, so a marker split across the seam is seen
		}
		if _, err := f.ReadAt(buf[:n], start); err != nil {
			return Unproven, ""
		}
		if bytes.Contains(buf[:n], []byte{0xFF, 0xD9}) {
			return Proven, "JPEG segment structure is intact and the end-of-image marker is present"
		}
		end = start
	}
	return Damaged, "JPEG has no end-of-image marker — the file is truncated"
}

// verifyPDF checks the header and the trailer. PDFs are routinely appended to
// (incremental updates), so only the two anchors are safe to insist on.
func verifyPDF(f *os.File, size int64) (Intactness, string) {
	tail := int64(2 << 10)
	if tail > size {
		tail = size
	}
	buf := make([]byte, tail)
	if _, err := f.ReadAt(buf, size-tail); err != nil {
		return Unproven, ""
	}
	if !bytes.Contains(buf, []byte("%%EOF")) {
		return Damaged, "PDF has no end-of-file marker — the file is truncated"
	}
	return Proven, "PDF header and end-of-file marker are both present"
}

// verifyISOBMFF walks the top-level box tree of the ISO base media formats —
// MP4, MOV, HEIC, AVIF. The boxes must tile the file exactly; anything else
// means bytes are missing.
func verifyISOBMFF(f *os.File, size int64) (Intactness, string) {
	pos := int64(0)
	boxes := 0
	for pos < size {
		if size-pos < 8 {
			return Damaged, fmt.Sprintf("media box list ends with %d stray bytes — the file is truncated", size-pos)
		}
		var hdr [16]byte
		if _, err := f.ReadAt(hdr[:8], pos); err != nil {
			return Damaged, "media box header is unreadable — the file is truncated"
		}
		bs := int64(binary.BigEndian.Uint32(hdr[0:4]))
		btype := string(hdr[4:8])
		hlen := int64(8)
		switch bs {
		case 0:
			// Box runs to end of file; legal only as the last box.
			bs = size - pos
		case 1:
			if size-pos < 16 {
				return Damaged, "media box claims a 64-bit size the file cannot hold — it is truncated"
			}
			if _, err := f.ReadAt(hdr[8:16], pos+8); err != nil {
				return Damaged, "media box size is unreadable — the file is truncated"
			}
			u := binary.BigEndian.Uint64(hdr[8:16])
			if u > uint64(size-pos) {
				return Damaged, fmt.Sprintf("media box %q claims %d bytes but only %d remain — the file is truncated", btype, u, size-pos)
			}
			bs = int64(u)
			hlen = 16
		}
		if bs < hlen || bs > size-pos {
			return Damaged, fmt.Sprintf("media box %q claims %d bytes but only %d remain — the file is truncated", btype, bs, size-pos)
		}
		boxes++
		pos += bs
	}
	if boxes == 0 {
		return Unproven, ""
	}
	return Proven, "media box structure tiles the file exactly"
}

// ---------------------------------------------------------- content compare

// DiffShape describes HOW two copies differ, which is often enough to say
// which one is wrong even when nothing else can. The three shapes worth
// telling apart:
//
//   - a run of NULs on one side where the other has data — an interrupted copy
//     or a restore that lost blocks, and the NUL side is the damaged one;
//   - a single flipped bit — classic bit rot or bad memory, but symmetric:
//     it says the set is genuinely damaged without saying which side;
//   - anything larger and non-NUL — two different files that merely share a
//     size and a timestamp, which is not corruption at all.
type DiffShape struct {
	Kind     string // "zeros", "tail", "bitflip", "mixed"
	ZeroSide int    // 0 or 1 for the side whose differing bytes are all NUL; -1 if neither
	FirstAt  int64  // offset of the first differing byte
	Bytes    int64  // how many bytes differ
	BitsSet  int    // popcount of the difference, meaningful only when bytes is tiny
}

// CompareContent reads two same-size files in lockstep and classifies their
// difference. It is the one check here that costs a second full read of two
// files, so the caller runs it only on sets already confirmed to differ and
// only when the cheaper evidence came back silent.
func CompareContent(aOpen, bOpen func() (*os.File, error), size int64, cancel <-chan struct{}) (*DiffShape, error) {
	af, err := aOpen()
	if err != nil {
		return nil, err
	}
	defer af.Close()
	bf, err := bOpen()
	if err != nil {
		return nil, err
	}
	defer bf.Close()

	const chunk = 256 << 10
	ab, bb := make([]byte, chunk), make([]byte, chunk)
	d := &DiffShape{ZeroSide: -1, FirstAt: -1}
	aAllZero, bAllZero := true, true
	var off, lastDiff int64
	for {
		select {
		case <-cancel:
			return nil, ErrCancelled
		default:
		}
		an, aerr := io.ReadFull(af, ab)
		bn, berr := io.ReadFull(bf, bb)
		n := an
		if bn < n {
			n = bn
		}
		for i := 0; i < n; i++ {
			if ab[i] == bb[i] {
				continue
			}
			if d.FirstAt < 0 {
				d.FirstAt = off + int64(i)
			}
			lastDiff = off + int64(i)
			d.Bytes++
			if ab[i] != 0 {
				aAllZero = false
			}
			if bb[i] != 0 {
				bAllZero = false
			}
			if d.Bytes <= 64 {
				d.BitsSet += bits.OnesCount8(ab[i] ^ bb[i])
			}
		}
		off += int64(n)
		if aerr != nil || berr != nil || an != bn {
			break
		}
	}
	if d.Bytes == 0 {
		return nil, nil
	}
	switch {
	case d.Bytes <= 8 && d.BitsSet <= 2:
		d.Kind = "bitflip"
	// The floor is what keeps this arm honest — see minZeroBytes. Below it the
	// difference falls through to "mixed", which reports what was seen and
	// convicts nobody: the safe direction, since the bitflip arm above only
	// protects runs of 8 bytes or fewer carrying 2 bits or fewer.
	case aAllZero != bAllZero && d.Bytes >= minZeroBytes:
		d.Kind = "zeros"
		d.ZeroSide = 1
		if aAllZero {
			d.ZeroSide = 0
		}
		// A NUL run that reaches the end of the file is the signature of a
		// transfer that stopped and left the rest of the allocation empty.
		if lastDiff == size-1 {
			d.Kind = "tail"
		}
	default:
		d.Kind = "mixed"
	}
	return d, nil
}

// Describe renders a DiffShape as the evidence string shown against the file
// at index side.
func (d *DiffShape) Describe(side int) string {
	switch d.Kind {
	case "bitflip":
		bit := "bit"
		if d.BitsSet != 1 {
			bit = "bits"
		}
		return fmt.Sprintf("%d %s differ at offset %d — the signature of bit rot or faulty memory", d.BitsSet, bit, d.FirstAt)
	case "zeros", "tail":
		where := fmt.Sprintf("%s of zeros at offset %d", FmtBytes(d.Bytes), d.FirstAt)
		if d.Kind == "tail" {
			where = fmt.Sprintf("%s of zeros from offset %d to the end of the file", FmtBytes(d.Bytes), d.FirstAt)
		}
		if side == d.ZeroSide {
			return "holds " + where + " where the other copy holds data — an interrupted copy or lost blocks"
		}
		return "holds data where the other copy holds " + where
	}
	return fmt.Sprintf("%s of content differs from the other copy, starting at offset %d", FmtBytes(d.Bytes), d.FirstAt)
}

// FmtBytes is the daemon-side counterpart of the UI's fmtBytes, used only in
// evidence strings. It reproduces that function's rounding exactly — whole
// bytes, then two decimals below 10, one below 100, none above — because the
// evidence lands in a grid cell beside a Size column the UI formats, and two
// renderings of one number on one row read as a discrepancy.
func FmtBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	v, i := float64(n), 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	switch {
	case i == 0:
		return fmt.Sprintf("%d B", n)
	case v >= 100:
		return fmt.Sprintf("%.0f %s", v, units[i])
	case v >= 10:
		return fmt.Sprintf("%.1f %s", v, units[i])
	}
	return fmt.Sprintf("%.2f %s", v, units[i])
}
