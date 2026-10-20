package metrics

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yundera/maison/internal/cgroupstat"
)

type fakeTargets struct {
	targets []cgroupstat.Target
	dirty   int
}

func (f *fakeTargets) Targets(context.Context) []cgroupstat.Target { return f.targets }
func (f *fakeTargets) MarkDirty()                                  { f.dirty++ }

// fakeCounters returns, per cgroup dir, counters advanced by fixed amounts per read.
type fakeCounters struct {
	c map[string]cgroupstat.Counters
}

func (f *fakeCounters) Read(dir string) (cgroupstat.Counters, error) {
	c, ok := f.c[dir]
	if !ok {
		return c, cgroupstat.ErrGone
	}
	return c, nil
}

func newTestAppSampler(t *testing.T, cpus int) (*AppSampler, *fakeTargets, *fakeCounters, *time.Time) {
	t.Helper()
	s := NewAppSampler(filepath.Join(t.TempDir(), "apps"), time.Minute, 60, cpus, nil, nil)
	src := &fakeTargets{}
	rd := &fakeCounters{c: map[string]cgroupstat.Counters{}}
	s.src, s.rd = src, rd
	now := base
	s.now = func() time.Time { return now }
	return s, src, rd, &now
}

func TestAppSamplerSumsContainersPerApp(t *testing.T) {
	s, src, rd, now := newTestAppSampler(t, 4)
	src.targets = []cgroupstat.Target{
		{ID: "a", Project: "immich", Dir: "/a"},
		{ID: "b", Project: "immich", Dir: "/b"},
	}
	rd.c["/a"] = cgroupstat.Counters{CPUUsec: 0, MemCurrent: 100, IORead: 0}
	rd.c["/b"] = cgroupstat.Counters{CPUUsec: 0, MemCurrent: 50}
	s.tick(context.Background()) // baseline only

	*now = now.Add(time.Minute)
	// Container a used one full core for the minute, b half a core: 1.5 of 4 cores.
	rd.c["/a"] = cgroupstat.Counters{CPUUsec: 60e6, MemCurrent: 100, IORead: 6000}
	rd.c["/b"] = cgroupstat.Counters{CPUUsec: 30e6, MemCurrent: 50}
	s.tick(context.Background())
	s.Flush()

	usage, _, err := s.Read("immich", base, base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 {
		t.Fatalf("got %d records, want 1 (the first tick is a baseline)", len(usage))
	}
	r := usage[0]
	if r.CPU != 37.5 {
		t.Errorf("CPU = %v, want 37.5 (1.5 cores of 4)", r.CPU)
	}
	if r.Mem != 150 || r.Containers != 2 {
		t.Errorf("Mem = %v, Containers = %d", r.Mem, r.Containers)
	}
	if r.DiskRead != 100 {
		t.Errorf("DiskRead = %v, want 100 B/s", r.DiskRead)
	}
}

// A recreated container (an update gives it a new ID) has no baseline: its first
// reading must not turn the whole cumulative counter into one minute's usage.
func TestAppSamplerRecreatedContainerStartsFresh(t *testing.T) {
	s, src, rd, now := newTestAppSampler(t, 1)
	src.targets = []cgroupstat.Target{{ID: "old", Project: "app", Dir: "/old"}}
	rd.c["/old"] = cgroupstat.Counters{CPUUsec: 1e6}
	s.tick(context.Background())

	*now = now.Add(time.Minute)
	src.targets = []cgroupstat.Target{{ID: "new", Project: "app", Dir: "/new"}}
	rd.c["/new"] = cgroupstat.Counters{CPUUsec: 500e9}
	s.tick(context.Background())
	s.Flush()

	usage, _, _ := s.Read("app", base, base.Add(time.Hour))
	if len(usage) != 0 {
		t.Errorf("got %+v, want nothing recorded for a baseline-only tick", usage)
	}
}

func TestAppSamplerGapIsNotSmeared(t *testing.T) {
	s, src, rd, now := newTestAppSampler(t, 1)
	src.targets = []cgroupstat.Target{{ID: "a", Project: "app", Dir: "/a"}}
	rd.c["/a"] = cgroupstat.Counters{}
	s.tick(context.Background())
	*now = now.Add(10 * time.Minute)
	rd.c["/a"] = cgroupstat.Counters{CPUUsec: 600e6}
	s.tick(context.Background())
	s.Flush()
	if usage, _, _ := s.Read("app", base, base.Add(time.Hour)); len(usage) != 0 {
		t.Errorf("a 10-minute gap was averaged into %+v", usage)
	}
}

func TestAppSamplerVanishedCgroupMarksDirty(t *testing.T) {
	s, src, _, _ := newTestAppSampler(t, 1)
	src.targets = []cgroupstat.Target{{ID: "a", Project: "app", Dir: "/gone"}}
	s.tick(context.Background())
	if src.dirty != 1 {
		t.Errorf("dirty = %d, want 1", src.dirty)
	}
}

// Staged records are visible before they are flushed: the graph's last minutes
// must not lag by up to five.
func TestAppSamplerReadIncludesStaged(t *testing.T) {
	s, src, rd, now := newTestAppSampler(t, 1)
	src.targets = []cgroupstat.Target{{ID: "a", Project: "app", Dir: "/a"}}
	rd.c["/a"] = cgroupstat.Counters{}
	s.tick(context.Background())
	*now = now.Add(time.Minute)
	s.tick(context.Background())

	if _, err := os.Stat(s.usagePath("app")); err == nil {
		t.Fatal("ring written before a flush was due")
	}
	usage, _, _ := s.Read("app", base, base.Add(time.Hour))
	if len(usage) != 1 {
		t.Errorf("got %d staged records, want 1", len(usage))
	}
}

func TestAppSamplerDiskAndLifecycle(t *testing.T) {
	s, _, _, _ := newTestAppSampler(t, 1)
	day := time.Unix(1_700_000_000/86400*86400, 0)
	s.RecordDisk(map[string]int64{"immich": 5 << 30, "jellyfin": 1 << 20, "../evil": 1}, day)

	_, disk, err := s.Read("immich", day, day.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(disk) != 1 || disk[0].Bytes != 5<<30 {
		t.Fatalf("disk = %+v", disk)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(s.dir), "evil.disk.ring")); err == nil {
		t.Error("an app ID escaped the history directory")
	}

	s.Prune(map[string]bool{"immich": true})
	if _, disk, _ := s.Read("jellyfin", day, day.Add(time.Hour)); len(disk) != 0 {
		t.Error("Prune kept an app that is no longer installed")
	}
	if s.Bytes() == 0 {
		t.Error("Bytes = 0 with a ring on disk")
	}
	s.Remove("immich")
	if s.Bytes() != 0 {
		t.Errorf("Bytes = %d after removing the last app", s.Bytes())
	}
}

func TestAppCodecRoundTrip(t *testing.T) {
	in := AppRecord{At: base, CPU: 12.34, Mem: 512 << 20, DiskRead: 1000, DiskWrite: 2000, Containers: 3}
	b := make([]byte, appCodec.Size)
	appCodec.Encode(b, in)
	out, ok := appCodec.Decode(b)
	if !ok || out.CPU != 12.34 || out.Mem != 512<<20 || out.Containers != 3 || out.DiskWrite != 2000 || !out.At.Equal(base) {
		t.Errorf("round trip: %+v", out)
	}
}

func TestDownsampleApp(t *testing.T) {
	recs := []AppRecord{
		{At: base, CPU: 10, Mem: 100, Containers: 1},
		{At: base.Add(time.Minute), CPU: 30, Mem: 300, Containers: 2},
		{At: base.Add(50 * time.Minute), CPU: 5, Mem: 50, Containers: 1},
	}
	got := DownsampleApp(recs, base, base.Add(time.Hour), 6) // 10-minute buckets
	if len(got) != 2 {
		t.Fatalf("got %d spans, want 2 (empty buckets omitted)", len(got))
	}
	if got[0].CPU != (MinAvgMax{Min: 10, Avg: 20, Max: 30}) || got[0].Containers != 2 || got[0].Points != 2 {
		t.Errorf("span 0 = %+v", got[0])
	}
}
