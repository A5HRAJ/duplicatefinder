// Content hashing through the pinned root handles, the bounded worker pool
// every content pass shares, and the move-time prefix re-check.
package main

import (
	"encoding/hex"
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"

	"lukechampine.com/blake3"
)

// parallelEach runs fn over items with a small worker pool, reporting
// progress. The pool is capped well below the CPU count: the work is
// dominated by disk reads, and more readers on a NAS array means more
// seeking, not more throughput.
func parallelEach[T any](items []T, cancel chan struct{}, fn func(T), prog func(done, total int)) {
	workers := runtime.NumCPU()
	if workers > 4 {
		workers = 4
	}
	if workers < 1 {
		workers = 1
	}
	ch := make(chan T)
	var wg sync.WaitGroup
	var done int64
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range ch {
				if cancelled(cancel) {
					continue
				}
				fn(f)
				d := atomic.AddInt64(&done, 1)
				if prog != nil && d%16 == 0 {
					prog(int(d), len(items))
				}
			}
		}()
	}
	for _, f := range items {
		if cancelled(cancel) {
			break
		}
		ch <- f
	}
	close(ch)
	wg.Wait()
}

// contentPrefixUnchanged re-reads a file's first 64 KiB and reports whether
// it still hashes to what the scan recorded. cp is the canonical path the
// move has already vetted; any failure to read reports "changed", because
// the caller uses this only to refuse.
func contentPrefixUnchanged(cp, want string) bool {
	if want == "" {
		return true
	}
	h, err := hashFile(func() (*os.File, error) { return os.Open(cp) }, 64*1024, nil)
	if err != nil || len(h) < len(want) {
		return false
	}
	return h[:len(want)] == want
}

// hashBufPool recycles hashFile's read buffer. Allocating (and zeroing) a
// fresh megabyte per call would cost as much as the warm 64 KiB prefix read
// it wraps, and put a GC cycle every hundred files under the hashing workers.
var hashBufPool = sync.Pool{New: func() any { b := make([]byte, 1024*1024); return &b }}

// hashFile hashes up to limit bytes of a file (-1 = whole file). open is
// the entry's pinned-handle opener (fEnt.openContent), so the bytes hashed
// are read from inside the vetted tree — never through a symlink swapped
// in after enumeration.
func hashFile(open func() (*os.File, error), limit int64, cancel chan struct{}) (string, error) {
	f, err := open()
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := blake3.New(32, nil)
	var r io.Reader = f
	if limit > 0 {
		r = io.LimitReader(f, limit)
	}
	bp := hashBufPool.Get().(*[]byte)
	defer hashBufPool.Put(bp)
	buf := *bp
	for {
		if cancelled(cancel) {
			return "", errCancelled
		}
		n, err := r.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
