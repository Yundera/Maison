package metrics

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// base is a step-aligned instant, so a test never straddles a bucket boundary by
// accident.
var base = time.Unix(1_700_000_000/60*60, 0)

func openRing(t *testing.T, slots int) (*Ring, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "metrics.ring")
	r, err := Open(path, time.Minute, slots)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r, path
}

// appendMinutes writes one record per minute starting at base+offset, with CPU set
// to the minute index so every record is identifiable.
func appendMinutes(t *testing.T, r *Ring, offset, n int) {
	t.Helper()
	for i := offset; i < offset+n; i++ {
		rec := Record{At: base.Add(time.Duration(i) * time.Minute), CPU: float64(i)}
		if err := r.Append(rec); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := r.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
}

func TestReadReturnsWhatWasAppended(t *testing.T) {
	r, _ := openRing(t, 60)
	appendMinutes(t, r, 0, 10)

	got, err := r.Read(base, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 10 {
		t.Fatalf("got %d records, want 10", len(got))
	}
	for i, rec := range got {
		if rec.CPU != float64(i) {
			t.Errorf("record %d: CPU = %v, want %d", i, rec.CPU, i)
		}
		if !rec.At.Equal(base.Add(time.Duration(i) * time.Minute)) {
			t.Errorf("record %d: At = %v, out of order", i, rec.At)
		}
	}
}

// The staged batch has to be visible to a read, or the graph would lag the live
// gauges by up to five minutes for no reason a user could understand.
func TestReadSeesStagedRecordsBeforeTheyAreFlushed(t *testing.T) {
	r, _ := openRing(t, 60)
	// Fewer than flushEvery, so nothing has reached the file yet.
	if err := r.Append(Record{At: base, CPU: 42}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := r.Read(base, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 1 || got[0].CPU != 42 {
		t.Fatalf("got %+v, want the staged record", got)
	}
}

// A lap of the ring must overwrite the oldest slot, and the survivors must be the
// newest window — not a mixture of two laps.
func TestWrapKeepsTheNewestWindow(t *testing.T) {
	const slots = 10
	r, _ := openRing(t, slots)
	appendMinutes(t, r, 0, 25) // two and a half laps

	got, err := r.Read(base, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != slots {
		t.Fatalf("got %d records, want %d (one full ring)", len(got), slots)
	}
	// Minutes 15..24 are the last lap.
	for i, rec := range got {
		want := float64(15 + i)
		if rec.CPU != want {
			t.Errorf("record %d: CPU = %v, want %v", i, rec.CPU, want)
		}
	}
}

// Downtime is the case the whole timestamp-per-record design exists for: the slots
// a stopped Maison did not write still hold an older lap's samples, and those must
// not be served as if they were recent.
func TestStaleSlotsFromAnEarlierLapAreNotReturned(t *testing.T) {
	const slots = 10
	r, _ := openRing(t, slots)

	appendMinutes(t, r, 0, 10) // fill the ring
	// ...then a gap of a full lap, and three fresh samples.
	appendMinutes(t, r, 20, 3)

	from := base.Add(20 * time.Minute)
	got, err := r.Read(from, from.Add(time.Hour))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d records, want 3 — stale slots leaked into the window", len(got))
	}
	for i, rec := range got {
		if want := float64(20 + i); rec.CPU != want {
			t.Errorf("record %d: CPU = %v, want %v", i, rec.CPU, want)
		}
	}
}

func TestWindowNarrowerThanTheRingReadsOnlyThatWindow(t *testing.T) {
	r, _ := openRing(t, 60)
	appendMinutes(t, r, 0, 30)

	from, to := base.Add(10*time.Minute), base.Add(14*time.Minute)
	got, err := r.Read(from, to)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 5 { // inclusive of both ends
		t.Fatalf("got %d records, want 5", len(got))
	}
	if got[0].CPU != 10 || got[4].CPU != 14 {
		t.Errorf("window = %v..%v, want 10..14", got[0].CPU, got[4].CPU)
	}
}

// A window that wraps the physical end of the file is two reads internally; it
// must still come back as one ordered series.
func TestWindowSpanningTheWrapPointIsContiguous(t *testing.T) {
	const slots = 10
	r, _ := openRing(t, slots)
	appendMinutes(t, r, 0, 14) // slots 0..9 then 0..3 again

	from, to := base.Add(8*time.Minute), base.Add(13*time.Minute)
	got, err := r.Read(from, to)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 6 {
		t.Fatalf("got %d records, want 6", len(got))
	}
	for i, rec := range got {
		if want := float64(8 + i); rec.CPU != want {
			t.Errorf("record %d: CPU = %v, want %v", i, rec.CPU, want)
		}
	}
}

func TestReopenKeepsHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.ring")
	r, err := Open(path, time.Minute, 60)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	appendMinutes(t, r, 0, 10)
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := Open(path, time.Minute, 60)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()
	got, err := again.Read(base, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 10 {
		t.Fatalf("got %d records after reopen, want 10", len(got))
	}
}

// A file written under a different geometry must be discarded, not reinterpreted:
// the bytes would decode into plausible nonsense.
func TestGeometryChangeResetsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.ring")
	r, err := Open(path, time.Minute, 60)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	appendMinutes(t, r, 0, 10)
	r.Close()

	again, err := Open(path, time.Minute, 120) // different slot count
	if err != nil {
		t.Fatalf("reopen with new geometry: %v", err)
	}
	defer again.Close()
	got, err := again.Read(base, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d records, want 0 — the old layout was read back", len(got))
	}
}

func TestCorruptHeaderIsRecreatedRatherThanFailing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.ring")
	if err := os.WriteFile(path, []byte("not a ring file at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Open(path, time.Minute, 60)
	if err != nil {
		t.Fatalf("Open over a corrupt file: %v", err)
	}
	defer r.Close()
	appendMinutes(t, r, 0, 3)
	got, err := r.Read(base, base.Add(time.Hour))
	if err != nil || len(got) != 3 {
		t.Fatalf("got %d records (err %v), want 3", len(got), err)
	}
}

// The file is a hole until it is written: a fresh install must not pay for the
// full 30 days up front, which is what the settings page reports.
func TestFreshRingIsSparse(t *testing.T) {
	r, _ := openRing(t, DefaultSlots)
	if n := r.Bytes(); n > 128*1024 {
		t.Errorf("fresh ring occupies %d bytes on disk, want a sparse file", n)
	}
}

func TestDeleteRemovesTheFile(t *testing.T) {
	r, path := openRing(t, 60)
	appendMinutes(t, r, 0, 6)
	if err := r.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file still present after Delete (err %v)", err)
	}
}

// Values are stored lossily on purpose; the loss must be bounded and must never
// wrap a large value round to a small one.
func TestEncodingRoundTripAndSaturation(t *testing.T) {
	for _, c := range []struct {
		name       string
		in, wantLo float64
		wantHi     float64
	}{
		{"typical", 37.42, 37.41, 37.43},
		{"zero", 0, 0, 0},
		{"negative clamps to zero", -5, 0, 0},
		{"full scale", 100, 99.99, 100.01},
		// A load average past the uint16 ceiling pins to the top rather than
		// wrapping to near-zero, which would read as an idle box under load.
		{"over scale saturates", 5000, 655, 656},
	} {
		t.Run(c.name, func(t *testing.T) {
			var b [recordSize]byte
			encode(&b, Record{At: base, CPU: c.in, Load1: c.in})
			got, ok := decode(b[:])
			if !ok {
				t.Fatal("decode reported an unwritten slot")
			}
			if got.CPU < c.wantLo || got.CPU > c.wantHi {
				t.Errorf("CPU round-trip = %v, want within [%v, %v]", got.CPU, c.wantLo, c.wantHi)
			}
		})
	}
}

func TestRatesSurviveTheirDynamicRange(t *testing.T) {
	var b [recordSize]byte
	const gigabit = 125_000_000.0
	encode(&b, Record{At: base, NetRx: gigabit, DiskWrite: 1.5})
	got, _ := decode(b[:])
	// float32 keeps ~7 significant digits; a 0.01% tolerance is well inside that.
	if d := got.NetRx/gigabit - 1; d > 1e-4 || d < -1e-4 {
		t.Errorf("NetRx = %v, want ~%v", got.NetRx, gigabit)
	}
	if got.DiskWrite != 1.5 {
		t.Errorf("DiskWrite = %v, want 1.5", got.DiskWrite)
	}
}

// An untouched slot is a hole, not a sample from the epoch.
func TestZeroTimestampIsAnUnwrittenSlot(t *testing.T) {
	if _, ok := decode(make([]byte, recordSize)); ok {
		t.Error("an all-zero slot decoded as a real record")
	}
}

func TestHeaderRecordsTheGeometry(t *testing.T) {
	_, path := openRing(t, 123)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b[:len(magic)]) != magic {
		t.Errorf("magic = %q, want %q", b[:len(magic)], magic)
	}
	if got := binary.LittleEndian.Uint32(b[14:18]); got != 123 {
		t.Errorf("slots in header = %d, want 123", got)
	}
	if want := int64(headerSize + 123*recordSize); int64(len(b)) != want {
		t.Errorf("file length = %d, want %d", len(b), want)
	}
}
