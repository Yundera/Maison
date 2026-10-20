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

// Codec is the byte layout of one record type. The ring itself only knows slots
// and timestamps; everything about what a record holds lives here, so the host
// history and the per-app history share one implementation of the slot arithmetic,
// the staging and the sparse file, and differ only in what they encode.
//
// Size must be at least 4: every layout starts with the record's own u32 Unix
// timestamp, and a zero timestamp is an unwritten slot.
type Codec[T any] struct {
	// Magic identifies the layout and is exactly eight bytes. Change it (or Size)
	// rather than adding a compatibility path — see the package comment.
	Magic  string
	Size   int
	Encode func(b []byte, v T)
	Decode func(b []byte) (T, bool)
	At     func(v T) time.Time
	SetAt  func(v *T, t time.Time)
}

// hostCodec is the host history's 32-byte layout.
var hostCodec = Codec[Record]{
	Magic:  magic,
	Size:   recordSize,
	Encode: func(b []byte, r Record) { encode((*[recordSize]byte)(b), r) },
	Decode: decode,
	At:     func(r Record) time.Time { return r.At },
	SetAt:  func(r *Record, t time.Time) { r.At = t },
}

// Ring is the host history: a RingOf Records.
type Ring = RingOf[Record]

// RingOf is a round-robin file of records of one Codec.
//
// A record's slot is derived from its timestamp (slot = bucket % slots), so a
// write never has to read anything first, and a lap of the ring overwrites the
// oldest sample without bookkeeping. Each record repeats its own timestamp, which
// is what makes downtime free: a slot left behind by a previous lap simply carries
// a timestamp outside the requested window, and Read drops it. Nothing has to be
// loaded at startup or flushed at shutdown, and a gap in the data renders as a gap
// in the graph rather than as a lie.
type RingOf[T any] struct {
	codec Codec[T]
	path  string
	step  time.Duration
	slots int

	mu      sync.Mutex
	f       *os.File
	pending []T
}

// Open opens (or creates) the ring at path. A file whose header does not match
// the requested geometry is recreated: the contents are samples, so re-deriving
// them costs history rather than data, and that is much safer than trying to
// reinterpret bytes written under a different layout.
func Open(path string, step time.Duration, slots int) (*Ring, error) {
	return OpenOf(path, step, slots, hostCodec)
}

// OpenOf opens (or creates) a ring of codec's records at path, with the same
// reset-on-mismatch rule as Open.
func OpenOf[T any](path string, step time.Duration, slots int, codec Codec[T]) (*RingOf[T], error) {
	if len(codec.Magic) != 8 || codec.Size < 4 {
		return nil, fmt.Errorf("metrics: bad codec magic=%q size=%d", codec.Magic, codec.Size)
	}
	if step <= 0 || slots <= 0 {
		return nil, fmt.Errorf("metrics: bad geometry step=%v slots=%d", step, slots)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	r := &RingOf[T]{codec: codec, path: path, step: step, slots: slots}

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
func (r *RingOf[T]) headerMatches() (bool, error) {
	buf := make([]byte, headerSize)
	n, err := r.f.ReadAt(buf, 0)
	if err != nil && n < headerSize {
		return false, nil // too short: treat as uninitialised
	}
	if string(buf[:len(r.codec.Magic)]) != r.codec.Magic {
		return false, nil
	}
	return binary.LittleEndian.Uint16(buf[8:10]) == uint16(r.codec.Size) &&
		binary.LittleEndian.Uint32(buf[10:14]) == uint32(r.step/time.Second) &&
		binary.LittleEndian.Uint32(buf[14:18]) == uint32(r.slots), nil
}

// reset rewrites the header and truncates the file to its full length. The body is
// left as a hole — the file is sparse until the slots are actually written, so a
// fresh install pays for a header, not for 1.32 MiB.
func (r *RingOf[T]) reset() error {
	buf := make([]byte, headerSize)
	copy(buf, r.codec.Magic)
	binary.LittleEndian.PutUint16(buf[8:10], uint16(r.codec.Size))
	binary.LittleEndian.PutUint32(buf[10:14], uint32(r.step/time.Second))
	binary.LittleEndian.PutUint32(buf[14:18], uint32(r.slots))

	if err := r.f.Truncate(0); err != nil {
		return err
	}
	if _, err := r.f.WriteAt(buf, 0); err != nil {
		return err
	}
	return r.f.Truncate(int64(headerSize + r.slots*r.codec.Size))
}

// bucket is the step-aligned start of the interval t falls in.
func (r *RingOf[T]) bucket(t time.Time) int64 {
	s := int64(r.step / time.Second)
	return t.Unix() / s * s
}

// slotOf maps a bucket start to its offset in the ring.
func (r *RingOf[T]) slotOf(bucketStart int64) int {
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
func (r *RingOf[T]) Append(rec T) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.codec.SetAt(&rec, time.Unix(r.bucket(r.codec.At(rec)), 0))
	r.pending = append(r.pending, rec)
	if len(r.pending) < flushEvery {
		return nil
	}
	return r.flushLocked()
}

// Flush writes any staged records immediately.
func (r *RingOf[T]) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.flushLocked()
}

// flushLocked writes the staged records, coalescing runs of adjacent slots into a
// single WriteAt. Samples arrive in time order, so in the steady state the whole
// batch is one run and one syscall; a wrap of the ring splits it in two.
func (r *RingOf[T]) flushLocked() error {
	if len(r.pending) == 0 || r.f == nil {
		return nil
	}
	size := r.codec.Size
	slots := make([]int, len(r.pending))
	for i, rec := range r.pending {
		slots[i] = r.slotOf(r.codec.At(rec).Unix())
	}
	order := make([]int, len(r.pending))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return slots[order[a]] < slots[order[b]] })
	// One buffer for the batch, in slot order, so a run of adjacent slots is a
	// subslice of it rather than a copy.
	buf := make([]byte, len(order)*size)
	for k, i := range order {
		r.codec.Encode(buf[k*size:(k+1)*size], r.pending[i])
	}

	for i := 0; i < len(order); {
		j := i + 1
		for j < len(order) && slots[order[j]] == slots[order[j-1]]+1 {
			j++
		}
		if _, err := r.f.WriteAt(buf[i*size:j*size], int64(headerSize+slots[order[i]]*size)); err != nil {
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
func (r *RingOf[T]) Read(from, to time.Time) ([]T, error) {
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

	size := r.codec.Size
	recs := make([]T, 0, count+len(r.pending))
	// Two reads at most: the span, and its wrapped remainder.
	for _, seg := range [][2]int{{start, min(count, r.slots-start)}, {0, count - min(count, r.slots-start)}} {
		if seg[1] <= 0 {
			continue
		}
		buf := make([]byte, seg[1]*size)
		if _, err := r.f.ReadAt(buf, int64(headerSize+seg[0]*size)); err != nil {
			return nil, err
		}
		for off := 0; off+size <= len(buf); off += size {
			rec, ok := r.codec.Decode(buf[off : off+size])
			if !ok {
				continue
			}
			if at := r.codec.At(rec); at.Before(from) || at.After(to) {
				continue
			}
			recs = append(recs, rec)
		}
	}
	for _, rec := range r.pending {
		if at := r.codec.At(rec); !at.Before(from) && !at.After(to) {
			recs = append(recs, rec)
		}
	}

	sort.Slice(recs, func(i, j int) bool { return r.codec.At(recs[i]).Before(r.codec.At(recs[j])) })
	return recs, nil
}

// Bytes is how much disk the ring actually occupies. The file is sparse until its
// slots have been written, so this reports allocated blocks rather than the
// nominal length — the settings page shows this number, and a fresh install
// claiming 1.32 MiB it has not used would be a lie.
func (r *RingOf[T]) Bytes() int64 {
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
func (r *RingOf[T]) Close() error {
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
func (r *RingOf[T]) Delete() error {
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
