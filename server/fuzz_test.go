package main

// Fuzz targets for everything that parses bytes it does not control: the
// media metadata readers, the container validators, the byte-level
// comparison, and the daemon's own on-disk formats (spill, hash store, state
// file), which are private but must fail a corrupt read rather than crash
// the daemon. Each target seeds its corpus from the builders in
// parsers_test.go, so mutation starts from files the readers accept, and
// asserts the reader's contract: no panic, a bounded run time, well-formed
// output. `go test` replays the seeds and any crashers saved under
// testdata/fuzz; test/fuzz.sh runs the targets for real.

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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

// quiet silences the daemon logger for a test that feeds it garbage.
func quiet(t *testing.T) {
	t.Helper()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
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

// ------------------------------------------------------------- validators

func FuzzVerifyContent(f *testing.F) {
	for _, s := range mediaSeeds() {
		f.Add(s)
	}
	f.Add([]byte{})
	f.Add([]byte{0x1F, 0x8B, 0x08, 0, 0, 0, 0, 0, 0, 0})
	f.Add(append([]byte("PK\x05\x06"), make([]byte, 18)...))
	f.Fuzz(func(t *testing.T, data []byte) {
		open, size := openerFor(t, data)
		var st intactness
		var why string
		bounded(t, func() { st, why = verifyContent(open, size) })
		if st != unproven && st != proven && st != damaged {
			t.Fatalf("verdict %d", st)
		}
		// unproven is the only verdict without evidence, and the only one
		// that must carry none: the evidence column prints whatever it gets.
		if (st == unproven) != (why == "") {
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
		var d *diffShape
		var err error
		bounded(t, func() { d, err = compareContent(oa, ob, int64(n), nil) })
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
		switch d.kind {
		case "zeros", "tail":
			if d.zeroSide != 0 && d.zeroSide != 1 {
				t.Fatalf("%s without a zero side: %+v", d.kind, d)
			}
		case "bitflip", "mixed":
		default:
			t.Fatalf("unknown shape %q", d.kind)
		}
		if d.firstAt < 0 || d.firstAt >= int64(n) || d.bytes <= 0 || d.bytes > int64(n) {
			t.Fatalf("shape out of range: %+v", d)
		}
		if d.describe(0) == "" || d.describe(1) == "" {
			t.Fatalf("empty evidence for %+v", d)
		}
	})
}

// ---------------------------------------------------------------- metadata

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

func FuzzExifCaptured(f *testing.F) {
	tiff := tiffWithDates(binary.BigEndian, "2021:05:04 10:11:12", "2019:12:31 23:59:58")
	f.Add(jpegWithExif(tiff))
	f.Add(tiff)
	f.Add(seedJPEG())
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, name := range []string{"a.jpg", "a.dng"} {
			open, _ := openerFor(t, data)
			var got string
			bounded(t, func() { got = exifCaptured(open, name) })
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

// --------------------------------------------------------- private formats

// spillSeed is a spill file holding two well-formed records.
func spillSeed(f *testing.F) []byte {
	sp, err := newSpill(f.TempDir())
	if err != nil {
		f.Fatal(err)
	}
	defer sp.close()
	sp.add(0, &fEnt{size: 10, mod: time.Unix(1700000000, 0), rel: "a/b.txt"})
	sp.addRaw(1, 20, 1700000001, "c.bin", 42)
	sp.w.Flush()
	sp.f.Seek(0, io.SeekStart)
	b, _ := io.ReadAll(sp.f)
	return b
}

func FuzzSpillEach(f *testing.F) {
	f.Add(spillSeed(f), uint8(2))
	f.Add([]byte{}, uint8(1))
	f.Fuzz(func(t *testing.T, data []byte, n uint8) {
		sp, err := newSpill(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer sp.close()
		if _, err := sp.w.Write(data); err != nil {
			t.Fatal(err)
		}
		sp.n = int(n)
		recs := 0
		var rerr error
		bounded(t, func() {
			rerr = sp.each(func(r *spillRec) error {
				recs++
				if r.rootIdx < 0 || r.rootIdx > math.MaxInt32 || len(r.rel) > spillRelMax {
					t.Fatalf("record out of bounds: %+v", r)
				}
				return nil
			})
		})
		// each reads exactly n records or reports why it could not.
		if recs > int(n) || (rerr == nil && recs != int(n)) {
			t.Fatalf("%d records for n=%d, err=%v", recs, n, rerr)
		}
	})
}

// hashCacheSeed is a store file holding one entry.
func hashCacheSeed(f *testing.F) []byte {
	dir := f.TempDir()
	c := loadHashCache(dir)
	c.record("/v/a", 10, 1700000000, strings.Repeat("ab", 32), strings.Repeat("cd", 32))
	if err := c.save(); err != nil {
		f.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, hashCacheFile))
	if err != nil {
		f.Fatal(err)
	}
	return b
}

func FuzzHashCacheLoad(f *testing.F) {
	f.Add(hashCacheSeed(f))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, hashCacheFile), data, 0o600); err != nil {
			t.Fatal(err)
		}
		var c *hashCache
		bounded(t, func() { c = loadHashCache(dir) })
		if c == nil || c.ents == nil {
			t.Fatal("nil store")
		}
		if len(c.ents) > hashCacheMax+1 {
			t.Fatalf("%d entries loaded past the cap", len(c.ents))
		}
		// Whatever the file held, the store must work: a fresh record
		// round-trips through lookup within this generation.
		pfx, full := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
		c.record("/v/x", 10, 1700000000, pfx, full)
		if got, ok := c.lookup("/v/x", 10, 1700000000, pfx); !ok || got != full {
			t.Fatalf("round trip after load: %q %v", got, ok)
		}
	})
}

// stateSeed is a small, valid results file.
func stateSeed(f *testing.F) []byte {
	ps := persistedState{
		Schema: 1,
		Results: map[string]*toolResult{"duplicates": {Tool: "duplicates", Groups: []Group{{
			ID: "g0", Size: 5, Hash: "h",
			Files: []FileEnt{{ID: "f1", Name: "a", Dir: "/v"}, {ID: "f2", Name: "a", Dir: "/w"}},
		}}}},
		RefDirs: []string{"/v/ref"}, Keepers: []string{"/v/k"}, LastTool: "duplicates", NextID: 3,
		SavedAt: "2026-01-01T00:00:00Z",
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(&ps); err != nil {
		f.Fatal(err)
	}
	zw.Close()
	return buf.Bytes()
}

func FuzzLoadState(f *testing.F) {
	f.Add(stateSeed(f))
	f.Add([]byte{})
	f.Add([]byte("{}"))
	f.Fuzz(func(t *testing.T, data []byte) {
		quiet(t)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, stateFile), data, 0o600); err != nil {
			t.Fatal(err)
		}
		s := &Server{results: map[string]*toolResult{}, varDir: dir}
		bounded(t, func() { s.loadState() })
		if s.results == nil {
			t.Fatal("results map nil after load")
		}
		for tool, r := range s.results {
			if r == nil {
				t.Fatalf("nil result kept for %q", tool)
			}
			if !validTools[tool] {
				t.Fatalf("result for unknown tool %q kept", tool)
			}
		}
		if s.lastTool != "" && !validTools[s.lastTool] {
			t.Fatalf("lastTool %q names an unknown tool", s.lastTool)
		}
	})
}
