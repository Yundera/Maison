package metrics

import (
	"testing"
	"time"
)

func series(n int, cpu func(i int) float64) []Record {
	out := make([]Record, n)
	for i := range out {
		out[i] = Record{At: base.Add(time.Duration(i) * time.Minute), CPU: cpu(i)}
	}
	return out
}

func TestDownsampleReducesToTheRequestedBudget(t *testing.T) {
	recs := series(600, func(i int) float64 { return float64(i % 100) })
	got := Downsample(recs, base, base.Add(600*time.Minute), 50)
	if len(got) > 50 {
		t.Fatalf("got %d spans, want at most 50", len(got))
	}
	total := 0
	for _, s := range got {
		total += s.Points
	}
	if total != len(recs) {
		t.Errorf("spans account for %d records, want %d", total, len(recs))
	}
}

// The envelope is the reason to downsample server-side rather than just thinning
// the series: a one-minute spike inside an hour-wide bucket has to survive as the
// bucket's max, or the graph flattens exactly the event worth looking at.
func TestDownsampleKeepsSpikesInTheEnvelope(t *testing.T) {
	recs := series(60, func(i int) float64 {
		if i == 30 {
			return 100
		}
		return 5
	})
	got := Downsample(recs, base, base.Add(60*time.Minute), 1)
	if len(got) != 1 {
		t.Fatalf("got %d spans, want 1", len(got))
	}
	s := got[0]
	if s.CPU.Max != 100 {
		t.Errorf("Max = %v, want 100 — the spike was averaged away", s.CPU.Max)
	}
	if s.CPU.Min != 5 {
		t.Errorf("Min = %v, want 5", s.CPU.Min)
	}
	if s.CPU.Avg < 6 || s.CPU.Avg > 8 {
		t.Errorf("Avg = %v, want ~6.6", s.CPU.Avg)
	}
}

// Downtime must stay visible. Emitting zeroes for the missing buckets would draw a
// confident flat line across a period when the box was off.
func TestDownsampleOmitsEmptyBucketsRatherThanZeroingThem(t *testing.T) {
	var recs []Record
	for i := 0; i < 10; i++ {
		recs = append(recs, Record{At: base.Add(time.Duration(i) * time.Minute), CPU: 50})
	}
	// ...an hour of nothing...
	for i := 70; i < 80; i++ {
		recs = append(recs, Record{At: base.Add(time.Duration(i) * time.Minute), CPU: 50})
	}

	got := Downsample(recs, base, base.Add(80*time.Minute), 8)
	for _, s := range got {
		if s.Points == 0 {
			t.Fatalf("an empty bucket was emitted: %+v", s)
		}
		if s.CPU.Avg != 50 {
			t.Errorf("Avg = %v, want 50 — a gap was averaged in", s.CPU.Avg)
		}
	}
	// The gap must show as a jump in the timestamps, which is what the client
	// breaks the line on.
	var maxGap time.Duration
	for i := 1; i < len(got); i++ {
		if d := got[i].At.Sub(got[i-1].At); d > maxGap {
			maxGap = d
		}
	}
	if maxGap < 30*time.Minute {
		t.Errorf("largest gap between spans is %v, want the hour of downtime to show", maxGap)
	}
}

func TestDownsampleHandlesEmptyInput(t *testing.T) {
	if got := Downsample(nil, base, base.Add(time.Hour), 10); len(got) != 0 {
		t.Errorf("got %d spans from no records, want 0", len(got))
	}
}

func TestStepIsTheBucketWidth(t *testing.T) {
	if got := Step(base, base.Add(time.Hour), 60); got != time.Minute {
		t.Errorf("Step = %v, want 1m", got)
	}
}
