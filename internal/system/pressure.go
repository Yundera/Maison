package system

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
)

// Pressure is the kernel's pressure-stall information (PSI), as cumulative totals.
//
// PSI answers the question a CPU or memory percentage cannot: was work actually
// held up? A box at 90% CPU running a niced transcode is doing its job; a box where
// tasks spend a quarter of their time queued for a core is not. "some" is the share
// of time at least one task was stalled, "full" the share in which every non-idle
// task was.
//
// The totals are microseconds since boot, NOT the avg10/avg60/avg300 figures beside
// them. A kernel-side average would be a sample of whatever the last five minutes
// happened to look like; the total differentiated over the sampler's own interval
// is exact, the same reason Counters carries CPU jiffies rather than a percentage.
//
// /proc/pressure is system-wide, not namespaced, so this reads the host's figures
// from inside a container. It is absent on kernels built without PSI or booted with
// psi=0 (Debian's default); OK is false there and every caller falls back.
type Pressure struct {
	OK bool

	CPUSome uint64
	MemSome uint64
	MemFull uint64
	IOFull  uint64
}

// ReadPressure reads /proc/pressure/{cpu,memory,io}. Memory is the one that must be
// there for OK; a kernel with PSI has all three.
func ReadPressure() Pressure {
	var p Pressure
	dir := filepath.Join(hostProc(), "pressure")
	cpu, cerr := os.ReadFile(filepath.Join(dir, "cpu"))
	mem, merr := os.ReadFile(filepath.Join(dir, "memory"))
	if cerr != nil || merr != nil {
		return p
	}
	var ok bool
	if p.CPUSome, ok = psiTotal(cpu, "some"); !ok {
		return p
	}
	if p.MemSome, ok = psiTotal(mem, "some"); !ok {
		return p
	}
	p.MemFull, _ = psiTotal(mem, "full")
	if io, err := os.ReadFile(filepath.Join(dir, "io")); err == nil {
		p.IOFull, _ = psiTotal(io, "full")
	}
	p.OK = true
	return p
}

// psiTotal returns the total= field of the line starting with kind ("some" or
// "full"):
//
//	some avg10=9.46 avg60=12.49 avg300=15.51 total=14739896105
func psiTotal(b []byte, kind string) (uint64, bool) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		fields := bytes.Fields(sc.Bytes())
		if len(fields) == 0 || string(fields[0]) != kind {
			continue
		}
		for _, f := range fields[1:] {
			if v, found := bytes.CutPrefix(f, []byte("total=")); found {
				n, err := strconv.ParseUint(string(v), 10, 64)
				return n, err == nil
			}
		}
	}
	return 0, false
}

// ReadOOMKills returns the host's cumulative OOM-kill count from /proc/vmstat
// (oom_kill, Linux 4.13+), and whether it could be read. Like PSI it is not
// namespaced: inside a container it is the host's count.
func ReadOOMKills() (uint64, bool) {
	b, err := os.ReadFile(filepath.Join(hostProc(), "vmstat"))
	if err != nil {
		return 0, false
	}
	return vmstatField(b, "oom_kill")
}

func vmstatField(b []byte, key string) (uint64, bool) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, found := bytes.Cut(sc.Bytes(), []byte(" "))
		if !found || string(k) != key {
			continue
		}
		n, err := strconv.ParseUint(string(bytes.TrimSpace(v)), 10, 64)
		return n, err == nil
	}
	return 0, false
}
