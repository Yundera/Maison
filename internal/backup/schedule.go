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
	"github.com/yundera/maison/internal/backup/retention"
	"github.com/yundera/maison/internal/backupconfig"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/incident"
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

	// Engines is what each destination made of this target.
	//
	// One ROW per app, not per (app, engine), because a row is one stop window: the app
	// is stopped once and every engine writes inside it, so splitting the row would
	// claim an outage per destination that does not happen. The verdict above is derived
	// from these — failed if any engine failed — so "3 of 9" still counts apps, which is
	// what the user is waiting for.
	Engines []EngineResult `json:"engines,omitempty"`

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

// EngineResult is one destination's outcome for one target.
//
// It exists because a backup can now land in several engines at once and they do not
// agree: a repository can be unreachable while the local archive is written perfectly,
// and a single Status/Err per target could only ever report one of those. The engine is
// also what the name belongs to — the local engine's zip mode produces "<stamp>.zip"
// where every other case is "<stamp>".
type EngineResult struct {
	Engine string `json:"engine"`
	Status string `json:"status"` // done | failed
	Name   string `json:"name,omitempty"`
	Err    string `json:"error,omitempty"`
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

	// Now and Backup exist so the sequencing — which target, in what order, and what
	// happens when one fails — can be tested without a clock or an engine. Nil means
	// the real thing.
	Now    func() time.Time
	Backup func(ctx context.Context, t Target) ([]apps.Result, error)

	// Report and Resolve hand the run's outcome to the box's incident register
	// (internal/incident), which owns everything about telling anyone: whether this is
	// news, how it is worded, how it is grouped with whatever else is wrong, and how
	// that survives a restart.
	//
	// The schedule used to own all of that itself, and asserting the outcome
	// unconditionally instead is the point of the move: deciding "have I already said
	// this?" in every subsystem is how a box ends up with several notifiers that each
	// get it slightly wrong. Nil means nobody is listening, which is what a test and a
	// standalone install both want.
	//
	// Function fields rather than a *incident.Store for the same reason Mail used to
	// be one: the schedule needs two operations, not a collaborator.
	Report  func(r incident.Report)
	Resolve func(id string)

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

// NextRun is when the schedule will fire next, or the zero time when it is off.
//
// It includes this box's jitter, because a bare hh:mm would be a promise Maison does
// not keep: the offset is up to half an hour and is what stops a fleet stampeding one
// bucket. It also reports the catch-up instant on a box that missed its window,
// rather than tomorrow's — which is the case where the honest answer differs most
// from the configured one.
func (s *Scheduler) NextRun() time.Time {
	conf := s.store.Get()
	if !conf.Enabled {
		return time.Time{}
	}
	now := s.now()
	if s.missedARun(conf) {
		return now.Add(time.Minute)
	}
	return now.Add(untilNext(now, conf.Hour, conf.Minute) + s.jitter())
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

// canBackUpUserData reports whether any engine the schedule writes to can hold the
// user-data set.
//
// The set does not fan out: unlike an app folder it is measured in terabytes, and
// writing it twice is a different proposition from writing an app twice. It goes to the
// first scheduled engine that can take it — the local one deliberately cannot, since its
// archives live inside the very tree it would be copying.
func (s *Scheduler) userDataEngine() UserDataEngine {
	if s.set == nil {
		return nil
	}
	for _, p := range s.set.Writers(apps.TriggerSchedule) {
		if e, ok := p.(UserDataEngine); ok {
			return e
		}
	}
	return nil
}

func (s *Scheduler) canBackUpUserData() bool { return s.userDataEngine() != nil }

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
		res, err := s.backupOne(ctx, t, s.targetProgress(i))
		s.endTarget(i, res, err)
	}

	s.mu.Lock()
	s.state.Running = false
	s.state.Finished = s.now()
	s.state.Current = ""
	failures := s.state.Failures
	s.mu.Unlock()
	s.changed()

	failed := failures > 0
	s.writeLastRun(failed)
	s.reportOutcome(failed)

	if failed {
		return fmt.Errorf("%d of %d backup targets failed", failures, len(targets))
	}
	return nil
}

func (s *Scheduler) backupOne(ctx context.Context, t Target, emit func(TargetState)) ([]apps.Result, error) {
	if s.Backup != nil {
		return s.Backup(ctx, t)
	}
	if t.Kind == KindUserData {
		// A restore is rewriting the very tree this would snapshot. Backing it up now
		// would capture a half-restored state that never existed — and that snapshot
		// counts against retention, so it can push out the good one the user is in the
		// middle of restoring from. Skipping one night is the cheap side of this trade.
		if s.RestoreInProgress != nil && s.RestoreInProgress() {
			return nil, skip("skipped: a restore of the user-data set is in progress")
		}
		// User data has no containers and no compose project, so it does not go
		// through the app registry at all.
		src := s.userDataEngine()
		if src == nil {
			return nil, fmt.Errorf("no backup engine set to run on a schedule can hold your files")
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
		name, err := src.BackupUserData(ctx, s.now().Format(apps.StampLayout), func(ev apps.Event) {
			p := tr.Observe(apps.PhaseCopy, ev.Pct, ev.Done, ev.Total)
			emit(TargetState{
				Phase: apps.PhaseCopy, Message: ev.Message, Pct: p.Pct,
				Done: ev.Done, Total: ev.Total, Rate: p.Rate, ETA: int(p.ETA.Seconds()),
			})
		})
		if err != nil {
			return nil, err
		}
		return []apps.Result{{Engine: src.(apps.Provider).ID(), Name: name}}, nil
	}
	if s.apps == nil {
		return nil, fmt.Errorf("docker unavailable")
	}
	conf := s.store.Get()
	// Reapplied before every backup rather than once at setup: the policy lives in
	// the engine's repository, so it outlives a Maison reinstall — and a Maison bug
	// can leave a stale one behind. It is idempotent and costs one call.
	//
	// Per destination, and with that destination's own resolved policy: engines differ
	// in what expiry their storage can survive, so one set of tiers pushed into all of
	// them would be wrong for at least one. See backupconfig.Config.Effective.
	for _, p := range s.set.Writers(apps.TriggerSchedule) {
		re, ok := p.(RetentionEngine)
		if !ok {
			continue
		}
		res := conf.Effective(p.ID(), backupconfig.Provisioned{})
		if err := re.EnsureRetention(ctx, t.App, res.Keep); err != nil {
			log.Printf("backup: setting retention for %s in %s: %v", t.App, p.ID(), err)
		}
	}
	// The empty engine means "wherever the settings say", deliberately: the nightly run
	// is exactly the case with nobody there to pick, and it may now be several engines
	// at once. A manual backup can narrow it to one; this cannot.
	//
	// Tracked, so the app's own tile carries the same bar it would if the user had
	// backed this app up from its Backups tab. It went through the untracked Backup
	// for a long time, which meant that pressing "Back up now" left every tile on the
	// box inert while the work was happening on them.
	res, err := s.apps.BackupTracked(ctx, t.App, "", false, func(ev apps.BackupEvent) {
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
			return nil, skip("skipped: %v", err)
		}
		// Every destination failed, or the operation never started. A partial failure
		// comes back with err nil and the detail in res, so it is endTarget that decides
		// what to call it — this branch is only the whole-operation refusal.
		return res, err
	}
	s.pruneLocal(t.App, conf)
	return res, nil
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

// pruneLocal expires an app's on-disk archives under the local engine's own retention.
//
// Local archives are Maison's to expire whatever the writer engine is: the local
// provider declares Caps.Retention false — there is no policy engine to delegate to —
// and this runs after every app target because the local directory also accumulates
// the rollback points an update takes (installer.BackupBeforeUpdate, which is always
// local because a rollback has to be a rename). Nothing else ever deletes them.
//
// The policy comes from Config.Effective for the local engine specifically, not from
// the box-wide tiers. Both floors that used to be spelled out here now belong to the
// planner and to that resolution:
//
//   - retention.Plan never drops the newest backup, whatever the policy says. That
//     replaces the old "keep N local, and at N=0 only delete what another engine has
//     actually listed" dance: keeping zero locally is no longer expressible, and it
//     was never right for the rollback point anyway.
//   - Effective degrades tiers to a count for this engine, because a local archive is
//     a full second copy rather than incremental history. See the comment there.
func (s *Scheduler) pruneLocal(app string, conf backupconfig.Config) {
	local := apps.ListBackups(s.cfg.BackupsDir(), app)
	// RetentionSnapshot: a local archive is a self-contained folder or zip, so any
	// generation may be dropped without breaking the ones around it.
	_, drop := retention.Plan(
		s.now(), local, apps.RetentionSnapshot,
		conf.Effective(apps.EngineLocal, backupconfig.Provisioned{}),
	)
	for _, b := range drop {
		if err := apps.DeleteBackup(s.cfg.BackupsDir(), app, b.Name); err != nil {
			log.Printf("backup: pruning local archive %s/%s: %v", app, b.Name, err)
		}
	}
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

func (s *Scheduler) endTarget(i int, res []apps.Result, err error) {
	s.mu.Lock()
	if i < len(s.state.Targets) {
		t := &s.state.Targets[i]
		t.Finished = s.now()
		t.Engines = engineResults(res)
		// The row's name is whichever destination produced one. They agree except in
		// spelling — the local engine's zip mode adds ".zip" — and the row needs a name
		// to show, not a set of them; the per-engine names are on Engines above.
		t.Name = ""
		for _, e := range t.Engines {
			if e.Name != "" {
				t.Name = e.Name
				break
			}
		}
		t.Err = errText(err)
		t.Status = StatusDone
		switch {
		case isSkip(err):
			t.Status = StatusSkipped
		case err != nil:
			t.Status = StatusFailed
		default:
			// A partial failure is a failure. Something was written, and the row says so
			// in its engines, but the destination the user is missing is the fact worth
			// alerting on — reporting the target as done because *a* copy landed is how
			// a repository stops receiving anything and nobody hears about it.
			if e := firstEngineErr(t.Engines); e != "" {
				t.Status, t.Err = StatusFailed, e
			}
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

// engineResults turns the registry's per-engine rows into the run's own, which carry a
// status rather than an error value because they are serialised to the page.
func engineResults(res []apps.Result) []EngineResult {
	if len(res) == 0 {
		return nil
	}
	out := make([]EngineResult, 0, len(res))
	for _, r := range res {
		e := EngineResult{Engine: r.Engine, Status: StatusDone, Name: r.Name}
		if r.Err != nil {
			e.Status, e.Err = StatusFailed, r.Err.Error()
		}
		out = append(out, e)
	}
	return out
}

// firstEngineErr is the failure to put on the row, in engine order so the message names
// a destination rather than whichever one happened to be last.
func firstEngineErr(es []EngineResult) string {
	for _, e := range es {
		if e.Err != "" {
			return e.Engine + ": " + e.Err
		}
	}
	return ""
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

	// Engines is the same two facts per destination, and it is what a box with more
	// than one of them has to be judged on.
	//
	// The flat pair above describes the RUN — it fires once, whatever it writes to —
	// and that was the whole answer while there was one destination. It is not any
	// more: At is written whatever the verdict, so an engine could stop receiving
	// anything for a month while the run kept finishing and the staleness check kept
	// reporting a healthy box. Per engine, At is the last time that engine actually
	// took a backup, which is the question "are my backups running" really asks.
	//
	// Absent in a file written before this existed, which decodes fine: the flat pair
	// still parses, and the first run after an upgrade fills this in.
	Engines map[string]engineRun `json:"engines,omitempty"`
}

// engineRun is one destination's last outcome.
type engineRun struct {
	// At is the last SUCCESSFUL write, not the last attempt. An engine that has been
	// failing for a week must not look recent because it kept being tried.
	At     time.Time `json:"at"`
	Failed bool      `json:"failed"`
	Err    string    `json:"error,omitempty"`
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

// LastRun reports when the schedule last finished and whether it failed.
//
// Exported for the incident detectors, which need two things this file already knows:
// how long it has been since a run at all (a schedule that never fires is a hole no
// per-run alert can see), and whether the box was already failing when it upgraded to
// the register.
//
// The settings page reads it for the same reason the detectors do, and it is why the
// answer comes off disk rather than out of State(): RunState lives in memory and is
// reset wholesale at the start of the next run, so "when was my last backup" would be
// year zero on a box that has rebooted and blank while a run is in flight.
func (s *Scheduler) LastRun() (at time.Time, failed, ok bool) {
	lr, found := s.readLastRun()
	return lr.At, lr.Failed, found
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
	lr := lastRun{At: s.now(), Failed: failed, Engines: s.engineOutcomes()}
	b, err := json.Marshal(lr)
	if err != nil {
		return
	}
	if err := os.WriteFile(s.lastRunPath, b, 0o644); err != nil {
		log.Printf("backup: recording last run: %v", err)
	}
}

// engineOutcomes summarises the run that just finished, per destination.
//
// A destination is failing if it failed for ANY target: one app that could not be
// written is a repository that is not holding a complete copy of the box, which is the
// thing worth saying. Its timestamp only moves when it actually took something, so an
// engine that has been failing all week does not look recent for having been tried.
//
// Previous outcomes are carried forward for engines this run did not touch — a target
// list with no apps in it must not read as every engine having gone quiet.
func (s *Scheduler) engineOutcomes() map[string]engineRun {
	out := map[string]engineRun{}
	if prev, ok := s.readLastRun(); ok {
		for id, e := range prev.Engines {
			out[id] = e
		}
	}
	now := s.now()
	for _, t := range s.State().Targets {
		for _, e := range t.Engines {
			cur := out[e.Engine]
			if e.Status == StatusFailed {
				cur.Failed, cur.Err = true, e.Err
			} else {
				cur.At = now
				// Cleared only by a success, so the reason survives long enough to be read.
				cur.Failed, cur.Err = false, ""
			}
			out[e.Engine] = cur
		}
	}
	return out
}

// LastRunIn is when a destination last took a backup, and whether it is failing.
//
// The per-engine answer the staleness check needs: the run's own timestamp says only
// that the schedule fired, which it does whether or not anything reached a given
// repository.
func (s *Scheduler) LastRunIn(engine string) (at time.Time, failed, ok bool) {
	lr, found := s.readLastRun()
	if !found {
		return time.Time{}, false, false
	}
	e, has := lr.Engines[engine]
	if !has {
		return time.Time{}, false, false
	}
	return e.At, e.Failed, true
}

// reportOutcome hands the run's verdict to the incident register, every time.
//
// There is deliberately NO "has this changed?" check here any more. The register
// dedups by ID and persists that across restarts, so asserting the same failure every
// night costs nothing and tells nobody twice — and the rule that used to live here,
// one mail on the way into failure and one on the way out, is now a property every
// reporter on the box gets rather than one this file has to remember.
//
// Resolve on a healthy run is a no-op unless a failure was actually recorded, which is
// what keeps a box whose first ever backup succeeds from announcing a recovery from
// nothing.
func (s *Scheduler) reportOutcome(failed bool) {
	if !failed {
		if s.Resolve != nil {
			s.Resolve(incident.IDBackupRun)
		}
		return
	}
	if s.Report == nil {
		return
	}
	s.Report(incident.Report{
		ID:       incident.IDBackupRun,
		Kind:     incident.KindBackupFailed,
		Severity: incident.Critical,
		Title:    "Backups are failing",
		Detail:   failureDetail(s.State()),
	})
}

// failureDetail writes what the owner actually needs: which targets failed, why, and
// how many succeeded — so the alert answers "is anything backed up at all", which is
// the only question they have.
//
// Pure and directly tested, like the mail composer it replaces. The subject line and
// the box's name are the register's job now; this is only the part that needs to know
// what a backup run is.
func failureDetail(st RunState) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The backup run that finished at %s did not complete.\n", st.Finished.Format(time.RFC1123))
	fmt.Fprintf(&b, "%d of %d targets failed:\n", st.Failures, st.Done())
	for _, t := range st.Targets {
		if t.Err != "" {
			fmt.Fprintf(&b, "  %s: %s\n", t.ID, t.Err)
		}
	}
	fmt.Fprintf(&b, "%d target(s) were backed up successfully.\n", st.Done()-st.Failures)
	return b.String()
}
