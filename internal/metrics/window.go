package metrics

import (
	"sort"
	"time"
)

// The arithmetic the resource alerts run over recorded history. Pure, so every
// threshold above it is testable without loading a box for a day.

// Mean averages field over the records in [from, to] and reports how much of the
// window was actually recorded.
//
// Coverage is the share of the window's steps that hold a usable sample (0-1).
// Values below zero are NoPressure and are skipped, so a pressure series on a kernel
// without PSI has zero coverage — which a caller reads as "cannot judge", not as
// "fine". That distinction is the point: a box Maison was not running on for half
// the day has no evidence either way, and a rule that treated the gap as calm would
// close an alert it never saw resolve.
func Mean(recs []Record, field func(Record) float64, from, to time.Time, step time.Duration) (avg, coverage float64) {
	if step <= 0 || !to.After(from) {
		return 0, 0
	}
	var sum float64
	n := 0
	for _, r := range recs {
		if r.At.Before(from) || r.At.After(to) {
			continue
		}
		v := field(r)
		if v < 0 {
			continue
		}
		sum += v
		n++
	}
	if n == 0 {
		return 0, 0
	}
	return sum / float64(n), min(float64(n)/float64(to.Sub(from)/step), 1)
}

// DiskTrend projects when the data disk fills, from a least-squares line through the
// recorded disk percentage.
//
// It returns the days until 100% at the fitted rate, measured from the latest
// reading, and false when there is no honest answer: under minSpan of history, or a
// disk that is not growing. A fit over a single noisy afternoon would predict a full
// disk from one large download, so the span floor is what keeps this an alert about
// a trend rather than about an event.
func DiskTrend(recs []Record, minSpan time.Duration) (days float64, ok bool) {
	if len(recs) < 2 {
		return 0, false
	}
	first, last := recs[0], recs[len(recs)-1]
	if last.At.Sub(first.At) < minSpan {
		return 0, false
	}
	// x in days from the first sample keeps the sums well-conditioned.
	var sx, sy, sxx, sxy float64
	n := float64(len(recs))
	for _, r := range recs {
		x := r.At.Sub(first.At).Hours() / 24
		sx += x
		sy += r.Disk
		sxx += x * x
		sxy += x * r.Disk
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return 0, false
	}
	slope := (n*sxy - sx*sy) / den // percent per day
	if slope <= 0 {
		return 0, false
	}
	return max(100-last.Disk, 0) / slope, true
}

// AppShare is one app's average use over a window.
type AppShare struct {
	ID  string
	CPU float64 // percent of the whole host
	Mem float64 // bytes
}

// TopApps averages each app's records and returns the top n by key, highest first.
// Apps that recorded nothing in the window are left out.
func TopApps(byApp map[string][]AppRecord, n int, key func(AppShare) float64) []AppShare {
	out := make([]AppShare, 0, len(byApp))
	for id, recs := range byApp {
		if len(recs) == 0 {
			continue
		}
		s := AppShare{ID: id}
		for _, r := range recs {
			s.CPU += r.CPU
			s.Mem += r.Mem
		}
		s.CPU /= float64(len(recs))
		s.Mem /= float64(len(recs))
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if a, b := key(out[i]), key(out[j]); a != b {
			return a > b
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}
