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

const udStamp = "2026-02-02_120000"

// fakeUserData is an engine that can serve the user-data set, recording what it was
// asked to do so a test can pin the *order* of the guards — which is where the value
// is, since a guard that runs after the overwrite is not a guard.
type fakeUserData struct {
	backuptest.Fake

	mu         sync.Mutex
	calls      []string
	list       []apps.Backup
	backupErr  error
	restoreErr error
	lastOpts   apps.UserDataRestoreOpts
}

func newFakeUserData() *fakeUserData {
	f := &fakeUserData{Fake: *backuptest.NewRemote("kopia")}
	b, _ := apps.ParseBackupName("userdata", udStamp)
	b.Tier, b.Engine, b.Size = apps.TierRemote, "kopia", 4096
	f.list = []apps.Backup{b}
	return f
}

func (f *fakeUserData) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fakeUserData) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeUserData) BackupUserData(_ context.Context, stamp string) (string, error) {
	f.record("backup:" + stamp)
	return stamp, f.backupErr
}

func (f *fakeUserData) ListUserData(context.Context) ([]apps.Backup, error) {
	return f.list, nil
}

func (f *fakeUserData) RestoreUserData(_ context.Context, stamp string, opts apps.UserDataRestoreOpts, _ func(apps.Event)) error {
	f.record("restore:" + stamp)
	f.mu.Lock()
	f.lastOpts = opts
	f.mu.Unlock()
	return f.restoreErr
}

func newUserData(t *testing.T, e apps.Provider) (*UserData, config.Config) {
	t.Helper()
	cfg := config.Config{DataRoot: t.TempDir()}
	if err := os.MkdirAll(cfg.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	store := backupconfig.New(filepath.Join(cfg.StateDir(), "backup.json"))
	if err := store.Set(backupconfig.Config{UserData: true, Keep: backupconfig.Keep{Latest: 1}}); err != nil {
		t.Fatal(err)
	}
	u := NewUserData(cfg, New(e), store)
	u.Now = func() time.Time { return time.Date(2026, 3, 3, 9, 0, 0, 0, time.UTC) }
	return u, cfg
}

// waitIdle waits for a detached restore to finish. The operation is deliberately
// asynchronous — it can run for hours — so every test here has to join it.
func waitIdle(t *testing.T, u *UserData) RestoreState {
	t.Helper()
	for i := 0; i < 200; i++ {
		if st := u.State(); !st.Running {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("restore never finished")
	return RestoreState{}
}

// The local engine cannot back up the tree its own archives live in, so a default
// install must say why rather than showing an empty list — which reads as "nothing to
// worry about" on the one page that exists to worry about it.
func TestUserDataUnavailableOnTheLocalEngineSaysWhy(t *testing.T) {
	u, _ := newUserData(t, apps.NewLocalProvider(config.Config{DataRoot: t.TempDir()}))

	ok, why := u.Available()
	if ok {
		t.Fatal("the local engine must not be offered as able to back up the user-data set")
	}
	if !strings.Contains(why, "disk") {
		t.Errorf("reason = %q; want it to explain that the copy would sit on the same disk", why)
	}
	if got := u.List(context.Background()); got != nil {
		t.Errorf("List = %+v; want nothing from an engine that cannot serve the set", got)
	}
}

// Switching the set off is a different answer from being unable to do it, and the page
// has to tell them apart: one is fixed by a checkbox, the other by changing engine.
func TestUserDataDisabledIsADifferentReasonFromUnsupported(t *testing.T) {
	u, cfg := newUserData(t, newFakeUserData())
	store := backupconfig.New(filepath.Join(cfg.StateDir(), "backup.json"))
	if err := store.Set(backupconfig.Config{UserData: false, Keep: backupconfig.Keep{Latest: 1}}); err != nil {
		t.Fatal(err)
	}
	u.store = store

	ok, why := u.Available()
	if ok {
		t.Fatal("Available must be false while the set is switched off")
	}
	if strings.Contains(why, "disk") {
		t.Errorf("reason = %q; want the switched-off reason, not the wrong-engine one", why)
	}
}

// The guard that matters most: in place, the current state is backed up **before**
// anything is overwritten. Order is the assertion — an undo snapshot taken afterwards
// would be a snapshot of the restored state, which undoes nothing.
func TestInPlaceRestoreTakesAnUndoSnapshotFirst(t *testing.T) {
	f := newFakeUserData()
	u, _ := newUserData(t, f)

	if err := u.Restore(context.Background(), udStamp, apps.UserDataRestoreOpts{}, nil); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if st := waitIdle(t, u); st.Error != "" {
		t.Fatalf("restore failed: %s", st.Error)
	}

	got := f.Calls()
	want := []string{"backup:2026-03-03_090000", "restore:" + udStamp}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("calls = %v; want the undo snapshot before the restore: %v", got, want)
	}
}

// And if the undo snapshot fails, nothing is overwritten. An unrecoverable overwrite is
// worse than a restore that did not happen.
func TestInPlaceRestoreIsRefusedWhenTheUndoSnapshotFails(t *testing.T) {
	f := newFakeUserData()
	f.backupErr = errors.New("repository unreachable")
	u, cfg := newUserData(t, f)

	if err := u.Restore(context.Background(), udStamp, apps.UserDataRestoreOpts{}, nil); err != nil {
		t.Fatalf("Restore should start and then fail, not refuse to start: %v", err)
	}
	st := waitIdle(t, u)

	if st.Error == "" {
		t.Fatal("a failed undo snapshot must fail the restore")
	}
	if !strings.Contains(st.Error, "nothing was changed") {
		t.Errorf("error = %q; want it to say nothing was changed", st.Error)
	}
	for _, c := range f.Calls() {
		if strings.HasPrefix(c, "restore:") {
			t.Fatalf("the engine was asked to restore despite the undo snapshot failing: %v", f.Calls())
		}
	}
	// No marker either: nothing was touched, so nothing is in an unknown state.
	if _, err := os.Stat(filepath.Join(cfg.StateDir(), "userdata-restoring")); !os.IsNotExist(err) {
		t.Error("a marker was left behind by a restore that never started")
	}
}

// Restoring into a new directory touches nothing that exists, so it must skip the undo
// snapshot entirely — paying for a full extra backup to copy files into an empty folder
// would be a tax on the safe operation.
func TestRestoreToADirectoryTakesNoUndoSnapshot(t *testing.T) {
	f := newFakeUserData()
	u, cfg := newUserData(t, f)
	dest := filepath.Join(cfg.DataRoot, "Restored")

	if err := u.Restore(context.Background(), udStamp, apps.UserDataRestoreOpts{Dest: dest}, nil); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if st := waitIdle(t, u); st.Error != "" {
		t.Fatalf("restore failed: %s", st.Error)
	}

	if got := strings.Join(f.Calls(), " "); got != "restore:"+udStamp {
		t.Errorf("calls = %q; want only the restore", got)
	}
	if f.lastOpts.Dest != dest {
		t.Errorf("dest = %q; want %q", f.lastOpts.Dest, dest)
	}
	// And it is not the destructive mode, so nothing should have been marked.
	if _, err := os.Stat(filepath.Join(cfg.StateDir(), "userdata-restoring")); !os.IsNotExist(err) {
		t.Error("restoring into a new directory must not write the in-place marker")
	}
}

// The marker outlives the process, which is the entire reason it is a file: an in-place
// restore cut short by a reboot leaves a tree that is neither state, and the next
// process has to say so.
func TestAnInterruptedInPlaceRestoreIsReportedAfterARestart(t *testing.T) {
	f := newFakeUserData()
	u, cfg := newUserData(t, f)

	// A restore that starts and never clears its marker is exactly what a power cut
	// leaves behind.
	if err := os.WriteFile(filepath.Join(cfg.StateDir(), "userdata-restoring"), []byte(udStamp), 0o644); err != nil {
		t.Fatal(err)
	}

	store := backupconfig.New(filepath.Join(cfg.StateDir(), "backup.json"))
	fresh := NewUserData(cfg, New(f), store) // a new process, same disk

	st := fresh.State()
	if u.State().Interrupted {
		t.Error("the coordinator that predates the marker should not have seen it")
	}
	if !st.Interrupted {
		t.Fatal("an interrupted restore was not reported after a restart")
	}
	if st.InterruptedStamp != udStamp {
		t.Errorf("InterruptedStamp = %q; want %q so the page can offer to finish the job",
			st.InterruptedStamp, udStamp)
	}
}

// A successful in-place restore clears the marker, or every later page load claims a
// problem that is over.
func TestASuccessfulInPlaceRestoreClearsTheMarker(t *testing.T) {
	f := newFakeUserData()
	u, cfg := newUserData(t, f)

	if err := u.Restore(context.Background(), udStamp, apps.UserDataRestoreOpts{}, nil); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if st := waitIdle(t, u); st.Error != "" || st.Interrupted {
		t.Fatalf("state after a clean restore = %+v; want no error and no marker", st)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir(), "userdata-restoring")); !os.IsNotExist(err) {
		t.Error("the marker survived a successful restore")
	}
}

// A failed restore leaves the marker: the tree really is in an unknown state, and that
// is the one case the warning exists for.
func TestAFailedInPlaceRestoreLeavesTheMarker(t *testing.T) {
	f := newFakeUserData()
	f.restoreErr = errors.New("connection reset")
	u, cfg := newUserData(t, f)

	if err := u.Restore(context.Background(), udStamp, apps.UserDataRestoreOpts{}, nil); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	st := waitIdle(t, u)

	if st.Error == "" {
		t.Fatal("a failed restore must be reported")
	}
	if !st.Interrupted {
		t.Error("a failed in-place restore must leave the tree marked as interrupted")
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir(), "userdata-restoring")); err != nil {
		t.Errorf("marker missing after a failed restore: %v", err)
	}
}

// Two writers over one tree produce a state that came from neither backup, so a second
// restore is refused rather than queued.
func TestASecondRestoreIsRefusedWhileOneIsRunning(t *testing.T) {
	f := newFakeUserData()
	release := make(chan struct{})
	u, _ := newUserData(t, f)

	// Hold the first restore open inside the engine.
	slow := &slowRestore{fakeUserData: f, release: release}
	u.set = New(slow)

	if err := u.Restore(context.Background(), udStamp, apps.UserDataRestoreOpts{}, nil); err != nil {
		t.Fatalf("first Restore: %v", err)
	}
	slow.waitStarted(t)

	err := u.Restore(context.Background(), udStamp, apps.UserDataRestoreOpts{}, nil)
	if err == nil {
		t.Error("a second concurrent restore was accepted; it must be refused")
	}
	close(release)
	waitIdle(t, u)
}

// A name that is not a backup name is refused before any engine is asked, and before
// the undo snapshot is paid for.
func TestRestoreRefusesANameThatIsNotOne(t *testing.T) {
	f := newFakeUserData()
	u, _ := newUserData(t, f)

	for _, bad := range []string{"notes", "../../etc/passwd", ""} {
		if err := u.Restore(context.Background(), bad, apps.UserDataRestoreOpts{}, nil); err == nil {
			t.Errorf("Restore(%q) was accepted", bad)
		}
	}
	if len(f.Calls()) != 0 {
		t.Errorf("the engine was asked to do something for an invalid name: %v", f.Calls())
	}
}

// A relative destination would be resolved against whatever the process's working
// directory happens to be, which is not a decision the caller gets to make implicitly.
func TestRestoreRefusesARelativeDestination(t *testing.T) {
	u, _ := newUserData(t, newFakeUserData())
	if err := u.Restore(context.Background(), udStamp, apps.UserDataRestoreOpts{Dest: "Restored"}, nil); err == nil {
		t.Error("a relative destination was accepted")
	}
}

// slowRestore holds a restore open so concurrency can be tested.
type slowRestore struct {
	*fakeUserData
	release chan struct{}
}

func (s *slowRestore) waitStarted(t *testing.T) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if s.fakeUserData.State() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the restore never reached the engine")
}

func (s *slowRestore) RestoreUserData(ctx context.Context, stamp string, opts apps.UserDataRestoreOpts, emit func(apps.Event)) error {
	s.fakeUserData.record("restore:" + stamp)
	<-s.release
	return nil
}

// State reports whether the engine has been asked to restore yet.
func (f *fakeUserData) State() bool {
	for _, c := range f.Calls() {
		if strings.HasPrefix(c, "restore:") {
			return true
		}
	}
	return false
}

// A restore while a backup run is going produces a snapshot of a half-rewritten tree —
// a state that never existed, which then counts against retention and can push out the
// good snapshot being restored from. The doc for Restore has always claimed this guard;
// this is the test that makes the claim true.
func TestRestoreIsRefusedWhileABackupIsRunning(t *testing.T) {
	f := newFakeUserData()
	u, _ := newUserData(t, f)
	u.Busy = func() bool { return true }

	err := u.Restore(context.Background(), udStamp, apps.UserDataRestoreOpts{}, nil)
	if err == nil {
		t.Fatal("a restore was accepted while a backup was running")
	}
	if !strings.Contains(err.Error(), "backup is running") {
		t.Errorf("error = %q; want it to name the backup as the reason", err)
	}
	if len(f.Calls()) != 0 {
		t.Errorf("the engine was asked to do something anyway: %v", f.Calls())
	}
}

// Copying the set back beside itself is the one restore that can fill the data disk, and
// filling it does not merely fail the restore — every app writing to that disk starts
// failing too.
func TestCopyRestoreIsRefusedWithoutRoom(t *testing.T) {
	f := newFakeUserData()
	// Larger than any test filesystem has free.
	f.list[0].Size = 1 << 62
	u, cfg := newUserData(t, f)

	err := u.Restore(context.Background(), udStamp,
		apps.UserDataRestoreOpts{Dest: filepath.Join(cfg.DataRoot, "Restored")}, nil)
	if err == nil {
		t.Fatal("a copy restore that cannot fit was accepted")
	}
	if !strings.Contains(err.Error(), "free space") {
		t.Errorf("error = %q; want it to say there is not enough room", err)
	}
	if len(f.Calls()) != 0 {
		t.Errorf("the engine was asked to restore anyway: %v", f.Calls())
	}
}

// The same restore in place needs no extra room — it streams over what is there — so the
// guard must not apply to it. Refusing here would make a full disk unrecoverable by the
// one operation that could fix it.
func TestInPlaceRestoreIsNotRefusedForSpace(t *testing.T) {
	f := newFakeUserData()
	f.list[0].Size = 1 << 62
	u, _ := newUserData(t, f)

	if err := u.Restore(context.Background(), udStamp, apps.UserDataRestoreOpts{}, nil); err != nil {
		t.Fatalf("an in-place restore was refused for space it does not need: %v", err)
	}
	if st := waitIdle(t, u); st.Error != "" {
		t.Fatalf("restore failed: %s", st.Error)
	}
}
