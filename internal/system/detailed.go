package system

import (
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"
	"github.com/shirou/gopsutil/v4/sensors"
)

// Detailed is the full host picture the Resources page shows: everything the
// dashboard gauges have, plus the per-interface, per-device, per-filesystem and
// per-process breakdowns they leave out.
//
// Nothing here is sampled unless somebody has the page open — see the "resources"
// channel in internal/live. That is the whole reason it is a separate type from
// Stats rather than more fields on it.
type Detailed struct {
	At       int64 `json:"at"` // unix milliseconds
	Uptime   uint64 `json:"uptime"`
	CPUCount int    `json:"cpu_count"`

	CPUPercent float64 `json:"cpu_percent"`
	CPUTempC   float64 `json:"cpu_temp_c"`
	Load1      float64 `json:"load1"`
	Load5      float64 `json:"load5"`
	Load15     float64 `json:"load15"`

	Mem MemStat `json:"mem"`

	Nets  []NetIfRate  `json:"nets"`
	Disks []DiskIORate `json:"disks"`

	Filesystems Filesystems `json:"filesystems"`

	TopProcesses []ProcStat `json:"top_processes"`

	// HostProc reports whether the host's own /proc is mounted. When it is false
	// the network table and the process list are unavailable, and the page says so
	// rather than showing this container's own figures as if they were the box's.
	HostProc bool `json:"host_proc"`
}

// MemStat is memory and swap, in bytes plus the derived percentage.
type MemStat struct {
	TotalBytes     uint64  `json:"total_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	CachedBytes    uint64  `json:"cached_bytes"`
	UsedPercent    float64 `json:"used_percent"`

	SwapTotalBytes uint64  `json:"swap_total_bytes"`
	SwapUsedBytes  uint64  `json:"swap_used_bytes"`
	SwapPercent    float64 `json:"swap_percent"`
}

// NetIfRate is an interface's totals and its current throughput.
type NetIfRate struct {
	Iface   string  `json:"iface"`
	RxBytes uint64  `json:"rx_bytes"`
	TxBytes uint64  `json:"tx_bytes"`
	RxBps   float64 `json:"rx_bps"`
	TxBps   float64 `json:"tx_bps"`
}

// DiskIORate is a block device's totals and its current throughput.
type DiskIORate struct {
	Device     string  `json:"device"`
	ReadBytes  uint64  `json:"read_bytes"`
	WriteBytes uint64  `json:"write_bytes"`
	ReadBps    float64 `json:"read_bps"`
	WriteBps   float64 `json:"write_bps"`
}

// ProcStat is one row of the process table. CPUPercent is the process's average
// over its lifetime, which is what `ps aux` reports in the same column.
type ProcStat struct {
	PID        int32   `json:"pid"`
	User       string  `json:"user"`
	Command    string  `json:"command"`
	CPUPercent float64 `json:"cpu_percent"`
	MemPercent float64 `json:"mem_percent"`
	MemBytes   uint64  `json:"mem_bytes"`
}

// topProcessCount is how many rows the process table shows. The admin panel it
// replaces showed ten; more than that and the tab becomes a `top` clone without
// being as good at it.
const topProcessCount = 10

// slowEvery is how many samples pass between refreshes of the two expensive
// readings — the process table (a /proc/<pid> read per process on the box) and
// the CPU temperature (a walk of /sys/class/hwmon). At the page's 2s cadence that
// puts both on a 6s refresh, which is what `top` defaults to and is far more
// responsiveness than either number needs.
const slowEvery = 3

// Detailer produces Detailed samples, holding the previous reading so it can
// derive rates.
//
// It keeps its OWN cpu baseline rather than calling cpu.Percent, for the same
// reason Counters does: cpu.Percent has a single package-level baseline that every
// caller overwrites, and Collector.Sample is already calling it every two seconds
// underneath this page. See the note on Counters.CPUBusy.
type Detailer struct {
	dataRoot string

	mu   sync.Mutex
	prev *detailPrev
	tick int

	// Cached results of the slow readings, reused between refreshes.
	procs []ProcStat
	tempC float64
}

type detailPrev struct {
	at          time.Time
	busy, total float64
	nets        map[string]NetIfStat
	disks       map[string]DiskIOStat
}

// NewDetailer returns a Detailer reporting filesystems for dataRoot.
func NewDetailer(dataRoot string) *Detailer {
	if dataRoot == "" {
		dataRoot = "/"
	}
	return &Detailer{dataRoot: dataRoot}
}

// Sample takes a full reading. The first call after construction reports no rates
// — a rate needs two points — and every call after that measures against the
// previous one, so the cadence the caller uses is the window the rates cover.
func (d *Detailer) Sample() Detailed {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	out := Detailed{
		At:       now.UnixMilli(),
		HostProc: HasHostProc(),
	}

	if up, err := host.Uptime(); err == nil {
		out.Uptime = up
	}
	if n, err := cpu.Counts(true); err == nil {
		out.CPUCount = n
	}
	if avg, err := load.Avg(); err == nil {
		out.Load1, out.Load5, out.Load15 = avg.Load1, avg.Load5, avg.Load15
	}
	if vm, err := mem.VirtualMemory(); err == nil {
		out.Mem.TotalBytes = vm.Total
		out.Mem.UsedBytes = vm.Total - vm.Available
		out.Mem.AvailableBytes = vm.Available
		out.Mem.CachedBytes = vm.Cached
		out.Mem.UsedPercent = vm.UsedPercent
	}
	if sw, err := mem.SwapMemory(); err == nil {
		out.Mem.SwapTotalBytes = sw.Total
		out.Mem.SwapUsedBytes = sw.Used
		out.Mem.SwapPercent = sw.UsedPercent
	}
	out.Filesystems = ReadFilesystems(d.dataRoot)

	// Current cumulative readings, which become the next call's baseline.
	cur := &detailPrev{at: now, nets: map[string]NetIfStat{}, disks: map[string]DiskIOStat{}}
	if times, err := cpu.Times(false); err == nil && len(times) > 0 {
		cur.busy, cur.total = busyTotal(times[0])
	}
	nets := readNetDev()
	for _, n := range nets {
		cur.nets[n.Iface] = n
	}
	disks := readDiskIO()
	for _, s := range disks {
		cur.disks[s.Device] = s
	}

	if p := d.prev; p != nil {
		secs := now.Sub(p.at).Seconds()
		if secs <= 0 {
			secs = 1
		}
		if dt := cur.total - p.total; dt > 0 {
			out.CPUPercent = round1((cur.busy - p.busy) / dt * 100)
		}
		for _, n := range nets {
			r := NetIfRate{Iface: n.Iface, RxBytes: n.RxBytes, TxBytes: n.TxBytes}
			if was, ok := p.nets[n.Iface]; ok {
				r.RxBps = perSecond(n.RxBytes, was.RxBytes, secs)
				r.TxBps = perSecond(n.TxBytes, was.TxBytes, secs)
			}
			out.Nets = append(out.Nets, r)
		}
		for _, s := range disks {
			r := DiskIORate{Device: s.Device, ReadBytes: s.ReadBytes, WriteBytes: s.WriteBytes}
			if was, ok := p.disks[s.Device]; ok {
				r.ReadBps = perSecond(s.ReadBytes, was.ReadBytes, secs)
				r.WriteBps = perSecond(s.WriteBytes, was.WriteBytes, secs)
			}
			out.Disks = append(out.Disks, r)
		}
	} else {
		for _, n := range nets {
			out.Nets = append(out.Nets, NetIfRate{Iface: n.Iface, RxBytes: n.RxBytes, TxBytes: n.TxBytes})
		}
		for _, s := range disks {
			out.Disks = append(out.Disks, DiskIORate{Device: s.Device, ReadBytes: s.ReadBytes, WriteBytes: s.WriteBytes})
		}
	}
	d.prev = cur

	if d.tick%slowEvery == 0 {
		d.procs = topProcesses(out.Mem.TotalBytes)
		d.tempC = readCPUTemp()
	}
	d.tick++
	out.TopProcesses = d.procs
	out.CPUTempC = d.tempC

	if out.Nets == nil {
		out.Nets = []NetIfRate{}
	}
	if out.Disks == nil {
		out.Disks = []DiskIORate{}
	}
	if out.TopProcesses == nil {
		out.TopProcesses = []ProcStat{}
	}
	return out
}

// perSecond derives a rate from two cumulative readings, treating a counter that
// went backwards as a restart rather than reporting a negative throughput. That
// happens for real: an interface is recreated, or a device is re-enumerated.
func perSecond(now, was uint64, secs float64) float64 {
	if now < was {
		return 0
	}
	return float64(now-was) / secs
}

// topProcesses returns the busiest processes on the box.
//
// This is the most expensive thing on the page — the enumeration alone is a
// /proc/<pid> read per process — which is why it runs on the slow sub-cadence and
// never on the history path. The expensive per-process details (owner, command
// line) are fetched only for the handful of rows that survive the sort.
func topProcesses(memTotal uint64) []ProcStat {
	procs, err := process.Processes()
	if err != nil {
		return nil
	}
	type scored struct {
		p   *process.Process
		cpu float64
		rss uint64
	}
	all := make([]scored, 0, len(procs))
	for _, p := range procs {
		c, err := p.CPUPercent()
		if err != nil {
			continue // exited while we were reading it
		}
		var rss uint64
		if mi, err := p.MemoryInfo(); err == nil && mi != nil {
			rss = mi.RSS
		}
		all = append(all, scored{p: p, cpu: c, rss: rss})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].cpu != all[j].cpu {
			return all[i].cpu > all[j].cpu
		}
		return all[i].rss > all[j].rss
	})
	if len(all) > topProcessCount {
		all = all[:topProcessCount]
	}

	out := make([]ProcStat, 0, len(all))
	for _, s := range all {
		row := ProcStat{
			PID:        s.p.Pid,
			CPUPercent: round1(s.cpu),
			MemBytes:   s.rss,
		}
		if memTotal > 0 {
			row.MemPercent = round1(float64(s.rss) / float64(memTotal) * 100)
		}
		if name, err := s.p.Name(); err == nil {
			row.Command = name
		}
		row.User = owner(s.p)
		out = append(out, row)
	}
	return out
}

// owner names the user a process belongs to.
//
// Username() resolves the uid against the passwd database of whichever filesystem
// this process is looking at — which, in a container, is the IMAGE's /etc/passwd,
// not the host's. So root resolves (uid 0 is root everywhere) and every real
// account on the box does not, leaving the column blank for exactly the processes
// an operator cares about. Falling back to the numeric uid is less pretty and
// always true.
func owner(p *process.Process) string {
	if name, err := p.Username(); err == nil && name != "" {
		return name
	}
	if uids, err := p.Uids(); err == nil && len(uids) > 0 {
		return "uid " + strconv.FormatUint(uint64(uids[0]), 10)
	}
	return ""
}

// readCPUTemp picks a CPU temperature out of the sensor list, or 0 when the box
// exposes none (a VM usually does not).
func readCPUTemp() float64 {
	temps, err := sensors.SensorsTemperatures()
	if err != nil {
		return 0
	}
	return pickCPUTemp(temps)
}
