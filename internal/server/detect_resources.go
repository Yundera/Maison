package server

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yundera/maison/internal/incident"
	"github.com/yundera/maison/internal/metrics"
	"github.com/yundera/maison/internal/system"
)

// Resource starvation: a box that has been short of CPU, memory or disk for long
// enough that it is the box's normal, not a spike.
//
// Three of these rules are judged over the recorded host history rather than over a
// reading of their own. That is what keeps them free: the history recorder is
// already the one thing measuring the host with nobody watching, and a rule about
// "the last 24 hours" needs those 24 hours to have been recorded by someone. The
// consequence is the switch: with history off these three are not judged at all, and
// any that were open are resolved, because an alert nothing will ever re-evaluate
// is an alert that never clears. The settings page says so next to the switch.
//
// They judge pressure (PSI) before percentages. A box at 90% CPU running a niced
// transcode is doing its job; one where tasks spend a quarter of their time queued
// for a core is not, and only the second is worth an email. Percentages are the
// fallback for kernels without PSI.
//
// OOM kills are the exception: an event, not a level, read from Docker's event
// stream and /proc/vmstat, and judged whether history is on or not.

const (
	// minCoverage is the share of a window that must have been recorded before it is
	// judged either way. A window Maison was down for half of has no evidence, and
	// treating the gap as calm would close an alert it never saw resolve.
	minCoverage = 0.8

	// CPU: open on a day of sustained stall, clear on six calm hours. The open rule
	// also requires the last six hours not to be calm, or a day-long average would
	// keep re-opening an alert the clear rule had just closed.
	cpuOpenWindow      = 24 * time.Hour
	cpuClearWindow     = 6 * time.Hour
	cpuPressureWarnAt  = 25.0
	cpuPressureClearAt = 10.0
	cpuBusyWarnAt      = 90.0 // fallback without PSI
	cpuBusyClearAt     = 75.0

	// Memory: stalls show up within the hour, so the pressure window is short. "full"
	// — every task waiting on memory at once — is thrashing, and critical. Used memory
	// already excludes reclaimable cache (MemAvailable), so 90% of it for six hours is
	// a box that is genuinely short, PSI or not.
	memPressureWindow  = time.Hour
	memUsedWindow      = 6 * time.Hour
	memPressureWarnAt  = 10.0
	memFullCriticalAt  = 10.0
	memPressureClearAt = 2.0
	memUsedWarnAt      = 90.0
	memUsedClearAt     = 85.0

	// Disk trend: a straight line through a week of the data disk, full within a
	// week. Three days at least, so one large download is an event, not a trend; 50%
	// at least, so a nearly empty disk filling fast is not news yet.
	diskTrendWindow    = 7 * 24 * time.Hour
	diskTrendMinSpan   = 72 * time.Hour
	diskTrendWarnDays  = 7.0
	diskTrendClearDays = 14.0
	diskTrendMinUsed   = 50.0

	// oomQuiet is how long an OOM incident stays open after the last kill.
	oomQuiet = 24 * time.Hour

	// topApps is how many apps an alert names as the likely cause.
	topApps = 3
)

// verdict is what one rule concludes from one pass.
type verdict int

const (
	judgeNone  verdict = iota // not enough evidence, or between thresholds: leave it be
	judgeOpen                 // report (or keep reporting)
	judgeClear                // resolve
)

// cpuJudgement is a CPU verdict and the figure it was reached on.
type cpuJudgement struct {
	verdict  verdict
	avg      float64 // over cpuOpenWindow
	pressure bool    // judged on PSI rather than on busy %
}

// judgeCPU applies the CPU rule to the recorded history. Pure.
func judgeCPU(recs []metrics.Record, now time.Time, step time.Duration) cpuJudgement {
	psi := func(r metrics.Record) float64 { return r.CPUPressure }
	busy := func(r metrics.Record) float64 { return r.CPU }

	field, warnAt, clearAt, pressure := psi, cpuPressureWarnAt, cpuPressureClearAt, true
	// PSI is used when the kernel provides it, judged on the short window so a box
	// that only just gained Maison is not stuck on the fallback for a day.
	if _, cov := metrics.Mean(recs, psi, now.Add(-cpuClearWindow), now, step); cov == 0 {
		field, warnAt, clearAt, pressure = busy, cpuBusyWarnAt, cpuBusyClearAt, false
	}

	long, longCov := metrics.Mean(recs, field, now.Add(-cpuOpenWindow), now, step)
	short, shortCov := metrics.Mean(recs, field, now.Add(-cpuClearWindow), now, step)
	j := cpuJudgement{avg: long, pressure: pressure}
	switch {
	case shortCov >= minCoverage && short < clearAt:
		j.verdict = judgeClear
	case longCov >= minCoverage && long >= warnAt && shortCov >= minCoverage:
		j.verdict = judgeOpen
	}
	return j
}

// memJudgement is a memory verdict and what it was reached on.
type memJudgement struct {
	verdict  verdict
	critical bool
	pressure float64 // memory some, over memPressureWindow; -1 when unavailable
	used     float64 // percent, over memUsedWindow
}

// judgeMemory applies the memory rule to the recorded history. Pure.
func judgeMemory(recs []metrics.Record, now time.Time, step time.Duration) memJudgement {
	from := now.Add(-memPressureWindow)
	some, someCov := metrics.Mean(recs, func(r metrics.Record) float64 { return r.MemPressure }, from, now, step)
	full, fullCov := metrics.Mean(recs, func(r metrics.Record) float64 { return r.MemFullPressure }, from, now, step)
	used, usedCov := metrics.Mean(recs, func(r metrics.Record) float64 { return r.Mem }, now.Add(-memUsedWindow), now, step)

	hasPSI := someCov >= minCoverage
	j := memJudgement{pressure: metrics.NoPressure, used: used}
	if hasPSI {
		j.pressure = some
	}
	stalled := hasPSI && some >= memPressureWarnAt
	thrashing := fullCov >= minCoverage && full >= memFullCriticalAt
	short := usedCov >= minCoverage && used >= memUsedWarnAt

	switch {
	case stalled || thrashing || short:
		j.verdict, j.critical = judgeOpen, thrashing
	case usedCov >= minCoverage && used < memUsedClearAt:
		// Without PSI the used share decides alone; with it, both must be calm. A
		// kernel that has PSI but recorded too little of this hour says nothing yet.
		if someCov == 0 || (hasPSI && some < memPressureClearAt) {
			j.verdict = judgeClear
		}
	}
	return j
}

// trendJudgement is a disk-trend verdict.
type trendJudgement struct {
	verdict verdict
	days    float64
	used    float64
}

// judgeDiskTrend applies the fill-date rule to a week of recorded history. Pure.
func judgeDiskTrend(recs []metrics.Record) trendJudgement {
	if len(recs) < 2 || recs[len(recs)-1].At.Sub(recs[0].At) < diskTrendMinSpan {
		return trendJudgement{}
	}
	used := recs[len(recs)-1].Disk
	days, growing := metrics.DiskTrend(recs, diskTrendMinSpan)
	switch {
	case growing && days <= diskTrendWarnDays && used >= diskTrendMinUsed:
		return trendJudgement{verdict: judgeOpen, days: days, used: used}
	case !growing || days > diskTrendClearDays:
		return trendJudgement{verdict: judgeClear, days: days, used: used}
	}
	return trendJudgement{days: days, used: used}
}

// checkResources runs the history-based rules and the OOM watch.
func (s *Server) checkResources(ctx context.Context, d *detector) {
	s.checkOOM(ctx, d, time.Now())

	if !s.settings.HistoryEnabled() {
		for _, id := range []string{incident.KindHostCPU, incident.KindHostMemory, incident.KindDiskTrend} {
			s.incidents.Resolve(id)
		}
		return
	}
	now := time.Now()
	recs, err := s.history.Read(now.Add(-diskTrendWindow), now)
	if err != nil {
		return
	}
	step := s.history.Step()

	if j := judgeCPU(recs, now, step); j.verdict == judgeClear {
		s.incidents.Resolve(incident.KindHostCPU)
	} else if j.verdict == judgeOpen {
		s.incidents.Report(s.cpuReport(ctx, j, now))
	}

	if j := judgeMemory(recs, now, step); j.verdict == judgeClear {
		s.incidents.Resolve(incident.KindHostMemory)
	} else if j.verdict == judgeOpen {
		s.incidents.Report(s.memReport(ctx, j, now))
	}

	// A disk that is already over the line is disk.full's to report; a forecast
	// beside it says the same thing less usefully.
	if s.anyOpen(incident.KindDiskFull) {
		s.incidents.Resolve(incident.KindDiskTrend)
		return
	}
	if j := judgeDiskTrend(recs); j.verdict == judgeClear {
		s.incidents.Resolve(incident.KindDiskTrend)
	} else if j.verdict == judgeOpen {
		days := max(int(j.days+0.5), 1)
		s.incidents.Report(incident.Report{
			ID: incident.KindDiskTrend, Kind: incident.KindDiskTrend, Severity: incident.Warning,
			Title: fmt.Sprintf("The data disk will be full in about %d days", days),
			Detail: fmt.Sprintf("The disk holding your data is %.0f%% full and has been filling steadily over the past week. "+
				"At that rate it runs out of space in about %d days.\n\n"+
				"Free some space or add storage before then: the Resources page shows what is using it.", j.used, days),
			Args: map[string]string{"days": strconv.Itoa(days), "percent": fmt.Sprintf("%.0f", j.used)},
		})
	}
}

func (s *Server) cpuReport(ctx context.Context, j cpuJudgement, now time.Time) incident.Report {
	what := fmt.Sprintf("Over the last 24 hours, programs spent %.0f%% of the time waiting for a free processor.", j.avg)
	if !j.pressure {
		what = fmt.Sprintf("Over the last 24 hours, the processor was %.0f%% busy on average.", j.avg)
	}
	apps := s.blame(ctx, incident.KindHostCPU, now.Add(-cpuOpenWindow), now,
		func(a metrics.AppShare) float64 { return a.CPU },
		func(a metrics.AppShare) string { return fmt.Sprintf("%.0f%%", a.CPU) })
	return incident.Report{
		ID: incident.KindHostCPU, Kind: incident.KindHostCPU, Severity: incident.Warning,
		Title:  "The processor has been overloaded for a day",
		Detail: what + blameLine(apps, "the processor") + "\n\nApps will feel slow. Stop or reschedule what is busiest, or move to a larger server.",
		Args:   map[string]string{"percent": fmt.Sprintf("%.0f", j.avg), "apps": apps},
	}
}

func (s *Server) memReport(ctx context.Context, j memJudgement, now time.Time) incident.Report {
	sev, title := incident.Warning, "The server is short of memory"
	if j.critical {
		sev, title = incident.Critical, "The server is out of memory"
	}
	what := fmt.Sprintf("Over the last 6 hours, %.0f%% of the memory was in use on average.", j.used)
	if j.pressure >= memPressureWarnAt {
		what = fmt.Sprintf("Over the last hour, programs spent %.0f%% of the time waiting for memory, with %.0f%% of it in use.",
			j.pressure, j.used)
	}
	apps := s.blame(ctx, incident.KindHostMemory, now.Add(-memUsedWindow), now,
		func(a metrics.AppShare) float64 { return a.Mem },
		func(a metrics.AppShare) string { return humanBytes(uint64(a.Mem)) })
	advice := "Apps will slow down and may be killed for lack of memory. Stop an app you do not need, lower one's memory use, or move to a larger server."
	return incident.Report{
		ID: incident.KindHostMemory, Kind: incident.KindHostMemory, Severity: sev,
		Title:  title,
		Detail: what + blameLine(apps, "memory") + "\n\n" + advice,
		Args:   map[string]string{"percent": fmt.Sprintf("%.0f", j.used), "apps": apps},
	}
}

// blame names the apps that used the most over [from, to], as "Jellyfin 61%, Immich
// 22%". Worked out once, when the incident opens: it reads every app's history, which
// is worth doing for an email and not every five minutes after it. A re-assertion
// carries the open incident's list forward.
func (s *Server) blame(ctx context.Context, id string, from, to time.Time,
	key func(metrics.AppShare) float64, show func(metrics.AppShare) string) string {
	if inc, ok := s.openIncident(id); ok {
		return inc.Args["apps"]
	}
	if s.apps == nil || s.appHistory == nil || !s.appHistory.Recording() {
		return ""
	}
	list, err := s.apps.List(ctx)
	if err != nil {
		return ""
	}
	names := map[string]string{}
	byApp := map[string][]metrics.AppRecord{}
	for _, a := range list {
		recs, _, err := s.appHistory.Read(a.ID, from, to)
		if err != nil || len(recs) == 0 {
			continue
		}
		names[a.ID] = a.Name
		byApp[a.ID] = recs
	}
	var parts []string
	for _, a := range metrics.TopApps(byApp, topApps, key) {
		if key(a) <= 0 {
			continue
		}
		parts = append(parts, names[a.ID]+" "+show(a))
	}
	return strings.Join(parts, ", ")
}

func blameLine(apps, what string) string {
	if apps == "" {
		return ""
	}
	return " The apps using the most " + what + ": " + apps + "."
}

func (s *Server) anyOpen(kind string) bool {
	for _, inc := range s.incidents.Snapshot().Open {
		if inc.Kind == kind {
			return true
		}
	}
	return false
}

// oomTally counts OOM kills per compose project between detector passes. It is fed
// from the Docker event stream, which runs on its own goroutine.
type oomTally struct {
	mu     sync.Mutex
	counts map[string]int
}

func (t *oomTally) record(project string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.counts == nil {
		t.counts = map[string]int{}
	}
	t.counts[project]++
}

// drain returns the kills since the last call and starts a new count.
func (t *oomTally) drain() map[string]int {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := t.counts
	t.counts = nil
	return out
}

// oomState is what the OOM watch carries from one pass to the next.
type oomState struct {
	vmstat    uint64
	hasVmstat bool
	last      map[string]time.Time // incident ID -> last kill
	count     map[string]int       // incident ID -> kills since it opened
}

// checkOOM reports apps (and the host) that the kernel has been killing for memory.
//
// An OOM kill is the one memory problem that is never ambiguous, and the one owners
// most often misread: in an app's logs it looks like the app crashing. It is an
// event rather than a level, so the incident stays open until a day has passed with
// no new kill.
//
// Kills in an app's containers come from Docker's "oom" event, which names the
// compose project. The host's own counter (/proc/vmstat oom_kill) catches the rest —
// a process outside every app — once the kills Docker already attributed are taken
// off it.
func (s *Server) checkOOM(ctx context.Context, d *detector, now time.Time) {
	st := &d.oom
	if st.last == nil {
		st.last, st.count = map[string]time.Time{}, map[string]int{}
		// Pick up incidents opened before a restart, so they still close a day after
		// their last kill rather than staying open forever.
		for _, inc := range s.incidents.Snapshot().Open {
			if inc.Kind == incident.KindAppOOM || inc.Kind == incident.KindHostOOM {
				st.last[inc.ID] = inc.Updated
				st.count[inc.ID], _ = strconv.Atoi(inc.Args["count"])
			}
		}
	}

	kills := s.oom.drain()
	attributed := 0
	var names map[string]string
	for project, n := range kills {
		attributed += n
		if project == "" {
			continue // a container outside compose: not an app, counted by the host below
		}
		if names == nil {
			names = s.appNames(ctx)
		}
		name := names[project]
		if name == "" {
			name = project
		}
		id := incident.KindAppOOM + ":" + project
		st.count[id] += n
		st.last[id] = now
		s.incidents.Report(incident.Report{
			ID: id, Kind: incident.KindAppOOM, Severity: incident.Warning,
			Title: name + " ran out of memory",
			Detail: fmt.Sprintf("The system has stopped part of %s %s because it ran out of memory. "+
				"From its logs this looks like the app crashing.\n\n"+
				"Give it a higher memory limit in its settings, or stop something else on the server.",
				name, times(st.count[id])),
			Args: map[string]string{"app": name, "count": strconv.Itoa(st.count[id])},
		})
	}

	if n, ok := system.ReadOOMKills(); ok {
		if st.hasVmstat && n > st.vmstat {
			if outside := int(n-st.vmstat) - attributed; outside > 0 {
				id := incident.KindHostOOM
				st.count[id] += outside
				st.last[id] = now
				s.incidents.Report(incident.Report{
					ID: id, Kind: incident.KindHostOOM, Severity: incident.Warning,
					Title: "The server ran out of memory",
					Detail: fmt.Sprintf("The system has stopped a program outside your apps %s because the server ran out of memory.\n\n"+
						"Stop an app you do not need, or move to a larger server.", times(st.count[id])),
					Args: map[string]string{"count": strconv.Itoa(st.count[id])},
				})
			}
		}
		st.vmstat, st.hasVmstat = n, true
	}

	for id, at := range st.last {
		if now.Sub(at) >= oomQuiet {
			s.incidents.Resolve(id)
			delete(st.last, id)
			delete(st.count, id)
		}
	}
}

func (s *Server) appNames(ctx context.Context) map[string]string {
	out := map[string]string{}
	if s.apps == nil {
		return out
	}
	list, err := s.apps.List(ctx)
	if err != nil {
		return out
	}
	for _, a := range list {
		out[a.ID] = a.Name
	}
	return out
}

func times(n int) string {
	if n == 1 {
		return "once"
	}
	return strconv.Itoa(n) + " times"
}
