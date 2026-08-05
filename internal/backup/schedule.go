package backup

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
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

// Result is what happened to one target.
type Result struct {
	Target Target `json:"target"`
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

	Started   time.Time `json:"started,omitempty"`
	Finished  time.Time `json:"finished,omitempty"`
	Current   string    `json:"current,omitempty"`
	Results   []Result  `json:"results,omitempty"`
	Failures  int       `json:"failures"`
	LastError string    `json:"last_error,omitempty"`
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
func (s *Scheduler) State() RunState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state
	st.Ran = !st.Finished.IsZero()
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
//   - Protected apps: the platform's own pieces, which is what ProtectedApps names.
//     Taking the gateway or the dashboard down nightly is not a backup strategy.
//
// The cost is that platform state is not backed up by the schedule. That is a
// deliberate gap, not an oversight: doing it properly means backing these up
// *without* stopping them, which is a different shape than the app path has.
func (s *Scheduler) skip(name string) bool {
	if filepath.Clean(filepath.Join(s.cfg.AppsDir(), name)) == filepath.Clean(s.cfg.StateDir()) {
		return true
	}
	return s.cfg.IsProtected("", name)
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
	s.changed()

	targets := s.Targets()
	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			break
		}
		s.setCurrent(t.ID())
		name, err := s.backupOne(ctx, t)
		s.record(Result{Target: t, Name: name, Err: errText(err)})
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

func (s *Scheduler) backupOne(ctx context.Context, t Target) (string, error) {
	if s.Backup != nil {
		return s.Backup(ctx, t)
	}
	if t.Kind == KindUserData {
		// User data has no containers and no compose project, so it does not go
		// through the app registry at all.
		src, ok := s.set.Writer().(UserDataEngine)
		if !ok {
			return "", fmt.Errorf("engine %s cannot back up user data", s.set.Writer().ID())
		}
		return src.BackupUserData(ctx, s.now().Format(apps.StampLayout))
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
	name, err := s.apps.Backup(ctx, t.App, false, nil)
	if err != nil {
		return "", err
	}
	s.pruneLocal(ctx, t.App, conf.KeepLocal)
	return name, nil
}

// UserDataEngine is implemented by engines that can back up the user-data set.
// The local engine cannot and must not: its archives live inside the very tree it
// would be copying.
type UserDataEngine interface {
	BackupUserData(ctx context.Context, stamp string) (string, error)
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

func (s *Scheduler) setCurrent(id string) {
	s.mu.Lock()
	s.state.Current = id
	s.mu.Unlock()
	s.changed()
}

func (s *Scheduler) record(res Result) {
	s.mu.Lock()
	s.state.Results = append(s.state.Results, res)
	if res.Err != "" {
		s.state.Failures++
		s.state.LastError = res.Err
		log.Printf("backup: %s failed: %s", res.Target.ID(), res.Err)
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
				st.Finished.Format(time.RFC1123), len(st.Results))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "The backup run that finished at %s did not complete.\n\n", st.Finished.Format(time.RFC1123))
	fmt.Fprintf(&b, "%d of %d targets failed:\n\n", st.Failures, len(st.Results))
	for _, r := range st.Results {
		if r.Err != "" {
			fmt.Fprintf(&b, "  %s\n    %s\n", r.Target.ID(), r.Err)
		}
	}
	ok := len(st.Results) - st.Failures
	fmt.Fprintf(&b, "\n%d target(s) were backed up successfully.\n", ok)
	b.WriteString("\nThis message is sent once when backups start failing, and once when they recover.\n")
	return "Backups are failing on " + where, b.String()
}
