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

	// Pressure shares, averaged over the samples that had them. Null when none
	// did — a kernel without PSI is not a box where nothing ever waited.
	CPUPressure *float64 `json:"cpu_pressure"`
	MemPressure *float64 `json:"mem_pressure"`
	IOPressure  *float64 `json:"io_pressure"`

	cpuP, memP, ioP pressureSum
}

// pressureSum averages a pressure series, skipping NoPressure samples.
type pressureSum struct {
	sum float64
	n   int
}

func (p *pressureSum) add(v float64) {
	if v >= 0 {
		p.sum += v
		p.n++
	}
}

func (p pressureSum) mean() *float64 {
	if p.n == 0 {
		return nil
	}
	v := p.sum / float64(p.n)
	return &v
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
	s.cpuP.add(r.CPUPressure)
	s.memP.add(r.MemPressure)
	s.ioP.add(r.IOPressure)
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
	s.CPUPressure, s.MemPressure, s.IOPressure = s.cpuP.mean(), s.memP.mean(), s.ioP.mean()
	return s
}

// AppSpan is one bucket of an app's downsampled usage, with the same envelope rule
// as Span: CPU and memory carry min/avg/max, the IO rates their mean.
type AppSpan struct {
	At         time.Time `json:"at"`
	CPU        MinAvgMax `json:"cpu"`
	Mem        MinAvgMax `json:"mem"`
	DskRd      float64   `json:"disk_read"`
	DskWr      float64   `json:"disk_write"`
	Containers int       `json:"containers"` // the most seen in the bucket
	Points     int       `json:"points"`
}

// DownsampleApp is Downsample for an app's records.
func DownsampleApp(recs []AppRecord, from, to time.Time, n int) []AppSpan {
	if len(recs) == 0 || n <= 0 {
		return []AppSpan{}
	}
	if to.Before(from) {
		from, to = to, from
	}
	width := max(to.Sub(from)/time.Duration(n), time.Second)

	out := make([]AppSpan, 0, min(n, len(recs)))
	var cur *AppSpan
	curIx := -1
	done := func() {
		if cur != nil && cur.Points > 0 {
			k := float64(cur.Points)
			cur.CPU.Avg /= k
			cur.Mem.Avg /= k
			cur.DskRd /= k
			cur.DskWr /= k
			out = append(out, *cur)
		}
	}
	for _, r := range recs {
		ix := min(max(int(r.At.Sub(from)/width), 0), n-1)
		if ix != curIx {
			done()
			curIx = ix
			cur = &AppSpan{
				At:  from.Add(time.Duration(ix) * width),
				CPU: MinAvgMax{Min: r.CPU, Max: r.CPU},
				Mem: MinAvgMax{Min: r.Mem, Max: r.Mem},
			}
		}
		cur.CPU.Avg += r.CPU
		cur.CPU.Min, cur.CPU.Max = min(cur.CPU.Min, r.CPU), max(cur.CPU.Max, r.CPU)
		cur.Mem.Avg += r.Mem
		cur.Mem.Min, cur.Mem.Max = min(cur.Mem.Min, r.Mem), max(cur.Mem.Max, r.Mem)
		cur.DskRd += r.DiskRead
		cur.DskWr += r.DiskWrite
		cur.Containers = max(cur.Containers, r.Containers)
		cur.Points++
	}
	done()
	return out
}
