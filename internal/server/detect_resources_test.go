package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yundera/maison/internal/incident"
	"github.com/yundera/maison/internal/metrics"
)

var resNow = time.Unix(1_700_000_000/60*60, 0)

// history builds one record a minute for the span before resNow; f fills each one
// given how long before now it is.
func history(span time.Duration, f func(ago time.Duration) metrics.Record) []metrics.Record {
	n := int(span / time.Minute)
	out := make([]metrics.Record, 0, n)
	for i := n; i >= 0; i-- {
		ago := time.Duration(i) * time.Minute
		r := f(ago)
		r.At = resNow.Add(-ago)
		out = append(out, r)
	}
	return out
}

func calm(time.Duration) metrics.Record {
	return metrics.Record{CPU: 20, Mem: 40, CPUPressure: 2, MemPressure: 0, MemFullPressure: 0}
}

func TestCPUOpensOnADayOfStall(t *testing.T) {
	recs := history(25*time.Hour, func(time.Duration) metrics.Record {
		return metrics.Record{CPU: 60, CPUPressure: 30}
	})
	j := judgeCPU(recs, resNow, time.Minute)
	if j.verdict != judgeOpen || !j.pressure || j.avg != 30 {
		t.Fatalf("got %+v, want open on pressure at 30", j)
	}
}

// 90% busy with nothing queued is a box doing its job.
func TestCPUBusyButNotStalledIsNotAnAlert(t *testing.T) {
	recs := history(25*time.Hour, func(time.Duration) metrics.Record {
		return metrics.Record{CPU: 95, CPUPressure: 5}
	})
	if j := judgeCPU(recs, resNow, time.Minute); j.verdict == judgeOpen {
		t.Fatalf("got %+v, want no alert", j)
	}
}

func TestCPUFallsBackToBusyWithoutPSI(t *testing.T) {
	recs := history(25*time.Hour, func(time.Duration) metrics.Record {
		return metrics.Record{CPU: 95, CPUPressure: metrics.NoPressure}
	})
	j := judgeCPU(recs, resNow, time.Minute)
	if j.verdict != judgeOpen || j.pressure {
		t.Fatalf("got %+v, want open on busy %%", j)
	}
}

// A day-long average still over the line must not re-open what six calm hours just
// closed.
func TestCPUClearsOnSixCalmHoursWithoutFlapping(t *testing.T) {
	recs := history(25*time.Hour, func(ago time.Duration) metrics.Record {
		if ago < 7*time.Hour {
			return metrics.Record{CPUPressure: 1}
		}
		return metrics.Record{CPUPressure: 60}
	})
	if j := judgeCPU(recs, resNow, time.Minute); j.verdict != judgeClear {
		t.Fatalf("got %+v, want clear", j)
	}
}

// Half a day recorded is no evidence about a day.
func TestCPUNeedsCoverage(t *testing.T) {
	recs := history(10*time.Hour, func(time.Duration) metrics.Record {
		return metrics.Record{CPUPressure: 60}
	})
	if j := judgeCPU(recs, resNow, time.Minute); j.verdict != judgeNone {
		t.Fatalf("got %+v, want none", j)
	}
}

func TestMemory(t *testing.T) {
	for _, c := range []struct {
		name     string
		rec      metrics.Record
		want     verdict
		critical bool
	}{
		{"calm", calm(0), judgeClear, false},
		{"stalled", metrics.Record{Mem: 70, MemPressure: 15}, judgeOpen, false},
		{"thrashing", metrics.Record{Mem: 95, MemPressure: 40, MemFullPressure: 20}, judgeOpen, true},
		{"full without PSI", metrics.Record{Mem: 93, MemPressure: metrics.NoPressure, MemFullPressure: metrics.NoPressure}, judgeOpen, false},
		{"calm without PSI", metrics.Record{Mem: 50, MemPressure: metrics.NoPressure, MemFullPressure: metrics.NoPressure}, judgeClear, false},
		{"between thresholds", metrics.Record{Mem: 87, MemPressure: 1}, judgeNone, false},
		{"pressure between thresholds", metrics.Record{Mem: 50, MemPressure: 5}, judgeNone, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			recs := history(7*time.Hour, func(time.Duration) metrics.Record { return c.rec })
			j := judgeMemory(recs, resNow, time.Minute)
			if j.verdict != c.want || j.critical != c.critical {
				t.Fatalf("got %+v, want verdict %d critical %v", j, c.want, c.critical)
			}
		})
	}
}

func TestDiskTrendVerdicts(t *testing.T) {
	grow := func(perDay, start float64) []metrics.Record {
		return history(5*24*time.Hour, func(ago time.Duration) metrics.Record {
			return metrics.Record{Disk: start + perDay*(5-ago.Hours()/24)}
		})
	}
	// 60% → 85% over five days: 5%/day, 3 days left.
	if j := judgeDiskTrend(grow(5, 60)); j.verdict != judgeOpen || j.days < 2.9 || j.days > 3.1 {
		t.Errorf("fast growth: got %+v, want open in ~3 days", j)
	}
	// 30% → 45%: filling fast, but too empty to be news.
	if j := judgeDiskTrend(grow(3, 30)); j.verdict == judgeOpen {
		t.Errorf("mostly empty disk: got %+v, want no alert", j)
	}
	// Slow growth: years away.
	if j := judgeDiskTrend(grow(0.1, 60)); j.verdict != judgeClear {
		t.Errorf("slow growth: got %+v, want clear", j)
	}
	// One day of history is not a trend.
	short := history(24*time.Hour, func(ago time.Duration) metrics.Record { return metrics.Record{Disk: 90 - ago.Hours()} })
	if j := judgeDiskTrend(short); j.verdict != judgeNone {
		t.Errorf("short history: got %+v, want none", j)
	}
}

func oomServer(t *testing.T, vmstat string) *Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vmstat"), []byte(vmstat), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOST_PROC", dir)
	return &Server{incidents: incident.New(filepath.Join(t.TempDir(), "incidents.json"))}
}

func setVmstat(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(os.Getenv("HOST_PROC"), "vmstat"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOOMAttributesAppsAndHost(t *testing.T) {
	s := oomServer(t, "oom_kill 5\n")
	d := &detector{}
	ctx := context.Background()

	s.checkOOM(ctx, d, resNow) // baseline: the 5 kills since boot are history
	if s.incidents.IsOpen(incident.KindHostOOM) {
		t.Fatal("kills from before Maison started were reported")
	}

	// Two kills in immich, one outside every app.
	s.oom.record("immich")
	s.oom.record("immich")
	setVmstat(t, "oom_kill 8\n")
	s.checkOOM(ctx, d, resNow.Add(5*time.Minute))

	inc, ok := s.openIncident("app.oom:immich")
	if !ok || inc.Args["count"] != "2" {
		t.Fatalf("app.oom:immich = %+v, %v; want open with count 2", inc, ok)
	}
	host, ok := s.openIncident(incident.KindHostOOM)
	if !ok || host.Args["count"] != "1" {
		t.Fatalf("host.oom = %+v, %v; want open with count 1", host, ok)
	}

	// A quiet day closes both.
	s.checkOOM(ctx, d, resNow.Add(5*time.Minute+oomQuiet))
	if s.incidents.IsOpen("app.oom:immich") || s.incidents.IsOpen(incident.KindHostOOM) {
		t.Fatal("OOM incidents still open after a quiet day")
	}
}

// After a restart the in-memory clock is gone; an incident opened before it must
// still close a day after its last kill.
func TestOOMSurvivesRestart(t *testing.T) {
	s := oomServer(t, "oom_kill 0\n")
	s.incidents.Now = func() time.Time { return resNow }
	s.incidents.Report(incident.Report{ID: "app.oom:immich", Kind: incident.KindAppOOM, Severity: incident.Warning,
		Title: "immich ran out of memory", Args: map[string]string{"count": "3"}})

	d := &detector{}
	s.checkOOM(context.Background(), d, resNow.Add(time.Hour))
	if !s.incidents.IsOpen("app.oom:immich") {
		t.Fatal("closed too early")
	}
	if d.oom.count["app.oom:immich"] != 3 {
		t.Fatalf("count = %d, want 3 carried over", d.oom.count["app.oom:immich"])
	}
	s.checkOOM(context.Background(), d, resNow.Add(oomQuiet))
	if s.incidents.IsOpen("app.oom:immich") {
		t.Fatal("still open a day after the last kill")
	}
}
