package system

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// NetIfStat is one interface's cumulative byte counters.
type NetIfStat struct {
	Iface   string `json:"iface"`
	RxBytes uint64 `json:"rx_bytes"`
	TxBytes uint64 `json:"tx_bytes"`
}

// readNetDev returns the HOST's per-interface counters.
//
// This is the one metric gopsutil cannot be pointed at with HOST_PROC alone.
// /proc/net is a symlink to `self/net`, so it resolves in the reading process's
// network namespace however it is reached — through a bind mount of the host's
// /proc included. From inside a container on a bridge network that means the
// veth, not the uplink, and the numbers look real while being about nothing.
//
// PID 1's directory is the way out: `<host proc>/1/net/dev` is unambiguously the
// host init's network namespace. Without the mount there is no such file, and the
// caller gets nothing rather than the container's own figures dressed up as the
// host's.
func readNetDev() []NetIfStat {
	f, err := os.Open(filepath.Join(hostProc(), "1", "net", "dev"))
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseNetDev(f)
}

// parseNetDev reads the two-line-header table in /proc/net/dev:
//
//	face |bytes packets errs drop fifo frame compressed multicast|bytes packets ...
//	 eth0: 1234    10     0    0    0     0          0         0   567      8 ...
//
// Receive bytes is the first counter, transmit bytes the ninth.
func parseNetDev(r io.Reader) []NetIfStat {
	var out []NetIfStat
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue // the two header lines
		}
		iface := strings.TrimSpace(name)
		if iface == "" || isVirtualInterface(iface) {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			continue
		}
		rx, err1 := strconv.ParseUint(fields[0], 10, 64)
		tx, err2 := strconv.ParseUint(fields[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, NetIfStat{Iface: iface, RxBytes: rx, TxBytes: tx})
	}
	return out
}
