package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/backup"
	"github.com/yundera/maison/internal/backup/kopia"
	"github.com/yundera/maison/internal/dockerx"
	"github.com/yundera/maison/internal/incident"
	"github.com/yundera/maison/internal/system"
)

// The polled half of the incident register.
//
// Most of what goes wrong on a box is discovered at a call site and pushed (a failed
// install, a failed update, a backup run that did not finish). What is left is the
// class of problem nobody is present for: a disk that fills over a fortnight, a
// schedule that stopped firing, an app that has been restarting since a reboot. Those
// need somebody to look, and this is the thing that looks.

const (
	// checkInterval is how often the box is examined. Nothing here is urgent on a
	// shorter timescale than that — a disk does not go from fine to full in five
	// minutes — and every one of these checks costs either a syscall storm or a
	// daemon round-trip.
	checkInterval = 5 * time.Minute

	// A filesystem opens an incident at 90% and does not clear it until 85%. The gap
	// is deliberate: without it a disk sitting exactly on the threshold would open and
	// close all day, and each edge is an email.
	diskWarnAt     = 90.0
	diskCriticalAt = 97.0
	diskClearAt    = 85.0

	// backupStaleAfter is how long a box with backups switched on may go without a
	// completed run before that is itself the problem. Two days rather than one, so a
	// box that was simply powered off overnight is not an alert.
	backupStaleAfter = 48 * time.Hour

	// settleChecks is how many consecutive passes a condition must hold before it is
	// reported. An app is briefly unhealthy every time it restarts, and an alert that
	// fires on that is an alert nobody reads.
	settleChecks = 2
)

// detector is the state one pass has to carry to the next: how long each condition has
// been continuously true.
type detector struct {
	streak map[string]int
	seen   map[string]bool

	// restarts is each app's total container restart count as of the previous pass.
	// A crash loop is not a state you can observe — a container that has restarted
	// forty times looks exactly like one that restarted forty times last month — so
	// it has to be a difference between two observations.
	restarts map[string]int
}

// runDetectors is the polling loop. It waits a full interval before the first pass,
// which is not laziness: at boot the apps are still coming up, and a check that ran
// immediately would find half of them unhealthy every time Maison restarted.
func (s *Server) runDetectors(ctx context.Context) {
	d := &detector{streak: map[string]int{}, restarts: map[string]int{}}
	t := time.NewTicker(checkInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.checkOnce(ctx, d)
		}
	}
}

func (s *Server) checkOnce(ctx context.Context, d *detector) {
	d.seen = map[string]bool{}
	s.checkDisk()
	s.checkBackup(ctx)
	s.checkApps(ctx, d)
	d.prune()
}

// settled reports whether cond has been true for enough consecutive passes to be worth
// announcing. A pass that does not observe an ID at all leaves its streak alone; see
// prune.
func (d *detector) settled(id string, cond bool) bool {
	d.seen[id] = true
	if !cond {
		delete(d.streak, id)
		return false
	}
	d.streak[id]++
	return d.streak[id] >= settleChecks
}

// prune drops streaks for conditions this pass could not evaluate — an app that was
// uninstalled, a filesystem that was unmounted. Without it the map is a slow leak, and
// worse, a re-appearing app would inherit a stale streak and alert on its first pass.
func (d *detector) prune() {
	for id := range d.streak {
		if !d.seen[id] {
			delete(d.streak, id)
		}
	}
}

// checkDisk watches for a filesystem filling up.
func (s *Server) checkDisk() {
	open, clear := diskReports(system.ReadFilesystems(s.cfg.DataRoot))
	for _, r := range open {
		s.incidents.Report(r)
	}
	for _, id := range clear {
		s.incidents.Resolve(id)
	}
}

// diskReports turns a filesystem table into incidents. Pure, so the thresholds and the
// hysteresis are testable without filling a disk.
//
// KEYED BY DEVICE, not by mountpoint. On a PCS /DATA is a directory on the root
// filesystem rather than a mount of its own, so one full disk shows up as several rows
// of the table — and keying by mountpoint would announce the same problem three times.
//
// A reading between the clear and warn thresholds produces neither a report nor a
// resolve: whatever state the incident is in is the state it stays in, which is what
// makes the gap hysteresis rather than a third threshold.
func diskReports(fs system.Filesystems) (open []incident.Report, clear []string) {
	worst := map[string]system.FilesystemStat{}
	for _, m := range fs.Mounts {
		key := m.Device
		if key == "" {
			key = m.Mountpoint
		}
		if cur, ok := worst[key]; !ok || m.UsedPercent > cur.UsedPercent {
			worst[key] = m
		}
	}
	for key, m := range worst {
		id := "disk.full:" + key
		switch {
		case m.UsedPercent >= diskWarnAt:
			sev := incident.Warning
			advice := "Free some space before it runs out: the Resources page shows what is using it."
			if m.UsedPercent >= diskCriticalAt {
				sev = incident.Critical
				advice = "Apps will start failing to write, and backups will stop working. Free space now — the Resources page shows what is using it."
			}
			open = append(open, incident.Report{
				ID: id, Kind: incident.KindDiskFull, Severity: sev,
				Title: fmt.Sprintf("Disk %s is %.0f%% full", where(m), m.UsedPercent),
				Detail: fmt.Sprintf("%s (%s) is %.0f%% full, with %s free of %s.\n\n%s",
					where(m), m.Device, m.UsedPercent, humanBytes(m.AvailBytes), humanBytes(m.SizeBytes), advice),
				Args: map[string]string{
					"mount":   where(m),
					"percent": fmt.Sprintf("%.0f", m.UsedPercent),
					"free":    humanBytes(m.AvailBytes),
				},
			})
		case m.UsedPercent < diskClearAt:
			clear = append(clear, id)
		}
	}
	return open, clear
}

func where(m system.FilesystemStat) string {
	if m.Mountpoint != "" {
		return m.Mountpoint
	}
	return m.LocalPath
}

func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// checkBackup watches the two things a per-run alert structurally cannot see: a
// schedule that has stopped firing at all, and a repository that has become
// unreachable between runs.
func (s *Server) checkBackup(ctx context.Context) {
	if s.backupSched == nil || s.backupConf == nil {
		return
	}
	enabled := s.backupConf.Get().Enabled
	at, _, ok := s.backupSched.LastRun()

	if backupIsStale(at, ok, enabled, time.Now()) {
		s.incidents.Report(incident.Report{
			ID: incident.IDBackupStale, Kind: incident.KindBackupStale, Severity: incident.Critical,
			Title: "Backups have stopped running",
			Detail: fmt.Sprintf("Backups are switched on, but the last completed run was %s.\n\n"+
				"Nothing new has been backed up since then. Open Settings → Backups and try a run now to see what is wrong.",
				at.Format(time.RFC1123)),
			Args: map[string]string{"since": at.Format(time.RFC1123)},
		})
	} else {
		s.incidents.Resolve(incident.IDBackupStale)
	}

	// A repository nobody is writing to is not a problem worth an alert, so this only
	// looks at engines the box is actually backing up to — and it looks at ALL of them.
	//
	// One incident per engine, keyed "backup.engine:<id>", the same shape "disk.full:sda1"
	// uses. It used to probe a single engine and resolve a bare "backup.engine" whenever
	// that one was not kopia, which under several destinations would clear a real
	// incident for a repository that was genuinely down, because a *different* engine
	// happened to be first.
	for _, id := range engineIDs(s.engines) {
		receives := receivesAnything(s.engines, id)

		// Per-destination staleness. The run-wide check above says the schedule is
		// firing; this says whether it is actually reaching this engine, which is a
		// different question the moment there is more than one. An engine that quietly
		// stopped accepting writes a fortnight ago passes the run-wide check every
		// night, because the run keeps finishing.
		staleID := incident.IDBackupStale + ":" + id
		engineAt, _, engineOK := s.backupSched.LastRunIn(id)
		if receives && backupIsStale(engineAt, engineOK, enabled, time.Now()) {
			s.incidents.Report(incident.Report{
				ID: staleID, Kind: incident.KindBackupStale, Severity: incident.Critical,
				Title: "One backup destination has stopped receiving backups",
				Detail: fmt.Sprintf("The last backup that reached %s was %s.\n\n"+
					"Other destinations may still be working, so this will not show up as a failed run. "+
					"Open Settings → Backups and try a run now to see what is wrong.",
					id, engineAt.Format(time.RFC1123)),
				Args: map[string]string{"engine": id, "since": engineAt.Format(time.RFC1123)},
			})
		} else {
			s.incidents.Resolve(staleID)
		}

		incidentID := "backup.engine:" + id
		p, isRepo := repoProvider(s.engines, id)
		if !enabled || !isRepo || !receives {
			s.incidents.Resolve(incidentID)
			continue
		}
		// Status caches for 30s, so polling it every five minutes costs one real probe.
		if st := p.Status(ctx); !st.Connected {
			s.incidents.Report(incident.Report{
				ID: incidentID, Kind: incident.KindBackupEngine, Severity: incident.Critical,
				Title:  "The backup repository cannot be reached",
				Detail: st.Detail + "\n\nBackups are set to go there but have nowhere to land, so nothing is being saved to it. Open Settings → Backups.",
				Args:   map[string]string{"engine": id},
			})
		} else {
			s.incidents.Resolve(incidentID)
		}
	}
	// The bare ID one boxes carried before this became per-engine. Resolved once so an
	// upgrade does not leave an incident nothing will ever clear.
	s.incidents.Resolve("backup.engine")
}

func engineIDs(set *backup.Set) []string {
	if set == nil {
		return nil
	}
	return set.IDs()
}

// repoProvider is the engine as a kopia repository, when it is one. Only a repository
// can be unreachable — the local engine is the data disk, whose problems are disk.full.
func repoProvider(set *backup.Set, id string) (*kopia.Provider, bool) {
	if set == nil {
		return nil, false
	}
	e, ok := set.Get(id)
	if !ok {
		return nil, false
	}
	p, isKopia := e.(*kopia.Provider)
	return p, isKopia
}

// receivesAnything reports whether any trigger writes to this engine. An engine holding
// only history is not a fault when it cannot be reached — nothing is trying to write.
func receivesAnything(set *backup.Set, id string) bool {
	if set == nil {
		return false
	}
	for _, t := range apps.Triggers {
		for _, p := range set.Writers(t) {
			if p.ID() == id {
				return true
			}
		}
	}
	return false
}

// backupIsStale is the "nobody has backed anything up in days" test, pure so the
// boundary can be tested without waiting two days.
//
// A box that has NEVER completed a run is deliberately not stale. The same reasoning
// the scheduler's missedARun gives applies: with no record of a run there is also no
// record of when backups were switched on, so "never" is indistinguishable from
// "enabled a minute ago" — and alerting on the latter would greet every new box.
func backupIsStale(at time.Time, ok, enabled bool, now time.Time) bool {
	return enabled && ok && now.Sub(at) > backupStaleAfter
}

// checkApps watches for an app that was working and is not any more.
//
// Only two conditions qualify, and the omission is the interesting part. A fully
// STOPPED app raises nothing, because Maison persists no desired state: Registry.Stop
// calls StopProject and that is all, so "stopped" is byte-identical whether the owner
// switched the app off on purpose or every container in it crashed. Alerting on it
// would mail people about apps they turned off themselves, which is the fastest way to
// make every later alert ignorable.
//
// Unhealthy (Docker's own health check says so) and partial (some containers up, some
// down — a deliberate stop takes the whole project down together) are unambiguous.
func (s *Server) checkApps(ctx context.Context, d *detector) {
	if s.apps == nil {
		return
	}
	list, err := s.apps.List(ctx)
	if err != nil {
		return // a daemon we cannot reach is not evidence about any particular app
	}

	live := map[string]bool{}
	for _, a := range list {
		live[a.ID] = true
		// Mid-operation is not a verdict: an app being installed, updated or restarted
		// is expected to look wrong while it happens.
		if a.Installing || a.Busy {
			continue
		}
		// HasReached is the "was working and broke" discriminator — a persisted marker
		// that this app has been observed reachable at least once. Without it a
		// half-configured app that never came up would alert forever, and the owner
		// already knows about that one: they were watching when it failed to install.
		if !s.apps.HasReached(a.ID) {
			continue
		}

		s.appCondition(d, "app.unhealthy:"+a.ID, a.Health == apps.HealthUnhealthy, incident.Report{
			Kind: incident.KindAppUnhealthy, Severity: incident.Warning,
			Title:  a.Name + " is not healthy",
			Detail: a.Name + " is running, but its own health check has been failing.\n\nOpening it may still work. Its logs, from the tile's menu, will say why.",
			Args:   map[string]string{"app": a.Name},
		})
		if a.Status == apps.StatusRunning {
			// Everything is up: whatever it was doing, it has stopped doing it.
			s.incidents.Resolve("app.crashloop:" + a.ID)
		} else {
			s.checkCrashLoop(ctx, d, a)
		}

		s.appCondition(d, "app.partial:"+a.ID, a.Status == apps.StatusPartial, incident.Report{
			Kind: incident.KindAppPartial, Severity: incident.Warning,
			Title:  a.Name + " is only partly running",
			Detail: a.Name + " has containers that are not running while others are.\n\nStopping an app takes all of it down together, so this is something failing rather than something switched off. Restarting it from the tile's menu is the first thing to try.",
			Args:   map[string]string{"app": a.Name},
		})
	}

	for id := range d.restarts {
		if !live[id] {
			delete(d.restarts, id)
		}
	}

	// An app that has been uninstalled takes its incidents with it. Otherwise the badge
	// outlives the app and nothing will ever clear it.
	//
	// KindAppInstall is deliberately absent: an install that failed leaves no app
	// behind, so "not in the list" is that incident's normal state rather than evidence
	// it has been dealt with.
	for _, inc := range s.incidents.Snapshot().Open {
		switch inc.Kind {
		case incident.KindAppUnhealthy, incident.KindAppPartial, incident.KindAppStackup, incident.KindAppUpdate:
			if _, app, ok := strings.Cut(inc.ID, ":"); ok && !live[app] {
				s.incidents.Resolve(inc.ID)
			}
		}
	}
}

// checkCrashLoop reports a container that keeps dying and being brought back.
//
// This is the one app condition that a fully-stopped app can legitimately raise, and
// the reason it escapes the "we cannot tell a crash from a deliberate stop" rule is
// that it is not a state at all: it is Docker's restart counter MOVING between two
// passes five minutes apart. An app the owner switched off has a counter that stands
// still, whatever number it stands at.
//
// The inspect it costs is only ever spent on an app that is already not running, so on
// a healthy box this is free.
func (s *Server) checkCrashLoop(ctx context.Context, d *detector, a apps.App) {
	if s.dx == nil {
		return
	}
	id := "app.crashloop:" + a.ID
	states, err := s.dx.ProjectRunStates(ctx, a.ID)
	if err != nil {
		return
	}
	total, worst, service := 0, dockerx.RunState{}, ""
	for name, st := range states {
		total += st.Restarts
		if st.Restarts >= worst.Restarts {
			worst, service = st, name
		}
	}

	prev, had := d.restarts[a.ID]
	d.restarts[a.ID] = total
	if !had {
		return // first sight of this app: there is nothing yet to have moved
	}
	if total <= prev {
		s.incidents.Resolve(id)
		return
	}

	why := fmt.Sprintf("last exit code %d", worst.ExitCode)
	advice := "Its logs, from the tile's menu, will say why."
	if worst.OOMKilled {
		// Worth naming explicitly: from the logs an OOM kill looks like the
		// application crashing, and the owner debugs the wrong thing for an evening.
		why = "it ran out of memory"
		advice = "Give it a higher memory limit, or stop something else on the box."
	}
	s.incidents.Report(incident.Report{
		ID: id, Kind: incident.KindAppCrashLoop, Severity: incident.Warning,
		Title: a.Name + " keeps restarting",
		Detail: fmt.Sprintf("%s has restarted %d more times in the last few minutes (%s: %s).\n\n%s",
			a.Name, total-prev, service, why, advice),
		Args: map[string]string{"app": a.Name},
	})
}

// appCondition is settle-then-report for one app-level condition. The ID is filled in
// here so that the streak, the report and the resolve can never drift apart.
func (s *Server) appCondition(d *detector, id string, cond bool, r incident.Report) {
	if d.settled(id, cond) {
		r.ID = id
		s.incidents.Report(r)
		return
	}
	if !cond {
		s.incidents.Resolve(id)
	}
}
