package apps

import (
	"sync"
	"time"
)

// Rate and ETA are derived here, from the numbers an engine reports, rather than
// read from the engine itself.
//
// Every backup tool prints its own ETA — kopia's progress line ends with "2m30s
// left" — and taking it would have been less code. It is the wrong call twice over.
// It is engine-specific: the next engine's estimate would have different semantics,
// different smoothing and a different idea of what "left" measures, so the number
// under the bar would mean something different depending on where the backup was
// going. And it is not available uniformly: kopia reports no ETA at all while it is
// still estimating, and the local engine reports none ever, so the UI would need
// this fallback regardless.
//
// So the engine's job stops at reporting what it observed — a percentage, or bytes
// done and expected — and everything derived from that is computed in one place, the
// same way, for every engine. An engine that reports nothing but a percentage gets a
// usable ETA; one that reports bytes gets a transfer rate as well. Neither has to
// know that either number exists.

const (
	// minSampleGap is how far apart two byte readings must be before the delta
	// between them is worth anything. Engines report progress per chunk, which on a
	// fast local copy is hundreds of times a second; a rate computed over a
	// millisecond is noise amplified by a thousand.
	minSampleGap = time.Second

	// minElapsed and minPct are what an estimate has to clear before it is shown at
	// all. An ETA offered in the first second is always wrong, and being wrong early
	// is how a progress indicator teaches people to ignore it.
	minElapsed = 5 * time.Second
	minPct     = 2.0

	// minSamples applies to the byte path only: two readings are the fewest that can
	// describe a rate rather than a single instant.
	minSamples = 2

	// smoothing is the EWMA weight given to the newest observation. Low enough that
	// one slow chunk does not double the estimate, high enough that the number still
	// tracks a genuine change in speed within a few seconds.
	smoothing = 0.3

	// maxETA is where an estimate stops being information. Early in a large upload
	// over a throttled link the arithmetic will happily produce weeks; clamping is
	// more honest than rendering it, and less alarming than it deserves to look.
	maxETA = 99 * time.Hour
)

// Progress is what a bar needs beyond "something is happening": how far along the
// operation is, how fast it is moving, and how long it has left.
//
// Every field has an explicit "not known" value, because for most of these the
// honest answer is frequently that nobody knows yet — kopia spends its opening
// seconds estimating, and an engine may never report bytes at all. A zero that means
// "no data" and a zero that means "zero bytes per second" would be indistinguishable
// to the UI, so Rate and ETA are only ever set once they are real.
type Progress struct {
	// Pct is 0-100, or PctUnknown when neither the engine nor the byte counts can
	// say. It is the engine's own figure when it reported one, and derived from the
	// byte counts otherwise — an engine that reports bytes therefore gets a bar
	// without having to compute a percentage as well.
	Pct float64

	// Rate is bytes per second, 0 when the engine reports no byte counts. There is no
	// way to infer it from a percentage: a percentage of an unknown quantity has no
	// size, which is exactly why this is separate from ETA rather than derived from it.
	Rate float64

	// ETA is how long the current phase has left, 0 when there is not yet an honest
	// answer.
	ETA time.Duration
}

// Tracker turns a stream of progress reports into a rate and an ETA.
//
// One Tracker follows one operation. It is reset automatically when the phase
// changes, because the percentages an engine reports are per-phase — Copy runs 0 to
// 100, then Sync runs 0 to 100 again — and an estimate carried across that boundary
// describes the wrong work. That reset is also what produces the most useful number
// on the screen for free: the Sync phase is the one during which the app is stopped,
// so its ETA is how much longer the app is down.
//
// Safe for concurrent use: progress arrives from the goroutine running the backup
// while the dashboard reads the state from a request handler.
type Tracker struct {
	// Now exists so the smoothing and the gating can be tested without sleeping.
	// Nil means the real clock.
	Now func() time.Time

	mu      sync.Mutex
	phase   string
	started time.Time

	// The anchor for the next byte delta: when it was taken, and what the count was.
	anchorAt   time.Time
	anchorDone int64
	samples    int

	rate float64 // bytes/second, smoothed
	eta  float64 // seconds, smoothed
}

func (t *Tracker) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// Observe records one report and returns what can be derived from it so far.
//
// done and total are bytes and are optional: an engine that does not count them
// passes zero, and the percentage path is used instead. pct is PctUnknown when the
// engine did not report one.
func (t *Tracker) Observe(phase string, pct float64, done, total int64) Progress {
	now := t.now()

	t.mu.Lock()
	defer t.mu.Unlock()

	if phase != t.phase || t.started.IsZero() {
		t.reset(phase, now)
	}
	elapsed := now.Sub(t.started)

	// The byte path. Preferred whenever it is available, because it measures the work
	// directly: a percentage can only be turned into an ETA by assuming the rest of
	// the phase goes at the average speed of the part already done, and the two passes
	// of a backup are not uniform enough for that to hold.
	if total > 0 && done >= 0 {
		if pct == PctUnknown || pct == 0 {
			pct = pctOf(done, total)
		}
		t.sample(now, done)
		if t.rate > 0 && t.samples >= minSamples && ready(elapsed, pct) {
			remaining := total - done
			if remaining < 0 {
				remaining = 0
			}
			t.blendETA(float64(remaining) / t.rate)
		}
		return Progress{Pct: pct, Rate: t.rate, ETA: t.etaDuration()}
	}

	// The percentage path — the fallback that makes this work for any engine that can
	// say how far along it is and nothing more.
	if pct != PctUnknown && ready(elapsed, pct) {
		t.blendETA(elapsed.Seconds() * (100 - pct) / pct)
	}
	return Progress{Pct: pct, ETA: t.etaDuration()}
}

// reset starts a fresh phase. Rate and ETA go with it: they described work that is
// finished, and carrying them over would show the user an estimate for a phase that
// has not begun.
func (t *Tracker) reset(phase string, now time.Time) {
	t.phase = phase
	t.started = now
	t.anchorAt = time.Time{}
	t.anchorDone = 0
	t.samples = 0
	t.rate = 0
	t.eta = 0
}

// sample folds one byte reading into the smoothed rate, if enough time has passed
// since the last one to make the delta meaningful.
func (t *Tracker) sample(now time.Time, done int64) {
	if t.anchorAt.IsZero() {
		t.anchorAt, t.anchorDone = now, done
		return
	}
	dt := now.Sub(t.anchorAt).Seconds()
	if dt < minSampleGap.Seconds() {
		// Deliberately leaves the anchor where it is, so the readings accumulate into
		// one usable interval rather than being discarded one by one.
		return
	}
	if delta := done - t.anchorDone; delta >= 0 {
		t.blendRate(float64(delta) / dt)
		t.samples++
	}
	// The anchor advances even when the delta was negative. A count going backwards
	// means the engine revised its own figures — kopia raises its estimate mid-snapshot
	// as it discovers the tree, and a restore can restart a file — and the old anchor
	// then describes a quantity that no longer exists. Keeping it would make the next
	// delta, and therefore the next rate, enormous.
	t.anchorAt, t.anchorDone = now, done
}

func (t *Tracker) blendRate(v float64) {
	if t.rate == 0 {
		t.rate = v
		return
	}
	t.rate = smoothing*v + (1-smoothing)*t.rate
}

func (t *Tracker) blendETA(seconds float64) {
	if seconds < 0 {
		seconds = 0
	}
	if t.eta == 0 {
		t.eta = seconds
		return
	}
	t.eta = smoothing*seconds + (1-smoothing)*t.eta
}

func (t *Tracker) etaDuration() time.Duration {
	if t.eta <= 0 {
		return 0
	}
	d := time.Duration(t.eta * float64(time.Second)).Round(time.Second)
	if d > maxETA {
		return maxETA
	}
	return d
}

// ready reports whether an estimate has enough behind it to be worth showing.
func ready(elapsed time.Duration, pct float64) bool {
	return elapsed >= minElapsed && pct >= minPct
}

// pctOf is the percentage form of a byte count, for an engine that reports the
// counts and leaves the arithmetic to us.
func pctOf(done, total int64) float64 {
	if total <= 0 {
		return PctUnknown
	}
	p := float64(done) / float64(total) * 100
	if p > 100 {
		return 100
	}
	return p
}
