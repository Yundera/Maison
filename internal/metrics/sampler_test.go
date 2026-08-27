package metrics

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// "Off" has to mean nothing is written. Reading the history page with recording
// disabled must not be what puts a file on the user's disk.
func TestDisabledSamplerNeverCreatesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.ring")
	s := NewSampler(path, t.TempDir(), time.Minute, 60, func() bool { return false })

	s.tick()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a disabled sampler created %s", path)
	}

	recs, err := s.Read(time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("got %d records with recording off, want 0", len(recs))
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("reading history created %s", path)
	}
	if n := s.Bytes(); n != 0 {
		t.Errorf("Bytes() = %d with no file, want 0", n)
	}
}

// Deleting when nothing was ever recorded must succeed quietly, not create a file
// in order to remove it.
func TestDeleteWithNoRecordingIsQuiet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.ring")
	s := NewSampler(path, t.TempDir(), time.Minute, 60, func() bool { return false })
	if err := s.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("Delete created the file it was asked to remove")
	}
}

// Turning it on starts recording; the first tick is only a baseline, so a rate has
// two readings behind it rather than being derived from one.
func TestEnabledSamplerRecordsFromTheSecondTick(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.ring")
	on := true
	s := NewSampler(path, "/", time.Minute, 60, func() bool { return on })
	defer s.Close()

	s.tick() // baseline only
	recs, _ := s.Read(time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if len(recs) != 0 {
		t.Fatalf("got %d records from the baseline tick, want 0", len(recs))
	}

	s.tick() // now there is a delta to record
	recs, _ = s.Read(time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if len(recs) != 1 {
		t.Fatalf("got %d records after a second tick, want 1", len(recs))
	}
}

// A long gap must not be averaged into one minute: the sampler drops that reading
// and re-baselines, leaving the missing slots empty.
func TestGapLongerThanThreeStepsIsDroppedNotSmeared(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.ring")
	on := true
	// A one-second step makes "more than three steps" reachable in a test.
	s := NewSampler(path, "/", time.Second, 60, func() bool { return on })
	defer s.Close()

	s.tick() // baseline
	// Simulate the sampler having been stopped: age the baseline well past 3 steps.
	s.mu.Lock()
	s.prev.At = s.prev.At.Add(-time.Minute)
	s.mu.Unlock()

	s.tick()
	recs, _ := s.Read(time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if len(recs) != 0 {
		t.Fatalf("got %d records across a gap, want 0 — the gap was averaged into a sample", len(recs))
	}
}
