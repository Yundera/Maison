package apps

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yundera/maison/internal/config"
)

// newTestRegistry builds a Registry over a temp DATA_ROOT with no Docker client:
// the container-removal step is then skipped, leaving the on-disk half of an
// uninstall (which is the half that carries the data-safety contract) testable
// without a daemon.
func newTestRegistry(t *testing.T) (r *Registry, appsDir, backupsDir string) {
	t.Helper()
	root := t.TempDir()
	cfg := config.Config{DataRoot: root}
	if err := os.MkdirAll(cfg.AppsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	return New(cfg, nil), cfg.AppsDir(), cfg.BackupsDir()
}

// waitFor polls cond for up to a second — StartUninstall is detached, so the
// test has to wait for its goroutine rather than for the call.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestStartUninstallArchivesAndClearsItsProgress(t *testing.T) {
	r, appsDir, backupsDir := newTestRegistry(t)
	seedApp(t, filepath.Join(appsDir, "jellyfin"))

	if err := r.StartUninstall("jellyfin", false); err != nil {
		t.Fatalf("StartUninstall: %v", err)
	}
	// It is tracked from the moment it is asked for, so the tile carries progress
	// without waiting for the first event.
	if got := r.Uninstalls(); len(got) != 1 || got[0].ID != "jellyfin" {
		t.Fatalf("Uninstalls() = %+v; want one entry for jellyfin", got)
	}

	waitFor(t, "the uninstall to finish", func() bool { return len(r.Uninstalls()) == 0 })

	if _, err := os.Stat(filepath.Join(appsDir, "jellyfin")); !os.IsNotExist(err) {
		t.Errorf("app folder still present after uninstall")
	}
	if got := ListBackups(backupsDir, "jellyfin"); len(got) != 1 {
		t.Fatalf("ListBackups = %+v; want the uninstall archive", got)
	}
}

func TestStartUninstallZipReportsProgressOnEveryTrack(t *testing.T) {
	r, appsDir, backupsDir := newTestRegistry(t)
	seedApp(t, filepath.Join(appsDir, "jellyfin"))

	var mu sync.Mutex
	var phases []string
	r.OnProgress = func() {
		mu.Lock()
		defer mu.Unlock()
		for _, st := range r.Uninstalls() {
			if len(phases) == 0 || phases[len(phases)-1] != st.Phase {
				phases = append(phases, st.Phase)
			}
		}
	}

	if err := r.StartUninstall("jellyfin", true); err != nil {
		t.Fatalf("StartUninstall: %v", err)
	}
	waitFor(t, "the uninstall to finish", func() bool { return len(r.Uninstalls()) == 0 })

	backups := ListBackups(backupsDir, "jellyfin")
	if len(backups) != 1 || !backups[0].Zip {
		t.Fatalf("ListBackups = %+v; want one zip archive", backups)
	}
	mu.Lock()
	defer mu.Unlock()
	// The order is the contract, not decoration: the app is backed up, that backup is
	// committed, and only then is anything removed. A bar that reported "Removing"
	// first would be describing the one thing that has definitely not happened yet.
	if !isSubsequence(phases, []string{PhaseBackup, PhaseArchive, PhaseRemove}) {
		t.Fatalf("phases = %v; want backup, then archive, then remove", phases)
	}
}

// isSubsequence reports whether want appears in got in order, allowing repeats and
// other phases in between — the tracks are reported as they progress, so a test that
// demanded an exact sequence would break on a chattier engine.
func isSubsequence(got, want []string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}

// The local engine must not copy the app in order to uninstall it.
//
// Its ordinary snapshot is a full second copy, which is what makes EstimateBackup
// refuse an app bigger than half the free disk. If an uninstall went through that, an
// app could be installed and then not removed — so Consume has to reach the engine,
// and the engine has to answer with a rename rather than a copy.
func TestUninstallNeverCopiesTheAppOnTheLocalEngine(t *testing.T) {
	r, appsDir, backupsDir := newTestRegistry(t)
	appDir := filepath.Join(appsDir, "jellyfin")
	seedApp(t, appDir)

	if _, err := r.Uninstall(context.Background(), "jellyfin", false, nil); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	if _, err := os.Stat(appDir); !os.IsNotExist(err) {
		t.Errorf("app folder still present after uninstall")
	}
	got := ListBackups(backupsDir, "jellyfin")
	if len(got) != 1 {
		t.Fatalf("ListBackups = %+v; want the uninstall backup", got)
	}
	// The archive is the app's own inode, moved — so its contents came across without
	// being read, and nothing was left behind in a staging directory.
	if body, err := os.ReadFile(filepath.Join(AppBackupDir(backupsDir, "jellyfin"), got[0].Name, "db", "data.sqlite")); err != nil || string(body) != "rows" {
		t.Errorf("archived data = %q, %v; want the app's own file", body, err)
	}
	entries, err := os.ReadDir(AppBackupDir(backupsDir, "jellyfin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("backup dir holds %d entries; want only the archive (a staging copy was left behind)", len(entries))
	}
}

// Consume must not let an engine take the folder before the commit point.
//
// Snapshot is not durable — an interrupted backup is discarded — so a local engine that
// moved the app folder in Snapshot would leave the user's only copy of their data in a
// staging directory that nothing lists, for the whole window until Commit ran.
func TestLocalConsumeSnapshotLeavesTheAppWhereItIs(t *testing.T) {
	r, appsDir, backupsDir := newTestRegistry(t)
	appDir := filepath.Join(appsDir, "jellyfin")
	seedApp(t, appDir)

	p := NewLocalProvider(r.cfg)
	opts := SnapshotOpts{Pass: 2, Consume: true}
	if err := p.Snapshot(context.Background(), "jellyfin", "2026-01-01_120000", opts, nil); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if _, err := os.Stat(appDir); err != nil {
		t.Fatalf("app folder was moved by Snapshot, before the commit point: %v", err)
	}
	// Abort is what runs on a failure from here, and it must not be able to reach the
	// app: on this path the app folder is still the only copy of the user's data.
	if err := p.Abort(context.Background(), "jellyfin", "2026-01-01_120000"); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if _, err := os.Stat(appDir); err != nil {
		t.Fatalf("Abort destroyed the app folder: %v", err)
	}
	if got := ListBackups(backupsDir, "jellyfin"); len(got) != 0 {
		t.Errorf("ListBackups = %+v; an uncommitted snapshot must not be listed", got)
	}
}

// A system app declares itself one, in its own compose — there is no
// operator-side list any more.
func TestStartUninstallRefusesSystemApps(t *testing.T) {
	root := t.TempDir()
	appsDir := filepath.Join(root, "AppData")
	if err := os.MkdirAll(appsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(appsDir, "maison")
	seedApp(t, dir)
	writeCompose(t, dir, "services: {}\nx-compose-app:\n  view: system\n")
	r := New(config.Config{DataRoot: root}, nil)

	if err := r.StartUninstall("maison", false); err != ErrProtected {
		t.Fatalf("StartUninstall = %v; want ErrProtected", err)
	}
	if len(r.Uninstalls()) != 0 {
		t.Errorf("a refused uninstall was tracked: %+v", r.Uninstalls())
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("system app folder was touched: %v", err)
	}
}

// Stop is refused for the same reason uninstall is — taking the dashboard down
// from its own tile leaves nothing running to bring it back.
func TestStopRefusesSystemApps(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "AppData", "maison")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	seedApp(t, dir)
	writeCompose(t, dir, "services: {}\nx-compose-app:\n  view: system\n")
	r := New(config.Config{DataRoot: root}, nil)

	if err := r.Stop(context.Background(), "maison"); err != ErrProtected {
		t.Fatalf("Stop = %v; want ErrProtected", err)
	}
}

// An ordinary app is neither, whatever else its compose says.
func TestOrdinaryAppIsNotProtected(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "AppData", "nextcloud")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	seedApp(t, dir)
	writeCompose(t, dir, "services: {}\nx-compose-app:\n  title: Nextcloud\n")
	if New(config.Config{DataRoot: root}, nil).Protected("nextcloud") {
		t.Fatal("an app with no `view: system` was reported protected")
	}
}

// writeCompose replaces a seeded app's compose file.
func writeCompose(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
