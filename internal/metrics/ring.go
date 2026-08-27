// Package metrics keeps the dashboard's host-utilisation history.
//
// The history is a fixed-size binary ring FILE, not an in-memory buffer. That is
// the whole design: Maison must not grow its resident set for a feature nobody is
// looking at, so the 30 days of samples live on disk and only the handful of
// records still waiting to be written are ever in RAM.
//
// The file is written with WriteAt at a computed offset rather than mmap'd. An
// mmap would count every touched page against RSS, which is exactly what this
// package exists to avoid; a pwrite leaves the data in the page cache, where the
// kernel owns it.
//
// There is deliberately no database. A fixed-width record and a round-robin slot
// give O(1) append, a size known at creation, and no schema, migration or
// dependency — and, because every record carries its own timestamp, no load on
// boot and no flush on shutdown either. See Ring for what that buys.
package metrics

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

const (
	// magic identifies the file and its byte layout in one go. Bump the trailing
	// digit rather than adding a compatibility path: the file is a cache of
	// samples, and throwing it away costs the user history, not data.
	magic = "MSNRING1"

	headerSize = 64
	recordSize = 32

	// DefaultStep and DefaultSlots give 30 days at one-minute resolution:
	// 43,200 records x 32 B = 1.32 MiB, plus the header.
	DefaultStep  = time.Minute
	DefaultSlots = 30 * 24 * 60

	// flushEvery is how many records are staged before they are written. Five
	// minutes of staging is 160 B of RAM and turns five pwrites into one; an
	// ungraceful stop loses at most that much, and loses it as a gap, which the
	// format already renders honestly.
	flushEvery = 5
)

// Record is one bucket of host utilisation.
//
// Percentages are stored as uint16 hundredths and rates as float32, which is what
// makes the record fixed-width — and fixed width is what lets a slot be addressed
// arithmetically instead of looked up. The cost is that per-interface and
// per-device detail cannot live here; history carries the summed rates, and the
// live channel carries the breakdown.
type Record struct {
	At        time.Time
	CPU       float64 // percent of all cores, 0-100
	Mem       float64 // percent
	Load1     float64
	Swap      float64 // percent
	Disk      float64 // percent, the filesystem backing the data root
	NetRx     float64 // bytes/sec, summed over real interfaces
	NetTx     float64
	DiskRead  float64 // bytes/sec, summed over real block devices
	DiskWrite float64
}

// Ring is a round-robin file of Records.
//
// A record's slot is derived from its timestamp (slot = bucket % slots), so a
// write never has to read anything first, and a lap of the ring overwrites the
// oldest sample without bookkeeping. Each record repeats its own timestamp, which
// is what makes downtime free: a slot left behind by a previous lap simply carries
// a timestamp outside the requested window, and Read drops it. Nothing has to be
// loaded at startup or flushed at shutdown, and a gap in the data renders as a gap
// in the graph rather than as a lie.
type Ring struct {
	path  string
	step  time.Duration
	slots int

	mu      sync.Mutex
	f       *os.File
	pending []Record
}

// Open opens (or creates) the ring at path. A file whose header does not match
// the requested geometry is recreated: the contents are samples, so re-deriving
// them costs history rather than data, and that is much safer than trying to
// reinterpret bytes written under a different layout.
func Open(path string, step time.Duration, slots int) (*Ring, error) {
	if step <= 0 || slots <= 0 {
		return nil, fmt.Errorf("metrics: bad geometry step=%v slots=%d", step, slots)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	r := &Ring{path: path, step: step, slots: slots}

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	r.f = f

	ok, err := r.headerMatches()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !ok {
		if err := r.reset(); err != nil {
			f.Close()
			return nil, err
		}
	}
	return r, nil
}

// headerMatches reports whether the open file already carries this geometry. A
// short file (including a freshly created empty one) is not an error, just a
// mismatch.
func (r *Ring) headerMatches() (bool, error) {
	buf := make([]byte, headerSize)
	n, err := r.f.ReadAt(buf, 0)
	if err != nil && n < headerSize {
		return false, nil // too short: treat as uninitialised
	}
	if string(buf[:len(magic)]) != magic {
		return false, nil
	}
	return binary.LittleEndian.Uint16(buf[8:10]) == recordSize &&
		binary.LittleEndian.Uint32(buf[10:14]) == uint32(r.step/time.Second) &&
		binary.LittleEndian.Uint32(buf[14:18]) == uint32(r.slots), nil
}

// reset rewrites the header and truncates the file to its full length. The body is
// left as a hole — the file is sparse until the slots are actually written, so a
// fresh install pays for a header, not for 1.32 MiB.
func (r *Ring) reset() error {
	buf := make([]byte, headerSize)
	copy(buf, magic)
	binary.LittleEndian.PutUint16(buf[8:10], recordSize)
	binary.LittleEndian.PutUint32(buf[10:14], uint32(r.step/time.Second))
	binary.LittleEndian.PutUint32(buf[14:18], uint32(r.slots))

	if err := r.f.Truncate(0); err != nil {
		return err
	}
	if _, err := r.f.WriteAt(buf, 0); err != nil {
		return err
	}
	return r.f.Truncate(int64(headerSize + r.slots*recordSize))
}

// bucket is the step-aligned start of the interval t falls in.
func (r *Ring) bucket(t time.Time) int64 {
	s := int64(r.step / time.Second)
	return t.Unix() / s * s
}

// slotOf maps a bucket start to its offset in the ring.
func (r *Ring) slotOf(bucketStart int64) int {
	s := int64(r.step / time.Second)
	n := bucketStart / s % int64(r.slots)
	if n < 0 {
		n += int64(r.slots)
	}
	return int(n)
}

// Append stages a record, writing the staged batch once it is full. The record's
// timestamp is snapped to its bucket so that the slot it lands in and the
// timestamp it carries always agree.
func (r *Ring) Append(rec Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	rec.At = time.Unix(r.bucket(rec.At), 0)
	r.pending = append(r.pending, rec)
	if len(r.pending) < flushEvery {
		return nil
	}
	return r.flushLocked()
}

// Flush writes any staged records immediately.
func (r *Ring) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.flushLocked()
}

// flushLocked writes the staged records, coalescing runs of adjacent slots into a
// single WriteAt. Samples arrive in time order, so in the steady state the whole
// batch is one run and one syscall; a wrap of the ring splits it in two.
func (r *Ring) flushLocked() error {
	if len(r.pending) == 0 || r.f == nil {
		return nil
	}
	type placed struct {
		slot int
		buf  []byte
	}
	out := make([]placed, 0, len(r.pending))
	for _, rec := range r.pending {
		var b [recordSize]byte
		encode(&b, rec)
		out = append(out, placed{slot: r.slotOf(rec.At.Unix()), buf: b[:]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].slot < out[j].slot })

	for i := 0; i < len(out); {
		j := i + 1
		for j < len(out) && out[j].slot == out[j-1].slot+1 {
			j++
		}
		run := make([]byte, 0, (j-i)*recordSize)
		for _, p := range out[i:j] {
			run = append(run, p.buf...)
		}
		if _, err := r.f.WriteAt(run, int64(headerSize+out[i].slot*recordSize)); err != nil {
			r.pending = nil // drop rather than retry forever on a broken file
			return err
		}
		i = j
	}
	r.pending = nil
	return nil
}

// Read returns the records whose timestamps fall in [from, to], oldest first,
// including any still staged. Slots left over from an earlier lap carry a
// timestamp outside the window and are skipped, which is how downtime becomes a
// gap instead of stale data.
func (r *Ring) Read(from, to time.Time) ([]Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil, nil
	}
	if to.Before(from) {
		from, to = to, from
	}

	// Read only the span the window covers, unless it covers the whole ring.
	step := int64(r.step / time.Second)
	buckets := (r.bucket(to)-r.bucket(from))/step + 1
	var (
		start int
		count int
	)
	if buckets >= int64(r.slots) {
		start, count = 0, r.slots
	} else {
		start, count = r.slotOf(r.bucket(from)), int(buckets)
	}

	recs := make([]Record, 0, count+len(r.pending))
	// Two reads at most: the span, and its wrapped remainder.
	for _, seg := range [][2]int{{start, min(count, r.slots - start)}, {0, count - min(count, r.slots-start)}} {
		if seg[1] <= 0 {
			continue
		}
		buf := make([]byte, seg[1]*recordSize)
		if _, err := r.f.ReadAt(buf, int64(headerSize+seg[0]*recordSize)); err != nil {
			return nil, err
		}
		for off := 0; off+recordSize <= len(buf); off += recordSize {
			rec, ok := decode(buf[off : off+recordSize])
			if !ok || rec.At.Before(from) || rec.At.After(to) {
				continue
			}
			recs = append(recs, rec)
		}
	}
	for _, rec := range r.pending {
		if !rec.At.Before(from) && !rec.At.After(to) {
			recs = append(recs, rec)
		}
	}

	sort.Slice(recs, func(i, j int) bool { return recs[i].At.Before(recs[j].At) })
	return recs, nil
}

// Bytes is how much disk the ring actually occupies. The file is sparse until its
// slots have been written, so this reports allocated blocks rather than the
// nominal length — the settings page shows this number, and a fresh install
// claiming 1.32 MiB it has not used would be a lie.
func (r *Ring) Bytes() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return 0
	}
	fi, err := r.f.Stat()
	if err != nil {
		return 0
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return fi.Size()
}

// Close flushes and closes the file.
func (r *Ring) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.flushLocked()
	if cerr := r.f.Close(); err == nil {
		err = cerr
	}
	r.f = nil
	return err
}

// Delete closes the ring and removes the file — the settings page's "delete
// history" action. The ring is unusable afterwards; the caller reopens it if the
// user turns history back on.
func (r *Ring) Delete() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending = nil
	if r.f != nil {
		r.f.Close()
		r.f = nil
	}
	if err := os.Remove(r.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ============================================================
// Wire format
// ============================================================

// pct encodes a percentage as hundredths, saturating rather than wrapping: a load
// average above 655 is pinned to the top of the scale, which is wrong by less than
// showing 0.05 would be.
func pct(v float64) uint16 {
	if !(v > 0) { // also catches NaN
		return 0
	}
	if v > math.MaxUint16/100 {
		return math.MaxUint16
	}
	return uint16(v*100 + 0.5)
}

func unpct(v uint16) float64 { return float64(v) / 100 }

func rate(v float64) float32 {
	if !(v > 0) {
		return 0
	}
	return float32(v)
}

func encode(b *[recordSize]byte, r Record) {
	binary.LittleEndian.PutUint32(b[0:4], uint32(r.At.Unix()))
	binary.LittleEndian.PutUint16(b[4:6], pct(r.CPU))
	binary.LittleEndian.PutUint16(b[6:8], pct(r.Mem))
	binary.LittleEndian.PutUint16(b[8:10], pct(r.Load1))
	binary.LittleEndian.PutUint16(b[10:12], pct(r.Swap))
	binary.LittleEndian.PutUint16(b[12:14], pct(r.Disk))
	// b[14:16] reserved
	binary.LittleEndian.PutUint32(b[16:20], math.Float32bits(rate(r.NetRx)))
	binary.LittleEndian.PutUint32(b[20:24], math.Float32bits(rate(r.NetTx)))
	binary.LittleEndian.PutUint32(b[24:28], math.Float32bits(rate(r.DiskRead)))
	binary.LittleEndian.PutUint32(b[28:32], math.Float32bits(rate(r.DiskWrite)))
}

// decode returns the record and whether the slot was ever written. A zero
// timestamp is an untouched slot, not a sample from 1970.
func decode(b []byte) (Record, bool) {
	ts := binary.LittleEndian.Uint32(b[0:4])
	if ts == 0 {
		return Record{}, false
	}
	return Record{
		At:        time.Unix(int64(ts), 0),
		CPU:       unpct(binary.LittleEndian.Uint16(b[4:6])),
		Mem:       unpct(binary.LittleEndian.Uint16(b[6:8])),
		Load1:     unpct(binary.LittleEndian.Uint16(b[8:10])),
		Swap:      unpct(binary.LittleEndian.Uint16(b[10:12])),
		Disk:      unpct(binary.LittleEndian.Uint16(b[12:14])),
		NetRx:     float64(math.Float32frombits(binary.LittleEndian.Uint32(b[16:20]))),
		NetTx:     float64(math.Float32frombits(binary.LittleEndian.Uint32(b[20:24]))),
		DiskRead:  float64(math.Float32frombits(binary.LittleEndian.Uint32(b[24:28]))),
		DiskWrite: float64(math.Float32frombits(binary.LittleEndian.Uint32(b[28:32]))),
	}, true
}
