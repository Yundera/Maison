package system

import (
	"errors"
	"strings"
	"testing"
)

// The fixtures below are verbatim captures from holyhorse (a live PCS), because
// the behaviour under test is entirely about what a real container sees versus
// what the real host sees. Trimmed only of the mounts that are noise here.
const (
	// Inside maison-app: the root is the container's overlay, and the host's disk
	// is reachable only through the /DATA bind.
	containerMountinfo = `
1191 269 0:53 / / rw,relatime shared:245 - overlay overlay rw,lowerdir=/var/lib/containerd/snapshots/4349/fs,upperdir=/var/lib/containerd/snapshots/4350/fs
1280 1191 8:1 /DATA /DATA rw,relatime shared:1 - ext4 /dev/sda1 rw,discard,errors=remount-ro,commit=30
1284 1191 8:1 /var/lib/docker/containers/420376ba/resolv.conf /etc/resolv.conf rw,relatime - ext4 /dev/sda1 rw,discard
1292 1191 8:1 /var/lib/docker/containers/420376ba/hostname /etc/hostname rw,relatime - ext4 /dev/sda1 rw,discard
1293 1191 8:1 /var/lib/docker/containers/420376ba/hosts /etc/hosts rw,relatime - ext4 /dev/sda1 rw,discard
`
	// On the host: one real disk plus the two boot partitions.
	hostMountinfo = `
29 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw,discard,errors=remount-ro,commit=30
44 29 259:0 / /boot rw,relatime shared:29 - ext4 /dev/sda16 rw
46 44 8:15 / /boot/efi rw,relatime shared:44 - vfat /dev/sda15 rw,fmask=0077,dmask=0077
`
)

func mounts(s string) []mountEntry { return parseMountinfo(strings.NewReader(s)) }

// statAt builds a stat function that only succeeds for the given paths, standing
// in for a container that can reach some of the host's filesystems and not others.
func statAt(m map[string]usage) func(string) (usage, error) {
	return func(p string) (usage, error) {
		u, ok := m[p]
		if !ok {
			return usage{}, errors.New("no such file or directory")
		}
		return u, nil
	}
}

// The headline case: on a PCS the data root is a bind of a subdirectory of the
// host's root filesystem, and the table must say so — /dev/sda1 mounted at /, not
// "/DATA", and certainly not the overlay.
func TestDataRootIsReportedAsTheHostFilesystemItActuallyIs(t *testing.T) {
	got := joinFilesystems(
		mounts(containerMountinfo), mounts(hostMountinfo), "/DATA",
		statAt(map[string]usage{
			"/DATA": {dev: "8:1", size: 415_000_000_000, used: 130_000_000_000, avail: 285_000_000_000},
		}),
	)

	if len(got.Mounts) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(got.Mounts), got.Mounts)
	}
	m := got.Mounts[0]
	if m.Device != "/dev/sda1" {
		t.Errorf("Device = %q, want /dev/sda1", m.Device)
	}
	if m.Mountpoint != "/" {
		t.Errorf("Mountpoint = %q, want / (the host's name for it)", m.Mountpoint)
	}
	if m.Fstype != "ext4" {
		t.Errorf("Fstype = %q, want ext4", m.Fstype)
	}
	if m.LocalPath != "/DATA" {
		t.Errorf("LocalPath = %q, want /DATA", m.LocalPath)
	}
	if m.UsedPercent < 31 || m.UsedPercent > 32 {
		t.Errorf("UsedPercent = %v, want ~31.3", m.UsedPercent)
	}
	// /boot and /boot/efi exist on the host and cannot be measured from in here.
	// Saying so is the difference between a complete table and one that only looks
	// complete.
	if got.Unmeasured != 2 {
		t.Errorf("Unmeasured = %d, want 2 (/boot, /boot/efi)", got.Unmeasured)
	}
}

// The trap this whole design exists to avoid: statfs("/") inside the container
// succeeds and returns the overlay, whose upper directory is on the same disk — so
// the numbers even look plausible. It must never reach the table.
func TestContainerOverlayIsNeverReportedAsAFilesystem(t *testing.T) {
	got := joinFilesystems(
		mounts(containerMountinfo), mounts(hostMountinfo), "/",
		statAt(map[string]usage{
			// Same plausible numbers, different (anonymous) device.
			"/":     {dev: "0:53", size: 415_000_000_000, used: 130_000_000_000, avail: 285_000_000_000},
			"/DATA": {dev: "8:1", size: 415_000_000_000, used: 130_000_000_000, avail: 285_000_000_000},
		}),
	)
	for _, m := range got.Mounts {
		if m.Device == "0:53" || m.Fstype == "overlay" {
			t.Fatalf("the overlay was reported as a filesystem: %+v", m)
		}
	}
	if len(got.Mounts) != 1 || got.Mounts[0].Device != "/dev/sda1" {
		t.Fatalf("got %+v, want just the real disk", got.Mounts)
	}
}

// A second disk mounted under the data root is a real filesystem and must appear.
func TestExtraDiskMountedUnderTheDataRootGetsItsOwnRow(t *testing.T) {
	local := containerMountinfo + "1300 1280 8:32 / /DATA/Media rw,relatime - ext4 /dev/sdc1 rw\n"
	host := hostMountinfo + "50 29 8:32 / /mnt/media rw,relatime - ext4 /dev/sdc1 rw\n"

	got := joinFilesystems(mounts(local), mounts(host), "/DATA",
		statAt(map[string]usage{
			"/DATA":       {dev: "8:1", size: 400, used: 100, avail: 300},
			"/DATA/Media": {dev: "8:32", size: 800, used: 200, avail: 600},
		}),
	)
	if len(got.Mounts) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(got.Mounts), got.Mounts)
	}
	var devices []string
	for _, m := range got.Mounts {
		devices = append(devices, m.Device)
	}
	if devices[0] != "/dev/sda1" || devices[1] != "/dev/sdc1" {
		t.Errorf("devices = %v, want [/dev/sda1 /dev/sdc1]", devices)
	}
}

// Two paths onto the same disk are one filesystem, not two. Without the device-id
// dedupe, every bind Docker makes would add a duplicate row with identical numbers.
func TestTwoPathsOntoOneDiskCollapseToOneRow(t *testing.T) {
	local := containerMountinfo + "1301 1280 8:1 /DATA/AppData /DATA/AppData rw,relatime - ext4 /dev/sda1 rw\n"
	got := joinFilesystems(mounts(local), mounts(hostMountinfo), "/DATA",
		statAt(map[string]usage{
			"/DATA":         {dev: "8:1", size: 400, used: 100, avail: 300},
			"/DATA/AppData": {dev: "8:1", size: 400, used: 100, avail: 300},
		}),
	)
	if len(got.Mounts) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(got.Mounts), got.Mounts)
	}
}

// Without the /proc mount there is no host table. The table must still work — it
// just names things the way this container sees them.
func TestWithoutTheHostTableItFallsBackToTheContainersOwnNames(t *testing.T) {
	got := joinFilesystems(mounts(containerMountinfo), nil, "/DATA",
		statAt(map[string]usage{
			"/DATA": {dev: "8:1", size: 400, used: 100, avail: 300},
		}),
	)
	if len(got.Mounts) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(got.Mounts), got.Mounts)
	}
	m := got.Mounts[0]
	if m.Device != "/dev/sda1" || m.Fstype != "ext4" {
		t.Errorf("got %s (%s), want /dev/sda1 (ext4) from the container's own mountinfo", m.Device, m.Fstype)
	}
	if m.Mountpoint != "/DATA" {
		t.Errorf("Mountpoint = %q, want /DATA — the host's name is unknown here", m.Mountpoint)
	}
	if got.Unmeasured != 0 {
		t.Errorf("Unmeasured = %d, want 0 — nothing is known to be missing", got.Unmeasured)
	}
}

// A path that cannot be measured must be skipped, not reported as an empty disk.
func TestUnreachablePathsAreSkipped(t *testing.T) {
	got := joinFilesystems(mounts(containerMountinfo), mounts(hostMountinfo), "/DATA",
		statAt(map[string]usage{}))
	if len(got.Mounts) != 0 {
		t.Fatalf("got %+v, want no rows", got.Mounts)
	}
	if got.Unmeasured != 3 {
		t.Errorf("Unmeasured = %d, want 3", got.Unmeasured)
	}
}

func TestParseMountinfoReadsTheOptionalFields(t *testing.T) {
	// Field (7) ("shared:1") is optional; a line without it must still parse.
	in := "29 1 8:1 / / rw,relatime - ext4 /dev/sda1 rw\n"
	got := parseMountinfo(strings.NewReader(in))
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	want := mountEntry{dev: "8:1", root: "/", mountpoint: "/", fstype: "ext4", source: "/dev/sda1"}
	if got[0] != want {
		t.Errorf("got %+v, want %+v", got[0], want)
	}
}

func TestParseMountinfoUnescapesPaths(t *testing.T) {
	in := `29 1 8:1 / /mnt/my\040disk rw - ext4 /dev/sda1 rw` + "\n"
	got := parseMountinfo(strings.NewReader(in))
	if len(got) != 1 || got[0].mountpoint != "/mnt/my disk" {
		t.Fatalf("mountpoint = %q, want %q", got[0].mountpoint, "/mnt/my disk")
	}
}

func TestUnderMatchesOnPathSegments(t *testing.T) {
	for _, c := range []struct {
		path, root string
		want       bool
	}{
		{"/DATA", "/DATA", true},
		{"/DATA/Media", "/DATA", true},
		// The bug a naive HasPrefix would have: a sibling directory whose name
		// starts with the root's.
		{"/DATABASE", "/DATA", false},
		{"/etc", "/DATA", false},
	} {
		if got := under(c.path, c.root); got != c.want {
			t.Errorf("under(%q, %q) = %v, want %v", c.path, c.root, got, c.want)
		}
	}
}

func TestIsPartitionCollapsesPartitionsIntoTheirDisk(t *testing.T) {
	all := []string{"sda", "sda1", "sda15", "nvme0n1", "nvme0n1p1", "mmcblk0", "mmcblk0p1", "sdb"}
	for _, c := range []struct {
		device string
		want   bool
	}{
		{"sda", false},
		{"sda1", true},
		{"sda15", true},
		{"nvme0n1", false},
		{"nvme0n1p1", true},
		{"mmcblk0p1", true},
		{"sdb", false},
	} {
		if got := isPartition(c.device, all); got != c.want {
			t.Errorf("isPartition(%q) = %v, want %v", c.device, got, c.want)
		}
	}
}

func TestIsVirtualInterfaceHidesDockerPlumbing(t *testing.T) {
	for _, c := range []struct {
		iface string
		want  bool
	}{
		{"eth0", false}, {"ens18", false}, {"wg0", false},
		{"lo", true}, {"docker0", true}, {"br-1a2b3c", true}, {"veth9f8e7d", true},
	} {
		if got := isVirtualInterface(c.iface); got != c.want {
			t.Errorf("isVirtualInterface(%q) = %v, want %v", c.iface, got, c.want)
		}
	}
}
