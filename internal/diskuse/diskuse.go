// Package diskuse measures what the data root holds, folder by folder — the
// "your data" half of Settings › Resources › Storage.
//
// statfs answers "how full is the disk" instantly but cannot say what filled it.
// On a PCS that question matters more than on most machines, because /DATA is not
// its own filesystem (see internal/system): it is a directory on the root disk,
// beside the OS and Docker's images. The only way to split the disk's used bytes
// into "the user's" and "the system's" is to add up what is under /DATA — a walk,
// like `du`, which on a big library takes minutes.
//
// So it runs on demand, in the background, one at a time, with its running total
// readable while it goes; and its last result is kept on disk, so reopening the page
// shows the previous answer at once rather than a spinner.
package diskuse

import (
	"bufio"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Status of a scan.
const (
	StatusIdle    = "idle"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusError   = "error"
)

// Entry is one folder's share.
type Entry struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

// Result is one scan, finished or in progress.
type Result struct {
	Status     string    `json:"status"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Error      string    `json:"error,omitempty"`

	// Bytes is everything under the root, as allocated on disk (what `du` reports,
	// not the sum of file lengths: a sparse VM image counts what it occupies).
	//
	// While a rescan runs, Bytes, Files, Top and Apps keep the PREVIOUS scan's
	// answer — a figure counting up from zero would read as the disk emptying — and
	// the running total is in ProgressBytes instead.
	Bytes         int64 `json:"bytes"`
	Files         int64 `json:"files"`
	ProgressBytes int64 `json:"progress_bytes,omitempty"`

	// Top is each entry directly under the root. Apps breaks the apps' data folder
	// down one level further, since "AppData is 80 GB" invites "which app?".
	Top  []Entry `json:"top"`
	Apps []Entry `json:"apps"`

	// Skipped are mountpoints under the root that were not walked — a network or
	// FUSE mount (the Seafile rclone mount, for one) is not on this disk, and a bind
	// of the same disk would count its files twice.
	Skipped []string `json:"skipped,omitempty"`
	// Unreadable counts entries the walk could not open.
	Unreadable int64 `json:"unreadable,omitempty"`
}

// Scanner measures one root.
type Scanner struct {
	root string
	// appsDir is the folder, directly under root, whose children are broken down.
	appsDir string
	// cache persists the last finished result. Empty disables it.
	cache string
	// mountinfo is read to find mountpoints under root.
	mountinfo string

	mu   sync.Mutex
	last Result
}

// New builds a scanner for root. The last result is loaded from cache when present.
func New(root, appsDir, cache string) *Scanner {
	s := &Scanner{root: filepath.Clean(root), appsDir: appsDir, cache: cache, mountinfo: "/proc/self/mountinfo"}
	s.last = Result{Status: StatusIdle, Top: []Entry{}, Apps: []Entry{}}
	if cache != "" {
		if b, err := os.ReadFile(cache); err == nil {
			var r Result
			if json.Unmarshal(b, &r) == nil && r.Status == StatusDone {
				s.last = r
			}
		}
	}
	return s
}

// State is the last result, or the running scan's progress.
func (s *Scanner) State() Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.last
	r.Top = append([]Entry{}, r.Top...)
	r.Apps = append([]Entry{}, r.Apps...)
	return r
}

// Start begins a scan unless one is running, and returns the state as of now.
func (s *Scanner) Start() Result {
	s.mu.Lock()
	if s.last.Status == StatusRunning {
		r := s.last
		s.mu.Unlock()
		return r
	}
	prev := s.last
	s.last = prev
	s.last.Status, s.last.StartedAt, s.last.Error, s.last.ProgressBytes = StatusRunning, time.Now(), "", 0
	r := s.last
	s.mu.Unlock()
	go s.run(prev)
	return r
}

// progressEvery is how many entries pass between two publications of the running
// total. The lock is the page's, too, so it is not taken per file.
const progressEvery = 2000

func (s *Scanner) run(prev Result) {
	rootInfo, err := os.Stat(s.root)
	if err != nil {
		prev.Status, prev.Error = StatusError, err.Error()
		s.finish(prev)
		return
	}
	rootDev := devOf(rootInfo)
	mounts := mountsUnder(s.mountinfo, s.root)

	type inode struct{ dev, ino uint64 }
	seen := map[inode]bool{}
	top := map[string]int64{}
	apps := map[string]int64{}
	var total, files, unreadable int64
	var skipped []string
	appsPrefix := ""
	if s.appsDir != "" {
		appsPrefix = s.appsDir + string(filepath.Separator)
	}

	walkErr := filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			unreadable++
			if d != nil && d.IsDir() && path != s.root {
				return fs.SkipDir
			}
			return nil
		}
		if path == s.root {
			return nil
		}
		if d.IsDir() && mounts[path] {
			skipped = append(skipped, path)
			return fs.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			unreadable++
			return nil
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil
		}
		if d.IsDir() && uint64(st.Dev) != rootDev {
			// A mount mountinfo did not list (it changed mid-walk): same rule.
			skipped = append(skipped, path)
			return fs.SkipDir
		}
		if !d.IsDir() && st.Nlink > 1 {
			k := inode{uint64(st.Dev), uint64(st.Ino)}
			if seen[k] {
				return nil
			}
			seen[k] = true
		}
		n := int64(st.Blocks) * 512
		rel := strings.TrimPrefix(path, s.root+string(filepath.Separator))
		first, _, _ := strings.Cut(rel, string(filepath.Separator))
		top[first] += n
		if appsPrefix != "" && strings.HasPrefix(rel, appsPrefix) {
			app, _, _ := strings.Cut(strings.TrimPrefix(rel, appsPrefix), string(filepath.Separator))
			apps[app] += n
		}
		total += n
		files++
		if files%progressEvery == 0 {
			s.mu.Lock()
			s.last.ProgressBytes = total
			s.mu.Unlock()
		}
		return nil
	})

	r := Result{
		Status:     StatusDone,
		Bytes:      total,
		Files:      files,
		Top:        sorted(top),
		Apps:       sorted(apps),
		Skipped:    skipped,
		Unreadable: unreadable,
	}
	if walkErr != nil {
		prev.Status, prev.Error = StatusError, walkErr.Error()
		r = prev
	}
	s.finish(r)
}

func (s *Scanner) finish(r Result) {
	s.mu.Lock()
	r.StartedAt = s.last.StartedAt
	r.ProgressBytes = 0
	if r.Status == StatusDone {
		// A failed scan keeps the previous answer, and with it the time it was taken.
		r.FinishedAt = time.Now()
	}
	if r.Top == nil {
		r.Top = []Entry{}
	}
	if r.Apps == nil {
		r.Apps = []Entry{}
	}
	s.last = r
	s.mu.Unlock()
	if s.cache != "" && r.Status == StatusDone {
		if b, err := json.Marshal(r); err == nil {
			tmp := s.cache + ".tmp"
			if os.WriteFile(tmp, b, 0o644) == nil {
				_ = os.Rename(tmp, s.cache)
			}
		}
	}
}

func sorted(m map[string]int64) []Entry {
	out := make([]Entry, 0, len(m))
	for k, v := range m {
		out = append(out, Entry{Name: k, Bytes: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bytes != out[j].Bytes {
			return out[i].Bytes > out[j].Bytes
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func devOf(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev)
	}
	return 0
}

// mountsUnder lists the mountpoints strictly below root, from a mountinfo file.
func mountsUnder(path, root string) map[string]bool {
	out := map[string]bool{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	prefix := root + "/"
	if root == "/" {
		prefix = "/"
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		mp := unescapeMount(fields[4])
		if mp != root && strings.HasPrefix(mp, prefix) {
			out[mp] = true
		}
	}
	return out
}

// unescapeMount undoes mountinfo's octal escapes (\040 for a space, and so on).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			o := s[i+1 : i+4]
			if o[0] >= '0' && o[0] <= '3' && o[1] >= '0' && o[1] <= '7' && o[2] >= '0' && o[2] <= '7' {
				b.WriteByte((o[0]-'0')<<6 | (o[1]-'0')<<3 | (o[2] - '0'))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
