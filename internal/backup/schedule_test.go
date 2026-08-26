package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/backup/backuptest"
	"github.com/yundera/maison/internal/backupconfig"
	"github.com/yundera/maison/internal/config"
)

func newScheduler(t *testing.T, appNames ...string) (*Scheduler, *backupconfig.Store) {
	t.Helper()
	cfg := config.Config{DataRoot: t.TempDir()}
	for _, n := range appNames {
		if err := os.MkdirAll(filepath.Join(cfg.AppsDir(), n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Directories the on-disk guard excludes, which the run must also skip.
	for _, n := range []string{".backups", ".staging-2026-01-01_000000"} {
		if err := os.MkdirAll(filepath.Join(cfg.AppsDir(), n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	store := backupconfig.New(filepath.Join(cfg.StateDir(), "backup.json"))
	// A real registry, because the run's skip guard asks it which apps are system
	// apps — that answer comes from each app's compose, not from configuration.
	return NewScheduler(cfg, apps.New(cfg, nil), New(), store), store
}

// seedSystemApp gives an app dir a compose that declares it a platform piece.
func seedSystemApp(t *testing.T, cfg config.Config, name string) {
	t.Helper()
	body := "services: {}\nx-compose-app:\n  view: system\n"
	path := filepath.Join(cfg.AppsDir(), name, "docker-compose.yml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The run enumerates apps with the same guard the on-disk paths use, so the
// backups directory and a crashed staging folder are excluded for free rather than
// by a second filter that can drift from it.
func TestTargetsSkipNonProjects(t *testing.T) {
	s, store := newScheduler(t, "jellyfin", "immich")
	if err := store.Set(backupconfig.Config{UserData: false, Hour: 3, Minute: 30, Keep: backupconfig.Keep{Latest: 1}}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tg := range s.Targets() {
		got = append(got, tg.ID())
	}
	want := "app:immich app:jellyfin"
	if strings.Join(got, " ") != want {
		t.Fatalf("Targets = %v, want %q", got, want)
	}
}

// A system app is left out of the nightly run: backing an app up stops it, and
// the platform's own pieces are exactly the ones that must not go down at 03:30.
func TestTargetsSkipSystemApps(t *testing.T) {
	s, store := newScheduler(t, "jellyfin", "yundera")
	seedSystemApp(t, s.cfg, "yundera")
	if err := store.Set(backupconfig.Config{UserData: false, Hour: 3, Minute: 30, Keep: backupconfig.Keep{Latest: 1}}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tg := range s.Targets() {
		got = append(got, tg.ID())
	}
	if want := "app:jellyfin"; strings.Join(got, " ") != want {
		t.Fatalf("Targets = %v, want %q", got, want)
	}
}

// Apps first, user data last: apps are what make a box usable and user data is
// where the terabytes are, so an interrupted run should have done the useful part.
func TestUserDataIsBackedUpLast(t *testing.T) {
	s, store := newScheduler(t, "jellyfin")
	s.set = New(&userDataCapable{Fake: *backuptest.NewRemote("kopia")})
	if err := store.Set(backupconfig.Config{UserData: true, Hour: 3, Minute: 30, Keep: backupconfig.Keep{Latest: 1}}); err != nil {
		t.Fatal(err)
	}
	got := s.Targets()
	if len(got) != 2 || got[0].Kind != KindApp || got[1].Kind != KindUserData {
		t.Fatalf("Targets = %+v, want the app first and user data last", got)
	}
	// Namespaced, so an app called "userdata" cannot collide with the tree target.
	if got[1].ID() != "userdata" || got[0].ID() != "app:jellyfin" {
		t.Errorf("target IDs = %q/%q", got[0].ID(), got[1].ID())
	}
}

// One target failing must not cost the user every other backup that night.
func TestRunAllContinuesPastAFailure(t *testing.T) {
	s, store := newScheduler(t, "alpha", "beta", "gamma")
	if err := store.Set(backupconfig.Config{UserData: false, Hour: 3, Minute: 30, Keep: backupconfig.Keep{Latest: 1}}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var ran []string
	s.Backup = func(_ context.Context, tg Target) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		ran = append(ran, tg.ID())
		if tg.App == "beta" {
			return "", errors.New("repository unreachable")
		}
		return "2026-01-01_000000", nil
	}

	err := s.RunAll(context.Background())
	if err == nil {
		t.Fatal("RunAll should report that a target failed")
	}
	if strings.Join(ran, " ") != "app:alpha app:beta app:gamma" {
		t.Fatalf("ran = %v, want every target attempted in order", ran)
	}
	st := s.State()
	if st.Failures != 1 || st.Done() != 3 {
		t.Fatalf("state = %+v, want 3 finished targets and 1 failure", st)
	}
	// The plan outlives the run, so what failed is still readable afterwards rather
	// than reduced to a count.
	if len(st.Targets) != 3 || st.Targets[1].Status != StatusFailed ||
		st.Targets[0].Status != StatusDone || st.Targets[2].Status != StatusDone {
		t.Fatalf("targets = %+v, want beta failed and the others done", st.Targets)
	}
	if st.Running {
		t.Error("run state still reports running after RunAll returned")
	}
}

// A run that overran its window must be skipped, not queued behind itself —
// otherwise a slow night compounds into a backlog.
func TestConcurrentRunIsSkippedNotQueued(t *testing.T) {
	s, store := newScheduler(t, "alpha")
	if err := store.Set(backupconfig.Config{UserData: false, Hour: 3, Minute: 30, Keep: backupconfig.Keep{Latest: 1}}); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	s.Backup = func(_ context.Context, _ Target) (string, error) {
		once.Do(func() { close(started) })
		<-release
		return "2026-01-01_000000", nil
	}

	done := make(chan struct{})
	go func() { _ = s.RunAll(context.Background()); close(done) }()
	<-started

	if err := s.RunAll(context.Background()); err == nil {
		t.Error("a second concurrent run was accepted; it must be skipped")
	}
	close(release)
	// Wait for it: the run writes its last-run file on the way out, and returning first
	// races that write against t.TempDir()'s cleanup of the tree it writes into.
	<-done
}

// The state a run reports is what the settings page renders, so it has to name what
// is happening while it happens.
func TestRunStateNamesTheCurrentTarget(t *testing.T) {
	s, store := newScheduler(t, "alpha")
	if err := store.Set(backupconfig.Config{UserData: false, Hour: 3, Minute: 30, Keep: backupconfig.Keep{Latest: 1}}); err != nil {
		t.Fatal(err)
	}
	seen := make(chan string, 1)
	s.Backup = func(_ context.Context, _ Target) (string, error) {
		select {
		case seen <- s.State().Current:
		default:
		}
		return "2026-01-01_000000", nil
	}
	if err := s.RunAll(context.Background()); err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if got := <-seen; got != "app:alpha" {
		t.Errorf("Current during the run = %q, want %q", got, "app:alpha")
	}
	if got := s.State().Current; got != "" {
		t.Errorf("Current after the run = %q, want it cleared", got)
	}
}

// The timer is recomputed from the wall clock each iteration rather than being a
// 24h ticker, so it does not drift an hour twice a year.
func TestUntilNextRollsOverMidnight(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 3, 1, h, m, 0, 0, time.UTC) }
	cases := []struct {
		now      time.Time
		h, m     int
		wantMins float64
	}{
		{at(1, 0), 3, 30, 150},   // later today
		{at(4, 0), 3, 30, 1410},  // already passed: tomorrow
		{at(3, 30), 3, 30, 1440}, // exactly now counts as passed, not a double-fire
	}
	for _, c := range cases {
		if got := untilNext(c.now, c.h, c.m).Minutes(); got != c.wantMins {
			t.Errorf("untilNext(%s, %02d:%02d) = %v min, want %v", c.now.Format("15:04"), c.h, c.m, got, c.wantMins)
		}
	}
}

// A fleet all firing at 03:30 is a thundering herd against one bucket. The offset
// must be stable for a box — one that jittered differently each night would defeat
// the point — and must differ between boxes.
func TestJitterIsStablePerBoxAndBounded(t *testing.T) {
	a := NewScheduler(config.Config{DataRoot: "/DATA", DataHostPath: "/opt/a/DATA"}, nil, New(), backupconfig.New(""))
	b := NewScheduler(config.Config{DataRoot: "/DATA", DataHostPath: "/opt/b/DATA"}, nil, New(), backupconfig.New(""))

	if a.jitter() != a.jitter() {
		t.Error("jitter is not stable for the same box")
	}
	if a.jitter() == b.jitter() {
		t.Error("two different boxes got the same offset, which defeats the purpose")
	}
	for _, s := range []*Scheduler{a, b} {
		if j := s.jitter(); j < 0 || j >= 30*time.Minute {
			t.Errorf("jitter %v is outside the half-hour window", j)
		}
	}
}

// The config store must not lose a false — the trap that disqualified usersettings.
func TestConfigRoundTripsFalse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.json")
	store := backupconfig.New(path)
	if err := store.Set(backupconfig.Config{Enabled: false, UserData: false, Hour: 4, Minute: 15, Keep: backupconfig.Keep{Latest: 1}}); err != nil {
		t.Fatal(err)
	}
	reloaded := backupconfig.New(path).Get()
	if reloaded.Enabled || reloaded.UserData {
		t.Errorf("a false value did not survive the round trip: %+v", reloaded)
	}
	if reloaded.Hour != 4 || reloaded.Minute != 15 {
		t.Errorf("schedule = %02d:%02d, want 04:15", reloaded.Hour, reloaded.Minute)
	}
}

// A nonsense schedule would otherwise produce a timer that never fires.
func TestConfigClampsAnImpossibleSchedule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.json")
	store := backupconfig.New(path)
	if err := store.Set(backupconfig.Config{Hour: 99, Minute: -3}); err != nil {
		t.Fatal(err)
	}
	got := store.Get()
	if got.Hour != backupconfig.Defaults().Hour || got.Minute != backupconfig.Defaults().Minute {
		t.Fatalf("schedule = %02d:%02d, want the defaults", got.Hour, got.Minute)
	}
}

// Alerting fires on a *change* of health, not once a night. A nightly message
// becomes noise, then a filter rule, and then the failure it reports is invisible
// again — which is the outcome the alert exists to prevent.
func TestNotifiesOnlyWhenHealthChanges(t *testing.T) {
	s, store := newScheduler(t, "alpha")
	if err := store.Set(backupconfig.Config{UserData: false, Hour: 3, Minute: 30, Keep: backupconfig.Keep{Latest: 1}}); err != nil {
		t.Fatal(err)
	}
	var fail bool
	s.Backup = func(_ context.Context, _ Target) (string, error) {
		if fail {
			return "", errors.New("repository unreachable")
		}
		return "2026-01-01_000000", nil
	}
	var subjects []string
	s.Notify = func(subject, _ string) error {
		subjects = append(subjects, subject)
		return nil
	}

	run := func() { _ = s.RunAll(context.Background()) }

	run() // first run, healthy: nothing to announce
	fail = true
	run() // broke: one alert
	run() // still broken: silence
	run() // still broken: silence
	fail = false
	run() // recovered: one alert

	if len(subjects) != 2 {
		t.Fatalf("sent %d mails (%v), want exactly one failure and one recovery", len(subjects), subjects)
	}
	if !strings.Contains(subjects[0], "failing") {
		t.Errorf("first mail = %q, want it to report the failure", subjects[0])
	}
	if !strings.Contains(subjects[1], "working again") {
		t.Errorf("second mail = %q, want it to report the recovery", subjects[1])
	}
}

// A broken mail configuration must never turn a successful backup into a failed one.
func TestABrokenMailerDoesNotFailTheRun(t *testing.T) {
	s, store := newScheduler(t, "alpha")
	if err := store.Set(backupconfig.Config{UserData: false, Hour: 3, Minute: 30, Keep: backupconfig.Keep{Latest: 1}}); err != nil {
		t.Fatal(err)
	}
	s.Backup = func(_ context.Context, _ Target) (string, error) { return "", errors.New("boom") }
	s.Notify = func(string, string) error { return errors.New("smtp refused") }

	// The run still reports its own failure, but the mailer's must not compound it.
	if err := s.RunAll(context.Background()); err == nil || strings.Contains(err.Error(), "smtp") {
		t.Fatalf("RunAll error = %v, want the backup failure, not the mail failure", err)
	}
}

// The alert has to answer "is anything backed up", not just "something broke".
func TestFailureMailNamesWhatFailedAndWhatDidNot(t *testing.T) {
	st := RunState{
		Finished: time.Date(2026, 3, 1, 3, 30, 0, 0, time.UTC),
		Failures: 1,
		Targets: []TargetState{
			{ID: "app:alpha", Kind: KindApp, App: "alpha", Status: StatusDone},
			{ID: "app:beta", Kind: KindApp, App: "beta", Status: StatusFailed, Err: "repository unreachable"},
		},
	}
	subject, body := failureMail("john.nsl.sh", true, st)
	if !strings.Contains(subject, "john.nsl.sh") {
		t.Errorf("subject %q does not say which box", subject)
	}
	for _, want := range []string{"app:beta", "repository unreachable", "1 target(s) were backed up successfully"} {
		if !strings.Contains(body, want) {
			t.Errorf("body is missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "app:alpha\n") {
		t.Error("the body lists a target that did not fail")
	}
}

// A box that has never backed up must not report a successful last run. Go
// serialises a zero time as year 0001 instead of omitting it, so a client testing
// the timestamp for truthiness would say "the last backup completed successfully"
// on a box that has never taken one.
func TestStateDoesNotClaimARunThatNeverHappened(t *testing.T) {
	s, _ := newScheduler(t)
	if s.State().Ran {
		t.Fatal("a scheduler that has never run reported that it had")
	}
	s.Backup = func(context.Context, Target) (string, error) { return "2026-01-01_000000", nil }
	if err := s.RunAll(context.Background()); err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if !s.State().Ran {
		t.Fatal("a completed run was not reported as having happened")
	}
}

// userDataCapable is an engine that can back up the user-data set, which the local
// engine deliberately cannot.
type userDataCapable struct{ backuptest.Fake }

func (*userDataCapable) BackupUserData(context.Context, string, func(apps.Event)) (string, error) {
	return "2026-01-01_000000", nil
}

// A default install is the local engine with user data switched on. The local
// engine cannot back up the tree its own archives live in, so that must not be
// offered as a target — otherwise every default box reports a failed backup, and
// mails its owner about it, when nothing is wrong.
func TestUserDataIsNotATargetForAnEngineThatCannotDoIt(t *testing.T) {
	s, store := newScheduler(t, "jellyfin")
	s.set = New(apps.NewLocalProvider(config.Config{DataRoot: t.TempDir()}))
	if err := store.Set(backupconfig.Config{UserData: true, Hour: 3, Minute: 30, Keep: backupconfig.Keep{Latest: 1}}); err != nil {
		t.Fatal(err)
	}
	for _, tg := range s.Targets() {
		if tg.Kind == KindUserData {
			t.Fatal("the local engine was offered the user-data target it cannot serve")
		}
	}
}

// The other half of the same rule: a scheduled run must not snapshot the user-data set
// while a restore is rewriting it. Apps are unaffected — they are a different set, and
// a user-data restore does not touch them.
func TestUserDataBackupIsSkippedDuringARestore(t *testing.T) {
	s, store := newScheduler(t, "jellyfin")
	s.set = New(&userDataCapable{Fake: *backuptest.NewRemote("kopia")})
	s.RestoreInProgress = func() bool { return true }
	if err := store.Set(backupconfig.Config{UserData: true, Hour: 3, Minute: 30, Keep: backupconfig.Keep{Latest: 1}}); err != nil {
		t.Fatal(err)
	}

	_, err := s.backupOne(context.Background(), Target{Kind: KindUserData}, func(TargetState) {})
	if err == nil {
		t.Fatal("the user-data set was backed up while a restore was rewriting it")
	}
	if !strings.Contains(err.Error(), "restore") {
		t.Errorf("error = %q; want it to name the restore as the reason", err)
	}

	// And the app half is untouched by that rule.
	if _, err := s.backupOne(context.Background(), Target{Kind: KindApp, App: "jellyfin"}, func(TargetState) {}); err != nil {
		t.Errorf("an app backup was blocked by a user-data restore: %v", err)
	}
}

// The plan is the point of the target list: it has to be on screen from the moment
// the button is pressed, not assembled as the run discovers what it is doing. Before
// this, a run in flight was a single string naming the app being worked on, so
// "backing up app:immich" was all the user got — no idea whether that was the first
// of two or the eighth of nine.
func TestThePlanIsPublishedBeforeTheFirstTargetRuns(t *testing.T) {
	s, store := newScheduler(t, "alpha", "beta", "gamma")
	if err := store.Set(backupconfig.Config{UserData: false, Hour: 3, Minute: 30, Keep: backupconfig.Keep{Latest: 1}}); err != nil {
		t.Fatal(err)
	}

	// Captured from inside the first target, so this is the state a client would have
	// seen while the first app was still being copied.
	var seen RunState
	first := true
	s.Backup = func(context.Context, Target) (string, error) {
		if first {
			first = false
			seen = s.State()
		}
		return "2026-01-01_000000", nil
	}
	if err := s.RunAll(context.Background()); err != nil {
		t.Fatalf("RunAll: %v", err)
	}

	if len(seen.Targets) != 3 {
		t.Fatalf("targets during the first backup = %d, want all 3 known up front", len(seen.Targets))
	}
	if seen.Targets[0].Status != StatusRunning {
		t.Errorf("first target status = %q, want %q", seen.Targets[0].Status, StatusRunning)
	}
	for _, tg := range seen.Targets[1:] {
		if tg.Status != StatusPending {
			t.Errorf("target %s = %q, want %q — the rest of the plan must be visible as pending",
				tg.ID, tg.Status, StatusPending)
		}
	}
	if seen.Done() != 0 {
		t.Errorf("Done() = %d before the first target finished, want 0", seen.Done())
	}
}

// A target reports its progress through the run, and the run keeps identity and
// status to itself: whatever is reporting must not be able to rename a target or
// declare itself finished.
func TestProgressUpdatesTheRunningTargetOnly(t *testing.T) {
	s, store := newScheduler(t, "alpha")
	if err := store.Set(backupconfig.Config{UserData: false, Hour: 3, Minute: 30, Keep: backupconfig.Keep{Latest: 1}}); err != nil {
		t.Fatal(err)
	}

	var mid RunState
	s.Backup = func(context.Context, Target) (string, error) { return "2026-01-01_000000", nil }
	// Drive one progress report by hand: s.Backup replaces backupOne wholesale, so
	// this exercises the bookkeeping rather than an engine.
	s.state = RunState{Running: true, Targets: []TargetState{
		{ID: "app:alpha", Kind: KindApp, App: "alpha", Status: StatusRunning, Pct: apps.PctUnknown},
	}}
	s.targetProgress(0)(TargetState{
		ID: "hijacked", Status: StatusDone, Phase: apps.PhaseSync,
		Message: "Syncing changes", Pct: 40, Done: 400, Total: 1000, Rate: 100, ETA: 6,
	})
	mid = s.State()

	got := mid.Targets[0]
	if got.ID != "app:alpha" || got.Status != StatusRunning {
		t.Errorf("target = %+v, want its identity and status untouched by the report", got)
	}
	if got.Phase != apps.PhaseSync || got.Pct != 40 || got.Rate != 100 || got.ETA != 6 {
		t.Errorf("progress = %+v, want the reported numbers", got)
	}
}

// A finished target must not look like it is still moving: a row showing a rate and
// an ETA after it is done reads as a backup that is still running.
func TestAFinishedTargetKeepsNoLiveProgress(t *testing.T) {
	s, _ := newScheduler(t, "alpha")
	s.state = RunState{Running: true, Targets: []TargetState{
		{ID: "app:alpha", Kind: KindApp, App: "alpha", Status: StatusRunning,
			Phase: apps.PhaseCopy, Pct: 40, Rate: 100, ETA: 6, Message: "Copying alpha"},
	}}
	s.endTarget(0, "2026-01-01_000000", nil)

	got := s.State().Targets[0]
	if got.Status != StatusDone || got.Name != "2026-01-01_000000" || got.Pct != 100 {
		t.Errorf("target = %+v, want it finished at 100%%", got)
	}
	if got.Rate != 0 || got.ETA != 0 || got.Phase != "" {
		t.Errorf("target = %+v, want the live progress cleared", got)
	}
}

// A target the run deliberately did not attempt is not a target that failed.
//
// The difference is what lands in the operator's inbox: a failure mails "backups are
// failing on your server", and sending that for a box where the right thing happened
// is how a useful alert turns into a filter rule. Both skips are cases where nothing
// is wrong — the user-data set is mid-restore, or somebody is already backing that app
// up by hand.
func TestASkippedTargetIsNotAFailure(t *testing.T) {
	s, store := newScheduler(t, "alpha", "beta")
	if err := store.Set(backupconfig.Config{UserData: false, Hour: 3, Minute: 30, Keep: backupconfig.Keep{Latest: 1}}); err != nil {
		t.Fatal(err)
	}
	s.Backup = func(_ context.Context, tg Target) (string, error) {
		if tg.App == "alpha" {
			return "", skip("skipped: %v", apps.ErrBackupInFlight)
		}
		return "2026-01-01_000000", nil
	}

	if err := s.RunAll(context.Background()); err != nil {
		t.Fatalf("RunAll reported a failure for a skipped target: %v", err)
	}
	st := s.State()
	if st.Failures != 0 {
		t.Errorf("failures = %d, want 0 — a skip is not a failure", st.Failures)
	}
	if st.Targets[0].Status != StatusSkipped {
		t.Errorf("status = %q, want %q", st.Targets[0].Status, StatusSkipped)
	}
	// The reason still reaches the user; it just does not raise the alarm.
	if !strings.Contains(st.Targets[0].Err, "already running") {
		t.Errorf("skipped target says %q, want it to say why", st.Targets[0].Err)
	}
	if st.Done() != 2 {
		t.Errorf("Done() = %d, want both targets accounted for", st.Done())
	}
}
