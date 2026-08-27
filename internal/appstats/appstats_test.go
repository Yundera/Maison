package appstats

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker/api/types/container"

	"github.com/yundera/maison/internal/dockerx"
)

// frame builds a stats sample as the daemon would report it, and a memory usage
// with `cache` of page cache in it. The system delta is one second across every
// core (that is what Docker counts), so cpuDelta reads as nanoseconds of one
// core: 1e9 is one core fully used, whatever `cpus` is.
func frame(cpuDelta uint64, cpus int, usage, cache uint64) container.StatsResponse {
	var v container.StatsResponse
	v.CPUStats.CPUUsage.TotalUsage = cpuDelta
	v.CPUStats.SystemUsage = uint64(1e9) * uint64(cpus)
	v.CPUStats.OnlineCPUs = uint32(cpus)
	v.MemoryStats.Usage = usage
	v.MemoryStats.Stats = map[string]uint64{"inactive_file": cache}
	return v
}

// One fully-used core out of four is 100% to Docker and a quarter of the box to
// us — the panel sits under the CPU gauge, so its rows have to be shares of the
// same whole.
func TestCPUPercentIsPerCoreAndSampleIsPerHost(t *testing.T) {
	v := frame(1e9, 4, 0, 0)
	if got := CPUPercent(v); got != 100 {
		t.Fatalf("CPUPercent = %v, want 100 (one full core)", got)
	}
	if got := CPUPercent(v) / onlineCPUs(v); got != 25 {
		t.Fatalf("host share = %v, want 25", got)
	}
}

func TestCPUPercentHandlesFirstFrameAndMissingCoreCount(t *testing.T) {
	// No delta yet (the frame is its own predecessor): 0, not a division by zero.
	var idle container.StatsResponse
	if got := CPUPercent(idle); got != 0 {
		t.Fatalf("CPUPercent on an empty frame = %v, want 0", got)
	}
	// cgroup v1 reports no OnlineCPUs; the per-CPU array stands in for it. One
	// full core of two here, which reads as 100% only if the fallback finds the
	// second core (and as 50% if it silently assumed one).
	v := frame(1e9, 2, 0, 0)
	v.CPUStats.OnlineCPUs = 0
	v.CPUStats.CPUUsage.PercpuUsage = []uint64{1, 2}
	if got := CPUPercent(v); got != 100 {
		t.Fatalf("CPUPercent = %v, want 100 (two cores from PercpuUsage)", got)
	}
	if got := onlineCPUs(container.StatsResponse{}); got != 1 {
		t.Fatalf("onlineCPUs with nothing to go on = %v, want 1", got)
	}
}

// Page cache is the kernel's, not the app's: an app that has read a large file
// must not be reported as holding it.
func TestMemUsageDeductsPageCache(t *testing.T) {
	if got := MemUsage(frame(0, 1, 900, 400)); got != 500 {
		t.Fatalf("MemUsage = %d, want 500", got)
	}
	// cgroup v1 spelling.
	v := frame(0, 1, 900, 0)
	v.MemoryStats.Stats = map[string]uint64{"total_inactive_file": 400}
	if got := MemUsage(v); got != 500 {
		t.Fatalf("MemUsage (cgroup v1) = %d, want 500", got)
	}
	// A cache figure larger than the usage is nonsense; report the raw usage
	// rather than underflowing an unsigned subtraction into gigabytes.
	if got := MemUsage(frame(0, 1, 100, 400)); got != 100 {
		t.Fatalf("MemUsage with oversized cache = %d, want 100", got)
	}
}

func TestAggregateSumsServicesAndOrdersBusiestFirst(t *testing.T) {
	got := aggregate([]sample{
		{project: "nextcloud", ok: true, cpuShare: 5, mem: 200},
		{project: "nextcloud", ok: true, cpuShare: 3, mem: 300}, // its database
		{project: "jellyfin", ok: true, cpuShare: 20, mem: 100},
		{project: "idle-a", ok: true, cpuShare: 0, mem: 50},
		{project: "idle-b", ok: true, cpuShare: 0, mem: 90},
		{project: "gone", ok: false, cpuShare: 99, mem: 999}, // failed to sample
		{project: "", ok: true, cpuShare: 1, mem: 1},         // not compose-managed
	}, 1000)

	var ids []string
	for _, st := range got {
		ids = append(ids, st.ID)
	}
	want := []string{"jellyfin", "nextcloud", "idle-b", "idle-a"}
	if len(ids) != len(want) {
		t.Fatalf("apps = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("apps = %v, want %v", ids, want)
		}
	}

	nc := got[1]
	if nc.CPUPercent != 8 || nc.MemUsage != 500 || nc.Containers != 2 {
		t.Fatalf("nextcloud = %+v, want cpu 8, mem 500, 2 containers", nc)
	}
	// 500 of 1000 bytes of host memory.
	if nc.MemPercent != 50 {
		t.Fatalf("nextcloud mem percent = %v, want 50", nc.MemPercent)
	}
}

// With no host memory figure, the byte counts still stand; only the percentage
// is withheld.
func TestAggregateWithoutHostMemory(t *testing.T) {
	got := aggregate([]sample{{project: "a", ok: true, mem: 100}}, 0)
	if len(got) != 1 || got[0].MemUsage != 100 || got[0].MemPercent != 0 {
		t.Fatalf("got %+v", got)
	}
}

type fakeDocker struct {
	list  []dockerx.Container
	stats map[string]container.StatsResponse
	err   error
}

func (f *fakeDocker) ListProjectContainers(context.Context) ([]dockerx.Container, error) {
	return f.list, f.err
}

func (f *fakeDocker) ContainerStatsOnce(_ context.Context, id string) (container.StatsResponse, error) {
	v, ok := f.stats[id]
	if !ok {
		return v, errors.New("no such container")
	}
	return v, nil
}

// Stopped containers have no stats to read, and a container that disappears
// mid-round drops out of its app rather than emptying the panel.
func TestSampleSkipsStoppedAndUnreadableContainers(t *testing.T) {
	dx := &fakeDocker{
		list: []dockerx.Container{
			{ID: "run", Project: "app", State: "running"},
			{ID: "dead", Project: "app", State: "exited"},
			{ID: "vanished", Project: "other", State: "running"},
		},
		stats: map[string]container.StatsResponse{
			"run": frame(2e9, 4, 1000, 0),
		},
	}
	s := &Sampler{dx: dx, memTotal: 4000}

	snap := s.Sample(context.Background())
	if len(snap.Apps) != 1 {
		t.Fatalf("apps = %+v, want just the running one", snap.Apps)
	}
	got := snap.Apps[0]
	// Two cores of four = half the box.
	if got.ID != "app" || got.CPUPercent != 50 || got.Containers != 1 {
		t.Fatalf("app = %+v, want cpu 50 over 1 container", got)
	}
	if got.MemPercent != 25 {
		t.Fatalf("mem percent = %v, want 25", got.MemPercent)
	}
	if snap.MemTotal != 4000 || snap.CPUCount != 4 {
		t.Fatalf("snapshot totals = %d bytes / %d cpus", snap.MemTotal, snap.CPUCount)
	}
}

// A daemon that cannot be listed yields an empty panel, not a nil payload the
// frontend would have to special-case.
func TestSampleWithoutDockerReturnsEmptyList(t *testing.T) {
	s := &Sampler{dx: &fakeDocker{err: errors.New("no daemon")}}
	snap := s.Sample(context.Background())
	if snap.Apps == nil || len(snap.Apps) != 0 {
		t.Fatalf("apps = %+v, want an empty list", snap.Apps)
	}
}
