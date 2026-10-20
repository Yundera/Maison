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
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"

	"github.com/yundera/maison/internal/cgroupstat"
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
	cpus     int

	// cg, when set, is the cgroup source: counters read straight from the kernel,
	// which answers at once instead of after Docker's one-second double sample.
	// Docker stats remain the path when the cgroup tree is not mounted.
	cg *cgroupSource
}

// cgroupSource is the live view's own reader and baseline. It must not share
// either with the history recorder: the two run on different cadences, and one
// baseline differentiated by both would corrupt both (see internal/cgroupstat).
type cgroupSource struct {
	tracker *cgroupstat.Tracker

	mu   sync.Mutex
	rd   *cgroupstat.Reader
	prev map[string]reading
}

type reading struct {
	at time.Time
	c  cgroupstat.Counters
}

// New returns a Sampler reading from dx, and from tracker's cgroups when tracker
// is non-nil.
func New(dx docker, tracker *cgroupstat.Tracker) *Sampler {
	s := &Sampler{dx: dx, cpus: HostCPUs()}
	if vm, err := mem.VirtualMemory(); err == nil {
		s.memTotal = vm.Total
	}
	if tracker != nil {
		s.cg = &cgroupSource{tracker: tracker, rd: cgroupstat.NewReader(), prev: map[string]reading{}}
	}
	return s
}

// HostCPUs is the host's logical core count — /proc/cpuinfo is not namespaced, so
// this is the box's, not a cpuset's.
func HostCPUs() int {
	if n, err := cpu.Counts(true); err == nil && n > 0 {
		return n
	}
	return 1
}

// MemTotal is the host's RAM.
func (s *Sampler) MemTotal() uint64 { return s.memTotal }

// Sample returns current per-app usage, busiest first. Containers that fail to
// report (stopping as we read them, say) are skipped rather than failing the
// round: a monitor that blanks because one container went away is worse than one
// that is briefly a row short.
func (s *Sampler) Sample(ctx context.Context) Snapshot {
	if s.cg != nil {
		return s.sampleCgroups(ctx)
	}
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

// baselineGap is how long the live baseline stays usable. The dashboard asks every
// few seconds while open; a baseline older than this is from a previous visit, and a
// rate over it would be an average of the whole time the page was closed.
const baselineGap = 30 * time.Second

// sampleCgroups is Sample from the cgroup tree. A first call with no fresh baseline
// takes two readings a quarter-second apart, so the first frame of a newly opened
// monitor already shows CPU rather than a row of zeroes.
func (s *Sampler) sampleCgroups(ctx context.Context) Snapshot {
	snap := Snapshot{Apps: []Stat{}, MemTotal: s.memTotal, CPUCount: s.cpus}
	targets := s.cg.tracker.Targets(ctx)

	s.cg.mu.Lock()
	defer s.cg.mu.Unlock()
	now := time.Now()
	stale := false
	for _, tg := range targets {
		if p, ok := s.cg.prev[tg.ID]; !ok || now.Sub(p.at) > baselineGap {
			stale = true
			break
		}
	}
	if stale {
		s.cg.readAll(targets, now)
		select {
		case <-ctx.Done():
			return snap
		case <-time.After(250 * time.Millisecond):
		}
		now = time.Now()
	}

	prev := s.cg.prev
	cur := s.cg.readAll(targets, now)
	samples := make([]sample, 0, len(targets))
	for _, tg := range targets {
		c, ok := cur[tg.ID]
		if !ok {
			continue
		}
		sm := sample{project: tg.Project, ok: true, mem: c.c.MemUsage(), cpus: s.cpus}
		if p, ok := prev[tg.ID]; ok {
			if dt := c.at.Sub(p.at).Seconds(); dt > 0 && c.c.CPUUsec >= p.c.CPUUsec {
				sm.cpuShare = float64(c.c.CPUUsec-p.c.CPUUsec) / (dt * 1e6 * float64(s.cpus)) * 100
			}
		}
		samples = append(samples, sm)
	}
	snap.Apps = aggregate(samples, s.memTotal)
	return snap
}

// readAll reads every target and makes the result the new baseline.
func (c *cgroupSource) readAll(targets []cgroupstat.Target, now time.Time) map[string]reading {
	next := make(map[string]reading, len(targets))
	for _, tg := range targets {
		v, err := c.rd.Read(tg.Dir)
		if err != nil {
			c.tracker.MarkDirty()
			continue
		}
		next[tg.ID] = reading{at: now, c: v}
	}
	c.prev = next
	return next
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
