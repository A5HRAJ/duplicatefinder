package media

// Fuzz targets for every reader in this package. Each seeds its corpus from
// the builders in parsers_test.go, so mutation starts from files the readers
// accept, and asserts the reader's contract: no panic, a bounded run time,
// well-formed output. `go test` replays the seeds and any crashers saved
// under testdata/fuzz; test/fuzz.sh runs the targets for real.

import (
	"bytes"
	"encoding/binary"
	"regexp"
	"strconv"
	"testing"
	"time"
)

var dateShape = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$`)

// checkDate asserts a capture date is absent or well-formed and plausible.
func checkDate(t *testing.T, got string) {
	t.Helper()
	if got == "" {
		return
	}
	if !dateShape.MatchString(got) {
		t.Fatalf("malformed date %q", got)
	}
	if y, _ := strconv.Atoi(got[:4]); y < 1971 || y > 2100 {
		t.Fatalf("implausible year in %q", got)
	}
}

// bounded fails when fn exceeds the readers' time budget: a hostile file
// must not be able to pin a scan.
func bounded(t *testing.T, fn func()) {
	t.Helper()
	start := time.Now()
	fn()
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %v", d)
	}
}

// mediaSeeds is every file the container and metadata readers accept.
func mediaSeeds() [][]byte {
	when := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	tiff := tiffWithDates(binary.BigEndian, "2021:05:04 10:11:12", "2019:12:31 23:59:58")
	return [][]byte{
		seedPNG(), seedGzip(), seedZip(), seedJPEG(), jpegWithExif(tiff), seedPDF(),
		seedMOV(when, false), seedMOV(when, true),
		seedMP4WithCreationDate(when, "2022-03-04T05:06:07+0100"),
		seedHEIF(tiff), tiff, tiffWithDates(binary.LittleEndian, "", "2020:02:29 08:00:00"),
	}
}

func FuzzVerifyContent(f *testing.F) {
	for _, s := range mediaSeeds() {
		f.Add(s)
	}
	f.Add([]byte{})
	f.Add([]byte{0x1F, 0x8B, 0x08, 0, 0, 0, 0, 0, 0, 0})
	f.Add(append([]byte("PK\x05\x06"), make([]byte, 18)...))
	f.Fuzz(func(t *testing.T, data []byte) {
		open, size := openerFor(t, data)
		var st Intactness
		var why string
		bounded(t, func() { st, why = VerifyContent(open, size, nil) })
		if st != Unproven && st != Proven && st != Damaged {
			t.Fatalf("verdict %d", st)
		}
		// Unproven is the only verdict without evidence, and the only one
		// that must carry none: the evidence column prints whatever it gets.
		if (st == Unproven) != (why == "") {
			t.Fatalf("verdict %d with evidence %q", st, why)
		}
	})
}

func FuzzZipEntryCount(f *testing.F) {
	f.Add(seedZip())
	f.Add(append([]byte("PK\x05\x06"), make([]byte, 18)...))
	f.Fuzz(func(t *testing.T, data []byte) {
		open, size := openerFor(t, data)
		fh, err := open()
		if err != nil {
			t.Fatal(err)
		}
		defer fh.Close()
		bounded(t, func() { zipEntryCount(fh, size) })
	})
}

func FuzzCompareContent(f *testing.F) {
	f.Add([]byte("same bytes"), []byte("same bytes"))
	f.Add(make([]byte, 1024), append(make([]byte, 512), bytes.Repeat([]byte{1}, 512)...))
	f.Add([]byte{0x00, 0x01, 0x02}, []byte{0x00, 0x03, 0x02})
	f.Fuzz(func(t *testing.T, a, b []byte) {
		// The comparison's contract is two same-size files.
		n := len(a)
		if len(b) < n {
			n = len(b)
		}
		a, b = a[:n], b[:n]
		if n == 0 {
			return
		}
		oa, _ := openerFor(t, a)
		ob, _ := openerFor(t, b)
		var d *DiffShape
		var err error
		bounded(t, func() { d, err = CompareContent(oa, ob, int64(n), nil) })
		if err != nil {
			t.Fatal(err)
		}
		if d == nil {
			if !bytes.Equal(a, b) {
				t.Fatal("no difference reported for different bytes")
			}
			return
		}
		if bytes.Equal(a, b) {
			t.Fatal("a difference reported for identical bytes")
		}
		switch d.Kind {
		case "zeros", "tail":
			if d.ZeroSide != 0 && d.ZeroSide != 1 {
				t.Fatalf("%s without a zero side: %+v", d.Kind, d)
			}
		case "bitflip", "mixed":
		default:
			t.Fatalf("unknown shape %q", d.Kind)
		}
		if d.FirstAt < 0 || d.FirstAt >= int64(n) || d.Bytes <= 0 || d.Bytes > int64(n) {
			t.Fatalf("shape out of range: %+v", d)
		}
		if d.Describe(0) == "" || d.Describe(1) == "" {
			t.Fatalf("empty evidence for %+v", d)
		}
	})
}

func FuzzHeifCaptured(f *testing.F) {
	f.Add(seedHEIF(tiffWithDates(binary.BigEndian, "", "2023:06:22 01:08:15")))
	f.Add(seedHEIF(nil))
	f.Fuzz(func(t *testing.T, data []byte) {
		open, _ := openerFor(t, data)
		var got string
		bounded(t, func() { got = heifCaptured(open) })
		checkDate(t, got)
	})
}

func FuzzQtCaptured(f *testing.F) {
	when := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	f.Add(seedMOV(when, false))
	f.Add(seedMOV(when, true))
	f.Add(seedMP4WithCreationDate(when, "2022-03-04T05:06:07+0100"))
	f.Fuzz(func(t *testing.T, data []byte) {
		open, _ := openerFor(t, data)
		var got string
		bounded(t, func() { got = qtCaptured(open) })
		checkDate(t, got)
	})
}

func FuzzCaptured(f *testing.F) {
	tiff := tiffWithDates(binary.BigEndian, "2021:05:04 10:11:12", "2019:12:31 23:59:58")
	f.Add(jpegWithExif(tiff))
	f.Add(tiff)
	f.Add(seedJPEG())
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, name := range []string{"a.jpg", "a.dng"} {
			open, _ := openerFor(t, data)
			var got string
			bounded(t, func() { got = Captured(open, name) })
			checkDate(t, got)
		}
	})
}

func FuzzParseTiffDate(f *testing.F) {
	f.Add(tiffWithDates(binary.BigEndian, "2021:05:04 10:11:12", "2019:12:31 23:59:58"))
	f.Add(tiffWithDates(binary.LittleEndian, "2021:05:04 10:11:12", ""))
	f.Fuzz(func(t *testing.T, data []byte) {
		var got string
		bounded(t, func() { got = parseTiffDate(data) })
		checkDate(t, got)
	})
}

func FuzzFindExifInJpeg(f *testing.F) {
	f.Add(jpegWithExif(tiffWithDates(binary.BigEndian, "", "2019:12:31 23:59:58")))
	f.Add(seedJPEG())
	f.Fuzz(func(t *testing.T, data []byte) {
		out := findExifInJpeg(data)
		if len(out) > len(data) {
			t.Fatalf("segment longer than the input: %d > %d", len(out), len(data))
		}
	})
}
