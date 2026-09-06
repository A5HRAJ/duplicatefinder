package main

// Bounded-memory enumeration primitives. The walk never materializes every
// file in RAM — each tool consumes the entry stream through a small
// accumulator:
//
//   - duplicates spool compact records to a spill file while a bounded
//     candidate-key counter finds collisions; the collision candidates are
//     then distilled into a second, smaller spill and processed one
//     PARTITION at a time, so daemon memory scales with the window, not with
//     the volume and not with the duplicate population;
//   - the flat tools keep bounded top-K heaps that reproduce a full
//     sort-then-cap exactly;
//   - duplicate groups accumulate in a bounded top-K heap of their own, so a
//     volume with millions of duplicates never holds more groups than the
//     stored-results cap can keep.
//
// The spill files are created unlinked (opened, then removed), so they are
// invisible, private, and reclaimed by the kernel on any exit — including a
// crash mid-scan.

import (
	"bufio"
	"container/heap"
	"dupfinder/internal/dirhandle"
	"encoding/binary"
	"errors"
	"hash/fnv"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// candHash is the pass-1 candidate-bucket key: size plus the requested
// pre-enrichment criteria. Created dates are deliberately absent — they are
// not known until File Station is asked, and only collision candidates are
// asked. A 64-bit hash stands in for the composite string key; a hash
// collision merely promotes a unique file into the candidate set, where the
// content hashes separate it again — it can never merge two different
// files into one duplicate group.
func candHash(f *fEnt, m MatchOpts) uint64 {
	h := fnv.New64a()
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(f.size))
	h.Write(b[:])
	if m.Name {
		io.WriteString(h, f.name)
		h.Write([]byte{0})
	}
	if m.Modified {
		io.WriteString(h, strconv.FormatInt(f.mod.Unix(), 10))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

// ------------------------------------------------------- candidate counter

// keyCounter answers one question per candidate key — "did more than one file
// land here?" — in bounded memory.
//
// It is EXACT while the key population is small: an ordinary map, ~14 bytes
// per unique key. On an 80 TB volume with tens of millions of files that map
// alone would run to hundreds of megabytes, so once it passes
// keyCounterExactMax the counts move into a fixed-size table of saturating
// 2-bit counters (two independent slots per key, Bloom-style) and the map is
// released. From then on the answer can only ever be a FALSE POSITIVE — two
// unrelated keys sharing both slots — which promotes a unique file into the
// candidate set, where the prefix and content hashes separate it again. It
// can never hide a real collision, so no duplicate is ever missed. With two
// slots the false-positive rate stays around 0.01% at 20 million keys, i.e.
// a rounding error of extra hashing rather than a correctness question.
type keyCounter struct {
	exact map[uint64]byte // nil once the table takes over
	tab   []byte          // 4 two-bit counters per byte
	mask  uint64
	// share halves both budgets once per step, so N counters running over one
	// walk cost what a single counter would. The duplicates scan runs two —
	// its own candidate key and the corrupted-files (size, mtime) key — and
	// the fixed 64 MB ceiling above is a promise about the daemon, not about
	// one counter. Halving raises the false-positive rate, which only ever
	// costs a little extra hashing.
	share uint
}

// Both are variables only so the tests can shrink them: forcing the
// degraded mode honestly needs millions of keys otherwise.
var (
	// ~2M keys ≈ 30 MB of map before the fixed table takes over.
	keyCounterExactMax = 2 << 20
	// 2^28 slots × 2 bits = 64 MB, flat for any volume size.
	keyCounterTabBits = uint(28)
)

// newKeyCounterShare builds a counter entitled to 1/2^share of the memory
// budget, for use when several counters are fed from one walk.
func newKeyCounterShare(share uint) *keyCounter {
	return &keyCounter{exact: map[uint64]byte{}, share: share}
}

func (k *keyCounter) exactMax() int {
	m := keyCounterExactMax >> k.share
	if m < 1 {
		m = 1
	}
	return m
}

func (k *keyCounter) add(key uint64) {
	if k.exact != nil {
		if c := k.exact[key]; c < 2 {
			k.exact[key] = c + 1
		}
		if len(k.exact) > k.exactMax() {
			k.degrade()
		}
		return
	}
	k.bump(key)
}

// hot reports whether at least two files shared this candidate key.
func (k *keyCounter) hot(key uint64) bool {
	if k.exact != nil {
		return k.exact[key] >= 2
	}
	if k.tab == nil {
		return false
	}
	a, b := k.slots(key)
	return k.get(a) >= 2 && k.get(b) >= 2
}

// release drops the counter's memory once the candidates are distilled.
func (k *keyCounter) release() { k.exact, k.tab = nil, nil }

// degrade replays the exact counts into the fixed table and frees the map.
func (k *keyCounter) degrade() {
	bits := keyCounterTabBits
	if bits > k.share+2 {
		bits -= k.share
	}
	k.tab = make([]byte, 1<<(bits-2))
	k.mask = 1<<bits - 1
	for key, c := range k.exact {
		k.bump(key)
		if c >= 2 {
			k.bump(key)
		}
	}
	k.exact = nil
}

// mix is a splitmix64 finalizer: it makes the second slot independent of the
// first (and of the partition index, which uses the raw key).
func mix(x uint64) uint64 {
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	x *= 0xc4ceb9fe1a85ec53
	x ^= x >> 33
	return x
}

func (k *keyCounter) slots(key uint64) (uint64, uint64) {
	return key & k.mask, mix(key) & k.mask
}

func (k *keyCounter) get(p uint64) byte {
	return (k.tab[p>>2] >> uint((p&3)*2)) & 3
}

func (k *keyCounter) bumpAt(p uint64) {
	if v := k.get(p); v < 2 {
		sh := uint((p & 3) * 2)
		k.tab[p>>2] = k.tab[p>>2]&^(3<<sh) | (v+1)<<sh
	}
}

func (k *keyCounter) bump(key uint64) {
	a, b := k.slots(key)
	k.bumpAt(a)
	k.bumpAt(b)
}

// ------------------------------------------------------------------- spill

// spill is an on-disk enumeration log: one compact record per file (root
// index, size, mtime, root-relative path — the display path and pinned
// handle are reattached on read). The duplicates scan writes two: the walk's
// full log, and the smaller distilled log of collision candidates.
type spill struct {
	f      *os.File
	w      *bufio.Writer
	n      int
	closed bool
	// onEach, when set, is called as a full pass streams: a scan of an 80 TB
	// volume spends real time in these passes, and without this the UI sits on
	// one unchanging label for minutes at a stretch.
	onEach func(done, total int)
}

func newSpill(dir string) (*spill, error) {
	f, err := os.CreateTemp(dir, "dupfinder-spill-*")
	if err != nil {
		return nil, err
	}
	// Unlink immediately: the open descriptor keeps the file alive, nothing
	// else can open it, and the space is reclaimed on any exit path.
	os.Remove(f.Name())
	return &spill{f: f, w: bufio.NewWriterSize(f, 256<<10)}, nil
}

func (s *spill) add(rootIdx int, f *fEnt) error {
	return s.addRaw(rootIdx, f.size, f.mod.Unix(), f.rel, 0)
}

// addRaw appends one record. tag is 0 on the walk and candidate spills; on a
// skew sub-spill it carries the file's content-prefix fingerprint, which is
// what that spill is partitioned by.
func (s *spill) addRaw(rootIdx int, size, mod int64, rel string, tag uint64) error {
	var buf [4 * binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], uint64(rootIdx))
	n += binary.PutUvarint(buf[n:], uint64(size))
	n += binary.PutVarint(buf[n:], mod)
	n += binary.PutUvarint(buf[n:], tag)
	if _, err := s.w.Write(buf[:n]); err != nil {
		return err
	}
	var lb [binary.MaxVarintLen64]byte
	ln := binary.PutUvarint(lb[:], uint64(len(rel)))
	if _, err := s.w.Write(lb[:ln]); err != nil {
		return err
	}
	if _, err := s.w.WriteString(rel); err != nil {
		return err
	}
	// The count bounds every later pass over this file. On the 32-bit ARM
	// build int holds two billion, and a tree that large is refused with a
	// reason rather than read back with a wrapped count.
	if s.n == math.MaxInt {
		return errors.New("too many files for this build to count")
	}
	s.n++
	return nil
}

// spillRec is one record as read back — no display path, no handle, so a
// full pass over a multi-million-file log allocates only the rel string.
type spillRec struct {
	rootIdx int
	size    int64
	mod     int64
	tag     uint64
	rel     string
}

// spillRelMax bounds one record's relative path; PATH_MAX is 4096.
const spillRelMax = 1 << 16

// each rewinds the spill and streams every record through fn. It never holds
// more than one record, so it is safe over any volume size.
func (s *spill) each(fn func(*spillRec) error) error {
	if err := s.w.Flush(); err != nil {
		return err
	}
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReaderSize(s.f, 256<<10)
	var rec spillRec
	for i := 0; i < s.n; i++ {
		if s.onEach != nil && i%4096 == 0 {
			s.onEach(i, s.n)
		}
		rootIdx, err := binary.ReadUvarint(r)
		if err != nil {
			return err
		}
		size, err := binary.ReadUvarint(r)
		if err != nil {
			return err
		}
		mod, err := binary.ReadVarint(r)
		if err != nil {
			return err
		}
		tag, err := binary.ReadUvarint(r)
		if err != nil {
			return err
		}
		relLen, err := binary.ReadUvarint(r)
		if err != nil {
			return err
		}
		// The spill is the daemon's own private file, but it lives on the
		// system partition and a corrupt block must fail the scan, not take
		// the daemon down: a garbage length would be a multi-gigabyte
		// allocation, and a garbage root index wraps negative in a 32-bit int
		// and walks straight past the `>= len(rootOf)` guards into a panic.
		if relLen > spillRelMax || rootIdx > math.MaxInt32 {
			return errors.New("scan spill file is corrupt")
		}
		relB := make([]byte, relLen)
		if _, err := io.ReadFull(r, relB); err != nil {
			return err
		}
		rec = spillRec{rootIdx: int(rootIdx), size: int64(size), mod: mod, tag: tag, rel: string(relB)}
		if err := fn(&rec); err != nil {
			return err
		}
	}
	return nil
}

// key recomputes a record's candidate key without building a full entry.
func (r *spillRec) key(m MatchOpts) uint64 {
	f := fEnt{size: r.size, mod: time.Unix(r.mod, 0)}
	if m.Name {
		f.name = filepath.Base(r.rel)
	}
	return candHash(&f, m)
}

// distil streams this spill into out, keeping only the records whose
// candidate key collided, and returns how many it kept. Nothing but one
// record is in memory at a time, so the candidate population — however
// large — never has to fit in RAM at this point.
func (s *spill) distil(counter *keyCounter, m MatchOpts, out *spill) (int, error) {
	n := 0
	err := s.each(func(r *spillRec) error {
		if !counter.hot(r.key(m)) {
			return nil
		}
		n++
		return out.addRaw(r.rootIdx, r.size, r.mod, r.rel, 0)
	})
	return n, err
}

// materialize rebuilds full in-memory entries for the records keep accepts,
// reattaching the display path and the pinned root handle from the walk's
// parallel slices. max is a last-resort backstop on the window as a whole (0
// disables it); the count it had to skip comes back so the scan can report it
// rather than quietly thinning its results.
func (s *spill) materialize(keep func(r *spillRec) bool, handles []*dirhandle.Handle, rootOf []string, max int) ([]fEnt, int, error) {
	var out []fEnt
	skipped := 0
	err := s.each(func(r *spillRec) error {
		if r.rootIdx >= len(rootOf) {
			return nil // corrupt record; skip rather than fabricate a path
		}
		if !keep(r) {
			return nil
		}
		if max > 0 && len(out) >= max {
			skipped++
			return nil
		}
		path := filepath.Join(rootOf[r.rootIdx], r.rel)
		out = append(out, fEnt{
			path: path, name: filepath.Base(r.rel), dir: filepath.Dir(path),
			size: r.size, mod: time.Unix(r.mod, 0),
			rel: r.rel, rh: handles[r.rootIdx],
		})
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return out, skipped, nil
}

// window materializes the records belonging to one partition of the
// candidate key space. Every file sharing a candidate key shares its
// partition, so a duplicate group can never be split across windows.
//
// Partitioning cannot split a KEY, though, and keys are not evenly populated:
// every macOS .DS_Store is 6148 bytes, so on a NAS full of Mac clients one
// key can cover hundreds of thousands of files. Rather than truncate such a
// key — which would silently lose whatever duplicates lived in the part that
// was dropped — window leaves it out of the ordinary window entirely and
// names it in over, for the caller to re-partition by content prefix. The
// counting pass that finds them allocates one small map entry per key, never
// a file record.
func (s *spill) window(part, parts int, m MatchOpts, handles []*dirhandle.Handle, rootOf []string, keyCap, max int) (ents []fEnt, over []uint64, skipped int, err error) {
	inPart := func(k uint64) bool { return parts <= 1 || k%uint64(parts) == uint64(part) }
	// counts holds one small entry per candidate KEY in this partition, never
	// a file record. In expectation that is about window/2 entries whatever
	// the volume holds — a hot key has at least two files, so there are at
	// most nCand/2 keys, while parts is nCand/window. It is NOT a hard bound
	// though: the counter's degraded mode can distil the odd singleton key,
	// and nothing forces keys to spread evenly across partitions. The true
	// worst case is the distinct keys of one partition, which an adversary
	// who can choose file sizes could concentrate.
	counts := map[uint64]int{}
	if err := s.each(func(r *spillRec) error {
		if k := r.key(m); inPart(k) {
			counts[k]++
		}
		return nil
	}); err != nil {
		return nil, nil, 0, err
	}
	overSet := map[uint64]bool{}
	for k, n := range counts {
		if keyCap > 0 && n > keyCap {
			overSet[k] = true
			over = append(over, k)
		}
	}
	sort.Slice(over, func(i, j int) bool { return over[i] < over[j] }) // deterministic order
	ents, skipped, err = s.materialize(func(r *spillRec) bool {
		k := r.key(m)
		return inPart(k) && !overSet[k]
	}, handles, rootOf, max)
	return ents, over, skipped, err
}

// tagWindow materializes one partition of a sub-spill, which is partitioned
// by the record TAG rather than by the candidate key. It mirrors window: a
// tag holding more records than tagCap is left out and named in over, for the
// caller to split further — truncating it here would lose duplicates exactly
// the way truncating a candidate key would.
func (s *spill) tagWindow(part, parts, tagCap, max int, handles []*dirhandle.Handle, rootOf []string) (ents []fEnt, over []uint64, crowded int, err error) {
	inPart := func(t uint64) bool { return parts <= 1 || t%uint64(parts) == uint64(part) }
	counts := map[uint64]int{}
	if err := s.each(func(r *spillRec) error {
		if inPart(r.tag) {
			counts[r.tag]++
		}
		return nil
	}); err != nil {
		return nil, nil, 0, err
	}
	overSet := map[uint64]bool{}
	for t, n := range counts {
		if tagCap > 0 && n > tagCap {
			overSet[t] = true
			over = append(over, t)
		}
	}
	sort.Slice(over, func(i, j int) bool { return over[i] < over[j] })
	ents, crowded, err = s.materialize(func(r *spillRec) bool {
		return inPart(r.tag) && !overSet[r.tag]
	}, handles, rootOf, max)
	// crowded > 0 means this partition did not fit even after its
	// over-populated tags were taken out — many distinct tags landed in it.
	// The caller must split it further, NOT accept the loss: these records
	// have not been examined, and a dropped one takes its duplicates with it.
	return ents, over, crowded, err
}

// tagsIn lists the distinct tags of one partition, in order, holding only the
// tags themselves. It is what turns a partition too crowded to materialize
// into a sequence of single-tag slices — every record still gets looked at.
func (s *spill) tagsIn(part, parts int) ([]uint64, error) {
	seen := map[uint64]bool{}
	if err := s.each(func(r *spillRec) error {
		if parts <= 1 || r.tag%uint64(parts) == uint64(part) {
			seen[r.tag] = true
		}
		return nil
	}); err != nil {
		return nil, err
	}
	out := make([]uint64, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// tagSlice materializes up to max records carrying one exact tag, reporting
// how many it had to leave. This is the terminal step of the skew ladder:
// by then a tag means "same full content", so the records share ONE group and
// the cap is the per-group cap — a real limit on the result, not a silent
// loss of some other group.
func (s *spill) tagSlice(tag uint64, handles []*dirhandle.Handle, rootOf []string, max int) ([]fEnt, int, error) {
	return s.materialize(func(r *spillRec) bool { return r.tag == tag }, handles, rootOf, max)
}

// tagsDiffer reports whether a sub-spill holds more than one tag — whether the
// files it covers disagree. It answers in CONSTANT memory: the question is a
// yes/no, and a per-tag map would scale with the very skew this is called to
// handle (a key with millions of members could carry millions of distinct
// prefixes).
func (s *spill) tagsDiffer() (bool, error) {
	var first uint64
	seen, differ := false, false
	err := s.each(func(r *spillRec) error {
		if !seen {
			first, seen = r.tag, true
			return nil
		}
		if r.tag != first {
			differ = true
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return differ, nil
}

func (s *spill) close() {
	if !s.closed {
		s.closed = true
		s.f.Close()
	}
}

// -------------------------------------------------------------- boundedTop

// boundedTop keeps the limit highest-ranked entries of a stream, counting
// what it drops. keepBefore(a, b) reports whether a outranks b in the final
// listing — the comparator a full sort would use, so the kept set and its
// order match a sort-then-truncate exactly.
type boundedTop struct {
	limit      int
	keepBefore func(a, b *fEnt) bool
	ents       []fEnt
	dropped    int
}

func newBoundedTop(limit int, keepBefore func(a, b *fEnt) bool) *boundedTop {
	return &boundedTop{limit: limit, keepBefore: keepBefore}
}

// heap.Interface over ents: the root is the lowest-ranked kept entry — the
// one the next better entry evicts.
func (t *boundedTop) Len() int           { return len(t.ents) }
func (t *boundedTop) Less(i, j int) bool { return t.keepBefore(&t.ents[j], &t.ents[i]) }
func (t *boundedTop) Swap(i, j int)      { t.ents[i], t.ents[j] = t.ents[j], t.ents[i] }
func (t *boundedTop) Push(x any)         { t.ents = append(t.ents, x.(fEnt)) }
func (t *boundedTop) Pop() any {
	x := t.ents[len(t.ents)-1]
	t.ents = t.ents[:len(t.ents)-1]
	return x
}

func (t *boundedTop) add(f fEnt) {
	if len(t.ents) < t.limit {
		heap.Push(t, f)
		return
	}
	t.dropped++
	if t.keepBefore(&f, &t.ents[0]) {
		t.ents[0] = f
		heap.Fix(t, 0)
	}
}

// final returns the kept entries in listing order plus the truncation
// report (nil when nothing was dropped).
func (t *boundedTop) final() ([]fEnt, *TruncInfo) {
	sort.Slice(t.ents, func(i, j int) bool { return t.keepBefore(&t.ents[i], &t.ents[j]) })
	if t.dropped == 0 {
		return t.ents, nil
	}
	return t.ents, &TruncInfo{Files: t.dropped, Cap: t.limit}
}

// ----------------------------------------------------------- group top-K

// rawGroup is a duplicate group before it becomes a result row: the pinned
// scanner entries, not FileEnts. Groups are accumulated in this form so that
// the expensive per-file work the result needs — ID assignment and the EXIF
// read — happens only for the groups that survive the stored-results cap.
type rawGroup struct {
	size  int64
	hash  string
	pfx   string // 64 KiB-prefix fingerprint, shared by definition: same content
	files []fEnt // sorted by path
	extra int    // members the per-group cap dropped, for the truncation report
}

// weight is the reclaimable space the group represents. It counts the members
// the per-group cap already dropped (extra) as well: a group cut down to the
// cap is still the biggest thing on the volume, and ranking it by its
// truncated length would let smaller groups displace it from the results.
func (g *rawGroup) weight() int64 { return g.size * int64(len(g.files)+g.extra-1) }

// betterGroup is the listing order: largest reclaimable space first. The
// tiebreaks (hash, then first path) make the order total, so a result set
// assembled from several scan windows is reproducible rather than dependent
// on map iteration order.
func betterGroup(a, b *rawGroup) bool {
	wa, wb := a.weight(), b.weight()
	if wa != wb {
		return wa > wb
	}
	if a.hash != b.hash {
		return a.hash < b.hash
	}
	return a.files[0].path < b.files[0].path
}

// groupTop keeps the limit best duplicate groups seen across every scan
// window, counting what it drops. limit is derived from the stored-results
// file budget: a group holds at least two files, so no more than budget/2+1
// groups can ever survive capDuplicateGroups — keeping that many is enough
// to reproduce the cap exactly while bounding memory on a volume with
// millions of duplicate groups.
type groupTop struct {
	limit         int
	gs            []rawGroup
	droppedGroups int
	droppedFiles  int
}

func newGroupTop(budget int) *groupTop {
	limit := budget/2 + 1
	if limit < 1 {
		limit = 1
	}
	return &groupTop{limit: limit}
}

func (t *groupTop) Len() int           { return len(t.gs) }
func (t *groupTop) Less(i, j int) bool { return betterGroup(&t.gs[j], &t.gs[i]) }
func (t *groupTop) Swap(i, j int)      { t.gs[i], t.gs[j] = t.gs[j], t.gs[i] }
func (t *groupTop) Push(x any)         { t.gs = append(t.gs, x.(rawGroup)) }
func (t *groupTop) Pop() any {
	x := t.gs[len(t.gs)-1]
	t.gs = t.gs[:len(t.gs)-1]
	return x
}

func (t *groupTop) add(g rawGroup) {
	if len(t.gs) < t.limit {
		heap.Push(t, g)
		return
	}
	if betterGroup(&g, &t.gs[0]) {
		t.droppedGroups++
		t.droppedFiles += len(t.gs[0].files) + t.gs[0].extra
		t.gs[0] = g
		heap.Fix(t, 0)
		return
	}
	t.droppedGroups++
	t.droppedFiles += len(g.files) + g.extra
}

// noteSkipped records files a scan never examined at all (the window caps),
// so they show up in the same truncation report as everything else dropped.
func (t *groupTop) noteSkipped(n int) { t.droppedFiles += n }

// sorted returns the kept groups in listing order.
func (t *groupTop) sorted() []rawGroup {
	sort.Slice(t.gs, func(i, j int) bool { return betterGroup(&t.gs[i], &t.gs[j]) })
	return t.gs
}
