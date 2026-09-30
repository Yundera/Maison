package apps

import (
	"os"

	"golang.org/x/sys/unix"
)

// writebackWindow is how much a bulk write may leave dirty in the page cache
// before it pushes that stretch to disk and drops it. Two windows are in flight at
// most — one being written back, one being filled — so a copy of any size holds
// about 16 MiB of dirty cache, not "as much as the kernel lets it".
const writebackWindow = 8 << 20

// writebackFile is an *os.File whose writes are pushed to disk as they go and then
// evicted from the page cache, so a bulk copy's memory footprint does not grow
// with the file.
//
// Why this exists: page cache is charged to the cgroup of the process that dirtied
// it, and Maison runs under a container memory limit. A plain io.Copy of a
// multi-GB file dirties pages faster than the kernel flushes them, and on
// watch.nsl.sh (2026-09-30) the pre-update rollback copy of a 5.5 GB Redis
// database filled maison-app's 512 MiB cgroup with 495 MiB of dirty pages and
// zero under writeback — Maison's own heap was 19 MiB — and the OOM killer took
// the process, and the in-memory "update all" run with it. Dirty pages cannot be
// reclaimed without writing them first, so neither a larger limit nor waiting
// fixes it: a bigger file just needs a bigger limit. Bounding what a copy leaves
// dirty does.
//
// The pattern is the classic streaming-writer one: once a window is full, start
// its writeback (SYNC_FILE_RANGE_WRITE, asynchronous), then wait for the PREVIOUS
// window to land and drop it from the cache. Writing and flushing overlap, so the
// copy stays near disk speed. It is not a durability guarantee (no fsync of
// metadata) — only a cap on memory.
//
// Only the DESTINATION is evicted. The source is live app data (a Redis dump, a
// redb blockstore) whose hot pages belong to the app that uses them, and
// FADV_DONTNEED would evict them for everyone. Clean pages Maison reads are
// charged to it only if nobody had them cached, and are cheap to reclaim.
type writebackFile struct {
	f       *os.File
	written int64 // bytes written so far
	flushed int64 // everything below this has had writeback started
	dropped int64 // everything below this has been written back and evicted
}

func newWritebackFile(f *os.File) *writebackFile {
	return &writebackFile{f: f}
}

func (w *writebackFile) Write(p []byte) (int, error) {
	n, err := w.f.Write(p)
	w.written += int64(n)
	if w.written-w.flushed >= writebackWindow {
		w.advance()
	}
	return n, err
}

// advance starts writeback of everything written since the last call, then waits
// for the window before that and evicts it.
func (w *writebackFile) advance() {
	fd := int(w.f.Fd())
	// Errors are ignored throughout: this only shapes memory use. A filesystem
	// that does not support the calls still gets a correct (if cache-heavy) copy,
	// and a real write error surfaces from Write or Close.
	_ = unix.SyncFileRange(fd, w.flushed, w.written-w.flushed, unix.SYNC_FILE_RANGE_WRITE)
	if w.flushed > w.dropped {
		_ = unix.SyncFileRange(fd, w.dropped, w.flushed-w.dropped,
			unix.SYNC_FILE_RANGE_WAIT_BEFORE|unix.SYNC_FILE_RANGE_WRITE|unix.SYNC_FILE_RANGE_WAIT_AFTER)
		_ = unix.Fadvise(fd, w.dropped, w.flushed-w.dropped, unix.FADV_DONTNEED)
		w.dropped = w.flushed
	}
	w.flushed = w.written
}

// Close drains what is still dirty, evicts it, and closes the file.
func (w *writebackFile) Close() error {
	if w.written > w.dropped {
		fd := int(w.f.Fd())
		_ = unix.SyncFileRange(fd, w.dropped, w.written-w.dropped,
			unix.SYNC_FILE_RANGE_WAIT_BEFORE|unix.SYNC_FILE_RANGE_WRITE|unix.SYNC_FILE_RANGE_WAIT_AFTER)
		_ = unix.Fadvise(fd, w.dropped, w.written-w.dropped, unix.FADV_DONTNEED)
		w.dropped = w.written
	}
	return w.f.Close()
}
