package kopia

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/yundera/maison/internal/apps"
)

// Reading kopia's progress output.
//
// This is the only kopia-specific part of Maison's progress reporting, and it is
// deliberately the *whole* of it: everything downstream — the rate, the ETA, the
// smoothing, the gating — is computed from apps.Event by apps.Tracker and knows
// nothing about this engine. A second engine reimplements this file and nothing else.
//
// Nothing here may fail a backup. A kopia release that restyles its output, adds a
// field, or changes its units must degrade to "no numbers this line", which the
// Tracker already handles as the ordinary indeterminate case — kopia itself spends
// its opening seconds there while it estimates the tree.

// sizeExpr matches a kopia byte quantity: "16 B", "100.8 MB", "1.2 GiB".
const sizeExpr = `[0-9]+(?:\.[0-9]+)?\s*[KMGTP]?i?B`

var (
	// The percentage kopia computes for itself, e.g. "(80.1%)".
	pctRe = regexp.MustCompile(`\(([0-9]+(?:\.[0-9]+)?)%\)`)

	// A snapshot's own accounting, from a line shaped like
	//
	//  | 1 hashing, 0 hashed (100.8 MB), 2 cached (16 B), uploaded 95.6 MB, estimated 125.8 MB (80.1%) 0s left
	//
	// Hashed plus cached is what the percentage above is computed against, so those are
	// the two that are summed. "uploaded" is deliberately not: after deduplication and
	// compression it is a fraction of what was read, and using it as the numerator
	// against "estimated" would put a bar on screen that disagrees with the percentage
	// printed beside it on the same line.
	hashedRe   = regexp.MustCompile(`hashed \((` + sizeExpr + `)\)`)
	cachedRe   = regexp.MustCompile(`cached \((` + sizeExpr + `)\)`)
	estimateRe = regexp.MustCompile(`estimated (` + sizeExpr + `)`)

	// A restore's accounting, which counts entries rather than sources and reads
	// "... (1.1 GB) of 2000 (5.5 GB)". Only the byte quantities are taken; the entry
	// counts are of no use to a bar measured in bytes.
	restoreRe = regexp.MustCompile(`\((` + sizeExpr + `)\)\s+of\s+\S+\s+\((` + sizeExpr + `)\)`)
)

// emitLine turns one line of kopia output into a progress event.
//
// The message is always the raw line, whether or not anything was parsed out of it:
// it is what makes a support log readable, and it is the only thing there is to show
// while kopia is still estimating.
func emitLine(emit func(apps.Event), line string) {
	if emit == nil {
		return
	}
	ev := apps.Event{Pct: apps.PctUnknown, Message: line}
	if m := pctRe.FindStringSubmatch(line); m != nil {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			ev.Pct = v
		}
	}
	ev.Done, ev.Total = bytesOf(line)
	emit(ev)
}

// bytesOf pulls the byte counts out of a progress line, returning zeroes for a line
// that carries none — which includes every line kopia prints before it has finished
// estimating, and every line that is not a progress line at all.
func bytesOf(line string) (done, total int64) {
	if m := estimateRe.FindStringSubmatch(line); m != nil {
		total = parseSize(m[1])
		if h := hashedRe.FindStringSubmatch(line); h != nil {
			done += parseSize(h[1])
		}
		if c := cachedRe.FindStringSubmatch(line); c != nil {
			done += parseSize(c[1])
		}
		return done, total
	}
	if m := restoreRe.FindStringSubmatch(line); m != nil {
		return parseSize(m[1]), parseSize(m[2])
	}
	return 0, 0
}

// parseSize converts one kopia byte quantity to bytes, or 0 if it cannot.
//
// Both unit families are accepted because which one appears is a kopia setting
// (`--units`): SI by default, binary when asked. Guessing wrong would be a silent
// 7% error in every rate and ETA on the box, which is precisely the kind of wrong
// nobody would ever notice.
func parseSize(s string) int64 {
	s = strings.TrimSpace(s)
	i := strings.IndexFunc(s, func(r rune) bool {
		return r != '.' && (r < '0' || r > '9')
	})
	if i <= 0 {
		return 0
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(s[:i]), 64)
	if err != nil {
		return 0
	}
	unit := strings.TrimSpace(s[i:])
	binary := strings.Contains(unit, "i")
	base := 1000.0
	if binary {
		base = 1024
	}
	mult := 1.0
	switch strings.ToUpper(unit[:1]) {
	case "K":
		mult = base
	case "M":
		mult = base * base
	case "G":
		mult = base * base * base
	case "T":
		mult = base * base * base * base
	case "P":
		mult = base * base * base * base * base
	}
	return int64(n * mult)
}
