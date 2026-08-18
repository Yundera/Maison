// Package retention decides which backups may be deleted.
//
// It is a pure function over a list and a policy: no I/O, no engine, no clock of its
// own. That is the point. Retention is the one part of a backup system whose bugs are
// invisible until the day someone needs the thing it deleted, so it is the part that
// has to be testable without a repository, a container or a night passing.
//
// Who *applies* the plan differs by engine. An engine with its own policy engine is
// handed the intent and expires snapshots itself — kopia's policies live inside the
// repository and therefore outlive a Maison reinstall, which is a property worth more
// than uniformity. An engine without one (a mirror, a directory of archives) is
// expired by Maison, through this planner and the provider's Delete. Both paths read
// the same resolved configuration, so the user is promised one thing either way.
//
// See docs/backup.md.
package retention

import (
	"fmt"
	"sort"
	"time"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/backupconfig"
)

// Plan splits list into what survives and what may be deleted, newest first in both.
//
// It is conservative wherever it is unsure: an unparseable stamp is never dropped, an
// engine whose storage model is unknown expires nothing, and the newest backup is
// kept unconditionally whatever the policy says. The cost of keeping too much is a
// bill; the cost of dropping too much is the reason the system exists.
func Plan(now time.Time, list []apps.Backup, model apps.RetentionModel, r backupconfig.Resolved) (keep, drop []apps.Backup) {
	if len(list) == 0 {
		return nil, nil
	}

	items := make([]item, 0, len(list))
	for _, b := range list {
		it := item{b: b}
		if t, err := time.Parse(apps.StampLayout, b.Stamp); err == nil {
			it.t, it.dated = t, true
		}
		items = append(items, it)
	}
	// Newest first. The stamp layout is fixed-width and lexicographically ordered, so
	// a string sort is a chronological one — and it also orders the stamps that did
	// not parse, which have to land somewhere deterministic.
	sort.Slice(items, func(i, j int) bool { return items[i].b.Stamp > items[j].b.Stamp })

	kept := make([]bool, len(items))
	switch {
	// Nothing is ever expired: an engine that has not declared how its history is
	// shaped, and storage that expires itself under a bucket lifecycle rule. In the
	// second case the bucket is applying the age; deleting objects underneath it would
	// be Maison and the storage both acting on the same policy.
	case model == apps.RetentionNone, model == apps.RetentionLifecycle:
		setAll(kept, true)

	case r.Mode == backupconfig.ModeAll, r.Mode == backupconfig.ModeInherit:
		// ModeInherit cannot reach here through Config.Effective, which always resolves
		// to a real mode. It can reach here through a zero-valued Resolved, and the
		// honest reading of "no policy" is "delete nothing".
		setAll(kept, true)

	case r.Mode == backupconfig.ModeCount:
		for i := range items {
			kept[i] = i < max(r.Count, 1)
		}

	case r.Mode == backupconfig.ModeAge:
		cutoff := now.Add(-r.MaxAge)
		for i, it := range items {
			kept[i] = !it.dated || !it.t.Before(cutoff)
		}

	default: // ModeSmart and ModeCustom, both of which are tiers
		tiers(items, r.Keep, kept)
	}

	// Unparseable stamps are never candidates. The stamp is how every other part of
	// Maison names a backup, so one that does not parse is a backup this code does not
	// understand — and deleting what you do not understand is not a retention policy.
	for i, it := range items {
		if !it.dated {
			kept[i] = true
		}
	}

	// There is always at least one remaining backup. Every consumer tool states this
	// and every user assumes it, and it is the one guarantee that costs nothing to
	// hold: a policy that keeps zero is a policy that has deleted the user's history
	// on the night their app broke.
	kept[0] = true

	if model == apps.RetentionChain {
		onlyTail(kept)
	}

	for i, it := range items {
		if kept[i] {
			keep = append(keep, it.b)
		} else {
			drop = append(drop, it.b)
		}
	}
	return keep, drop
}

type item struct {
	b     apps.Backup
	t     time.Time
	dated bool
}

// tiers is grandfather-father-son selection, and it counts *periods that contain a
// backup* rather than sliding a window back from now.
//
// The difference matters on a PCS, which is a machine that gets switched off. Under a
// window, a box that was away for three weeks comes back to find its dailies expired
// by the passage of time alone — the tier is empty and the history is gone. Counting
// occupied periods instead keeps the last 7 days it actually has, whenever they were.
// It is also what kopia's own policy engine does, so an engine that self-expires and
// one that Maison expires agree.
func tiers(items []item, k backupconfig.Keep, kept []bool) {
	for i := range items {
		if i >= k.Latest {
			break
		}
		kept[i] = true
	}
	for _, t := range []struct {
		n   int
		key func(time.Time) string
	}{
		{k.Daily, func(t time.Time) string { return t.Format("2006-01-02") }},
		{k.Weekly, func(t time.Time) string { y, w := t.ISOWeek(); return fmt.Sprintf("%04d-W%02d", y, w) }},
		{k.Monthly, func(t time.Time) string { return t.Format("2006-01") }},
		{k.Annual, func(t time.Time) string { return t.Format("2006") }},
	} {
		if t.n <= 0 {
			continue
		}
		seen, last := 0, ""
		for i, it := range items {
			if !it.dated {
				continue
			}
			key := t.key(it.t)
			if key == last {
				continue // an older backup from a period already represented
			}
			if seen >= t.n {
				break // items are newest first, so every remaining period is older still
			}
			last, seen = key, seen+1
			kept[i] = true // the newest backup in the period is the one that represents it
		}
	}
}

// onlyTail reduces a keep/drop decision to the oldest contiguous run of drops.
//
// On chained storage — a mirror plus a dated --backup-dir — a generation is only
// meaningful together with every generation newer than it, because restoring an old
// state means overlaying them in order. Deleting one from the middle does not cost
// that generation, it costs every restore point behind it. So a tiered plan, which is
// full of middle deletions, is reinterpreted as "keep everything newer than the oldest
// backup the tiers wanted to keep" and only the tail beyond it is truncated.
//
// This is deliberately a demotion rather than a refusal: the user asked for a bound on
// history, and truncating the tail honours it as far as the storage allows. What it
// must never do is delete something the storage cannot afford to lose.
func onlyTail(kept []bool) {
	last := -1
	for i, k := range kept {
		if k {
			last = i
		}
	}
	for i := 0; i <= last; i++ {
		kept[i] = true
	}
}

func setAll(b []bool, v bool) {
	for i := range b {
		b[i] = v
	}
}
