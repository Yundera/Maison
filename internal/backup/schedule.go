package backup

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/backupconfig"
	"github.com/yundera/maison/internal/config"
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
	Running   bool      `json:"running"`
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

	// Now and Backup exist so the sequencing can be tested without a clock or an
	// engine. Nil means the real thing.
	Now    func() time.Time
	Backup func(ctx context.Context, t Target) (string, error)

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
	return s.state
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
	if s.store.Get().UserData {
		out = append(out, Target{Kind: KindUserData})
	}
	return out
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

	s.mu.Lock()
	s.state.Running = false
	s.state.Finished = s.now()
	s.state.Current = ""
	failures := s.state.Failures
	s.mu.Unlock()
	s.changed()
	s.writeLastRun()

	if failures > 0 {
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
	return s.apps.Backup(ctx, t.App, false, nil)
}

// UserDataEngine is implemented by engines that can back up the user-data set.
// The local engine cannot and must not: its archives live inside the very tree it
// would be copying.
type UserDataEngine interface {
	BackupUserData(ctx context.Context, stamp string) (string, error)
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

func (s *Scheduler) missedARun(conf backupconfig.Config) bool {
	b, err := os.ReadFile(s.lastRunPath)
	if err != nil {
		return false // never run: wait for the first scheduled window rather than firing at boot
	}
	last, err := time.Parse(time.RFC3339, string(b))
	if err != nil {
		return false
	}
	return s.now().Sub(last) > 24*time.Hour+time.Hour
}

func (s *Scheduler) writeLastRun() {
	_ = os.MkdirAll(s.cfg.StateDir(), 0o755)
	if err := os.WriteFile(s.lastRunPath, []byte(s.now().Format(time.RFC3339)), 0o644); err != nil {
		log.Printf("backup: recording last run: %v", err)
	}
}
