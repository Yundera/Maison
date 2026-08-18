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

func TestStartUninstallZipReportsProgressOnBothTracks(t *testing.T) {
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
	// Both tracks must have been reported, in order, or the tile's bar would
	// jump straight from empty to gone.
	if len(phases) < 2 || phases[0] != PhaseRemove {
		t.Fatalf("phases = %v; want remove first", phases)
	}
	var sawArchive bool
	for _, p := range phases {
		if p == PhaseArchive {
			sawArchive = true
		}
	}
	if !sawArchive {
		t.Errorf("phases = %v; want an archive phase", phases)
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
