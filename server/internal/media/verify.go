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
//  2. Decoders. A JPEG carries no checksum, but its entropy-coded image data
//     either decodes to the end or it does not: a complete decode covers
//     every byte of the payload, which is what lets a JPEG be called Proven.
//     A PDF's compressed streams are zlib streams, each closed by an Adler-32
//     checksum, so a PDF whose every stream is compressed proves itself the
//     same way the archive formats do.
//  3. Structural walks. PDF anchors and the ISO base-media box tree (MP4/MOV/
//     HEIC/AVIF) have to tile exactly to the end of the file, so they catch
//     truncation. They say nothing about the bytes inside, and so they can
//     only ever report DAMAGE — a clean walk stays Unproven.
//
// Proven therefore always means the check reached the payload. A validator
// never reports "intact" on its own even then: a file can satisfy every
// checksum it carries and still be the wrong bytes. Its silence becomes
// evidence only alongside a sibling that is positively damaged.
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
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image/jpeg"
	"io"
	"math/bits"
	"os"
	"strconv"
)

// Intactness is what one file's own structure says about it. Deliberately
// three-valued: "no validator covers this type" and "this file checks out"
// must never collapse together, or every unrecognised extension would read as
// a clean bill of health.
type Intactness int

const (
	Unproven Intactness = iota // no validator covers the payload, or the check could not finish
	Proven                     // a payload-covering check passed: every checksum agreed, or the image decoded completely
	Damaged                    // structure, a checksum or the decode is definitively wrong
)

// ErrCancelled is returned by CompareContent when the scan's cancel channel
// closes mid-read. The scanner shares this value for its own reads, so a
// single errors.Is answers "was this a cancel?" for either.
var ErrCancelled = errors.New("cancelled")

// maxInflate bounds decompressed output, so a zip bomb inside a corrupted set
// cannot turn a verdict into an outage. Hitting it yields unproven — the check
// did not fail, it did not finish. There is deliberately no bound on the size
// of the file itself: a check that stops at a size cap leaves exactly the
// large files unproven, and every validator here polls the scan's cancel
// channel, so a long check is one the user can stop.
const maxInflate = 8 << 30

// maxDecodePixels bounds the JPEG decode. Decoding materialises the image, so
// a 100-megapixel photo costs hundreds of megabytes on a NAS that may have
// half a gigabyte in total; past this the file is left unproven.
const maxDecodePixels = 50_000_000

// cancelReader fails a read as soon as the scan's cancel channel closes, so a
// decoder or decompressor deep inside a large file stops within one read.
type cancelReader struct {
	r      io.Reader
	cancel <-chan struct{}
}

func (c *cancelReader) Read(p []byte) (int, error) {
	select {
	case <-c.cancel:
		return 0, ErrCancelled
	default:
	}
	return c.r.Read(p)
}

func isCancelled(cancel <-chan struct{}) bool {
	select {
	case <-cancel:
		return true
	default:
		return false
	}
}

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
// verdict is meaningful. A closed cancel channel ends the check early with
// Unproven; nil means no cancellation.
func VerifyContent(open func() (*os.File, error), size int64, cancel <-chan struct{}) (Intactness, string) {
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
		return verifyPNG(f, size, cancel)
	// CM (byte 3) must be 8 — deflate is the only method gzip defines. Every
	// other arm here matches on 3–8 bytes; matching gzip on 2 would route any
	// file that happens to begin 1F 8B into a validator that then convicts it.
	case bytes.HasPrefix(magic, []byte{0x1F, 0x8B, 0x08}):
		return verifyGzip(f, size, cancel)
	case bytes.HasPrefix(magic, []byte{'P', 'K', 0x03, 0x04}),
		bytes.HasPrefix(magic, []byte{'P', 'K', 0x05, 0x06}):
		return verifyZip(f, size, cancel)
	case bytes.HasPrefix(magic, []byte{0xFF, 0xD8, 0xFF}):
		return verifyJPEG(f, size, cancel)
	case bytes.HasPrefix(magic, []byte("%PDF-")):
		return verifyPDF(f, size, cancel)
	case len(magic) >= 12 && string(magic[4:8]) == "ftyp":
		return verifyISOBMFF(f, size, cancel)
	}
	// GIF and the TIFF family carry no checksum and no end marker worth
	// testing, so they deliberately have no arm above.
	return Unproven, ""
}

// verifyPNG walks the chunk list and recomputes every CRC32. This is the
// strongest check available anywhere in this file: a PNG proves its own
// integrity byte for byte, with no second copy to compare against.
func verifyPNG(f *os.File, size int64, cancel <-chan struct{}) (Intactness, string) {
	pos := int64(8)
	sawIHDR, sawIEND := false, false
	crcBuf := make([]byte, 64<<10)
	for pos < size {
		if isCancelled(cancel) {
			return Unproven, ""
		}
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
func verifyGzip(f *os.File, size int64, cancel <-chan struct{}) (Intactness, string) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Unproven, ""
	}
	zr, err := gzip.NewReader(&cancelReader{f, cancel})
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
	case errors.Is(err, ErrCancelled):
		return Unproven, ""
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
func verifyZip(f *os.File, size int64, cancel <-chan struct{}) (Intactness, string) {
	// archive/zip materialises one File per central-directory record before
	// anything is checked, so an archive holding millions of entries would
	// cost gigabytes of heap on the scan goroutine. Read the entry count out
	// of the end-of-central-directory record first.
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
		n, err := io.Copy(io.Discard, io.LimitReader(&cancelReader{rc, cancel}, maxInflate-total))
		rc.Close()
		total += n
		switch {
		case errors.Is(err, ErrCancelled):
			return Unproven, ""
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

// verifyJPEG walks the marker segments to the start of scan, then decodes the
// image completely. JPEG carries no checksum, but the entropy-coded data
// either decodes to its end or fails: a complete decode covers every byte of
// the payload, which is why a clean result here is Proven. The image is
// materialised by the decode, so very large pictures are left unproven rather
// than costing a small NAS its memory (maxDecodePixels).
func verifyJPEG(f io.ReaderAt, size int64, cancel <-chan struct{}) (Intactness, string) {
	pos := int64(2)
	for pos < size-1 {
		if isCancelled(cancel) {
			return Unproven, ""
		}
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
		// so the walk stops and the decode below takes over.
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
	// The decoder reads from the start and stops at the end-of-image marker,
	// so whatever follows it is ignored. Phone photos are the reason that
	// matters: a Google/Samsung Motion Photo or an Apple Live Photo is a
	// complete JPEG with an entire MP4 appended after its EOI, and on a NAS
	// full of phone backups that is an ordinary file.
	cfg, err := jpeg.DecodeConfig(&cancelReader{io.NewSectionReader(f, 0, size), cancel})
	if err != nil {
		return jpegVerdict(err)
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxDecodePixels {
		return Unproven, ""
	}
	if _, err := jpeg.Decode(&cancelReader{io.NewSectionReader(f, 0, size), cancel}); err != nil {
		return jpegVerdict(err)
	}
	return Proven, "the JPEG decodes completely, so its image data is consistent from start to end"
}

// jpegVerdict maps a decoder error to a verdict. Only what the decoder is
// SURE about convicts: a malformed stream or one that ends early. A variant
// the decoder does not implement (arithmetic coding, 12-bit samples, lossless
// JPEG) is left unproven — the check could not run, the file is not damaged.
func jpegVerdict(err error) (Intactness, string) {
	var unsupported jpeg.UnsupportedError
	switch {
	case errors.Is(err, ErrCancelled):
		return Unproven, ""
	case errors.As(err, &unsupported):
		return Unproven, ""
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return Damaged, "the JPEG image data ends before the picture is complete — the file is truncated"
	}
	var format jpeg.FormatError
	if errors.As(err, &format) {
		return Damaged, "the JPEG image data does not decode (" + string(format) + ") — the stored bytes are damaged"
	}
	return Unproven, ""
}

// verifyPDF checks the two anchors every PDF must have, then verifies every
// compressed stream in the file. A stream compressed with FlateDecode is a
// zlib stream, and zlib closes each stream with an Adler-32 checksum of the
// uncompressed data; a stream compressed with DCTDecode is an embedded JPEG
// and decodes like one. A PDF whose every stream is one of those proves its
// payload the way an archive does. Uncompressed streams, other filters and
// encrypted documents cannot be checked, and such a file stays unproven — but
// a checksum that fails, or a stream that ends early where the file itself
// says where it ends, still convicts.
func verifyPDF(f *os.File, size int64, cancel <-chan struct{}) (Intactness, string) {
	tail := int64(64 << 10)
	if tail > size {
		tail = size
	}
	tbuf := make([]byte, tail)
	if _, err := f.ReadAt(tbuf, size-tail); err != nil {
		return Unproven, ""
	}
	if !bytes.Contains(tbuf, []byte("%%EOF")) {
		return Damaged, "PDF has no end-of-file marker — the file is truncated"
	}
	p := pdfStreams{f: f, size: size, cancel: cancel}
	return p.verifyAll()
}

// pdfStreams walks a PDF's stream objects in file order. It is deliberately
// not a PDF parser: it finds each "stream" keyword, reads the object
// dictionary just before it for the filter and the declared length, locates
// the matching "endstream", checks the data, and skips past it — so binary
// stream data that happens to contain the word "stream" is never mistaken for
// a keyword.
type pdfStreams struct {
	f      *os.File
	size   int64
	cancel <-chan struct{}
}

// pdfScanChunk is how much of the file is searched at a time for the next
// keyword; the tail of one chunk is re-read at the head of the next so a
// keyword split across the seam is still seen.
const pdfScanChunk = 1 << 20

func (p *pdfStreams) verifyAll() (Intactness, string) {
	var total int64 // inflated bytes so far, against maxInflate
	streams, verified := 0, 0
	unverifiable := false
	buf := make([]byte, pdfScanChunk+16)
	pos := int64(0)
	for pos < p.size {
		if isCancelled(p.cancel) {
			return Unproven, ""
		}
		n := int64(len(buf))
		if p.size-pos < n {
			n = p.size - pos
		}
		if _, err := p.f.ReadAt(buf[:n], pos); err != nil && err != io.EOF {
			return Unproven, ""
		}
		// An encrypted document's streams are ciphertext, which no check here
		// can tell from damage. The marker can sit in any trailer or
		// cross-reference stream dictionary, so every chunk is checked.
		if bytes.Contains(buf[:n], []byte("/Encrypt")) {
			return Unproven, ""
		}
		i := indexStreamKeyword(buf[:n])
		if i < 0 {
			// No keyword in this chunk: step on, keeping the overlap.
			if pos+n >= p.size {
				break
			}
			pos += n - 16
			continue
		}
		kw := pos + int64(i)
		dataStart := kw + int64(len("stream"))
		// The keyword is followed by CRLF or LF; the data starts after it.
		switch {
		case bytes.HasPrefix(buf[i+6:n], []byte("\r\n")):
			dataStart += 2
		case bytes.HasPrefix(buf[i+6:n], []byte("\n")):
			dataStart++
		default:
			// "stream" not followed by an end of line is not the keyword (an
			// identifier such as /StreamType). Keep scanning past it.
			pos = kw + 6
			continue
		}
		dict := p.readBack(kw, 4<<10)
		length, lengthKnown := pdfDeclaredLength(dict)
		filter := pdfFilter(dict)
		end, confident := p.streamEnd(dataStart, length, lengthKnown)
		if end < 0 {
			return Unproven, "" // no endstream anywhere after it: not a stream we can bound
		}
		streams++
		switch filter {
		case "FlateDecode":
			st, why, inflated := p.verifyFlate(dataStart, end-dataStart, confident, maxInflate-total)
			total += inflated
			if st == Damaged {
				return Damaged, why
			}
			if st == Proven {
				verified++
			} else {
				unverifiable = true
			}
		case "DCTDecode":
			st, why := verifyJPEG(io.NewSectionReader(p.f, dataStart, end-dataStart), end-dataStart, p.cancel)
			if st == Damaged {
				return Damaged, "an image inside the PDF is damaged: " + why
			}
			if st == Proven {
				verified++
			} else {
				unverifiable = true
			}
		default:
			unverifiable = true
		}
		// Continue after "endstream".
		pos = end + int64(len("endstream"))
	}
	if streams == 0 || unverifiable || isCancelled(p.cancel) {
		return Unproven, ""
	}
	plural := "s"
	if verified == 1 {
		plural = ""
	}
	return Proven, fmt.Sprintf("every compressed stream in the PDF (%d stream%s) matches its own checksum", verified, plural)
}

// indexStreamKeyword finds the first "stream" keyword in b that is not the
// tail of "endstream". -1 when there is none.
func indexStreamKeyword(b []byte) int {
	from := 0
	for {
		i := bytes.Index(b[from:], []byte("stream"))
		if i < 0 {
			return -1
		}
		i += from
		if i >= 3 && string(b[i-3:i]) == "end" {
			from = i + 6
			continue
		}
		return i
	}
}

// readBack returns up to n bytes ending just before off: the object
// dictionary that precedes a stream keyword.
func (p *pdfStreams) readBack(off, n int64) []byte {
	if n > off {
		n = off
	}
	b := make([]byte, n)
	if _, err := p.f.ReadAt(b, off-n); err != nil && err != io.EOF {
		return nil
	}
	// Only the dictionary of THIS object: cut at the last "obj" keyword so a
	// previous object's /Filter is never read as this one's.
	if i := bytes.LastIndex(b, []byte(" obj")); i >= 0 {
		b = b[i:]
	}
	return b
}

// pdfDeclaredLength parses a direct "/Length N" from the dictionary. An
// indirect reference ("/Length 12 0 R") is reported as unknown.
func pdfDeclaredLength(dict []byte) (int64, bool) {
	i := bytes.LastIndex(dict, []byte("/Length"))
	if i < 0 {
		return 0, false
	}
	rest := bytes.TrimLeft(dict[i+len("/Length"):], " \t\r\n")
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	if j == 0 || j > 15 {
		return 0, false
	}
	after := bytes.TrimLeft(rest[j:], " \t\r\n")
	// "N 0 R" is a reference to another object, not a length.
	if len(after) > 0 && after[0] >= '0' && after[0] <= '9' {
		k := 0
		for k < len(after) && after[k] >= '0' && after[k] <= '9' {
			k++
		}
		if tail := bytes.TrimLeft(after[k:], " \t\r\n"); len(tail) > 0 && tail[0] == 'R' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(string(rest[:j]), 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// pdfFilter returns the single filter name applied to the stream, or "" when
// there is none, more than one, or one this file cannot check. A filter array
// with one element counts as that filter.
func pdfFilter(dict []byte) string {
	i := bytes.LastIndex(dict, []byte("/Filter"))
	if i < 0 {
		return ""
	}
	rest := bytes.TrimLeft(dict[i+len("/Filter"):], " \t\r\n")
	array := false
	if len(rest) > 0 && rest[0] == '[' {
		array = true
		rest = bytes.TrimLeft(rest[1:], " \t\r\n")
	}
	if len(rest) == 0 || rest[0] != '/' {
		return ""
	}
	j := 1
	for j < len(rest) && (rest[j] >= 'A' && rest[j] <= 'Z' || rest[j] >= 'a' && rest[j] <= 'z' || rest[j] >= '0' && rest[j] <= '9') {
		j++
	}
	name := string(rest[1:j])
	if array {
		// More than one filter in the array means a chain this file does not
		// decode (e.g. ASCII85 then Flate).
		if after := bytes.TrimLeft(rest[j:], " \t\r\n"); len(after) == 0 || after[0] != ']' {
			return ""
		}
	}
	return name
}

// streamEnd locates the end of the stream data. With a direct /Length that
// agrees with an "endstream" keyword right after it, the bounds are the
// file's own word (confident, which only sharpens the wording of a verdict);
// otherwise the next "endstream" is searched for. Compressed data cannot
// contain that keyword by accident in practice, and zlib stops at its own
// trailer anyway, so inferred bounds are enough to verify and to convict.
func (p *pdfStreams) streamEnd(dataStart, length int64, lengthKnown bool) (end int64, confident bool) {
	if lengthKnown && dataStart+length <= p.size {
		var tail [11]byte
		n, _ := p.f.ReadAt(tail[:], dataStart+length)
		t := bytes.TrimLeft(tail[:n], "\r\n")
		if bytes.HasPrefix(t, []byte("endstream")) {
			return dataStart + length + int64(n-len(t)), true
		}
	}
	// Search forward for "endstream", chunk by chunk.
	buf := make([]byte, pdfScanChunk+16)
	pos := dataStart
	for pos < p.size {
		n := int64(len(buf))
		if p.size-pos < n {
			n = p.size - pos
		}
		if _, err := p.f.ReadAt(buf[:n], pos); err != nil && err != io.EOF {
			return -1, false
		}
		if i := bytes.Index(buf[:n], []byte("endstream")); i >= 0 {
			end = pos + int64(i)
			// The keyword is preceded by an end of line that is not stream data.
			return end, false
		}
		if pos+n >= p.size {
			break
		}
		pos += n - 16
	}
	return -1, false
}

// verifyFlate inflates one stream to nothing, which is what makes zlib check
// its Adler-32 trailer. budget bounds the inflated bytes (maxInflate across the
// whole file); the returned count is what this stream consumed of it.
func (p *pdfStreams) verifyFlate(start, length int64, confident bool, budget int64) (Intactness, string, int64) {
	if length <= 0 || budget <= 0 {
		return Unproven, "", 0
	}
	// The data may be followed by an end of line before "endstream"; zlib
	// stops at its own trailer, so trailing whitespace is harmless.
	src := &cancelReader{io.NewSectionReader(p.f, start, length), p.cancel}
	zr, err := zlib.NewReader(src)
	if err != nil {
		// A header zlib does not recognise: some producers write raw deflate
		// under this filter name, which is not damage, so it is not convicted.
		return Unproven, "", 0
	}
	defer zr.Close()
	n, err := io.Copy(io.Discard, io.LimitReader(zr, budget))
	switch {
	case errors.Is(err, ErrCancelled):
		return Unproven, "", n
	case errors.Is(err, zlib.ErrChecksum):
		return Damaged, "a compressed stream in the PDF fails its own checksum — the stored bytes are damaged", n
	case errors.Is(err, io.ErrUnexpectedEOF):
		if confident {
			return Damaged, "a compressed stream in the PDF ends before its declared length — the stored bytes are damaged", n
		}
		return Damaged, "a compressed stream in the PDF ends early — the stored bytes are damaged", n
	case err != nil:
		// The header was valid (zlib.NewReader accepted it), so a failure inside
		// the stream is the stream's own bytes, not a mislabelled filter.
		return Damaged, "a compressed stream in the PDF cannot be decompressed — the stored bytes are damaged", n
	case n >= budget:
		return Unproven, "", n
	}
	return Proven, "", n
}

// verifyISOBMFF walks the top-level box tree of the ISO base media formats —
// MP4, MOV, HEIC, AVIF. The boxes must tile the file exactly; anything else
// means bytes are missing. These containers carry no checksum and nothing here
// decodes their samples, so a clean walk proves nothing about the payload and
// is reported as Unproven: only damage is ever asserted.
func verifyISOBMFF(f *os.File, size int64, cancel <-chan struct{}) (Intactness, string) {
	pos := int64(0)
	for pos < size {
		if isCancelled(cancel) {
			return Unproven, ""
		}
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
		pos += bs
	}
	return Unproven, ""
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
