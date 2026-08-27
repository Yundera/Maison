// Package appstats samples per-app resource usage for the dashboard's monitor
// panel: one row per compose project, aggregated over that project's containers.
//
// It is deliberately separate from internal/system, which samples the host. The
// two answer different questions on the same screen — the gauges say how loaded
// the box is, this says which app is doing it — and are normalised so the second
// explains the first: an app's CPU is its share of the *whole* host (0-100
// across every core), not Docker's per-core figure where 100% means one core.
package appstats

import (
	"context"
	"sort"
	"sync"

	"github.com/docker/docker/api/types/container"
	"github.com/shirou/gopsutil/v4/mem"

	"github.com/yundera/maison/internal/dockerx"
)

// maxConcurrent caps how many containers are sampled at once. Each sample is a
// daemon round-trip that blocks for ~1s (see dockerx.ContainerStatsOnce), so the
// wall clock is set by this cap, not by the container count — while a box with
// forty containers still never opens forty connections at once.
const maxConcurrent = 8

// Stat is one app's usage, summed over its containers.
type Stat struct {
	// ID is the compose project name, which is also apps.App.ID — the frontend
	// joins on it to get the tile's name and icon rather than duplicating them
	// on every sample.
	ID string `json:"id"`
	// CPUPercent is the share of the whole host, 0-100 across all cores, so the
	// rows are comparable with the CPU gauge and with each other.
	CPUPercent float64 `json:"cpu_percent"`
	MemUsage   uint64  `json:"mem_usage"`
	MemPercent float64 `json:"mem_percent"`
	// Containers is how many of the app's containers were sampled: running ones
	// only, since a stopped container has no stats to read.
	Containers int `json:"containers"`
}

// Snapshot is one sampling round.
type Snapshot struct {
	Apps []Stat `json:"apps"`
	// MemTotal and CPUCount are what the percentages above are relative to, so a
	// client rendering this needs no second source to label them.
	MemTotal uint64 `json:"mem_total"`
	CPUCount int    `json:"cpu_count"`
}

// docker is the slice of dockerx.Client this package uses, named so tests can
// substitute a fake without a daemon.
type docker interface {
	ListProjectContainers(ctx context.Context) ([]dockerx.Container, error)
	ContainerStatsOnce(ctx context.Context, id string) (container.StatsResponse, error)
}

// Sampler reads per-app usage from Docker.
type Sampler struct {
	dx docker

	// memTotal is read once: the amount of RAM in the box does not change while
	// Maison is running, and re-reading it every tick would be a syscall per
	// sample for a constant.
	memTotal uint64
}

// New returns a Sampler reading from dx.
func New(dx docker) *Sampler {
	s := &Sampler{dx: dx}
	if vm, err := mem.VirtualMemory(); err == nil {
		s.memTotal = vm.Total
	}
	return s
}

// Sample returns current per-app usage, busiest first. Containers that fail to
// report (stopping as we read them, say) are skipped rather than failing the
// round: a monitor that blanks because one container went away is worse than one
// that is briefly a row short.
func (s *Sampler) Sample(ctx context.Context) Snapshot {
	snap := Snapshot{Apps: []Stat{}, MemTotal: s.memTotal}

	list, err := s.dx.ListProjectContainers(ctx)
	if err != nil {
		return snap
	}
	running := make([]dockerx.Container, 0, len(list))
	for _, c := range list {
		if c.State == "running" {
			running = append(running, c)
		}
	}

	samples := make([]sample, len(running))
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	for i, c := range running {
		wg.Add(1)
		go func(i int, c dockerx.Container) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			v, err := s.dx.ContainerStatsOnce(ctx, c.ID)
			if err != nil {
				return
			}
			cpus := onlineCPUs(v)
			samples[i] = sample{
				project: c.Project,
				ok:      true,
				// Docker's percentage counts one full core as 100%; dividing by the
				// core count restates it as a share of the box.
				cpuShare: CPUPercent(v) / cpus,
				mem:      MemUsage(v),
				cpus:     int(cpus),
			}
		}(i, c)
	}
	wg.Wait()

	for _, sm := range samples {
		if sm.ok && sm.cpus > snap.CPUCount {
			snap.CPUCount = sm.cpus
		}
	}
	snap.Apps = aggregate(samples, s.memTotal)
	return snap
}

// sample is one container's contribution, before aggregation.
type sample struct {
	project  string
	ok       bool
	cpuShare float64
	mem      uint64
	cpus     int
}

// aggregate sums samples per compose project and orders them busiest first.
func aggregate(samples []sample, memTotal uint64) []Stat {
	byProject := make(map[string]*Stat)
	order := make([]string, 0, len(samples))
	for _, sm := range samples {
		if !sm.ok || sm.project == "" {
			continue
		}
		st, ok := byProject[sm.project]
		if !ok {
			st = &Stat{ID: sm.project}
			byProject[sm.project] = st
			order = append(order, sm.project)
		}
		st.CPUPercent += sm.cpuShare
		st.MemUsage += sm.mem
		st.Containers++
	}

	out := make([]Stat, 0, len(order))
	for _, id := range order {
		st := byProject[id]
		st.CPUPercent = round1(st.CPUPercent)
		if memTotal > 0 {
			st.MemPercent = round1(float64(st.MemUsage) / float64(memTotal) * 100)
		}
		out = append(out, *st)
	}
	// Busiest first, memory breaking a CPU tie — an idle box is all zeroes on the
	// CPU column, and ordering those by name would put the heaviest app last.
	sort.Slice(out, func(i, j int) bool {
		if out[i].CPUPercent != out[j].CPUPercent {
			return out[i].CPUPercent > out[j].CPUPercent
		}
		if out[i].MemUsage != out[j].MemUsage {
			return out[i].MemUsage > out[j].MemUsage
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// CPUPercent derives Docker's CPU percentage from a stats frame (which carries
// the previous sample for the delta), where 100% is one fully-used core.
func CPUPercent(v container.StatsResponse) float64 {
	cpuDelta := float64(v.CPUStats.CPUUsage.TotalUsage) - float64(v.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(v.CPUStats.SystemUsage) - float64(v.PreCPUStats.SystemUsage)
	if sysDelta > 0 && cpuDelta > 0 {
		return round1((cpuDelta / sysDelta) * onlineCPUs(v) * 100)
	}
	return 0
}

// MemUsage is a container's memory usage with its page cache deducted, matching
// the MEM USAGE column of `docker stats`. The raw counter includes file cache the
// kernel will drop under pressure, which on a box that has been reading media
// makes an idle app look like it is holding gigabytes.
func MemUsage(v container.StatsResponse) uint64 {
	usage := v.MemoryStats.Usage
	// cgroup v2 names it inactive_file, v1 total_inactive_file.
	cache := v.MemoryStats.Stats["inactive_file"]
	if c, ok := v.MemoryStats.Stats["total_inactive_file"]; ok {
		cache = c
	}
	if cache < usage {
		return usage - cache
	}
	return usage
}

// onlineCPUs is how many cores the frame was measured across, falling back to the
// per-CPU array (cgroup v1) and finally to 1 so callers never divide by zero.
func onlineCPUs(v container.StatsResponse) float64 {
	if v.CPUStats.OnlineCPUs > 0 {
		return float64(v.CPUStats.OnlineCPUs)
	}
	if n := len(v.CPUStats.CPUUsage.PercpuUsage); n > 0 {
		return float64(n)
	}
	return 1
}

func round1(f float64) float64 {
	return float64(int64(f*10+0.5)) / 10
}
