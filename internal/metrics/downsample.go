package metrics

import "time"

// Span is one bucket of a downsampled series.
//
// CPU and memory carry a min/avg/max envelope; everything else carries the mean.
// That asymmetry is deliberate: the record on disk stores only averages, so the
// envelope has to be recovered from the spread of the one-minute samples inside a
// bucket — and it is only worth the extra bytes on the two series a user reads for
// spikes. At a 30-day zoom one bucket is over an hour, and an hourly mean with no
// envelope hides exactly the event the user opened the page to find.
type Span struct {
	At time.Time `json:"at"`

	CPU    MinAvgMax `json:"cpu"`
	Mem    MinAvgMax `json:"mem"`
	Load1  float64   `json:"load1"`
	Swap   float64   `json:"swap"`
	Disk   float64   `json:"disk"`
	NetRx  float64   `json:"net_rx"`
	NetTx  float64   `json:"net_tx"`
	DskRd  float64   `json:"disk_read"`
	DskWr  float64   `json:"disk_write"`
	Points int       `json:"points"`
}

// MinAvgMax is a series' envelope over a bucket.
type MinAvgMax struct {
	Min float64 `json:"min"`
	Avg float64 `json:"avg"`
	Max float64 `json:"max"`
}

// Downsample groups recs into at most n buckets spanning [from, to].
//
// Empty buckets are omitted rather than emitted as zeroes: a box that was off has
// no measurements, and inventing 0% CPU for that hour would draw a plausible flat
// line over a hole. The caller is told the bucket width (see Step) so it can break
// the line wherever consecutive points are further apart than that.
func Downsample(recs []Record, from, to time.Time, n int) []Span {
	if len(recs) == 0 || n <= 0 {
		return []Span{}
	}
	if to.Before(from) {
		from, to = to, from
	}
	total := to.Sub(from)
	if total <= 0 {
		total = time.Second
	}
	width := total / time.Duration(n)
	if width <= 0 {
		width = time.Second
	}

	out := make([]Span, 0, n)
	var (
		cur   *Span
		curIx = -1
	)
	for _, r := range recs {
		ix := int(r.At.Sub(from) / width)
		if ix < 0 {
			ix = 0
		}
		if ix >= n {
			ix = n - 1
		}
		if ix != curIx {
			if cur != nil {
				out = append(out, finish(*cur))
			}
			curIx = ix
			s := Span{At: from.Add(time.Duration(ix) * width)}
			s.CPU = MinAvgMax{Min: r.CPU, Max: r.CPU}
			s.Mem = MinAvgMax{Min: r.Mem, Max: r.Mem}
			cur = &s
		}
		accumulate(cur, r)
	}
	if cur != nil {
		out = append(out, finish(*cur))
	}
	return out
}

// Step is the bucket width Downsample used, which the client needs in order to
// tell a gap in the data from two adjacent buckets.
func Step(from, to time.Time, n int) time.Duration {
	if to.Before(from) {
		from, to = to, from
	}
	if n <= 0 {
		return 0
	}
	return to.Sub(from) / time.Duration(n)
}

func accumulate(s *Span, r Record) {
	s.CPU.Avg += r.CPU
	s.CPU.Min = min(s.CPU.Min, r.CPU)
	s.CPU.Max = max(s.CPU.Max, r.CPU)
	s.Mem.Avg += r.Mem
	s.Mem.Min = min(s.Mem.Min, r.Mem)
	s.Mem.Max = max(s.Mem.Max, r.Mem)
	s.Load1 += r.Load1
	s.Swap += r.Swap
	s.Disk += r.Disk
	s.NetRx += r.NetRx
	s.NetTx += r.NetTx
	s.DskRd += r.DiskRead
	s.DskWr += r.DiskWrite
	s.Points++
}

func finish(s Span) Span {
	n := float64(s.Points)
	if n == 0 {
		return s
	}
	s.CPU.Avg /= n
	s.Mem.Avg /= n
	s.Load1 /= n
	s.Swap /= n
	s.Disk /= n
	s.NetRx /= n
	s.NetTx /= n
	s.DskRd /= n
	s.DskWr /= n
	return s
}
