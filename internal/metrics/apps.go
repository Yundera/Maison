package metrics

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yundera/maison/internal/cgroupstat"
)

// Per-app history: the same ring, one file per app, fed from cgroup counters.
//
// It is the second thing in Maison that measures with nobody looking, and it is
// held to the host sampler's budget. Per tick it reads a few small cgroup files per
// running container — no Docker call (see internal/cgroupstat) — and keeps in
// memory only the previous reading per container and the records not yet written.
// Rings are opened, written and closed once per flush rather than held open, so a
// box with thirty apps does not hold thirty descriptors for a write every five
// minutes.

const (
	appMagic  = "MSNAPPR1"
	diskMagic = "MSNAPPD1"

	// DiskStep and DiskSlots: one size reading a day, kept for ninety days. A
	// folder's size changes slowly and measuring it is a walk, so a finer step would
	// cost a lot and show nothing.
	DiskStep  = 24 * time.Hour
	DiskSlots = 90
)

// AppRecord is one bucket of an app's usage, summed over its running containers.
type AppRecord struct {
	At         time.Time
	CPU        float64 // percent of the whole host, 0-100 across all cores
	Mem        float64 // bytes, page cache deducted
	DiskRead   float64 // bytes/sec
	DiskWrite  float64
	Containers int
}

// appCodec is 32 bytes: u32 ts, u16 cpu hundredths, u16 containers, f32 mem
// bytes, f32 read, f32 write, 12 reserved. Memory as a float32 holds seven
// significant digits — kilobyte precision on a gigabyte, which no graph can show.
var appCodec = Codec[AppRecord]{
	Magic: appMagic,
	Size:  32,
	Encode: func(b []byte, r AppRecord) {
		binary.LittleEndian.PutUint32(b[0:4], uint32(r.At.Unix()))
		binary.LittleEndian.PutUint16(b[4:6], pct(r.CPU))
		binary.LittleEndian.PutUint16(b[6:8], uint16(min(max(r.Containers, 0), math.MaxUint16)))
		binary.LittleEndian.PutUint32(b[8:12], math.Float32bits(rate(r.Mem)))
		binary.LittleEndian.PutUint32(b[12:16], math.Float32bits(rate(r.DiskRead)))
		binary.LittleEndian.PutUint32(b[16:20], math.Float32bits(rate(r.DiskWrite)))
	},
	Decode: func(b []byte) (AppRecord, bool) {
		ts := binary.LittleEndian.Uint32(b[0:4])
		if ts == 0 {
			return AppRecord{}, false
		}
		return AppRecord{
			At:         time.Unix(int64(ts), 0),
			CPU:        unpct(binary.LittleEndian.Uint16(b[4:6])),
			Containers: int(binary.LittleEndian.Uint16(b[6:8])),
			Mem:        float64(math.Float32frombits(binary.LittleEndian.Uint32(b[8:12]))),
			DiskRead:   float64(math.Float32frombits(binary.LittleEndian.Uint32(b[12:16]))),
			DiskWrite:  float64(math.Float32frombits(binary.LittleEndian.Uint32(b[16:20]))),
		}, true
	},
	At:    func(r AppRecord) time.Time { return r.At },
	SetAt: func(r *AppRecord, t time.Time) { r.At = t },
}

// DiskRecord is one day's size of an app's data folder.
type DiskRecord struct {
	At    time.Time
	Bytes float64
}

var diskCodec = Codec[DiskRecord]{
	Magic: diskMagic,
	Size:  16,
	Encode: func(b []byte, r DiskRecord) {
		binary.LittleEndian.PutUint32(b[0:4], uint32(r.At.Unix()))
		binary.LittleEndian.PutUint64(b[8:16], math.Float64bits(max(r.Bytes, 0)))
	},
	Decode: func(b []byte) (DiskRecord, bool) {
		ts := binary.LittleEndian.Uint32(b[0:4])
		if ts == 0 {
			return DiskRecord{}, false
		}
		return DiskRecord{At: time.Unix(int64(ts), 0), Bytes: math.Float64frombits(binary.LittleEndian.Uint64(b[8:16]))}, true
	},
	At:    func(r DiskRecord) time.Time { return r.At },
	SetAt: func(r *DiskRecord, t time.Time) { r.At = t },
}

// targetSource is the slice of cgroupstat.Tracker the sampler uses.
type targetSource interface {
	Targets(ctx context.Context) []cgroupstat.Target
	MarkDirty()
}

// counterReader is the slice of cgroupstat.Reader the sampler uses.
type counterReader interface {
	Read(dir string) (cgroupstat.Counters, error)
}

// AppSampler records per-app usage into ${dir}/<id>.ring and per-app folder sizes
// into ${dir}/<id>.disk.ring.
type AppSampler struct {
	dir     string
	step    time.Duration
	slots   int
	enabled func() bool
	cpus    float64

	// src and rd are nil when cgroup counters are unavailable; the sampler then
	// records folder sizes only.
	src targetSource
	rd  counterReader

	mu      sync.Mutex
	prev    map[string]appReading // by container ID
	pending map[string][]AppRecord
	ticks   int
	now     func() time.Time
}

type appReading struct {
	at time.Time
	c  cgroupstat.Counters
}

// NewAppSampler returns a sampler writing under dir. src may be nil (no cgroup
// mount): CPU/memory/IO history is then off and only folder sizes are recorded.
// cpus is the host's core count, which CPU shares are relative to.
func NewAppSampler(dir string, step time.Duration, slots int, cpus int, enabled func() bool, src *cgroupstat.Tracker) *AppSampler {
	s := &AppSampler{
		dir: dir, step: step, slots: slots, enabled: enabled, cpus: float64(max(cpus, 1)),
		prev: map[string]appReading{}, pending: map[string][]AppRecord{}, now: time.Now,
	}
	if src != nil {
		s.src, s.rd = src, cgroupstat.NewReader()
	}
	return s
}

// Recording reports whether usage (not just size) history is being recorded.
func (s *AppSampler) Recording() bool { return s.src != nil }

// Run samples until ctx is cancelled.
func (s *AppSampler) Run(ctx context.Context) {
	if s.src == nil {
		return
	}
	t := time.NewTicker(s.step)
	defer t.Stop()
	s.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			s.Flush()
			return
		case <-t.C:
			s.tick(ctx)
		}
	}
}

func (s *AppSampler) tick(ctx context.Context) {
	if s.enabled != nil && !s.enabled() {
		s.mu.Lock()
		s.prev = map[string]appReading{}
		s.pending = map[string][]AppRecord{}
		s.mu.Unlock()
		return
	}
	now := s.now()
	targets := s.src.Targets(ctx)

	type sum struct {
		rec  AppRecord
		seen bool
	}
	byApp := make(map[string]*sum, len(targets))
	next := make(map[string]appReading, len(targets))

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, tg := range targets {
		c, err := s.rd.Read(tg.Dir)
		if err != nil {
			s.src.MarkDirty() // stopped or recreated since the index: rebuild next tick
			continue
		}
		next[tg.ID] = appReading{at: now, c: c}
		a := byApp[tg.Project]
		if a == nil {
			a = &sum{rec: AppRecord{At: now}}
			byApp[tg.Project] = a
		}
		a.rec.Mem += float64(c.MemUsage())
		a.rec.Containers++

		p, ok := s.prev[tg.ID]
		elapsed := now.Sub(p.at)
		// No baseline (first sight, or a container recreated by an update under a
		// new ID), or a gap far past a step: this reading is a baseline only, so the
		// rates are not smeared across time nobody measured.
		if !ok || elapsed <= 0 || elapsed > 3*s.step {
			continue
		}
		a.seen = true
		secs := elapsed.Seconds()
		if c.CPUUsec >= p.c.CPUUsec {
			a.rec.CPU += float64(c.CPUUsec-p.c.CPUUsec) / (secs * 1e6 * s.cpus) * 100
		}
		a.rec.DiskRead += delta(c.IORead, p.c.IORead, secs)
		a.rec.DiskWrite += delta(c.IOWrite, p.c.IOWrite, secs)
	}
	s.prev = next

	for id, a := range byApp {
		if a.seen { // every container a baseline: nothing measured yet
			s.pending[id] = append(s.pending[id], a.rec)
		}
	}
	s.ticks++
	if s.ticks%flushEvery == 0 {
		s.flushLocked()
	}
}

// Flush writes every staged record.
func (s *AppSampler) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked()
}

func (s *AppSampler) flushLocked() {
	for id, recs := range s.pending {
		if err := s.write(id, recs); err != nil {
			log.Printf("metrics: app %s: %v", id, err)
		}
	}
	s.pending = map[string][]AppRecord{}
}

func (s *AppSampler) write(id string, recs []AppRecord) error {
	if len(recs) == 0 || !validID(id) {
		return nil
	}
	r, err := OpenOf(s.usagePath(id), s.step, s.slots, appCodec)
	if err != nil {
		return err
	}
	for _, rec := range recs {
		if err := r.Append(rec); err != nil {
			r.Close()
			return err
		}
	}
	return r.Close()
}

// RecordDisk appends one folder-size reading per app.
func (s *AppSampler) RecordDisk(sizes map[string]int64, at time.Time) {
	if s.enabled != nil && !s.enabled() {
		return
	}
	for id, n := range sizes {
		if !validID(id) {
			continue
		}
		r, err := OpenOf(s.diskPath(id), DiskStep, DiskSlots, diskCodec)
		if err != nil {
			log.Printf("metrics: app %s disk: %v", id, err)
			continue
		}
		if err := r.Append(DiskRecord{At: at, Bytes: float64(n)}); err != nil {
			log.Printf("metrics: app %s disk: %v", id, err)
		}
		r.Close()
	}
}

// Read returns an app's usage records and folder sizes in [from, to]. It never
// creates a file: an app that was never recorded reads as empty.
func (s *AppSampler) Read(id string, from, to time.Time) ([]AppRecord, []DiskRecord, error) {
	if !validID(id) {
		return nil, nil, fmt.Errorf("metrics: bad app id %q", id)
	}
	var usage []AppRecord
	if _, err := os.Stat(s.usagePath(id)); err == nil {
		r, err := OpenOf(s.usagePath(id), s.step, s.slots, appCodec)
		if err != nil {
			return nil, nil, err
		}
		usage, err = r.Read(from, to)
		r.Close()
		if err != nil {
			return nil, nil, err
		}
	}
	// The last few minutes are still staged here, not in the file.
	s.mu.Lock()
	for _, rec := range s.pending[id] {
		if !rec.At.Before(from) && !rec.At.After(to) {
			usage = append(usage, rec)
		}
	}
	s.mu.Unlock()

	var disk []DiskRecord
	if _, err := os.Stat(s.diskPath(id)); err == nil {
		r, err := OpenOf(s.diskPath(id), DiskStep, DiskSlots, diskCodec)
		if err != nil {
			return nil, nil, err
		}
		disk, err = r.Read(from.Add(-DiskStep), to) // the day containing `from` too
		r.Close()
		if err != nil {
			return nil, nil, err
		}
	}
	return usage, disk, nil
}

// Remove discards one app's history — the app was uninstalled.
func (s *AppSampler) Remove(id string) {
	if !validID(id) {
		return
	}
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
	for _, p := range []string{s.usagePath(id), s.diskPath(id)} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Printf("metrics: remove %s: %v", p, err)
		}
	}
}

// Prune removes the history of every app not in keep — a safety net for an
// uninstall whose hook did not run (Maison stopped mid-uninstall, say).
func (s *AppSampler) Prune(keep map[string]bool) {
	for _, id := range s.recorded() {
		if !keep[id] {
			s.Remove(id)
		}
	}
}

// DeleteAll discards every app's history.
func (s *AppSampler) DeleteAll() error {
	s.mu.Lock()
	s.pending = map[string][]AppRecord{}
	s.prev = map[string]appReading{}
	s.mu.Unlock()
	if err := os.RemoveAll(s.dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Bytes is how much disk the per-app history occupies (allocated blocks: the files
// are sparse).
func (s *AppSampler) Bytes() int64 {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return 0
	}
	var n int64
	for _, e := range ents {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			n += st.Blocks * 512
		} else {
			n += info.Size()
		}
	}
	return n
}

// Step is the resolution of a stored usage point.
func (s *AppSampler) Step() time.Duration { return s.step }

// recorded lists the app IDs that have a history file.
func (s *AppSampler) recorded() []string {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range ents {
		name := e.Name()
		id := strings.TrimSuffix(strings.TrimSuffix(name, ".ring"), ".disk")
		if id == name || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func (s *AppSampler) usagePath(id string) string { return filepath.Join(s.dir, id+".ring") }
func (s *AppSampler) diskPath(id string) string  { return filepath.Join(s.dir, id+".disk.ring") }

// validID keeps an app ID — a compose project name, and on the read path a URL
// parameter — from naming a file outside dir.
func validID(id string) bool {
	if id == "" || id == "." || id == ".." || strings.HasPrefix(id, ".") {
		return false
	}
	return !strings.ContainsAny(id, `/\`)
}
