package main

// Fuzz targets for the daemon's own on-disk formats: the spill file, the hash
// store and the results file. They are private to the package user, but a
// corrupt read must fail the scan or start the daemon empty, never crash it.
// The media readers have their own targets in internal/media. `go test`
// replays the seeds and any crashers saved under testdata/fuzz; test/fuzz.sh
// runs the targets for real.

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bounded fails when fn exceeds a generous time budget: a corrupt file must
// not be able to pin the daemon.
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

// spillSeed is a spill file holding two well-formed records.
func spillSeed(f *testing.F) []byte {
	sp, err := newSpill(f.TempDir())
	if err != nil {
		f.Fatal(err)
	}
	defer sp.close()
	sp.add(0, &fEnt{size: 10, mod: time.Unix(1700000000, 0), rel: "a/b.txt"})
	sp.addRaw(1, 20, 1700000001, "c.bin", 42, fileLink{})
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
