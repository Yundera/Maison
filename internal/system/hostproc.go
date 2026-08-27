package system

import (
	"os"
	"path/filepath"
	"strings"
)

// hostProc is where the HOST's /proc is readable from inside this container.
//
// Most of /proc is not namespaced — /proc/stat, /proc/meminfo and /proc/diskstats
// already report the host from inside a container — so the mount this points at
// changes nothing for CPU, memory or disk IO. What it buys is the three things
// that ARE per-namespace or per-process: the host's network interface counters,
// the host's process table, and the host's mount table.
//
// Everything that reads through here degrades when the mount is absent, because a
// deployment that has not taken the compose change yet must still get a working
// page rather than a broken tab. See HasHostProc.
func hostProc() string {
	if p := os.Getenv("HOST_PROC"); p != "" {
		return p
	}
	return "/proc"
}

// HasHostProc reports whether the host's own /proc is reachable — i.e. whether
// HOST_PROC names a directory that is not simply this process's own /proc.
//
// The probe is PID 1's network directory specifically, because that is the file
// the network tab needs and the one most likely to be missing: /proc/net is a
// symlink to `self/net`, so it resolves in the READER's network namespace no
// matter which /proc it is reached through. Only an explicit PID gets the host's.
func HasHostProc() bool {
	if os.Getenv("HOST_PROC") == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(hostProc(), "1", "net", "dev"))
	return err == nil
}

// isVirtualInterface hides the interfaces Docker creates. A PCS runs dozens of
// containers, each with a veth pair, so without this the network tab is a wall of
// `veth1a2b3c` rows that tell an operator nothing.
func isVirtualInterface(iface string) bool {
	switch {
	case iface == "lo", iface == "docker0":
		return true
	case strings.HasPrefix(iface, "br-"),
		strings.HasPrefix(iface, "veth"),
		strings.HasPrefix(iface, "cni"),
		strings.HasPrefix(iface, "flannel"):
		return true
	}
	return false
}

// isPseudoDevice drops the block devices that are not disks: loop devices (one per
// snap), ramdisks, and device-mapper nodes, which double-count the disk beneath
// them.
func isPseudoDevice(name string) bool {
	return strings.HasPrefix(name, "loop") ||
		strings.HasPrefix(name, "ram") ||
		strings.HasPrefix(name, "dm-") ||
		strings.HasPrefix(name, "zram")
}

// isPartition reports whether device is a partition of another device in the same
// list — `sda1` under `sda`, `nvme0n1p1` under `nvme0n1`, `mmcblk0p1` under
// `mmcblk0`. Partitions and their parent report the same IO, so counting both
// doubles every number on the disk tab.
func isPartition(device string, all []string) bool {
	has := func(s string) bool {
		for _, d := range all {
			if d == s {
				return true
			}
		}
		return false
	}
	if s := strings.TrimRight(device, "0123456789"); s != device && has(s) {
		return true
	}
	// nvme0n1p1 -> nvme0n1, mmcblk0p1 -> mmcblk0
	if i := strings.LastIndex(device, "p"); i > 0 {
		if s := device[:i]; strings.Trim(device[i+1:], "0123456789") == "" && has(s) {
			return true
		}
	}
	return false
}
