package apps

import (
	"testing"
	"time"
)

// clock is a hand-cranked time source, so the gating below is exercised without
// sleeping through it.
type clock struct{ t time.Time }

func newClock() *clock {
	return &clock{t: time.Date(2026, 3, 1, 3, 30, 0, 0, time.UTC)}
}

func (c *clock) now() time.Time       { return c.t }
func (c *clock) tick(d time.Duration) { c.t = c.t.Add(d) }

// The point of the whole design: an engine that reports nothing but a percentage
// still gets an ETA. If this stops working, "generic progress" has quietly become
// "progress for engines that count bytes", which is one engine.
func TestETAFromAPercentageAlone(t *testing.T) {
	c := newClock()
	tr := &Tracker{Now: c.now}

	tr.Observe(PhaseCopy, 0, 0, 0)
	c.tick(10 * time.Second)
	got := tr.Observe(PhaseCopy, 25, 0, 0)

	// A quarter done in ten seconds: thirty seconds left.
	if got.ETA < 25*time.Second || got.ETA > 35*time.Second {
		t.Errorf("ETA = %v, want about 30s", got.ETA)
	}
	if got.Rate != 0 {
		t.Errorf("Rate = %v, want 0 — a percentage has no size, so a byte rate is not inferable", got.Rate)
	}
}

// Byte counts buy the rate, and the percentage comes free — an engine that counts
// bytes should not also have to compute a percentage.
func TestRateAndDerivedPercentageFromBytes(t *testing.T) {
	c := newClock()
	tr := &Tracker{Now: c.now}

	const mb = 1 << 20
	tr.Observe(PhaseCopy, PctUnknown, 0, 100*mb)
	for i := 1; i <= 10; i++ {
		c.tick(time.Second)
		tr.Observe(PhaseCopy, PctUnknown, int64(i)*10*mb, 100*mb)
	}
	got := tr.Observe(PhaseCopy, PctUnknown, 50*mb, 100*mb)

	if got.Pct < 49 || got.Pct > 51 {
		t.Errorf("Pct = %v, want it derived from the byte counts as ~50", got.Pct)
	}
	if got.Rate < 9*mb || got.Rate > 11*mb {
		t.Errorf("Rate = %v, want about 10 MB/s", got.Rate)
	}
}

// An estimate offered in the first second is always wrong, and being wrong early is
// how a progress indicator teaches people to ignore it.
func TestNoETAUntilThereIsSomethingBehindIt(t *testing.T) {
	c := newClock()
	tr := &Tracker{Now: c.now}

	tr.Observe(PhaseCopy, 0, 0, 0)
	c.tick(time.Second)
	if got := tr.Observe(PhaseCopy, 40, 0, 0); got.ETA != 0 {
		t.Errorf("ETA = %v after one second, want none offered yet", got.ETA)
	}

	// Nor from a percentage too small to divide by: at 0.1% the arithmetic produces
	// a number that is pure noise.
	c.tick(time.Minute)
	if got := tr.Observe(PhaseCopy, 0.1, 0, 0); got.ETA != 0 {
		t.Errorf("ETA = %v at 0.1%%, want none offered yet", got.ETA)
	}
}

// The tracks are per-phase, so an estimate must not survive the boundary between
// them. The stopped pass is the one that matters: its ETA is how much longer the app
// is down, and inheriting the live pass's numbers would describe the wrong work.
func TestPhaseChangeResetsTheEstimate(t *testing.T) {
	c := newClock()
	tr := &Tracker{Now: c.now}

	const mb = 1 << 20
	tr.Observe(PhaseCopy, PctUnknown, 0, 100*mb)
	for i := 1; i <= 10; i++ {
		c.tick(time.Second)
		tr.Observe(PhaseCopy, PctUnknown, int64(i)*mb, 100*mb)
	}
	slow := tr.Observe(PhaseCopy, PctUnknown, 10*mb, 100*mb)
	if slow.ETA == 0 {
		t.Fatal("the copy phase produced no ETA to carry over in the first place")
	}

	c.tick(time.Second)
	got := tr.Observe(PhaseSync, PctUnknown, 0, 5*mb)
	if got.Rate != 0 || got.ETA != 0 {
		t.Errorf("phase change left rate=%v eta=%v behind, want both cleared", got.Rate, got.ETA)
	}
}

// An engine discovering the tree as it walks it revises its own total upward, so the
// count can go backwards relative to it. That is normal, not a fault, and it must not
// produce a nonsense rate on the next sample.
func TestARevisedTotalDoesNotBlowUpTheRate(t *testing.T) {
	c := newClock()
	tr := &Tracker{Now: c.now}

	const mb = 1 << 20
	tr.Observe(PhaseCopy, PctUnknown, 0, 100*mb)
	c.tick(time.Second)
	tr.Observe(PhaseCopy, PctUnknown, 50*mb, 100*mb)

	// The engine restarts its accounting against a much larger tree.
	c.tick(time.Second)
	tr.Observe(PhaseCopy, PctUnknown, 1*mb, 900*mb)
	c.tick(time.Second)
	got := tr.Observe(PhaseCopy, PctUnknown, 2*mb, 900*mb)

	if got.Rate > 60*mb {
		t.Errorf("Rate = %v after the total was revised, want it to stay near the real speed", got.Rate)
	}
	if got.Pct > 100 || got.Pct < 0 {
		t.Errorf("Pct = %v, want it inside 0-100", got.Pct)
	}
}

// Engines report per chunk, which on a local copy is hundreds of times a second. A
// rate computed over a millisecond is noise, so the readings have to accumulate into
// an interval worth dividing by rather than being thrown away one at a time.
func TestSubSecondReportsAccumulateRatherThanBeingDiscarded(t *testing.T) {
	c := newClock()
	tr := &Tracker{Now: c.now}

	const mb = 1 << 20
	tr.Observe(PhaseCopy, PctUnknown, 0, 100*mb)
	// Ten reports 100ms apart, 1 MB each: one second of wall clock, 10 MB moved.
	for i := 1; i <= 10; i++ {
		c.tick(100 * time.Millisecond)
		tr.Observe(PhaseCopy, PctUnknown, int64(i)*mb, 100*mb)
	}
	c.tick(time.Second)
	got := tr.Observe(PhaseCopy, PctUnknown, 20*mb, 100*mb)

	if got.Rate < 8*mb || got.Rate > 12*mb {
		t.Errorf("Rate = %v, want about 10 MB/s from reports arriving faster than the sample gap", got.Rate)
	}
}

// Early in a large transfer over a slow link the arithmetic will happily produce
// weeks. Rendering that is worse than saying nothing.
func TestAnAbsurdEstimateIsClamped(t *testing.T) {
	c := newClock()
	tr := &Tracker{Now: c.now}

	tr.Observe(PhaseCopy, 0, 0, 0)
	c.tick(time.Hour)
	got := tr.Observe(PhaseCopy, 2, 0, 0)
	if got.ETA > maxETA {
		t.Errorf("ETA = %v, want it clamped to %v", got.ETA, maxETA)
	}
}
