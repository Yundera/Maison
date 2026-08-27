// Package bench runs the on-demand disk and network measurements behind the
// Resources page.
//
// Both are ports of the shell one-liners the admin dashboard ran over SSH, but
// implemented in Go rather than by shelling out. That is not tidiness: the runtime
// image is alpine with busybox, which has no curl at all, and whose dd supports
// iflag=direct only depending on how it was built. A benchmark that silently
// measures the page cache because a flag was ignored is worse than no benchmark.
package bench

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// diskSize is what gets written. Large enough to swamp the drive's own cache
	// and the kernel's readahead window so sustained throughput dominates, small
	// enough to finish in a few seconds on slow hardware.
	diskSize = 256 << 20 // 256 MiB
	// chunk is the IO size per call. O_DIRECT requires the buffer, the file offset
	// and the length to all be block-aligned; a page-aligned 4 MiB chunk satisfies
	// every logical block size in use.
	chunk = 4 << 20

	scratchName = ".bench.tmp"
)

// DiskResult is one disk measurement.
type DiskResult struct {
	WriteBps     float64 `json:"write_bps"`
	ReadBps      float64 `json:"read_bps"`
	WriteSeconds float64 `json:"write_seconds"`
	ReadSeconds  float64 `json:"read_seconds"`
	SizeBytes    int64   `json:"size_bytes"`
	Target       string  `json:"target"`

	// Direct reports whether the read bypassed the page cache. When it is false
	// the filesystem refused O_DIRECT (tmpfs and some network mounts do) and the
	// read figure is measuring memory, not the disk — so the UI labels it rather
	// than presenting a fantasy number as a benchmark.
	Direct bool `json:"direct"`
}

// ScratchPath is where the disk benchmark writes.
func ScratchPath(dir string) string { return filepath.Join(dir, scratchName) }

// CleanScratch removes a scratch file left behind by an interrupted run. Called at
// startup: without it, a crash mid-benchmark leaves 256 MiB of zeroes on the
// user's data disk indefinitely.
func CleanScratch(dir string) {
	_ = os.Remove(ScratchPath(dir))
}

// RunDisk measures write-then-read throughput on the data disk.
//
// Unlike the admin dashboard's version this needs no sudo: that one ran as an
// unprivileged SSH user, whereas Maison already owns its data root.
func RunDisk(ctx context.Context, dir string) (DiskResult, error) {
	path := ScratchPath(dir)
	res := DiskResult{SizeBytes: diskSize, Target: path}
	defer os.Remove(path)

	buf, err := alignedBuffer(chunk)
	if err != nil {
		return res, err
	}
	defer unix.Munmap(buf)

	// ---- write ----
	// O_DIRECT alone does not guarantee the data reached the platter, so the write
	// is timed inclusive of the fdatasync — otherwise this measures how fast the
	// kernel accepts bytes, which is a property of RAM.
	wf, direct, err := openScratch(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return res, err
	}
	start := time.Now()
	for written := 0; written < diskSize; written += len(buf) {
		if err := ctx.Err(); err != nil {
			wf.Close()
			return res, err
		}
		if _, err := wf.Write(buf); err != nil {
			wf.Close()
			return res, fmt.Errorf("write: %w", err)
		}
	}
	if err := wf.Sync(); err != nil {
		wf.Close()
		return res, fmt.Errorf("sync: %w", err)
	}
	res.WriteSeconds = time.Since(start).Seconds()
	wf.Close()

	// ---- read ----
	rf, readDirect, err := openScratch(path, os.O_RDONLY)
	if err != nil {
		return res, err
	}
	defer rf.Close()
	start = time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		n, err := rf.Read(buf)
		if n == 0 || err != nil {
			break
		}
	}
	res.ReadSeconds = time.Since(start).Seconds()
	res.Direct = direct && readDirect

	if res.WriteSeconds > 0 {
		res.WriteBps = float64(diskSize) / res.WriteSeconds
	}
	if res.ReadSeconds > 0 {
		res.ReadBps = float64(diskSize) / res.ReadSeconds
	}
	return res, nil
}

// openScratch opens the scratch file with O_DIRECT when the filesystem allows it,
// reporting whether it got it. tmpfs and several network filesystems reject
// O_DIRECT outright; falling back keeps the benchmark usable there instead of
// failing, and the caller surfaces which mode was used.
func openScratch(path string, flags int) (*os.File, bool, error) {
	if f, err := os.OpenFile(path, flags|unix.O_DIRECT, 0o600); err == nil {
		return f, true, nil
	}
	f, err := os.OpenFile(path, flags, 0o600)
	return f, false, err
}

// alignedBuffer returns a page-aligned buffer of n bytes.
//
// O_DIRECT requires the user buffer to be aligned to the device's logical block
// size. An anonymous mmap is page-aligned by definition, which sidesteps the usual
// trick of over-allocating a Go slice and indexing to an aligned offset — that one
// depends on the garbage collector never relocating the backing array, which is
// true today and is not a promise.
func alignedBuffer(n int) ([]byte, error) {
	b, err := unix.Mmap(-1, 0, n, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANON)
	if err != nil {
		return nil, fmt.Errorf("allocate aligned buffer: %w", err)
	}
	return b, nil
}
