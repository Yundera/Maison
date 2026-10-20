package cgroupstat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yundera/maison/internal/dockerx"
)

func hexID(c byte) string { return strings.Repeat(string(c), 64) }

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// container lays out the files Read needs in dir, as cgroup v2 presents them.
func container(t *testing.T, dir string) {
	t.Helper()
	write(t, dir, "cpu.stat", "usage_usec 123456\nuser_usec 100000\nsystem_usec 23456\nnr_periods 0\n")
	write(t, dir, "memory.current", "104857600\n")
	write(t, dir, "memory.stat", "anon 50000000\nfile 54857600\nactive_file 20000000\ninactive_file 30000000\n")
	write(t, dir, "memory.max", "max\n")
	write(t, dir, "io.stat", "8:0 rbytes=1000 wbytes=2000 rios=1 wios=2 dbytes=0 dios=0\n8:16 rbytes=10 wbytes=20 rios=1 wios=1 dbytes=0 dios=0\n")
}

func TestReadParsesCgroupFiles(t *testing.T) {
	sysDevBlock = t.TempDir() // nothing resolves: every device counts
	pseudoCache = map[string]bool{}
	dir := t.TempDir()
	container(t, dir)

	c, err := NewReader().Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.CPUUsec != 123456 {
		t.Errorf("CPUUsec = %d", c.CPUUsec)
	}
	if c.MemCurrent != 104857600 || c.InactiveFile != 30000000 {
		t.Errorf("mem = %d / inactive %d", c.MemCurrent, c.InactiveFile)
	}
	if c.MemUsage() != 104857600-30000000 {
		t.Errorf("MemUsage = %d", c.MemUsage())
	}
	if c.MemMax != 0 {
		t.Errorf("MemMax = %d, want 0 for \"max\"", c.MemMax)
	}
	if c.IORead != 1010 || c.IOWrite != 2020 {
		t.Errorf("io = %d/%d, want 1010/2020", c.IORead, c.IOWrite)
	}
}

// A device-mapper node re-reports the IO of the disk under it; counting both
// doubles every figure.
func TestIOStatSkipsPseudoDevices(t *testing.T) {
	sysDevBlock = t.TempDir()
	pseudoCache = map[string]bool{}
	if err := os.Symlink("../../devices/virtual/block/dm-0", filepath.Join(sysDevBlock, "253:0")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../devices/pci/block/sda", filepath.Join(sysDevBlock, "8:0")); err != nil {
		t.Fatal(err)
	}
	r, w := ioStat([]byte("8:0 rbytes=100 wbytes=200\n253:0 rbytes=100 wbytes=200\n"))
	if r != 100 || w != 200 {
		t.Errorf("io = %d/%d, want 100/200", r, w)
	}
}

func TestReadOfVanishedCgroupIsErrGone(t *testing.T) {
	if _, err := NewReader().Read(filepath.Join(t.TempDir(), "gone")); err != ErrGone {
		t.Errorf("err = %v, want ErrGone", err)
	}
}

func TestIndexFindsEveryLayout(t *testing.T) {
	root := t.TempDir()
	docker := filepath.Join(root, "system.slice", "docker-"+hexID('a')+".scope")
	cgroupfs := filepath.Join(root, "docker", hexID('b'))
	podman := filepath.Join(root, "machine.slice", "libpod-"+hexID('c')+".scope")
	conmon := filepath.Join(root, "machine.slice", "libpod-conmon-"+hexID('d')+".scope")
	rootless := filepath.Join(root, "user.slice", "user-1000.slice", "user@1000.service", "user.slice", "libpod-"+hexID('e')+".scope")
	nested := filepath.Join(docker, "init.scope", "docker-"+hexID('f')+".scope")
	for _, d := range []string{docker, cgroupfs, podman, conmon, rootless, nested} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	got, err := Index(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{hexID('a'): docker, hexID('b'): cgroupfs, hexID('c'): podman, hexID('e'): rootless}
	if len(got) != len(want) {
		t.Errorf("index = %v, want %v", got, want)
	}
	for id, dir := range want {
		if got[id] != dir {
			t.Errorf("%s… -> %q, want %q", id[:4], got[id], dir)
		}
	}
}

func TestContainerID(t *testing.T) {
	cases := map[string]string{
		"docker-" + hexID('a') + ".scope":      hexID('a'),
		hexID('b'):                             hexID('b'),
		"system.slice":                         "",
		"docker-" + hexID('a')[:63] + ".scope": "",
		hexID('a') + "0":                       "", // 65 digits is not an ID
	}
	for in, want := range cases {
		if got := containerID(in); got != want {
			t.Errorf("containerID(%q) = %q, want %q", in, got, want)
		}
	}
}

type fakeLister struct {
	list  []dockerx.Container
	calls int
}

func (f *fakeLister) ListProjectContainers(context.Context) ([]dockerx.Container, error) {
	f.calls++
	return f.list, nil
}

func TestTrackerRebuildsOnlyWhenDirty(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "system.slice", "docker-"+hexID('a')+".scope")
	os.MkdirAll(dir, 0o755)
	f := &fakeLister{list: []dockerx.Container{
		{ID: hexID('a'), Project: "jellyfin", Service: "app", State: "running"},
		{ID: hexID('b'), Project: "jellyfin", Service: "db", State: "exited"},
	}}
	tr := NewTracker(root, f)

	got := tr.Targets(context.Background())
	if len(got) != 1 || got[0].Project != "jellyfin" || got[0].Dir != dir {
		t.Fatalf("targets = %+v", got)
	}
	tr.Targets(context.Background())
	if f.calls != 1 {
		t.Errorf("list calls = %d, want 1 (clean tracker must not re-list)", f.calls)
	}
	tr.MarkDirty()
	tr.Targets(context.Background())
	if f.calls != 2 {
		t.Errorf("list calls = %d, want 2 after MarkDirty", f.calls)
	}
}
