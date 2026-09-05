package media

// Tests for the validators and the parser bounds. The positive tests for the
// metadata readers, and the builders every test here uses, are in
// parsers_test.go.

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// isoBox builds an ISOBMFF box: uint32 size, 4-char type, payload.
func isoBox(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[0:4], uint32(8+len(payload)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

func fileWith(t *testing.T, data []byte) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "box-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// verifyBytes runs the validator over data written to a fresh file.
func verifyBytes(t *testing.T, data []byte) (Intactness, string) {
	t.Helper()
	open, size := openerFor(t, data)
	return VerifyContent(open, size)
}

// -------------------------------------------------------------- validators

// The validator is chosen by magic bytes alone: VerifyContent never sees a
// file name, so a PNG under any extension is verified as a PNG.
func TestVerifyContentPNG(t *testing.T) {
	good := seedPNG()
	if st, why := verifyBytes(t, good); st != Proven || why == "" {
		t.Errorf("intact PNG: want proven, got %v (%s)", st, why)
	}
	rot := append([]byte(nil), good...)
	rot[len(rot)/2] ^= 0x01
	if st, _ := verifyBytes(t, rot); st != Damaged {
		t.Errorf("PNG with a flipped byte: want damaged, got %v", st)
	}
	if st, _ := verifyBytes(t, good[:len(good)-20]); st != Damaged {
		t.Errorf("truncated PNG: want damaged, got %v", st)
	}
}

func TestVerifyContentGzip(t *testing.T) {
	good := seedGzip()
	if st, why := verifyBytes(t, good); st != Proven || why == "" {
		t.Errorf("intact gzip: want proven, got %v (%s)", st, why)
	}
	// Corrupt the payload but leave the trailing CRC32 alone, so only the
	// checksum comparison can catch it.
	rot := append([]byte(nil), good...)
	rot[len(rot)/2] ^= 0x40
	if st, _ := verifyBytes(t, rot); st != Damaged {
		t.Errorf("gzip failing its CRC32: want damaged, got %v", st)
	}
	// 1F 8B 08 by chance is not a gzip stream and must not convict.
	if st, _ := verifyBytes(t, append([]byte{0x1F, 0x8B, 0x08, 0xFF, 0xFF}, bytes.Repeat([]byte{7}, 64)...)); st != Unproven {
		t.Errorf("gzip magic on a non-gzip file: want unproven, got %v", st)
	}
}

func TestVerifyContentZip(t *testing.T) {
	if st, why := verifyBytes(t, seedZip()); st != Proven || why == "" {
		t.Errorf("intact zip: want proven, got %v (%s)", st, why)
	}
	// One entry large enough that a byte flipped a little way into its
	// compressed data leaves the framing intact and only the CRC to notice.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("content.txt")
	w.Write(bytes.Repeat([]byte("spreadsheet row\n"), 400))
	zw.Close()
	rot := buf.Bytes()
	rot[60] ^= 0x7F
	if st, _ := verifyBytes(t, rot); st != Damaged {
		t.Errorf("zip entry failing its CRC32: want damaged, got %v", st)
	}
}

func TestVerifyContentUnknownTypeStaysUnproven(t *testing.T) {
	if st, why := verifyBytes(t, []byte("just some text")); st != Unproven || why != "" {
		t.Errorf("a format with no validator must not read as a clean bill of health, got %v %q", st, why)
	}
	if st, _ := verifyBytes(t, nil); st != Unproven {
		t.Errorf("an empty file: got %v", st)
	}
}

// A PNG built by hand so the CRC is right, then truncated mid-chunk: catches
// the "claims more bytes than remain" arm, which is the arithmetic that has to
// stay overflow-safe on 32-bit ARM builds.
func TestVerifyPNGRejectsAnOversizedChunkLength(t *testing.T) {
	var b bytes.Buffer
	b.Write([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A})
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], 0x7FFFFFF0) // far past the end of the file
	b.Write(l[:])
	b.WriteString("IHDR")
	b.Write(make([]byte, 13))
	var c [4]byte
	binary.BigEndian.PutUint32(c[:], crc32.ChecksumIEEE(append([]byte("IHDR"), make([]byte, 13)...)))
	b.Write(c[:])
	if st, _ := verifyBytes(t, b.Bytes()); st != Damaged {
		t.Errorf("PNG chunk longer than the file: want damaged, got %v", st)
	}
}

func TestZipEntryCountReadsTheDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, n := range []string{"a", "b", "c"} {
		w, _ := zw.Create(n)
		w.Write([]byte("x"))
	}
	zw.Close()
	f.Close()
	rf, _ := os.Open(path)
	defer rf.Close()
	st, _ := rf.Stat()
	if n, ok := zipEntryCount(rf, st.Size()); !ok || n != 3 {
		t.Fatalf("entry count = %d ok=%v, want 3", n, ok)
	}
	nf := fileWith(t, []byte("not a zip at all, but long enough to hold an EOCD record"))
	if _, ok := zipEntryCount(nf, 60); ok {
		t.Fatal("no EOCD must report !ok")
	}
}

// ----------------------------------------------------------- parser bounds

// An infe size just under 2^31 wraps `pos+size` negative on the 32-bit
// build, where an additive bound would pass and the slice would panic; the
// guard is a subtraction from len(buf), which cannot wrap.
func TestHeifChildSizeNearIntMaxIsRefused(t *testing.T) {
	payload := []byte{0, 0, 0, 0, 0, 1} // version 0, flags, entry_count = 1
	child := make([]byte, 24)
	binary.BigEndian.PutUint32(child[0:4], 0x7FFFFFF0)
	copy(child[4:8], "infe")
	payload = append(payload, child...)
	data := isoBox("iinf", payload)
	f := fileWith(t, data)
	if id, ok := heifExifItemID(f, 0, int64(len(data))); ok {
		t.Fatalf("hostile size accepted: id=%d", id)
	}
}

func TestQtKeySizeNearIntMaxIsRefused(t *testing.T) {
	kp := []byte{0, 0, 0, 0, 0, 0, 0, 1} // version/flags, count = 1
	entry := make([]byte, 16)
	binary.BigEndian.PutUint32(entry[0:4], 0x7FFFFFF0)
	copy(entry[4:8], "mdta")
	kp = append(kp, entry...)
	data := isoBox("keys", kp)
	f := fileWith(t, data)
	if d := qtKeysIlst(f, 0, int64(len(data))); d != "" {
		t.Fatalf("hostile size produced a date %q", d)
	}
}

// Extents whose offset, length and index fields occupy zero bytes consume
// nothing, so count × extent_count iterations would run on a buffer
// heifBoxMax keeps small — hours of CPU from one crafted file. Refused
// outright.
func TestHeifIlocZeroSizedExtentsIsBounded(t *testing.T) {
	p := []byte{0, 0, 0, 0, 0x00, 0x00} // version 0, flags, all size nibbles 0
	const count = 20000
	p = append(p, byte(count>>8), byte(count&0xFF))
	for i := 0; i < count; i++ {
		p = append(p, 0, 1, 0, 0, 0xFF, 0xFF) // item_id, data_ref_idx, extent_count = 65535
	}
	data := isoBox("iloc", p)
	f := fileWith(t, data)
	start := time.Now()
	if _, _, ok := heifItemLocation(f, 0, int64(len(data)), 1); ok {
		t.Fatal("zero-sized extents located an item")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("iloc walk took %v", d)
	}
}

func TestCaptureDateWindowAndFormatter(t *testing.T) {
	if captureDate(time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)) != "" {
		t.Fatal("a 1904 date must read as absent")
	}
	if parseISODate("1904-01-01T00:00:00Z") != "" || parseISODate("0001-01-01T00:00:00") != "" {
		t.Fatal("implausible ISO dates must read as absent")
	}
	if parseISODate("2023-06-22T01:11:21+0200") == "" {
		t.Fatal("a plausible ISO date must format")
	}
	for n, want := range map[int64]string{512: "512 B", 1536: "1.50 KB", 15360: "15.0 KB", 153600: "150 KB", 0: "0 B"} {
		if got := FmtBytes(n); got != want {
			t.Fatalf("FmtBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
