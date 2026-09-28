package diskuse

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

func wait(t *testing.T, s *Scanner) Result {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if r := s.State(); r.Status != StatusRunning {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("scan did not finish")
	return Result{}
}

func find(es []Entry, name string) (Entry, bool) {
	for _, e := range es {
		if e.Name == name {
			return e, true
		}
	}
	return Entry{}, false
}

func TestScanBreaksDownTopAndApps(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "Media", "movie.bin"), 256*1024)
	write(t, filepath.Join(root, "AppData", "jellyfin", "db.bin"), 128*1024)
	write(t, filepath.Join(root, "AppData", "nextcloud", "x", "y.bin"), 64*1024)
	// A hard link is one file on disk, however many names it has.
	if err := os.Link(filepath.Join(root, "Media", "movie.bin"), filepath.Join(root, "Media", "movie-link.bin")); err != nil {
		t.Fatal(err)
	}

	cache := filepath.Join(t.TempDir(), "diskuse.json")
	s := New(root, "AppData", cache)
	s.mountinfo = filepath.Join(t.TempDir(), "none") // no mounts
	s.Start()
	r := wait(t, s)
	if r.Status != StatusDone {
		t.Fatalf("status %s: %s", r.Status, r.Error)
	}

	media, _ := find(r.Top, "Media")
	appdata, _ := find(r.Top, "AppData")
	if media.Bytes < 256*1024 || media.Bytes >= 2*256*1024 {
		t.Fatalf("Media = %d: the hard link must be counted once", media.Bytes)
	}
	if appdata.Bytes < 192*1024 {
		t.Fatalf("AppData = %d, want at least its two files", appdata.Bytes)
	}
	jf, ok := find(r.Apps, "jellyfin")
	nc, ok2 := find(r.Apps, "nextcloud")
	if !ok || !ok2 || jf.Bytes < 128*1024 || nc.Bytes < 64*1024 {
		t.Fatalf("apps breakdown wrong: %+v", r.Apps)
	}
	if r.Bytes < media.Bytes+appdata.Bytes {
		t.Fatalf("total %d below its parts", r.Bytes)
	}

	// The next process starts from the persisted answer.
	again := New(root, "AppData", cache)
	if st := again.State(); st.Status != StatusDone || st.Bytes != r.Bytes {
		t.Fatalf("cached result not loaded: %+v", st)
	}
}

func TestMountsUnderAreSkipped(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "Seafile", "big.bin"), 512*1024)
	write(t, filepath.Join(root, "Documents", "a.txt"), 4096)
	mi := filepath.Join(t.TempDir(), "mountinfo")
	line := "100 1 0:50 / " + filepath.Join(root, "Seafile") + " rw - fuse.rclone remote rw\n"
	if err := os.WriteFile(mi, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(root, "AppData", "")
	s.mountinfo = mi
	s.Start()
	r := wait(t, s)
	if _, ok := find(r.Top, "Seafile"); ok {
		t.Fatalf("a mount under the root must not be walked: %+v", r.Top)
	}
	if len(r.Skipped) != 1 {
		t.Fatalf("skipped = %v", r.Skipped)
	}
}

func TestUnescapeMount(t *testing.T) {
	if got := unescapeMount(`/DATA/My\040Files`); got != "/DATA/My Files" {
		t.Fatalf("got %q", got)
	}
}
