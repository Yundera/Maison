package system

import (
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
)

// Counters is the cheap, once-a-minute reading the history sampler takes.
//
// It is deliberately much less than Detailed: no temperature (walking
// /sys/class/hwmon costs more than everything else here put together), no process
// table (a /proc/<pid>/stat read per PID on the box), no per-interface or
// per-device breakdown (the history record is fixed-width and stores sums). Those
// belong to the page, which is only sampled while someone has it open. This is the
// only thing that runs when nobody is looking, so it stays small.
type Counters struct {
	At time.Time

	// CPUBusy and CPUTotal are cumulative jiffies, NOT a percentage.
	//
	// This is the important part. gopsutil's cpu.Percent keeps ONE package-level
	// baseline (lastCPUPercent), which every call overwrites — so two callers on
	// different cadences silently corrupt each other's deltas. Collector.Sample
	// already calls it every 2s while the dashboard is open. If this sampler called
	// it too, both would be wrong, and only while somebody was watching, which is
	// exactly when nobody would notice it was the measurement and not the machine.
	// So the history path carries its own baseline and differentiates these itself.
	CPUBusy  float64
	CPUTotal float64

	// Cumulative byte counters, summed over real interfaces / real block devices.
	NetRx, NetTx        uint64
	DiskRead, DiskWrite uint64

	// Instantaneous readings, which need no delta.
	MemPercent  float64
	SwapPercent float64
	DiskPercent float64
	Load1       float64
}

// ReadCounters takes one cheap sample. Individual metrics degrade to zero rather
// than failing the whole reading — a box with no swap, or a container that cannot
// see the host's interfaces, still gets CPU and memory history.
func ReadCounters(dataRoot string) Counters {
	c := Counters{At: time.Now()}

	if times, err := cpu.Times(false); err == nil && len(times) > 0 {
		c.CPUBusy, c.CPUTotal = busyTotal(times[0])
	}
	if vm, err := mem.VirtualMemory(); err == nil {
		c.MemPercent = vm.UsedPercent
	}
	if sw, err := mem.SwapMemory(); err == nil {
		c.SwapPercent = sw.UsedPercent
	}
	if avg, err := load.Avg(); err == nil {
		c.Load1 = avg.Load1
	}
	if u, err := statfsPath(dataRoot); err == nil && u.used+u.avail > 0 {
		c.DiskPercent = float64(u.used) / float64(u.used+u.avail) * 100
	}
	for _, n := range readNetDev() {
		c.NetRx += n.RxBytes
		c.NetTx += n.TxBytes
	}
	for _, d := range readDiskIO() {
		c.DiskRead += d.ReadBytes
		c.DiskWrite += d.WriteBytes
	}
	return c
}

// busyTotal splits a CPU times reading into busy and total jiffies. Idle and
// iowait both count as not-busy: a core waiting on the disk is not doing work, and
// counting iowait as busy is what makes a box with a slow disk look CPU-bound.
func busyTotal(t cpu.TimesStat) (busy, total float64) {
	total = t.User + t.Nice + t.System + t.Idle + t.Iowait +
		t.Irq + t.Softirq + t.Steal
	return total - (t.Idle + t.Iowait), total
}

// DiskIOStat is one block device's cumulative IO.
type DiskIOStat struct {
	Device     string `json:"device"`
	ReadBytes  uint64 `json:"read_bytes"`
	WriteBytes uint64 `json:"write_bytes"`
}

// readDiskIO returns per-disk counters with partitions and pseudo devices removed.
// /proc/diskstats is not namespaced, so these are the host's figures with or
// without the HOST_PROC mount.
func readDiskIO() []DiskIOStat {
	raw, err := disk.IOCounters()
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(raw))
	for name := range raw {
		if !isPseudoDevice(name) {
			names = append(names, name)
		}
	}
	out := make([]DiskIOStat, 0, len(names))
	for _, name := range names {
		if isPartition(name, names) {
			continue
		}
		s := raw[name]
		out = append(out, DiskIOStat{Device: name, ReadBytes: s.ReadBytes, WriteBytes: s.WriteBytes})
	}
	return out
}
