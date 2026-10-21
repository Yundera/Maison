package metrics

import (
	"math"
	"testing"
	"time"
)

func minutes(n int, f func(i int) Record) []Record {
	out := make([]Record, n)
	for i := range out {
		r := f(i)
		r.At = base.Add(time.Duration(i) * time.Minute)
		out[i] = r
	}
	return out
}

func TestMeanAndCoverage(t *testing.T) {
	// Two hours of window, one hour recorded at a steady 40%.
	recs := minutes(60, func(int) Record { return Record{CPUPressure: 40} })
	avg, cov := Mean(recs, func(r Record) float64 { return r.CPUPressure }, base, base.Add(2*time.Hour), time.Minute)
	if avg != 40 {
		t.Errorf("avg = %v, want 40", avg)
	}
	if math.Abs(cov-0.5) > 0.01 {
		t.Errorf("coverage = %v, want 0.5", cov)
	}
}

// A kernel without PSI is "cannot judge", not "calm".
func TestMeanSkipsNoPressure(t *testing.T) {
	recs := minutes(60, func(int) Record { return Record{CPUPressure: NoPressure} })
	if _, cov := Mean(recs, func(r Record) float64 { return r.CPUPressure }, base, base.Add(time.Hour), time.Minute); cov != 0 {
		t.Errorf("coverage = %v, want 0", cov)
	}
}

func TestDiskTrend(t *testing.T) {
	// 4 days, one sample an hour, growing 2% a day from 70%: at 78% after 4 days,
	// so full in 11 days.
	recs := make([]Record, 0, 97)
	for h := 0; h <= 96; h++ {
		recs = append(recs, Record{At: base.Add(time.Duration(h) * time.Hour), Disk: 70 + 2*float64(h)/24})
	}
	days, ok := DiskTrend(recs, 72*time.Hour)
	if !ok || math.Abs(days-11) > 0.01 {
		t.Fatalf("DiskTrend = %v, %v; want 11, true", days, ok)
	}

	if _, ok := DiskTrend(recs[:48], 72*time.Hour); ok {
		t.Error("two days of history should not be enough to predict")
	}

	flat := make([]Record, len(recs))
	for i, r := range recs {
		flat[i] = Record{At: r.At, Disk: 80 - float64(i)/100}
	}
	if _, ok := DiskTrend(flat, 72*time.Hour); ok {
		t.Error("a shrinking disk has no fill date")
	}
}

func TestTopApps(t *testing.T) {
	got := TopApps(map[string][]AppRecord{
		"jellyfin": {{CPU: 60}, {CPU: 62}},
		"immich":   {{CPU: 20}},
		"idle":     {{CPU: 0.1}},
		"gone":     nil,
	}, 2, func(a AppShare) float64 { return a.CPU })
	if len(got) != 2 || got[0].ID != "jellyfin" || got[0].CPU != 61 || got[1].ID != "immich" {
		t.Fatalf("got %+v", got)
	}
}
