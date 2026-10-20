// Package cgroupstat reads per-container resource counters straight from the
// host's cgroup v2 tree.
//
// It exists because the Docker API's answer to the same question — a stats call —
// blocks for about a second per container while the daemon takes two samples, and
// so is banned from anything that runs while nobody is looking (see
// docs/resources.md, "The idle budget"). The kernel already keeps the counters a
// stats call derives its numbers from, as a handful of small text files per
// container; reading those is microseconds and involves no daemon at all.
//
// The package is deliberately stateless and engine-agnostic:
//
//   - It returns CUMULATIVE counters, never rates. Each consumer keeps its own
//     baseline. Two consumers on different cadences sharing one baseline would
//     silently corrupt each other's deltas — the same trap as gopsutil's
//     cpu.Percent, described in docs/resources.md.
//   - It finds a container's cgroup by looking for its ID in directory names, not
//     by reading /proc/<pid>/cgroup. Maison runs in a private cgroup namespace, so
//     the latter answers with paths relative to Maison's own cgroup (`/../…`),
//     which cannot be joined onto the host tree. A directory name carries the full
//     ID under every layout that matters — `docker-<id>.scope` (systemd driver),
//     `docker/<id>` (cgroupfs), `libpod-<id>.scope` (Podman, rootful or rootless).
//
// Everything is gated on CGROUP_ROOT naming a mounted cgroup v2 tree; without it
// the callers fall back to Docker stats (live views) or record nothing (history).
package cgroupstat

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Root is where the host's cgroup v2 tree is mounted inside this container, or ""
// when the deployment has not opted in.
func Root() string {
	return os.Getenv("CGROUP_ROOT")
}

// Available reports whether Root is set and is a cgroup v2 tree. cgroup v1 has no
// cgroup.controllers at its root, and a v1 box is not worth a second reader: every
// PCS image is v2.
func Available() bool {
	root := Root()
	if root == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(root, "cgroup.controllers"))
	return err == nil
}

// Counters are one container's cumulative readings.
type Counters struct {
	// CPUUsec is total CPU time consumed, in microseconds, across all cores.
	CPUUsec uint64
	// MemCurrent is memory.current: everything charged to the cgroup, page cache
	// included.
	MemCurrent uint64
	// InactiveFile is page cache the kernel will drop under pressure; MemUsage
	// deducts it, matching `docker stats`.
	InactiveFile uint64
	// MemMax is memory.max, or 0 when the cgroup is unlimited.
	MemMax uint64
	// IORead and IOWrite are bytes moved to and from real block devices.
	IORead  uint64
	IOWrite uint64
}

// MemUsage is the container's memory with reclaimable page cache deducted — the
// MEM USAGE column of `docker stats`, and what appstats.MemUsage computes from a
// stats frame.
func (c Counters) MemUsage() uint64 {
	if c.InactiveFile < c.MemCurrent {
		return c.MemCurrent - c.InactiveFile
	}
	return c.MemCurrent
}

// ErrGone is returned when a container's cgroup directory has disappeared — the
// container stopped or was recreated between the index and the read.
var ErrGone = errors.New("cgroupstat: cgroup gone")

// Reader reads Counters through one reused buffer, so a round over every container
// on the box allocates only the path strings the open syscalls need. Not safe for concurrent use: give each consumer its
// own.
type Reader struct {
	buf  []byte
	path []byte
}

// NewReader returns a Reader. memory.stat is the largest file read, about 1.5 KiB
// on current kernels; 8 KiB leaves room for growth.
func NewReader() *Reader {
	return &Reader{buf: make([]byte, 8<<10), path: make([]byte, 0, 256)}
}

// Read returns the counters of the cgroup at dir.
func (r *Reader) Read(dir string) (Counters, error) {
	var c Counters

	b, err := r.file(dir, "cpu.stat")
	if err != nil {
		return c, err
	}
	c.CPUUsec, _ = field(b, "usage_usec")

	if b, err = r.file(dir, "memory.current"); err != nil {
		return c, err
	}
	c.MemCurrent = parseUint(bytes.TrimSpace(b))

	if b, err = r.file(dir, "memory.stat"); err == nil {
		c.InactiveFile, _ = field(b, "inactive_file")
	}
	if b, err = r.file(dir, "memory.max"); err == nil {
		c.MemMax = parseUint(bytes.TrimSpace(b)) // "max" parses as 0: unlimited
	}
	if b, err = r.file(dir, "io.stat"); err == nil {
		c.IORead, c.IOWrite = ioStat(b)
	}
	return c, nil
}

// file reads dir/name into the shared buffer. The returned slice is only valid
// until the next call.
func (r *Reader) file(dir, name string) ([]byte, error) {
	r.path = append(r.path[:0], dir...)
	r.path = append(r.path, '/')
	r.path = append(r.path, name...)
	f, err := os.Open(string(r.path))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrGone
		}
		return nil, err
	}
	defer f.Close()
	n, err := io.ReadFull(f, r.buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	return r.buf[:n], nil
}

// field finds `key value` on its own line in a flat-keyed cgroup file.
func field(b []byte, key string) (uint64, bool) {
	for len(b) > 0 {
		line := b
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			line, b = b[:i], b[i+1:]
		} else {
			b = nil
		}
		if len(line) > len(key) && line[len(key)] == ' ' && string(line[:len(key)]) == key {
			return parseUint(line[len(key)+1:]), true
		}
	}
	return 0, false
}

// ioStat sums rbytes and wbytes over the devices listed in io.stat, one line per
// device: `8:0 rbytes=… wbytes=… rios=…`. Pseudo devices are skipped for the same
// reason the host disk tab skips them (system.isPseudoDevice): device-mapper and
// loop devices re-report the IO of the disk beneath them.
func ioStat(b []byte) (read, write uint64) {
	for len(b) > 0 {
		line := b
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			line, b = b[:i], b[i+1:]
		} else {
			b = nil
		}
		sp := bytes.IndexByte(line, ' ')
		if sp <= 0 || pseudoDevice(line[:sp]) {
			continue
		}
		rest := line[sp+1:]
		for len(rest) > 0 {
			kv := rest
			if i := bytes.IndexByte(rest, ' '); i >= 0 {
				kv, rest = rest[:i], rest[i+1:]
			} else {
				rest = nil
			}
			switch {
			case bytes.HasPrefix(kv, []byte("rbytes=")):
				read += parseUint(kv[len("rbytes="):])
			case bytes.HasPrefix(kv, []byte("wbytes=")):
				write += parseUint(kv[len("wbytes="):])
			}
		}
	}
	return read, write
}

// sysDevBlock is where `MAJ:MIN` resolves to a device name. sysfs is not
// namespaced for block devices, so the container's own /sys answers for the host.
var sysDevBlock = "/sys/dev/block"

var (
	pseudoMu    sync.Mutex
	pseudoCache = map[string]bool{}
)

// pseudoDevice reports whether the device `MAJ:MIN` is a loop, ram, zram or
// device-mapper node. The answer is cached: a box has a handful of devices, and
// resolving one is a readlink. A device that cannot be resolved is counted — an
// over-count is a better failure than a disk whose IO silently reads as zero.
func pseudoDevice(majmin []byte) bool {
	pseudoMu.Lock()
	defer pseudoMu.Unlock()
	if v, ok := pseudoCache[string(majmin)]; ok {
		return v
	}
	key := string(majmin)
	v := false
	if link, err := os.Readlink(filepath.Join(sysDevBlock, key)); err == nil {
		name := filepath.Base(link)
		v = strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") ||
			strings.HasPrefix(name, "dm-") || strings.HasPrefix(name, "zram")
	}
	pseudoCache[key] = v
	return v
}

func parseUint(b []byte) uint64 {
	var n uint64
	for _, c := range b {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + uint64(c-'0')
	}
	return n
}

// Index walks the cgroup tree under root and maps every container ID found in a
// directory name to that directory.
//
// The walk is a few hundred directories and runs only when the set of containers
// changes (see Tracker). It does not descend into a container's own cgroup — a
// container running systemd or its own nested containers has child cgroups whose
// counters are already included in its parent's — and it skips Podman's conmon
// scopes, which carry the container's ID but hold only the monitor process.
func Index(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil // a cgroup removed mid-walk is not an error
		}
		if !d.IsDir() || path == root {
			return nil
		}
		name := d.Name()
		id := containerID(name)
		if id == "" {
			return nil
		}
		if strings.Contains(name, "conmon") {
			return fs.SkipDir
		}
		if _, dup := out[id]; !dup {
			out[id] = path
		}
		return fs.SkipDir
	})
	return out, err
}

// containerID returns the first run of exactly 64 hex digits in name, or "".
func containerID(name string) string {
	run := 0
	for i := 0; i < len(name); i++ {
		c := name[i]
		if ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') {
			run++
			continue
		}
		if run == 64 {
			return name[i-64 : i]
		}
		run = 0
	}
	if run == 64 {
		return name[len(name)-64:]
	}
	return ""
}
