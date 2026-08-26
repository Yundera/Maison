package backup

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/backupconfig"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/notify"
)

// Kind distinguishes the two things a run backs up. They are genuinely different —
// an app has a compose project and containers to stop, user data has neither — and
// collapsing them into one would push a pseudo-app name through guards written for
// real project names.
type Kind string

const (
	KindApp      Kind = "app"
	KindUserData Kind = "userdata"
)

// Target is one thing a run backs up.
type Target struct {
	Kind Kind
	App  string // compose project; empty for user data
}

// ID is the target's stable identifier, namespaced so that an app called
// "userdata" cannot collide with the user-data set.
func (t Target) ID() string {
	if t.Kind == KindUserData {
		return "userdata"
	}
	return "app:" + t.App
}

// Target statuses, in the order a target passes through them.
const (
	StatusPending = "pending"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
	// StatusSkipped is a target the run deliberately did not attempt, which is not
	// the same as one that failed and must not be reported as one. The two cases are
	// a user-data set that a restore is currently rewriting, and an app somebody is
	// already backing up by hand — in both, the right thing happened.
	//
	// The distinction is not cosmetic: a failure mails the operator "backups are
	// failing on your server", and doing that for a box where nothing is wrong is how
	// a useful alert becomes a filter rule.
	StatusSkipped = "skipped"
)

// skipError marks a target that was deliberately not attempted. The reason still
// reaches the user — it is shown on the row — it simply does not count as a failure.
type skipError struct{ reason string }

func (e skipError) Error() string { return e.reason }

// skip builds one. Exported behaviour, unexported type: nothing outside this package
// decides what counts as a skip.
func skip(format string, args ...any) error {
	return skipError{reason: fmt.Sprintf(format, args...)}
}

func isSkip(err error) bool {
	var s skipError
	return errors.As(err, &s)
}

// TargetState is one target's place in a run: where it is, and — while it is the
// one running — how it is getting on.
//
// The whole list is built before the first target starts, which is the point of it.
// A run used to be a single string naming whatever was in flight, so until it
// finished there was no way to know whether it was a quarter done or nearly there,
// and the only thing on screen was a compose project name. Knowing the plan up front
// turns that into "3 of 9", a checklist of what is coming, and — because the failures
// stay in the list rather than being counted — a record of what went wrong that is
// still readable when the run ends.
//
// Deliberately no display name: resolving one means asking Docker for every app on
// the box, and the dashboard already holds the names and icons it renders elsewhere.
// The identity travels; the presentation stays where presentation belongs.
type TargetState struct {
	ID   string `json:"id"`             // "app:jellyfin", or "userdata"
	Kind Kind   `json:"kind"`           // app | userdata
	App  string `json:"app,omitempty"`  // compose project; empty for user data
	Name string `json:"name,omitempty"` // the backup this produced, once it has

	Status string `json:"status"` // pending | running | done | failed
	Err    string `json:"error,omitempty"`

	// Live progress, meaningful while Status is running. Phase is the engine-agnostic
	// step (apps.PhaseCopy, PhaseSync, …) and is what makes "the app is stopped right
	// now" visible; the rest is what apps.Tracker derived from whatever the engine
	// reported. Zero means not known — for Pct that is PctUnknown, since 0% is a real
	// answer that must not read as "no idea".
	Phase   string  `json:"phase,omitempty"`
	Message string  `json:"message,omitempty"`
	Pct     float64 `json:"pct"`
	Done    int64   `json:"done,omitempty"`
	Total   int64   `json:"total,omitempty"`
	Rate    float64 `json:"rate,omitempty"`
	ETA     int     `json:"eta,omitempty"`

	Started  time.Time `json:"started,omitempty"`
	Finished time.Time `json:"finished,omitempty"`
}

// RunState is a snapshot of the current or last run, for the settings page.
type RunState struct {
	Running bool `json:"running"`

	// Ran is false until a run has finished.
	//
	// It exists because `omitempty` does nothing for a time.Time — it is a struct,
	// never "empty" — so the timestamps below serialise as year 0001 rather than
	// being left out, and a client testing one for truthiness would cheerfully
	// report a successful backup on a box that has never taken one.
	Ran bool `json:"ran"`

	Started  time.Time `json:"started,omitempty"`
	Finished time.Time `json:"finished,omitempty"`

	// Current is the ID of the target in flight. Redundant against Targets, and kept
	// because it is the one thing a caller that does not want the whole plan still
	// needs — including the notification mail, which runs after the fact.
	Current string `json:"current,omitempty"`

	// Targets is every target of this run, in the order the run does them, including
	// the ones it has not reached yet.
	Targets   []TargetState `json:"targets,omitempty"`
	Failures  int           `json:"failures"`
	LastError string        `json:"last_error,omitempty"`
}

// Done reports how many targets the run has finished with, by any route. It is what
// the "3 of 9" on the settings page counts.
func (st RunState) Done() int {
	n := 0
	for _, t := range st.Targets {
		switch t.Status {
		case StatusDone, StatusFailed, StatusSkipped:
			n++
		}
	}
	return n
}

// Scheduler runs backups on a timetable.
//
// It is Maison's rather than the engine's, and cannot be delegated at any price: a
// consistent app snapshot requires stopping that app's containers, which no backup
// tool's own scheduler can do.
type Scheduler struct {
	cfg   config.Config
	apps  *apps.Registry
	set   *Set
	store *backupconfig.Store

	// OnChange, if set, is called when the run state changes, so the dashboard can
	// rebroadcast it.
	OnChange func()

	// RestoreInProgress reports a user-data restore in flight. A hook rather than a
	// *UserData field because the dependency is one-way and informational: the scheduler
	// needs to know whether to skip its user-data target, not to drive a restore.
	//
	// Nil means "never", which is only right in a test.
	RestoreInProgress func() bool

	// Now, Backup and Notify exist so the sequencing — which target, in what order,
	// what happens when one fails, and who gets told — can be tested without a clock,
	// an engine, or an SMTP server. Nil means the real thing.
	Now    func() time.Time
	Backup func(ctx context.Context, t Target) (string, error)
	Notify func(subject, body string) error

	mu    sync.Mutex
	state RunState
	// lastRun is persisted so a box that was off at its scheduled time backs up when
	// it returns instead of silently skipping a day.
	lastRunPath string
	reload      chan struct{}
}

// NewScheduler builds the scheduler. apps may be nil on a box with no Docker, in
// which case only the user-data target is available.
func NewScheduler(cfg config.Config, reg *apps.Registry, set *Set, store *backupconfig.Store) *Scheduler {
	return &Scheduler{
		cfg: cfg, apps: reg, set: set, store: store,
		lastRunPath: cfg.StateDir() + "/backup-last-run",
		reload:      make(chan struct{}, 1),
	}
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// State returns the current or last run.
//
// The target list is copied rather than shared. Copying a RunState copies the slice
// header alone, so a caller ranging over it while the run advances would be reading
// elements the run is writing — a race the race detector would only find on the
// unlucky schedule, and the payload here is serialised to JSON on a request
// goroutine while the run mutates it on its own.
func (s *Scheduler) State() RunState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state
	st.Ran = !st.Finished.IsZero()
	st.Targets = append([]TargetState(nil), s.state.Targets...)
	return st
}

// Reload tells a running schedule that the configured time has changed, so an edit
// takes effect without restarting Maison.
func (s *Scheduler) Reload() {
	select {
	case s.reload <- struct{}{}:
	default:
	}
}

// Targets is everything a run would back up, in the order it would do it.
//
// Apps come first and user data last: apps are small and are what makes a box
// usable, user data is where the terabytes are. On a run that is interrupted — or a
// restore, later — that ordering is the difference between "usable in minutes" and
// "usable when the media library finishes".
func (s *Scheduler) Targets() []Target {
	var out []Target
	entries, err := os.ReadDir(s.cfg.AppsDir())
	if err == nil {
		var names []string
		for _, e := range entries {
			// The same guard the on-disk paths use, so ".backups", ".staging-*" and
			// anything else with a dot are excluded for free rather than by a second,
			// drifting filter.
			if !e.IsDir() || !apps.ValidProjectName(e.Name()) {
				continue
			}
			if s.skip(e.Name()) {
				continue
			}
			names = append(names, e.Name())
		}
		sort.Strings(names)
		for _, n := range names {
			out = append(out, Target{Kind: KindApp, App: n})
		}
	}
	// Only when the engine can actually do it. The local engine cannot and must not:
	// its archives live under the very tree it would be copying, so it would be
	// backing up its own output.
	//
	// Offered as a *target* it would fail on every run of a default install — local
	// engine, user data on — which would report a failed backup and mail the user
	// about it on a box where nothing is wrong. An engine that cannot do this has no
	// such target; the settings page says why.
	if s.store.Get().UserData && s.canBackUpUserData() {
		out = append(out, Target{Kind: KindUserData})
	}
	return out
}

func (s *Scheduler) canBackUpUserData() bool {
	if s.set == nil {
		return false
	}
	w := s.set.Writer()
	if w == nil {
		return false
	}
	_, ok := w.(UserDataEngine)
	return ok
}

// skip reports whether an app directory must be left out of a scheduled run.
//
// Two exclusions, both because backing an app up *stops* it:
//
//   - Maison's own state directory. It sits at AppData/maison and therefore looks
//     exactly like an app — deliberately, so the dashboard tiles itself. Stopping it
//     would kill the process running the backup, and the run would end mid-flight
//     with nothing to report it.
//   - System apps: the platform's own pieces, which is what `view: system` names.
//     Taking the gateway or the dashboard down nightly is not a backup strategy.
//
// The cost is that platform state is not backed up by the schedule. That is a
// deliberate gap, not an oversight: doing it properly means backing these up
// *without* stopping them, which is a different shape than the app path has.
func (s *Scheduler) skip(name string) bool {
	if filepath.Clean(filepath.Join(s.cfg.AppsDir(), name)) == filepath.Clean(s.cfg.StateDir()) {
		return true
	}
	return s.apps.Protected(name)
}

// RunAll backs up every target, one at a time.
//
// Strictly sequential: the registry's per-app lock protects a single app, and
// nothing else stops a nightly run from taking six apps down at once. One at a time
// means one app is briefly unavailable rather than the whole box.
//
// A target that fails does not stop the rest — a broken app should not cost the
// user every other backup that night — and the failures are collected for one
// summary at the end.
func (s *Scheduler) RunAll(ctx context.Context) error {
	s.mu.Lock()
	if s.state.Running {
		s.mu.Unlock()
		// Skip, do not queue. A run that overran its window and is still going will
		// only fall further behind if the next one waits behind it.
		return fmt.Errorf("a backup run is already in progress")
	}
	s.state = RunState{Running: true, Started: s.now()}
	s.mu.Unlock()

	// The plan is published before the first target is touched, so the page has
	// something to show from the moment the button is pressed rather than after the
	// first app finishes. Computed outside the lock: it reads the apps directory.
	targets := s.Targets()
	plan := make([]TargetState, len(targets))
	for i, t := range targets {
		plan[i] = TargetState{
			ID: t.ID(), Kind: t.Kind, App: t.App,
			Status: StatusPending, Pct: apps.PctUnknown,
		}
	}
	s.mu.Lock()
	s.state.Targets = plan
	s.mu.Unlock()
	s.changed()

	for i, t := range targets {
		if err := ctx.Err(); err != nil {
			break
		}
		s.beginTarget(i, t)
		name, err := s.backupOne(ctx, t, s.targetProgress(i))
		s.endTarget(i, name, err)
	}

	prev, hadPrev := s.readLastRun()

	s.mu.Lock()
	s.state.Running = false
	s.state.Finished = s.now()
	s.state.Current = ""
	failures := s.state.Failures
	s.mu.Unlock()
	s.changed()

	failed := failures > 0
	s.writeLastRun(failed)
	s.notifyOutcome(prev, hadPrev, failed)

	if failed {
		return fmt.Errorf("%d of %d backup targets failed", failures, len(targets))
	}
	return nil
}

func (s *Scheduler) backupOne(ctx context.Context, t Target, emit func(TargetState)) (string, error) {
	if s.Backup != nil {
		return s.Backup(ctx, t)
	}
	if t.Kind == KindUserData {
		// A restore is rewriting the very tree this would snapshot. Backing it up now
		// would capture a half-restored state that never existed — and that snapshot
		// counts against retention, so it can push out the good one the user is in the
		// middle of restoring from. Skipping one night is the cheap side of this trade.
		if s.RestoreInProgress != nil && s.RestoreInProgress() {
			return "", skip("skipped: a restore of the user-data set is in progress")
		}
		// User data has no containers and no compose project, so it does not go
		// through the app registry at all.
		src, ok := s.set.Writer().(UserDataEngine)
		if !ok {
			return "", fmt.Errorf("engine %s cannot back up user data", s.set.Writer().ID())
		}
		// User data has no tile, so this run panel is the only place its progress can
		// appear — which is why the emit matters more here than anywhere else: it is
		// the biggest target on the box by a wide margin, and it used to report
		// nothing at all between "started" and "finished".
		//
		// The tracker is this scheduler's own, because nothing else is watching this
		// target. The app path below does not get one here: the registry already runs a
		// tracker for the tile, and a second one would derive a second, slightly
		// different ETA for the same bytes.
		tr := &apps.Tracker{}
		return src.BackupUserData(ctx, s.now().Format(apps.StampLayout), func(ev apps.Event) {
			p := tr.Observe(apps.PhaseCopy, ev.Pct, ev.Done, ev.Total)
			emit(TargetState{
				Phase: apps.PhaseCopy, Message: ev.Message, Pct: p.Pct,
				Done: ev.Done, Total: ev.Total, Rate: p.Rate, ETA: int(p.ETA.Seconds()),
			})
		})
	}
	if s.apps == nil {
		return "", fmt.Errorf("docker unavailable")
	}
	conf := s.store.Get()
	// Reapplied before every backup rather than once at setup: the policy lives in
	// the engine's repository, so it outlives a Maison reinstall — and a Maison bug
	// can leave a stale one behind. It is idempotent and costs one call.
	if re, ok := s.set.Writer().(RetentionEngine); ok {
		if err := re.EnsureRetention(ctx, t.App, conf.Keep); err != nil {
			log.Printf("backup: setting retention for %s: %v", t.App, err)
		}
	}
	// The empty engine is the default one, deliberately: the nightly run is exactly
	// the case with nobody there to pick a target, and it is what the "default engine"
	// setting means. A manual backup can name another engine; this cannot.
	//
	// Tracked, so the app's own tile carries the same bar it would if the user had
	// backed this app up from its Backups tab. It went through the untracked Backup
	// for a long time, which meant that pressing "Back up now" left every tile on the
	// box inert while the work was happening on them.
	name, err := s.apps.BackupTracked(ctx, t.App, "", false, func(ev apps.BackupEvent) {
		emit(TargetState{
			Phase: ev.Phase, Message: ev.Message, Pct: ev.TrackPct(),
			Done: ev.Done, Total: ev.Total, Rate: ev.Rate, ETA: ev.ETA,
		})
	})
	if err != nil {
		// Someone is already backing this app up by hand. Waiting behind it would
		// back the same app up twice in a row for nothing, and reporting it as a
		// failure would mail the operator about a box where the app has, in fact,
		// just been backed up.
		if errors.Is(err, apps.ErrBackupInFlight) {
			return "", skip("skipped: %v", err)
		}
		return "", err
	}
	s.pruneLocal(ctx, t.App, conf.KeepLocal)
	return name, nil
}

// UserDataEngine is implemented by engines that can back up the user-data set.
// The local engine cannot and must not: its archives live inside the very tree it
// would be copying.
type UserDataEngine interface {
	BackupUserData(ctx context.Context, stamp string, emit func(apps.Event)) (string, error)
}

// RetentionEngine is implemented by engines that apply retention themselves.
//
// Delegating is not laziness: each app is one source accumulating snapshots over
// time, which is precisely the shape a retention policy is designed for, and the
// engine can expire a snapshot without transferring anything. Maison expresses the
// intent; the engine decides what that means in its own repository.
type RetentionEngine interface {
	EnsureRetention(ctx context.Context, app string, keep backupconfig.Keep) error
}

// pruneLocal trims an app's on-disk archives to the configured count.
//
// Local archives are Maison's to manage — they cost real disk rather than a remote
// quota, and no engine policy governs them.
//
// The floor is what makes this safe. Keeping zero local copies is only meaningful
// when something else holds the backup, so at N=0 an archive is deleted only once
// another engine has been asked and has actually listed it. "The upload command
// exited 0" is not the same as "the backup is there", and this is the one place in
// Maison where being wrong about that destroys the only copy.
func (s *Scheduler) pruneLocal(ctx context.Context, app string, keep int) {
	local := apps.ListBackups(s.cfg.BackupsDir(), app)
	if len(local) <= keep {
		return
	}
	for _, b := range local[keep:] {
		if keep == 0 && !s.heldElsewhere(ctx, app, b.Name) {
			continue
		}
		if err := apps.DeleteBackup(s.cfg.BackupsDir(), app, b.Name); err != nil {
			log.Printf("backup: pruning local archive %s/%s: %v", app, b.Name, err)
		}
	}
}

// heldElsewhere asks every non-local engine whether it actually has this backup.
func (s *Scheduler) heldElsewhere(ctx context.Context, app, name string) bool {
	for _, p := range s.set.providers() {
		if p.ID() == apps.EngineLocal {
			continue
		}
		got, err := p.List(ctx, app)
		if err != nil {
			continue
		}
		for _, b := range got {
			if b.Name == name {
				return true
			}
		}
	}
	return false
}

func (s *Scheduler) beginTarget(i int, t Target) {
	s.mu.Lock()
	s.state.Current = t.ID()
	if i < len(s.state.Targets) {
		s.state.Targets[i].Status = StatusRunning
		s.state.Targets[i].Started = s.now()
	}
	s.mu.Unlock()
	s.changed()
}

// targetProgress returns the callback the target reports through. Only the progress
// fields are taken from it — identity and status belong to the run, not to whatever
// is reporting — so an engine cannot rename a target or declare itself finished.
func (s *Scheduler) targetProgress(i int) func(TargetState) {
	return func(p TargetState) {
		s.mu.Lock()
		if i < len(s.state.Targets) {
			t := &s.state.Targets[i]
			t.Phase, t.Message, t.Pct = p.Phase, p.Message, p.Pct
			t.Done, t.Total, t.Rate, t.ETA = p.Done, p.Total, p.Rate, p.ETA
		}
		s.mu.Unlock()
		s.changed()
	}
}

func (s *Scheduler) endTarget(i int, name string, err error) {
	s.mu.Lock()
	if i < len(s.state.Targets) {
		t := &s.state.Targets[i]
		t.Finished = s.now()
		t.Name = name
		t.Err = errText(err)
		t.Status = StatusDone
		switch {
		case isSkip(err):
			t.Status = StatusSkipped
		case err != nil:
			t.Status = StatusFailed
		}
		// A finished target keeps no live progress: leaving a rate and an ETA on a row
		// that is done reads as though it were still moving.
		t.Phase, t.Message, t.Rate, t.ETA = "", "", 0, 0
		t.Pct = 100
		if err != nil {
			t.Pct = apps.PctUnknown
		}
		if t.Status == StatusFailed {
			s.state.Failures++
			s.state.LastError = t.Err
			log.Printf("backup: %s failed: %s", t.ID, t.Err)
		}
	}
	s.mu.Unlock()
	s.changed()
}

func (s *Scheduler) changed() {
	if s.OnChange != nil {
		s.OnChange()
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// --- the timetable -----------------------------------------------------------

// Start runs the schedule until ctx is done.
//
// It follows the same shape as the app store's daily refresh: a timer recomputed
// from the wall clock on every iteration, rather than a 24h ticker, so it survives
// daylight-saving changes and clock corrections instead of drifting an hour twice a
// year.
//
// Two deliberate differences. It does not run at startup — a backup on every
// container restart would stop every app on the box — and it re-reads its time on
// every iteration, so changing the schedule takes effect without a restart.
func (s *Scheduler) Start(ctx context.Context) {
	go func() {
		for {
			conf := s.store.Get()
			wait := untilNext(s.now(), conf.Hour, conf.Minute) + s.jitter()

			// A box that was switched off through its window backs up when it returns,
			// rather than silently skipping the day. Bounded to once — catching up is
			// not the same as running every missed night at once.
			if conf.Enabled && s.missedARun(conf) {
				wait = time.Minute
			}

			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.reload:
				timer.Stop()
				continue
			case <-timer.C:
			}

			if !s.store.Get().Enabled {
				continue
			}
			if err := s.RunAll(ctx); err != nil {
				log.Printf("backup: scheduled run: %v", err)
			}
		}
	}()
}

// jitter spreads a fleet's runs across half an hour.
//
// A thousand boxes all starting at 03:30 is a self-inflicted thundering herd against
// one bucket. The offset is derived from the data path so it is stable for a given
// box — a box that jittered differently each night would defeat the point — and
// derived rather than random so it needs nothing persisted.
func (s *Scheduler) jitter() time.Duration {
	sum := sha256.Sum256([]byte(s.cfg.DataHostPath + "|" + s.cfg.DataRoot))
	return time.Duration(binary.BigEndian.Uint32(sum[:4])%uint32(30*time.Minute/time.Second)) * time.Second
}

// untilNext is how long until the next hh:mm, in local time.
func untilNext(now time.Time, hour, minute int) time.Duration {
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next.Sub(now)
}

// lastRun is what survives a restart: when the schedule last ran, and whether it
// was failing. The failure flag is persisted rather than kept in memory so that a
// Maison restart does not re-announce a failure the operator has already been told
// about — nor stay silent about a recovery it never saw the failure for.
type lastRun struct {
	At     time.Time `json:"at"`
	Failed bool      `json:"failed"`
}

func (s *Scheduler) readLastRun() (lastRun, bool) {
	b, err := os.ReadFile(s.lastRunPath)
	if err != nil {
		return lastRun{}, false
	}
	var lr lastRun
	if json.Unmarshal(b, &lr) != nil {
		return lastRun{}, false
	}
	return lr, true
}

func (s *Scheduler) missedARun(conf backupconfig.Config) bool {
	lr, ok := s.readLastRun()
	if !ok {
		return false // never run: wait for the first window rather than firing at boot
	}
	return s.now().Sub(lr.At) > 24*time.Hour+time.Hour
}

func (s *Scheduler) writeLastRun(failed bool) {
	_ = os.MkdirAll(s.cfg.StateDir(), 0o755)
	b, err := json.Marshal(lastRun{At: s.now(), Failed: failed})
	if err != nil {
		return
	}
	if err := os.WriteFile(s.lastRunPath, b, 0o644); err != nil {
		log.Printf("backup: recording last run: %v", err)
	}
}

// notifyOutcome mails the operator when the run's health *changes*.
//
// One mail on the transition into failure and one on recovery — not one per failed
// run. A nightly message becomes noise, then a filter rule, and then the failure it
// was reporting is invisible again, which is the exact outcome this exists to
// prevent.
//
// A mail that cannot be sent is logged and swallowed: a broken SMTP configuration
// must never turn a successful backup into a failed one.
func (s *Scheduler) notifyOutcome(prev lastRun, hadPrev bool, failed bool) {
	if hadPrev && prev.Failed == failed {
		return
	}
	if !hadPrev && !failed {
		return // first ever run, and it worked: nothing to announce
	}
	st := s.State()
	subject, body := failureMail(s.cfg.AppDomain(), failed, st)
	if err := s.notify(subject, body); err != nil {
		log.Printf("backup: sending the %s notification: %v", map[bool]string{true: "failure", false: "recovery"}[failed], err)
	}
}

func (s *Scheduler) notify(subject, body string) error {
	if s.Notify != nil {
		return s.Notify(subject, body)
	}
	return notify.Send(s.store.Get().SMTP, subject, body)
}

// failureMail writes what the operator actually needs: which targets failed, why,
// and how many succeeded — so the mail itself answers "is anything backed up".
func failureMail(domain string, failed bool, st RunState) (subject, body string) {
	where := domain
	if where == "" {
		where = "your server"
	}
	if !failed {
		return "Backups are working again on " + where,
			fmt.Sprintf("The backup run that finished at %s completed with no failures.\n\n%d targets were backed up.\n",
				st.Finished.Format(time.RFC1123), st.Done())
	}
	var b strings.Builder
	fmt.Fprintf(&b, "The backup run that finished at %s did not complete.\n\n", st.Finished.Format(time.RFC1123))
	fmt.Fprintf(&b, "%d of %d targets failed:\n\n", st.Failures, st.Done())
	for _, t := range st.Targets {
		if t.Err != "" {
			fmt.Fprintf(&b, "  %s\n    %s\n", t.ID, t.Err)
		}
	}
	ok := st.Done() - st.Failures
	fmt.Fprintf(&b, "\n%d target(s) were backed up successfully.\n", ok)
	b.WriteString("\nThis message is sent once when backups start failing, and once when they recover.\n")
	return "Backups are failing on " + where, b.String()
}
