package metrics

import (
	"context"
	"log"
	"os"
	"sync"
	"time"

	"github.com/yundera/maison/internal/system"
)

// Sampler is the only thing in Maison that measures the host when nobody is
// looking at it.
//
// Everything else — the dashboard gauges, the per-app monitor, the Resources page
// — is gated on a live subscription and costs nothing while closed. History cannot
// be: a graph of the last thirty days has to have been recorded during those
// thirty days. So this loop is the one place where the idle budget is actually
// spent, and it is kept to a single cheap reading per minute (see
// system.ReadCounters for what is deliberately NOT in it) writing 32 bytes into a
// file. The staged records are the only part held in memory.
type Sampler struct {
	dataRoot string
	path     string
	step     time.Duration
	slots    int

	// enabled is read on every tick rather than captured once, so the settings
	// toggle takes effect without restarting Maison.
	enabled func() bool

	mu   sync.Mutex
	ring *Ring
	prev *system.Counters
}

// NewSampler returns a sampler writing to path. Nothing is opened or measured
// until Run is called and enabled() first returns true — a deployment with history
// switched off never creates the file at all.
func NewSampler(path, dataRoot string, step time.Duration, slots int, enabled func() bool) *Sampler {
	return &Sampler{
		dataRoot: dataRoot,
		path:     path,
		step:     step,
		slots:    slots,
		enabled:  enabled,
	}
}

// Run samples until ctx is cancelled. Intended to be started as a goroutine.
func (s *Sampler) Run(ctx context.Context) {
	t := time.NewTicker(s.step)
	defer t.Stop()
	// Take a baseline immediately so the first recorded point is one step away
	// rather than two.
	s.tick()
	for {
		select {
		case <-ctx.Done():
			s.Close()
			return
		case <-t.C:
			s.tick()
		}
	}
}

func (s *Sampler) tick() {
	if s.enabled != nil && !s.enabled() {
		s.disable()
		return
	}
	r, err := s.open()
	if err != nil {
		log.Printf("metrics: %v (history disabled until it recovers)", err)
		return
	}

	cur := system.ReadCounters(s.dataRoot)

	s.mu.Lock()
	prev := s.prev
	s.prev = &cur
	s.mu.Unlock()

	if prev == nil {
		return // first reading is a baseline, not a sample
	}
	elapsed := cur.At.Sub(prev.At)
	// A gap much larger than a step means the sampler was off, the box was
	// suspended, or the toggle was flipped back on. Averaging across it would
	// smear whatever happened in between over a single minute, so the reading is
	// dropped and the next one starts from this baseline. The missing slots stay
	// empty, which the graph renders as the gap it was.
	if elapsed <= 0 || elapsed > 3*s.step {
		return
	}

	secs := elapsed.Seconds()
	rec := Record{
		At:    cur.At,
		Mem:   cur.MemPercent,
		Load1: cur.Load1,
		Swap:  cur.SwapPercent,
		Disk:  cur.DiskPercent,
	}
	if dt := cur.CPUTotal - prev.CPUTotal; dt > 0 {
		rec.CPU = (cur.CPUBusy - prev.CPUBusy) / dt * 100
	}
	rec.NetRx = delta(cur.NetRx, prev.NetRx, secs)
	rec.NetTx = delta(cur.NetTx, prev.NetTx, secs)
	rec.DiskRead = delta(cur.DiskRead, prev.DiskRead, secs)
	rec.DiskWrite = delta(cur.DiskWrite, prev.DiskWrite, secs)

	if err := r.Append(rec); err != nil {
		log.Printf("metrics: append: %v", err)
	}
}

// delta turns two cumulative counters into a rate, treating a counter that went
// backwards (an interface recreated, a device re-enumerated) as no traffic rather
// than as a negative rate.
func delta(now, was uint64, secs float64) float64 {
	if now < was || secs <= 0 {
		return 0
	}
	return float64(now-was) / secs
}

// open returns the ring, opening it on first use.
func (s *Sampler) open() (*Ring, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ring != nil {
		return s.ring, nil
	}
	r, err := Open(s.path, s.step, s.slots)
	if err != nil {
		return nil, err
	}
	s.ring = r
	return r, nil
}

// disable flushes and releases the file. Turning history off should stop costing
// anything, including the open descriptor.
func (s *Sampler) disable() {
	s.mu.Lock()
	r := s.ring
	s.ring = nil
	s.prev = nil
	s.mu.Unlock()
	if r != nil {
		r.Close()
	}
}

// Read returns the history in [from, to].
//
// It opens an existing ring the sampler has not needed yet, so a recording that is
// switched off still shows what it captured before. It does NOT create one: with
// history disabled, opening the page must not put a file on the user's disk, which
// is precisely what "off" is supposed to mean.
func (s *Sampler) Read(from, to time.Time) ([]Record, error) {
	s.mu.Lock()
	open := s.ring != nil
	s.mu.Unlock()
	if !open {
		if _, err := os.Stat(s.path); err != nil {
			return nil, nil
		}
	}
	r, err := s.open()
	if err != nil {
		return nil, err
	}
	return r.Read(from, to)
}

// Bytes is how much disk the history currently occupies, for the settings page.
func (s *Sampler) Bytes() int64 {
	s.mu.Lock()
	r := s.ring
	s.mu.Unlock()
	if r == nil {
		return 0
	}
	return r.Bytes()
}

// Retention is the window the ring can hold.
func (s *Sampler) Retention() time.Duration { return s.step * time.Duration(s.slots) }

// Step is the resolution of a stored point.
func (s *Sampler) Step() time.Duration { return s.step }

// Delete discards the recorded history. The next enabled tick recreates the file.
func (s *Sampler) Delete() error {
	s.mu.Lock()
	r := s.ring
	s.ring = nil
	s.prev = nil
	s.mu.Unlock()
	if r != nil {
		return r.Delete()
	}
	// Not open: remove the file directly rather than opening one just to delete it,
	// which would create it first on a deployment that has never recorded anything.
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Close flushes any staged records.
func (s *Sampler) Close() {
	s.disable()
}
