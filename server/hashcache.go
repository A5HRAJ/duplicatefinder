package main

// Persistent full-content hash store. It serves two purposes, and the
// split between them is a correctness rule, not a tuning choice:
//
//   - WITHIN one scan it is a read-dedup: the skew ladder, dupWindow and the
//     conflicting-files pass all want the same full hashes, and a file read
//     once this scan need not be read again this scan. lookup() therefore
//     answers ONLY for entries recorded under the current generation.
//   - ACROSS scans it is history, never authority. A previous scan's hash is
//     structurally incapable of proving a file unchanged — size, mtime and
//     even the first 64 KiB can all stand while bytes beyond them rot — so
//     every scan re-reads every candidate in full, and the stored entry is
//     used the other way around: record() compares the fresh hash against it
//     and captures "content moved while size and mtime did not", which is
//     the conflicting-files scan's bit-rot evidence. Perfect rot detection
//     over speed is the maintainer's explicit priority here; serving stale
//     hashes to skip reads is the one thing this file must never do again.
//
// Entries are keyed by a hash of the display path and carry the file's
// size, mtime, 64 KiB prefix hash and full-content hash. The store is
// loaded at the start of a duplicates scan, saved (atomically, 0600) and
// released at the end, so idle daemon memory stays flat. A generation
// counter both gates lookup() to the current scan and provides LRU-ish
// retention when the entry cap trims the file.

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"lukechampine.com/blake3"
)

const hashCacheFile = "hashcache.bin"

// Entry caps. Variables only so the tests can shrink them — filling half a
// million entries to prove the trim would be a slow way to say it.
var (
	hashCacheMax = 500000 // ≈ 38 MB on disk, freed from RAM between scans
	// The in-memory map is trimmed back to hashCacheMax whenever it passes
	// this mark, so a scan that misses on millions of files cannot grow it
	// without bound before save() would have trimmed the file.
	hashCacheHigh = hashCacheMax + hashCacheMax/4
)

// Bumped to 2 when entries gained the path tag: an old file is simply
// ignored (a clean start costs one slower scan, never a wrong hash).
var hashCacheMagic = [8]byte{'D', 'F', 'H', 'C', '2', 0, 0, 0}

type hcEnt struct {
	Size int64
	Mod  int64
	Tag  [16]byte // BLAKE3 of the path — see pathTag
	Pfx  [16]byte // first half of the 64 KiB-prefix BLAKE3
	Hash [32]byte // full-content BLAKE3
	Gen  uint32
}

type hashCache struct {
	mu    sync.Mutex
	path  string // "" disables persistence (still works as an in-run cache)
	gen   uint32
	ents  map[uint64]hcEnt
	dirty bool
	// changed records paths whose CONTENT moved while their size AND modified
	// time stayed put — the prefix hash, the full hash, or both. Nothing that
	// edits a file leaves size and mtime untouched, so it is the
	// corrupted-files scan's historical bit-rot evidence, and since every scan
	// now re-reads candidates in full, it sees rot anywhere in the file, not
	// only inside the first 64 KiB.
	//
	// The VALUE is the path's BLAKE3 tag, checked again on every query: the
	// key alone is 64-bit FNV, and answering on the bare key would let a path
	// collision hand one file another file's bit-rot conviction — the exact
	// cross-identification the Tag field exists to rule out (see pathKey).
	//
	// It has to be captured HERE, at the moment record() overwrites the bucket,
	// because by the time that scan asks, the duplicates pass has already
	// rewritten every entry it touched and the before-value is gone. Asking the
	// live bucket alone made the whole rung dead code.
	//
	// It is scan-lifetime only and never persisted: one entry per file that
	// genuinely changed underneath its metadata, which is rare by construction,
	// and capped anyway. A daemon death therefore loses any capture the dying
	// run had made, and — because record() already overwrote the bucket — a
	// RESUMED run cannot re-derive it: the conflicting SET still surfaces
	// (contents still disagree on fresh comparison), but its rung-2 verdict
	// reads Undetermined instead of Corrupted. Accepted: rung-2 evidence is
	// one-shot by nature (even an uninterrupted conviction is not repeated by
	// the next scan), and persisting the set for this edge would buy one
	// verdict at the price of another moving part in the crash path.
	changed map[uint64][16]byte
}

// changedMax bounds the evidence set. A volume with more than this many files
// whose content moved under an unchanged mtime has a systemic problem, and the
// sets already reported say so — this only stops a pathological case growing
// daemon memory. A full set refuses new entries (record still returns true);
// the corrupt skew pass therefore carries its own fresh evidence out-of-band
// (hashSubSpill's onRot), so the cap can only cost evidence that crossed
// passes, never a finding made and consumed in the same one.
const changedMax = 1 << 17

// pathKey buckets an entry; pathTag identifies it. The bucket is a cheap
// 64-bit FNV, which is not collision-resistant — and a collision between two
// paths that also happened to agree on size, mtime and 64 KiB prefix would
// hand one file the OTHER's full-content hash and group two different files
// as duplicates. The BLAKE3 tag stored alongside makes that need a
// simultaneous collision in both, which is not something a chosen filename
// can arrange.
func pathKey(p string) uint64 {
	h := fnv.New64a()
	io.WriteString(h, p)
	return h.Sum64()
}

func pathTag(p string) [16]byte {
	sum := blake3.Sum256([]byte(p))
	var t [16]byte
	copy(t[:], sum[:16])
	return t
}

// loadHashCache reads the cache file (missing or corrupt → empty cache) and
// advances the generation for the scan that is starting.
func loadHashCache(dir string) *hashCache {
	c := &hashCache{ents: map[uint64]hcEnt{}}
	if dir == "" {
		return c
	}
	c.path = filepath.Join(dir, hashCacheFile)
	f, err := os.Open(c.path)
	if err != nil {
		c.gen = 1
		return c
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 256<<10)
	var magic [8]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil || magic != hashCacheMagic {
		c.gen = 1
		return c
	}
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		c.gen = 1
		return c
	}
	gen := binary.LittleEndian.Uint32(hdr[:4])
	count := binary.LittleEndian.Uint32(hdr[4:])
	for i := uint32(0); i < count && int(i) <= hashCacheMax; i++ {
		var rec [8 + 8 + 8 + 16 + 16 + 32 + 4]byte
		if _, err := io.ReadFull(r, rec[:]); err != nil {
			break // truncated tail: keep what parsed cleanly
		}
		key := binary.LittleEndian.Uint64(rec[0:])
		e := hcEnt{
			Size: int64(binary.LittleEndian.Uint64(rec[8:])),
			Mod:  int64(binary.LittleEndian.Uint64(rec[16:])),
			Gen:  binary.LittleEndian.Uint32(rec[88:]),
		}
		copy(e.Tag[:], rec[24:40])
		copy(e.Pfx[:], rec[40:56])
		copy(e.Hash[:], rec[56:88])
		c.ents[key] = e
	}
	c.gen = gen + 1
	return c
}

// loadHashCacheResume loads the store to CONTINUE an interrupted scan: the
// marker recorded the generation that run was recording under, and adopting
// it makes the dead run's own reads servable again while everything older
// stays history. That is the one sanctioned exception to "a scan serves only
// its own reads" — resuming IS that scan, at the user's explicit request
// (handleScan honors the flag only while the interruption marker stands, so
// a COMPLETED scan can never be "resumed" into). gen 0 means the dead run
// died before it ever opened the store: nothing of its reading exists, so
// this falls back to a normal load and the resume degenerates to a full
// re-read rather than guessing at a generation.
func loadHashCacheResume(dir string, gen uint32) *hashCache {
	c := loadHashCache(dir)
	if gen != 0 {
		c.gen = gen
	}
	return c
}

// lookup returns the full-content hash for a path that was already read IN
// THIS SCAN and whose size, mtime and prefix hash still match. Entries from
// earlier scans never hit — the generation gate is what guarantees that every
// hash a scan reports rests on bytes read during that scan, so bit rot under
// an unchanged size and mtime can never hide behind a stale entry. (Entries
// recorded this scan already carry the current generation, so no re-stamp
// is needed on a hit.)
func (c *hashCache) lookup(path string, size, mod int64, pfxHex string) (string, bool) {
	if c == nil {
		return "", false
	}
	pfx, err := hex.DecodeString(pfxHex)
	if err != nil || len(pfx) < 16 {
		return "", false
	}
	key := pathKey(path)
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.ents[key]
	if !ok || e.Gen != c.gen || e.Size != size || e.Mod != mod || e.Tag != pathTag(path) {
		return "", false
	}
	for i := 0; i < 16; i++ {
		if e.Pfx[i] != pfx[i] {
			return "", false
		}
	}
	return hex.EncodeToString(e.Hash[:]), true
}

// priorContentChanged reports that an earlier scan recorded this path at this
// same size and mtime with DIFFERENT content. Nothing that edits a file
// leaves its size and modified time untouched, so a change underneath them is
// the corrupted-files scan's strongest historical evidence — a timestamped
// before and after.
//
// Two sources, because the duplicates pass runs first and rewrites the bucket
// for every file it hashes: the `changed` set records what record() saw as it
// overwrote (prefix OR full-hash movement), and the live bucket answers for
// paths this scan has not written yet — with only the freshly-read prefix to
// compare, since that is all the caller has before the full read. A deep
// change in a not-yet-recorded path is instead caught by record() itself the
// moment the fresh full hash lands (its return value says so). Consulting
// only the live bucket left the rung answering "no" for exactly the files the
// duplicates pass had already read — which is most of them.
func (c *hashCache) priorContentChanged(path string, size, mod int64, pfxHex string) bool {
	if c == nil {
		return false
	}
	pfx, err := hex.DecodeString(pfxHex)
	if err != nil || len(pfx) < 16 {
		return false
	}
	key := pathKey(path)
	c.mu.Lock()
	defer c.mu.Unlock()
	if tag, ok := c.changed[key]; ok && tag == pathTag(path) {
		return true
	}
	e, ok := c.ents[key]
	// e.Gen == c.gen means the entry is this scan's own read coming back, not
	// history: a mismatch against it says only that the file moved DURING the
	// scan (an ordinary edit landing mid-run), and calling that rot convicted
	// healthy files — the walk's size and mtime were never re-observed.
	if !ok || e.Gen == c.gen || e.Size != size || e.Mod != mod || e.Tag != pathTag(path) {
		return false // no comparable history: not evidence either way
	}
	for i := 0; i < 16; i++ {
		if e.Pfx[i] != pfx[i] {
			return true
		}
	}
	return false
}

// record stores a freshly-computed full hash and reports whether it displaced
// an earlier scan's entry for the same path at the same size and mtime with
// different content — i.e. whether this write is itself evidence of bit rot.
// The comparison covers the full hash, not just the prefix, because the fresh
// full read is in hand right here; this is the only place a deep change in a
// path no other rung has asked about can ever be seen.
func (c *hashCache) record(path string, size, mod int64, pfxHex, fullHex string) bool {
	if c == nil {
		return false
	}
	pfx, err1 := hex.DecodeString(pfxHex)
	full, err2 := hex.DecodeString(fullHex)
	if err1 != nil || err2 != nil || len(pfx) < 16 || len(full) != 32 {
		return false
	}
	e := hcEnt{Size: size, Mod: mod, Gen: c.gen, Tag: pathTag(path)}
	copy(e.Pfx[:], pfx[:16])
	copy(e.Hash[:], full)
	key := pathKey(path)
	c.mu.Lock()
	moved := false
	// Capture the before-value while it still exists: this overwrite is what
	// destroys the corrupted-files scan's only historical evidence. Only an
	// OLDER generation's entry is evidence — the two observations then carry
	// two independent walks' size and mtime. A same-generation mismatch means
	// the file moved DURING this scan (an ordinary edit landing mid-run, its
	// new mtime never re-observed), and treating that as rot convicted
	// healthy files.
	if old, ok := c.ents[key]; ok && old.Gen != c.gen &&
		old.Size == size && old.Mod == mod && old.Tag == e.Tag &&
		(old.Pfx != e.Pfx || old.Hash != e.Hash) {
		moved = true
		if c.changed == nil {
			c.changed = map[uint64][16]byte{}
		}
		if len(c.changed) < changedMax {
			c.changed[key] = e.Tag
		}
	}
	c.ents[key] = e
	c.dirty = true
	// The entry cap has to hold in RAM, not only on disk. A scan of a volume
	// with millions of never-before-hashed files would otherwise grow this
	// map until the daemon died — the file it eventually wrote would have
	// been trimmed, but only after the memory was already spent.
	if len(c.ents) > hashCacheHigh {
		c.trimMidScanLocked(hashCacheMax)
	}
	c.mu.Unlock()
	return moved
}

// changedPath reports whether record() has captured this path's content
// moving under an unchanged size and mtime during this scan. The skew
// sampler uses it to guarantee a member carrying rot evidence is listed —
// a per-tag quota must never sample away the very finding. It answers only
// for what the capped `changed` set could hold: evidence the skew pass
// proves itself travels beside it in that pass's own rotted map.
func (c *hashCache) changedPath(path string) bool {
	if c == nil {
		return false
	}
	key := pathKey(path)
	c.mu.Lock()
	defer c.mu.Unlock()
	tag, ok := c.changed[key]
	return ok && tag == pathTag(path)
}

// trimMidScanLocked bounds the map while a scan is still CONSUMING history.
// Current-generation entries go first: their displacement evidence was
// captured into `changed` the moment record() overwrote, and a later reuse
// merely costs a re-read — dropping them can never lose a conviction. Only
// when that is not enough does older history go, oldest generation first;
// that loss is real (the dropped path cannot be compared against its past,
// this scan or next), which is the bounded-evidence trade the entry cap
// forces. The old behavior — keep-newest even mid-scan — deleted exactly
// the history the rest of the scan still needed, and a probe showed a
// single partition save at cap losing prior-scan evidence in 182/200 runs.
func (c *hashCache) trimMidScanLocked(keep int) {
	for k, e := range c.ents {
		if len(c.ents) <= keep {
			return
		}
		if e.Gen == c.gen {
			delete(c.ents, k)
		}
	}
	c.trimLocked(keep)
}

// trimLocked drops the oldest generations until at most keep entries remain,
// so retention favours what recent scans actually touched. Caller holds mu.
func (c *hashCache) trimLocked(keep int) {
	if len(c.ents) <= keep {
		return
	}
	gens := make([]uint32, 0, len(c.ents))
	for _, e := range c.ents {
		gens = append(gens, e.Gen)
	}
	// Keep the `keep` newest generations: find the generation on the
	// boundary, then drop everything strictly older than it.
	sort.Slice(gens, func(i, j int) bool { return gens[i] > gens[j] })
	cutoff := gens[keep-1]
	for k, e := range c.ents {
		if e.Gen < cutoff {
			delete(c.ents, k)
		}
	}
	// Everything left may share the boundary generation (a single huge scan
	// stamps one generation on every entry); drop arbitrary members of it
	// until the cap holds. A dropped entry costs at most one piece of rot
	// HISTORY (the next scan cannot compare that path against its past), never
	// a wrong hash — identity always comes from this scan's own reads. Bounded
	// evidence is the same trade changedMax makes.
	for k := range c.ents {
		if len(c.ents) <= keep {
			break
		}
		delete(c.ents, k)
	}
}

// save writes the store atomically (temp + rename, 0600) at the END of a
// scan, trimming the map to the entry cap newest-generation-first — the scan
// has consumed all the history it will, so retention now only serves the
// next scan, which wants the freshest baselines. A store without a path
// (dev runs without a state dir) or without changes is a no-op.
func (c *hashCache) save() error { return c.saveTrim(true) }

// saveMid writes the store DURING a scan — the per-window crash insurance.
// It caps what it writes (newest generations first) but deletes nothing from
// RAM: the in-memory history must survive intact for the rest of this scan's
// rot comparisons, and the RAM bound is trimMidScanLocked's job.
func (c *hashCache) saveMid() error { return c.saveTrim(false) }

func (c *hashCache) saveTrim(final bool) error {
	if c == nil || c.path == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dirty {
		return nil
	}
	type kv struct {
		key uint64
		e   hcEnt
	}
	if final {
		c.trimLocked(hashCacheMax)
	}
	all := make([]kv, 0, len(c.ents))
	for k, e := range c.ents {
		all = append(all, kv{k, e})
	}
	if !final && len(all) > hashCacheMax {
		// The file must stay loadable within the cap (load stops reading
		// there), so write a newest-first view and leave the map alone.
		sort.Slice(all, func(i, j int) bool { return all[i].e.Gen > all[j].e.Gen })
		all = all[:hashCacheMax]
	}
	err := writeAtomic(c.path, 0o600, false, func(tmp *os.File) error {
		w := bufio.NewWriterSize(tmp, 256<<10)
		w.Write(hashCacheMagic[:])
		var hdr [8]byte
		binary.LittleEndian.PutUint32(hdr[:4], c.gen)
		binary.LittleEndian.PutUint32(hdr[4:], uint32(len(all)))
		w.Write(hdr[:])
		for _, it := range all {
			var rec [8 + 8 + 8 + 16 + 16 + 32 + 4]byte
			binary.LittleEndian.PutUint64(rec[0:], it.key)
			binary.LittleEndian.PutUint64(rec[8:], uint64(it.e.Size))
			binary.LittleEndian.PutUint64(rec[16:], uint64(it.e.Mod))
			copy(rec[24:40], it.e.Tag[:])
			copy(rec[40:56], it.e.Pfx[:])
			copy(rec[56:88], it.e.Hash[:])
			binary.LittleEndian.PutUint32(rec[88:], it.e.Gen)
			if _, err := w.Write(rec[:]); err != nil {
				return err
			}
		}
		return w.Flush()
	})
	if err != nil {
		return err
	}
	c.dirty = false
	return nil
}
