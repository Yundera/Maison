package server

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yundera/maison/internal/incident"
	"github.com/yundera/maison/internal/system"
)

func mount(dev, at string, pct float64) system.FilesystemStat {
	return system.FilesystemStat{
		Device: dev, Mountpoint: at, LocalPath: at,
		SizeBytes: 100 << 30, AvailBytes: uint64((100 - pct) / 100 * float64(100<<30)),
		UsedPercent: pct,
	}
}

// The gap between the warn and clear thresholds is the whole point: without it a disk
// sitting on the line opens and closes an incident all day, and every edge is an email.
func TestDiskHysteresisLeavesTheMiddleBandAlone(t *testing.T) {
	for _, c := range []struct {
		pct                 float64
		wantOpen, wantClear bool
	}{
		{50, false, true},   // comfortably fine: clear it if it was open
		{84.9, false, true}, // just under the clear threshold
		{87, false, false},  // in the gap: whatever it was, it stays
		{89.9, false, false},
		{90, true, false}, // opens
		{99, true, false},
	} {
		open, clear := diskReports(system.Filesystems{Mounts: []system.FilesystemStat{mount("/dev/sda1", "/", c.pct)}})
		if got := len(open) == 1; got != c.wantOpen {
			t.Errorf("at %.1f%%: opened = %v, want %v", c.pct, got, c.wantOpen)
		}
		if got := len(clear) == 1; got != c.wantClear {
			t.Errorf("at %.1f%%: cleared = %v, want %v", c.pct, got, c.wantClear)
		}
	}
}

func TestDiskGoesCriticalBeforeItIsTooLate(t *testing.T) {
	open, _ := diskReports(system.Filesystems{Mounts: []system.FilesystemStat{mount("/dev/sda1", "/", 98)}})
	if len(open) != 1 {
		t.Fatalf("want one incident, got %d", len(open))
	}
	if open[0].Severity != incident.Critical {
		t.Errorf("at 98%% the severity is %q, want critical", open[0].Severity)
	}
	if !strings.Contains(open[0].Detail, "backups will stop working") {
		t.Errorf("a critical disk should say what it will break:\n%s", open[0].Detail)
	}

	open, _ = diskReports(system.Filesystems{Mounts: []system.FilesystemStat{mount("/dev/sda1", "/", 91)}})
	if open[0].Severity != incident.Warning {
		t.Errorf("at 91%% the severity is %q, want warning", open[0].Severity)
	}
}

// On a PCS /DATA is a directory on the root filesystem, not a mount of its own, so one
// full disk appears as several rows. Keying by mountpoint would announce it three
// times, which is the fan-out the register exists to prevent.
func TestOneFullDiskIsOneIncidentHoweverManyMountsShowIt(t *testing.T) {
	open, _ := diskReports(system.Filesystems{Mounts: []system.FilesystemStat{
		mount("/dev/sda1", "/", 93),
		mount("/dev/sda1", "/DATA", 93),
		mount("/dev/sda1", "/var/lib/docker", 94),
	}})
	if len(open) != 1 {
		ids := make([]string, len(open))
		for i, r := range open {
			ids[i] = r.ID
		}
		t.Fatalf("one device produced %d incidents: %v", len(open), ids)
	}
	if open[0].ID != "disk.full:/dev/sda1" {
		t.Errorf("the incident should be keyed by device, got %q", open[0].ID)
	}
	// And it should report the worst reading, not whichever row came first.
	if !strings.Contains(open[0].Title, "94%") {
		t.Errorf("want the worst reading in the title, got %q", open[0].Title)
	}
}

// Separate devices are separate problems and do get separate incidents.
func TestTwoFullDisksAreTwoIncidents(t *testing.T) {
	open, _ := diskReports(system.Filesystems{Mounts: []system.FilesystemStat{
		mount("/dev/sda1", "/", 93),
		mount("/dev/sdb1", "/mnt/media", 96),
	}})
	if len(open) != 2 {
		t.Fatalf("two devices produced %d incidents", len(open))
	}
}

func TestBackupStalenessNeedsBackupsToBeOnAndToHaveRunBefore(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	long := now.Add(-5 * 24 * time.Hour)
	recent := now.Add(-2 * time.Hour)

	for _, c := range []struct {
		name     string
		at       time.Time
		ok       bool
		enabled  bool
		want     bool
		whyNot   string
		explains string
	}{
		{name: "on, and days since the last run", at: long, ok: true, enabled: true, want: true},
		{name: "on, and it ran this morning", at: recent, ok: true, enabled: true, want: false},
		{name: "switched off", at: long, ok: true, enabled: false, want: false,
			whyNot: "backups nobody asked for cannot be late"},
		// A box with no record of a run also has no record of when backups were switched
		// on, so "never" and "enabled a minute ago" are the same observation. Alerting
		// would greet every new box with a warning.
		{name: "on, but has never completed a run", at: time.Time{}, ok: false, enabled: true, want: false,
			whyNot: "a brand new box would otherwise alert immediately"},
	} {
		if got := backupIsStale(c.at, c.ok, c.enabled, now); got != c.want {
			t.Errorf("%s: stale = %v, want %v %s", c.name, got, c.want, c.whyNot)
		}
	}

	// The boundary itself.
	if backupIsStale(now.Add(-backupStaleAfter+time.Minute), true, true, now) {
		t.Error("a run just inside the window should not be stale")
	}
	if !backupIsStale(now.Add(-backupStaleAfter-time.Minute), true, true, now) {
		t.Error("a run just outside the window should be stale")
	}
}

// An app is briefly unhealthy every time it restarts. An alert that fires on that is an
// alert that gets filtered, and then the real one is invisible too.
func TestAConditionMustHoldForTwoPassesBeforeItCounts(t *testing.T) {
	d := &detector{streak: map[string]int{}, seen: map[string]bool{}}

	if d.settled("app.unhealthy:jellyfin", true) {
		t.Error("one observation should not be enough")
	}
	if !d.settled("app.unhealthy:jellyfin", true) {
		t.Error("two consecutive observations should settle")
	}
	// Recovering resets the count, so a flapping app has to hold the condition again.
	if d.settled("app.unhealthy:jellyfin", false) {
		t.Error("a recovered condition should not report")
	}
	if d.settled("app.unhealthy:jellyfin", true) {
		t.Error("the streak should have reset when the condition cleared")
	}
}

// Without pruning the streak map is a slow leak, and an app that is uninstalled and
// reinstalled would inherit its old streak and alert on the very first pass.
func TestAStreakDoesNotOutliveTheThingItWasCounting(t *testing.T) {
	d := &detector{streak: map[string]int{}, seen: map[string]bool{}}
	d.settled("app.partial:immich", true)

	// A pass in which immich is not observed at all — it has been uninstalled.
	d.seen = map[string]bool{}
	d.prune()

	if len(d.streak) != 0 {
		t.Fatalf("streaks survived the thing they were about: %v", d.streak)
	}
	d.seen = map[string]bool{}
	if d.settled("app.partial:immich", true) {
		t.Error("a reinstalled app inherited its old streak and alerted immediately")
	}
}

func TestHumanBytesReadsLikeSomethingAPersonWouldSay(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"512", "512 B"},
		{"1536", "1.5 KiB"},
		{"1073741824", "1.0 GiB"},
	} {
		var n uint64
		for _, r := range c.in {
			n = n*10 + uint64(r-'0')
		}
		if got := humanBytes(n); got != c.want {
			t.Errorf("humanBytes(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Every kind a detector or reporter can raise needs a label in the dashboard, and the
// mute list is built from this set. A kind added without one renders as its own key.
func TestEveryKindTheServerRaisesIsKnown(t *testing.T) {
	known := []string{
		incident.KindBackupFailed, incident.KindBackupStale, incident.KindBackupEngine,
		incident.KindDiskFull, incident.KindAppUnhealthy, incident.KindAppPartial,
		incident.KindAppCrashLoop, incident.KindAppInstall, incident.KindAppUpdate,
		incident.KindAppStackup, incident.KindStoreSource, incident.KindTest,
	}
	open, _ := diskReports(system.Filesystems{Mounts: []system.FilesystemStat{mount("/dev/sda1", "/", 95)}})
	for _, r := range open {
		if !slices.Contains(known, r.Kind) {
			t.Errorf("diskReports raised an unknown kind %q", r.Kind)
		}
	}
}
