package system

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// FilesystemStat is one filesystem's occupation, named as the HOST names it.
type FilesystemStat struct {
	// Device and Mountpoint come from the host's mount table when it is readable,
	// so a filesystem the container reaches at /DATA is reported as what it
	// actually is — /dev/sda1 mounted at / — rather than by the path this process
	// happens to see it through.
	Device     string `json:"device"`
	Mountpoint string `json:"mountpoint"`
	Fstype     string `json:"fstype"`

	// LocalPath is where the numbers were measured. It differs from Mountpoint
	// whenever the host mount is reached through a bind, which on a PCS is always.
	LocalPath string `json:"local_path"`

	SizeBytes   uint64  `json:"size_bytes"`
	UsedBytes   uint64  `json:"used_bytes"`
	AvailBytes  uint64  `json:"avail_bytes"`
	UsedPercent float64 `json:"used_percent"`
}

// Filesystems is the disk-occupation table plus an honest count of what could not
// be measured from in here.
type Filesystems struct {
	Mounts []FilesystemStat `json:"mounts"`

	// Unmeasured is how many real host filesystems exist that this process cannot
	// statfs, because they are not mounted anywhere inside the container. On a
	// stock PCS this is /boot and /boot/efi. Reporting the number is the difference
	// between a table that is complete and one that merely looks complete.
	Unmeasured int `json:"unmeasured"`
}

// mountEntry is one line of a mountinfo file.
type mountEntry struct {
	dev        string // "major:minor" — the join key
	root       string // which subtree of the filesystem is mounted
	mountpoint string
	fstype     string
	source     string
}

// pseudoFstypes are the filesystem types that are not storage. They are dropped
// from the table AND from the unmeasured count: an operator asking "how full is my
// disk" is not asking about cgroup or mqueue, and on a container host those
// outnumber the real answers many times over.
//
// A deny list rather than an allow list because the set of real filesystems is
// open-ended (ext4, xfs, btrfs, zfs, vfat, nfs, cifs, …) while the kernel's
// virtual ones are a known, slow-moving set. Anything unlisted is shown, so a new
// storage filesystem appears on its own; a new virtual one appears once, as an
// over-count in "not measurable", which is a far better failure than hiding a
// disk that is filling up.
var pseudoFstypes = map[string]bool{
	"autofs": true, "binfmt_misc": true, "bpf": true, "cgroup": true,
	"cgroup2": true, "configfs": true, "debugfs": true, "devpts": true,
	"devtmpfs": true, "efivarfs": true, "fuse.lxcfs": true, "fusectl": true,
	"hugetlbfs": true, "mqueue": true, "nsfs": true, "overlay": true,
	"proc": true, "pstore": true, "ramfs": true, "rpc_pipefs": true,
	"securityfs": true, "selinuxfs": true, "squashfs": true, "sysfs": true,
	"tracefs": true, "tmpfs": true,
}

// usage is a statfs result plus the device the path actually lives on.
type usage struct {
	dev                     string
	size, used, avail       uint64
}

// ReadFilesystems builds the disk-occupation table for the data root.
//
// The obvious implementation — list the host's mounts and statfs each one — does
// not work from inside a container, and fails in the worst possible way. On a PCS
// the host's mount table says the data lives on `/`; that path exists in here too,
// but it is the container's overlay, so the call SUCCEEDS and returns numbers for
// the wrong filesystem. (It is worse than it sounds: the overlay's upper directory
// is on the same disk, so the numbers even look right.)
//
// So the direction is inverted. Enumerate what this process can actually measure
// — the data root and anything mounted under it — statfs those, and then use the
// device id to look up what the HOST calls each one. The device id is exact where
// a path comparison is not: the overlay has its own anonymous device, matches no
// host entry, and drops out on its own.
func ReadFilesystems(dataRoot string) Filesystems {
	local := readMountinfo(filepath.Join("/proc", "self", "mountinfo"))
	host := readMountinfo(filepath.Join(hostProc(), "1", "mountinfo"))
	return joinFilesystems(local, host, dataRoot, statfsPath)
}

// statfsPath measures a path and identifies the device beneath it.
func statfsPath(path string) (usage, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return usage{}, err
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return usage{}, err
	}
	bsize := uint64(fs.Bsize)
	total := fs.Blocks * bsize
	avail := fs.Bavail * bsize
	return usage{
		dev:   devID(uint64(st.Dev)),
		size:  total,
		used:  (fs.Blocks - fs.Bfree) * bsize,
		avail: avail,
	}, nil
}

func devID(dev uint64) string {
	return itoa(unix.Major(dev)) + ":" + itoa(unix.Minor(dev))
}

func itoa(v uint32) string {
	if v == 0 {
		return "0"
	}
	var b [10]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// joinFilesystems is the pure half of ReadFilesystems, with the host reads passed
// in so it can be tested against captured mountinfo.
func joinFilesystems(local, host []mountEntry, dataRoot string, stat func(string) (usage, error)) Filesystems {
	out := Filesystems{Mounts: []FilesystemStat{}}

	// What we can reach: the data root itself (which need not be a mountpoint —
	// on a PCS it is a plain directory on the root filesystem) plus anything
	// mounted beneath it.
	candidates := []string{dataRoot}
	for _, e := range local {
		if pseudoFstypes[e.fstype] || e.mountpoint == dataRoot {
			continue
		}
		if under(e.mountpoint, dataRoot) {
			candidates = append(candidates, e.mountpoint)
		}
	}
	sort.Strings(candidates)

	// Index the host's table by device, preferring the entry that mounts the whole
	// filesystem (root "/") at the shallowest path — that is the name a person
	// would use for it, rather than one of the binds Docker makes of the same disk.
	byDev := map[string]mountEntry{}
	for _, e := range host {
		if pseudoFstypes[e.fstype] {
			continue
		}
		cur, seen := byDev[e.dev]
		if !seen || better(e, cur) {
			byDev[e.dev] = e
		}
	}

	measured := map[string]bool{}
	for _, path := range candidates {
		u, err := stat(path)
		if err != nil || u.size == 0 || measured[u.dev] {
			continue
		}
		measured[u.dev] = true

		row := FilesystemStat{
			Device:     u.dev,
			Mountpoint: path,
			LocalPath:  path,
			SizeBytes:  u.size,
			UsedBytes:  u.used,
			AvailBytes: u.avail,
		}
		if h, ok := byDev[u.dev]; ok {
			row.Device, row.Mountpoint, row.Fstype = h.source, h.mountpoint, h.fstype
		} else if l, ok := findByMountpoint(local, path); ok {
			// No host table (the /proc mount is absent): fall back to what this
			// container's own mountinfo says, which still names the real device.
			row.Device, row.Fstype = l.source, l.fstype
			if pseudoFstypes[l.fstype] {
				continue
			}
		}
		if u.size > 0 {
			row.UsedPercent = float64(u.used) / float64(u.used+u.avail) * 100
		}
		out.Mounts = append(out.Mounts, row)
	}

	for dev := range byDev {
		if !measured[dev] {
			out.Unmeasured++
		}
	}

	sort.Slice(out.Mounts, func(i, j int) bool { return out.Mounts[i].Mountpoint < out.Mounts[j].Mountpoint })
	return out
}

// better reports whether a should replace b as the name for their shared device.
func better(a, b mountEntry) bool {
	if (a.root == "/") != (b.root == "/") {
		return a.root == "/"
	}
	return len(a.mountpoint) < len(b.mountpoint)
}

func findByMountpoint(entries []mountEntry, path string) (mountEntry, bool) {
	for _, e := range entries {
		if e.mountpoint == path {
			return e, true
		}
	}
	return mountEntry{}, false
}

// under reports whether path is at or below root.
func under(path, root string) bool {
	if path == root {
		return true
	}
	if !strings.HasSuffix(root, "/") {
		root += "/"
	}
	return strings.HasPrefix(path, root)
}

func readMountinfo(path string) []mountEntry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseMountinfo(f)
}

// parseMountinfo reads proc_pid_mountinfo(5):
//
//	36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
//	(1)(2)(3)  (4)   (5)   (6)        (7)     (8) (9)  (10)     (11)
//
// Fields (6) and (7) are optional, so the line is split on the " - " separator
// rather than by counting from the left.
func parseMountinfo(r io.Reader) []mountEntry {
	var out []mountEntry
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		head, tail, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		hf := strings.Fields(head)
		tf := strings.Fields(tail)
		if len(hf) < 5 || len(tf) < 2 {
			continue
		}
		out = append(out, mountEntry{
			dev:        hf[2],
			root:       hf[3],
			mountpoint: unescapeMount(hf[4]),
			fstype:     tf[0],
			source:     unescapeMount(tf[1]),
		})
	}
	return out
}

// unescapeMount undoes the octal escaping mountinfo applies to space, tab,
// newline and backslash in paths.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			v := 0
			ok := true
			for _, c := range []byte(s[i+1 : i+4]) {
				if c < '0' || c > '7' {
					ok = false
					break
				}
				v = v*8 + int(c-'0')
			}
			if ok {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
