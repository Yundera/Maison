package system

import (
	"os"
	"path/filepath"
	"testing"
)

// Captured on holyhorse (Ubuntu 24.04, kernel 6.8), 2026-10-21.
const (
	psiCPU = "some avg10=9.46 avg60=12.49 avg300=15.51 total=14739896105\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n"
	psiMem = "some avg10=0.00 avg60=0.00 avg300=0.00 total=3719194\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=3221401\n"
	psiIO  = "some avg10=0.51 avg60=0.95 avg300=1.03 total=876130644\nfull avg10=0.22 avg60=0.33 avg300=0.34 total=469228053\n"
)

func TestPSITotal(t *testing.T) {
	if v, ok := psiTotal([]byte(psiCPU), "some"); !ok || v != 14739896105 {
		t.Fatalf("cpu some = %d, %v", v, ok)
	}
	if v, ok := psiTotal([]byte(psiMem), "full"); !ok || v != 3221401 {
		t.Fatalf("mem full = %d, %v", v, ok)
	}
	if _, ok := psiTotal([]byte("garbage"), "some"); ok {
		t.Fatal("garbage parsed")
	}
}

func fakeProc(t *testing.T, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOST_PROC", dir)
}

func TestReadPressure(t *testing.T) {
	fakeProc(t, map[string]string{
		"pressure/cpu":    psiCPU,
		"pressure/memory": psiMem,
		"pressure/io":     psiIO,
	})
	p := ReadPressure()
	want := Pressure{OK: true, CPUSome: 14739896105, MemSome: 3719194, MemFull: 3221401, IOFull: 469228053}
	if p != want {
		t.Fatalf("got %+v, want %+v", p, want)
	}
}

// A kernel without PSI has no /proc/pressure at all.
func TestReadPressureAbsent(t *testing.T) {
	fakeProc(t, map[string]string{"vmstat": "oom_kill 0\n"})
	if p := ReadPressure(); p.OK {
		t.Fatalf("got %+v, want not OK", p)
	}
}

func TestReadOOMKills(t *testing.T) {
	fakeProc(t, map[string]string{"vmstat": "nr_free_pages 1234\noom_kill 7\nnr_foo 9\n"})
	if n, ok := ReadOOMKills(); !ok || n != 7 {
		t.Fatalf("got %d, %v", n, ok)
	}
}
