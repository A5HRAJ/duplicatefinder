package main

// Positive tests for the file-format readers: EXIF in JPEG and the TIFF
// family, the EXIF item in HEIF, QuickTime creation dates, and the PDF and
// ISO base-media structural checks. The builders here are also the seed
// corpus for the fuzz targets in fuzz_test.go, so every format the fuzzers
// mutate starts from a file the readers accept.

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --------------------------------------------------------------- builders

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func be16(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }
func be32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func be64(v uint64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, v); return b }

// fullBox prefixes an ISOBMFF FullBox payload with its version and flags.
func fullBox(version byte, payload []byte) []byte {
	return append([]byte{version, 0, 0, 0}, payload...)
}

// tiffWithDates builds a minimal TIFF/EXIF block: IFD0 holding an optional
// DateTime (tag 0x0132) and, when original is set, a pointer to an Exif IFD
// holding DateTimeOriginal (tag 0x9003). Both dates are "YYYY:MM:DD HH:MM:SS".
func tiffWithDates(bo binary.ByteOrder, dateTime, original string) []byte {
	u16 := func(v uint16) []byte { x := make([]byte, 2); bo.PutUint16(x, v); return x }
	u32 := func(v uint32) []byte { x := make([]byte, 4); bo.PutUint32(x, v); return x }
	entry := func(tag, typ uint16, count, val uint32) []byte {
		return cat(u16(tag), u16(typ), u32(count), u32(val))
	}
	n0 := 0
	if dateTime != "" {
		n0++
	}
	if original != "" {
		n0++
	}
	ifd0Len := 2 + 12*n0 + 4
	exifLen := 0
	if original != "" {
		exifLen = 2 + 12 + 4
	}
	strBase := uint32(8 + ifd0Len + exifLen)
	var ifd0, exif, strs []byte
	if dateTime != "" {
		ifd0 = append(ifd0, entry(0x0132, 2, uint32(len(dateTime)+1), strBase+uint32(len(strs)))...)
		strs = append(append(strs, dateTime...), 0)
	}
	if original != "" {
		ifd0 = append(ifd0, entry(0x8769, 4, 1, uint32(8+ifd0Len))...)
		exif = cat(u16(1), entry(0x9003, 2, uint32(len(original)+1), strBase+uint32(len(strs))), u32(0))
		strs = append(append(strs, original...), 0)
	}
	magic := "II"
	if bo == binary.BigEndian {
		magic = "MM"
	}
	return cat([]byte(magic), u16(42), u32(8), u16(uint16(n0)), ifd0, u32(0), exif, strs)
}

// seedJPEG is a real, tiny baseline JPEG.
func seedJPEG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.Set(x, y, color.RGBA{uint8(60 * x), uint8(60 * y), 128, 255})
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, nil)
	return buf.Bytes()
}

// jpegWithExif inserts an APP1 Exif segment right after the SOI marker of a
// real JPEG, which is where cameras put it.
func jpegWithExif(tiff []byte) []byte {
	j := seedJPEG()
	app1 := cat([]byte{0xFF, 0xE1}, be16(uint16(2+6+len(tiff))), []byte("Exif\x00\x00"), tiff)
	return cat(j[:2], app1, j[2:])
}

func seedPNG() []byte {
	img := image.NewGray(image.Rect(0, 0, 8, 8))
	for i := range img.Pix {
		img.Pix[i] = uint8(i * 3)
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func seedGzip() []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(bytes.Repeat([]byte("compress me please "), 40))
	zw.Close()
	return buf.Bytes()
}

func seedZip() []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range []string{"a.txt", "dir/b.txt"} {
		w, _ := zw.Create(n)
		w.Write(bytes.Repeat([]byte(n+"\n"), 20))
	}
	zw.Close()
	return buf.Bytes()
}

func seedPDF() []byte {
	return []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\ntrailer\n<< /Root 1 0 R >>\n%%EOF\n")
}

// mvhdBox is a movie header whose creation time is t; v1 uses the 64-bit
// layout.
func mvhdBox(t time.Time, v1 bool) []byte {
	secs := uint64(t.Unix() + 2082844800) // QuickTime counts from 1904
	if v1 {
		return isoBox("mvhd", fullBox(1, cat(be64(secs), be64(secs), be32(600), be64(0))))
	}
	return isoBox("mvhd", fullBox(0, cat(be32(uint32(secs)), be32(uint32(secs)), be32(600), be32(0))))
}

// seedMOV is a QuickTime file whose only date is the movie header's.
func seedMOV(t time.Time, v1 bool) []byte {
	return cat(isoBox("ftyp", cat([]byte("qt  "), be32(0), []byte("qt  "))), isoBox("moov", mvhdBox(t, v1)))
}

// seedMP4WithCreationDate carries com.apple.quicktime.creationdate in
// moov/meta/{keys,ilst} the way iPhones write it, beside a movie header
// date that must lose to it.
func seedMP4WithCreationDate(mvhdTime time.Time, iso string) []byte {
	name := "com.apple.quicktime.creationdate"
	keys := isoBox("keys", fullBox(0, cat(be32(1), be32(uint32(8+len(name))), []byte("mdta"), []byte(name))))
	data := isoBox("data", cat(be32(1), be32(0), []byte(iso)))
	item := cat(be32(uint32(8+len(data))), be32(1), data)
	meta := isoBox("meta", fullBox(0, cat(keys, isoBox("ilst", item))))
	moov := isoBox("moov", cat(mvhdBox(mvhdTime, false), meta))
	return cat(isoBox("ftyp", cat([]byte("mp42"), be32(0), []byte("mp42isom"))), moov)
}

// seedHEIF is a HEIF container whose meta box declares one Exif item and
// locates its payload (an Exif header plus the given TIFF block) in mdat.
func seedHEIF(tiff []byte) []byte {
	ftyp := isoBox("ftyp", cat([]byte("heic"), be32(0), []byte("mif1heic")))
	infe := isoBox("infe", fullBox(2, cat(be16(1), be16(0), []byte("Exif"), []byte{0})))
	iinf := isoBox("iinf", fullBox(0, cat(be16(1), infe)))
	payload := cat(be32(6), []byte("Exif\x00\x00"), tiff)
	iloc := func(off, ln uint32) []byte {
		// offset_size 4, length_size 4, base_offset_size 0; one item, one
		// extent, construction method 0 (absolute file offsets).
		return isoBox("iloc", fullBox(0, cat([]byte{0x44, 0x00}, be16(1), be16(1), be16(0), be16(1), be32(off), be32(ln))))
	}
	metaLen := len(isoBox("meta", fullBox(0, cat(iinf, iloc(0, 0)))))
	off := uint32(len(ftyp) + metaLen + 8)
	meta := isoBox("meta", fullBox(0, cat(iinf, iloc(off, uint32(len(payload))))))
	return cat(ftyp, meta, isoBox("mdat", payload))
}

// openerFor writes data to a temp file and returns an opener for it, the
// shape every reader takes.
func openerFor(t testing.TB, data []byte) (func() (*os.File, error), int64) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return func() (*os.File, error) { return os.Open(p) }, int64(len(data))
}

// ------------------------------------------------------------------ EXIF

func TestExifCapturedFromJPEG(t *testing.T) {
	for _, bo := range []binary.ByteOrder{binary.BigEndian, binary.LittleEndian} {
		open, _ := openerFor(t, jpegWithExif(tiffWithDates(bo, "2021:05:04 10:11:12", "2019:12:31 23:59:58")))
		if got := exifCaptured(open, "IMG_0001.JPG"); got != "2019-12-31 23:59:58" {
			t.Errorf("%v: DateTimeOriginal must win: got %q", bo, got)
		}
	}
}

func TestExifCapturedFallsBackToDateTime(t *testing.T) {
	open, _ := openerFor(t, jpegWithExif(tiffWithDates(binary.LittleEndian, "2021:05:04 10:11:12", "")))
	if got := exifCaptured(open, "a.jpeg"); got != "2021-05-04 10:11:12" {
		t.Errorf("IFD0 DateTime fallback: got %q", got)
	}
}

func TestExifCapturedFromTIFFFamily(t *testing.T) {
	tiff := tiffWithDates(binary.BigEndian, "", "2020:02:29 08:00:00")
	for _, name := range []string{"raw.dng", "raw.NEF", "raw.cr2", "raw.arw", "scan.tif"} {
		open, _ := openerFor(t, tiff)
		if got := exifCaptured(open, name); got != "2020-02-29 08:00:00" {
			t.Errorf("%s: got %q", name, got)
		}
	}
}

func TestExifCapturedIgnoresOtherTypesAndBadDates(t *testing.T) {
	open, _ := openerFor(t, jpegWithExif(tiffWithDates(binary.BigEndian, "", "2020:02:29 08:00:00")))
	if got := exifCaptured(open, "notes.txt"); got != "" {
		t.Errorf("non-image name must not be parsed: %q", got)
	}
	for _, bad := range []string{"0001:01:01 00:00:00", "not a date", "2020:13:40 99:00:00"} {
		open, _ := openerFor(t, jpegWithExif(tiffWithDates(binary.BigEndian, "", bad)))
		if got := exifCaptured(open, "a.jpg"); got != "" {
			t.Errorf("%q must read as absent, got %q", bad, got)
		}
	}
	plain, _ := openerFor(t, seedJPEG())
	if got := exifCaptured(plain, "a.jpg"); got != "" {
		t.Errorf("JPEG without APP1: got %q", got)
	}
}

// ------------------------------------------------------------------ HEIF

func TestHeifCapturedReadsTheExifItem(t *testing.T) {
	open, _ := openerFor(t, seedHEIF(tiffWithDates(binary.BigEndian, "", "2023:06:22 01:08:15")))
	if got := heifCaptured(open); got != "2023-06-22 01:08:15" {
		t.Fatalf("got %q", got)
	}
	// The same file reached through exifCaptured's extension dispatch.
	for _, name := range []string{"IMG.HEIC", "img.heif", "img.hif", "img.avif"} {
		open, _ := openerFor(t, seedHEIF(tiffWithDates(binary.LittleEndian, "", "2023:06:22 01:08:15")))
		if got := exifCaptured(open, name); got != "2023-06-22 01:08:15" {
			t.Errorf("%s: got %q", name, got)
		}
	}
}

func TestHeifCapturedWithoutAnExifItem(t *testing.T) {
	// A meta box whose only item is an image, not Exif.
	infe := isoBox("infe", fullBox(2, cat(be16(1), be16(0), []byte("hvc1"), []byte{0})))
	meta := isoBox("meta", fullBox(0, isoBox("iinf", fullBox(0, cat(be16(1), infe)))))
	open, _ := openerFor(t, cat(isoBox("ftyp", []byte("heic\x00\x00\x00\x00mif1")), meta))
	if got := heifCaptured(open); got != "" {
		t.Fatalf("no Exif item, got %q", got)
	}
	none, _ := openerFor(t, seedMOV(time.Unix(1600000000, 0), false))
	if got := heifCaptured(none); got != "" {
		t.Fatalf("no meta box, got %q", got)
	}
}

func TestTiffFromExifItemFindsTheHeaderEitherWay(t *testing.T) {
	tiff := tiffWithDates(binary.BigEndian, "", "2022:01:02 03:04:05")
	// Header inside the first 64 bytes: found by scanning.
	if got := parseTiffDate(tiffFromExifItem(cat(be32(6), []byte("Exif\x00\x00"), tiff))); got != "2022-01-02 03:04:05" {
		t.Errorf("scanned header: got %q", got)
	}
	// Header beyond the scan window: found through the declared offset.
	pad := bytes.Repeat([]byte{'x'}, 100)
	if got := parseTiffDate(tiffFromExifItem(cat(be32(100), pad, tiff))); got != "2022-01-02 03:04:05" {
		t.Errorf("declared offset: got %q", got)
	}
	if tiffFromExifItem(cat(be32(1000), pad)) != nil || tiffFromExifItem([]byte{1, 2}) != nil {
		t.Error("an unlocatable header must yield nil")
	}
}

// ------------------------------------------------------------- QuickTime

func TestQtCapturedFromTheMovieHeader(t *testing.T) {
	when := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, v1 := range []bool{false, true} {
		open, _ := openerFor(t, seedMOV(when, v1))
		if got := qtCaptured(open); got != "2020-01-02 03:04:05" {
			t.Errorf("v1=%v: got %q", v1, got)
		}
	}
	open, _ := openerFor(t, seedMOV(when, false))
	if got := exifCaptured(open, "clip.mov"); got != "2020-01-02 03:04:05" {
		t.Errorf("through exifCaptured: got %q", got)
	}
	// A zero creation time is absent, not 1904.
	zero, _ := openerFor(t, seedMOV(time.Unix(-2082844800, 0), false))
	if got := qtCaptured(zero); got != "" {
		t.Errorf("zero mvhd time: got %q", got)
	}
}

func TestQtCapturedPrefersAppleCreationDate(t *testing.T) {
	open, _ := openerFor(t, seedMP4WithCreationDate(time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC), "2022-03-04T05:06:07+0100"))
	// The wall-clock capture time, in its own zone — not the UTC header.
	if got := qtCaptured(open); got != "2022-03-04 05:06:07" {
		t.Fatalf("got %q", got)
	}
	// An unparsable value falls back to the header.
	bad, _ := openerFor(t, seedMP4WithCreationDate(time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC), "yesterday"))
	if got := qtCaptured(bad); got != "2020-01-02 03:04:05" {
		t.Fatalf("fallback: got %q", got)
	}
}

// --------------------------------------------------------------- verifiers

func TestVerifyPDFNeedsItsTrailer(t *testing.T) {
	open, size := openerFor(t, seedPDF())
	if st, why := verifyContent(open, size); st != proven || why == "" {
		t.Errorf("intact PDF: %v %q", st, why)
	}
	cut := bytes.Replace(seedPDF(), []byte("%%EOF"), []byte("%%EO"), 1)
	open, size = openerFor(t, cut)
	if st, _ := verifyContent(open, size); st != damaged {
		t.Errorf("PDF without %%EOF: %v", st)
	}
}

func TestVerifyISOBMFFTilesTheFile(t *testing.T) {
	when := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	good := seedMOV(when, false)
	open, size := openerFor(t, good)
	if st, why := verifyContent(open, size); st != proven || why == "" {
		t.Errorf("tiling boxes: %v %q", st, why)
	}
	open, size = openerFor(t, good[:len(good)-3])
	if st, _ := verifyContent(open, size); st != damaged {
		t.Errorf("truncated last box: %v", st)
	}
	// A 64-bit-sized box and a size-0 (to end of file) box both tile.
	wide := cat(be32(1), []byte("free"), be64(16+4), []byte("abcd"))
	last := cat(be32(0), []byte("free"), []byte("tail"))
	open, size = openerFor(t, cat(good, wide, last))
	if st, _ := verifyContent(open, size); st != proven {
		t.Errorf("64-bit and open-ended boxes: %v", st)
	}
	// A box header claiming less than its own header length is damage.
	open, size = openerFor(t, cat(good, be32(4), []byte("free")))
	if st, _ := verifyContent(open, size); st != damaged {
		t.Errorf("undersized box: %v", st)
	}
	// Stray bytes that cannot hold a header.
	open, size = openerFor(t, cat(good, []byte{1, 2, 3}))
	if st, _ := verifyContent(open, size); st != damaged {
		t.Errorf("stray tail bytes: %v", st)
	}
}

func TestVerifyJPEGWithExifAndTrailingMovie(t *testing.T) {
	// A phone "motion photo": a complete JPEG with an MP4 appended after EOI.
	motion := cat(jpegWithExif(tiffWithDates(binary.BigEndian, "", "2020:02:29 08:00:00")), seedMOV(time.Unix(1600000000, 0), false))
	open, size := openerFor(t, motion)
	if st, _ := verifyContent(open, size); st != proven {
		t.Errorf("JPEG with trailing movie must not read as truncated: %v", st)
	}
	j := seedJPEG()
	open, size = openerFor(t, j[:len(j)-2])
	if st, _ := verifyContent(open, size); st != damaged {
		t.Errorf("JPEG without EOI: %v", st)
	}
	// Fill bytes before a marker are legal.
	filled := cat(j[:2], []byte{0xFF}, j[2:])
	open, size = openerFor(t, filled)
	if st, _ := verifyContent(open, size); st != proven {
		t.Errorf("fill byte before a marker: %v", st)
	}
}
