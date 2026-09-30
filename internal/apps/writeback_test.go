package apps

import (
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// The writer only shapes page-cache use; what lands on disk must be byte-for-byte
// what went in, across window boundaries, short tails and odd write sizes.
func TestWritebackFileRoundTrip(t *testing.T) {
	for _, size := range []int{0, 1, writebackWindow - 1, writebackWindow, writebackWindow + 1, 3*writebackWindow + 12345} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			want := make([]byte, size)
			if _, err := rand.Read(want); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "out")
			f, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			w := newWritebackFile(f)
			// An odd buffer size, so windows are crossed mid-write.
			if _, err := io.CopyBuffer(w, bytes.NewReader(want), make([]byte, 100_003)); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("content differs: got %d bytes, want %d", len(got), len(want))
			}
		})
	}
}

// TestCopyFileLargeUnderMemoryLimit is the watch.nsl.sh reproduction, run by hand:
// build the test binary, then run it in a container whose memory limit is far
// below the file size. Before bounded writeback the copy was OOM-killed.
//
//	MAISON_BIGCOPY_SRC=/data/big.bin MAISON_BIGCOPY_DST=/data/big.copy ./apps.test -test.run LargeUnderMemoryLimit
func TestCopyFileLargeUnderMemoryLimit(t *testing.T) {
	src, dst := os.Getenv("MAISON_BIGCOPY_SRC"), os.Getenv("MAISON_BIGCOPY_DST")
	if src == "" || dst == "" {
		t.Skip("manual: set MAISON_BIGCOPY_SRC and MAISON_BIGCOPY_DST")
	}
	fi, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst, fi, func(int64) {}); err != nil {
		t.Fatal(err)
	}
	got, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if got.Size() != fi.Size() {
		t.Fatalf("copied %d bytes, want %d", got.Size(), fi.Size())
	}
}

// TestMirrorManyFilesUnderMemoryLimit is the second watch.nsl.sh reproduction, run by
// hand: mirror a tree of very many small files inside a memory-limited container.
// Before, mirror held every stale path in a map and nntmux's 1.27M-file NZB store
// took Maison's heap past 512 MiB.
//
//	MAISON_BIGMIRROR_SRC=/data/tree MAISON_BIGMIRROR_DST=/data/copy ./apps.test -test.run ManyFiles
func TestMirrorManyFilesUnderMemoryLimit(t *testing.T) {
	src, dst := os.Getenv("MAISON_BIGMIRROR_SRC"), os.Getenv("MAISON_BIGMIRROR_DST")
	if src == "" || dst == "" {
		t.Skip("manual: set MAISON_BIGMIRROR_SRC and MAISON_BIGMIRROR_DST")
	}
	if err := mirror(src, dst, nil, nil); err != nil {
		t.Fatal(err)
	}
}
