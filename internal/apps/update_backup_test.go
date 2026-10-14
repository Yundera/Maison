package apps_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/backup"
	"github.com/yundera/maison/internal/backup/backuptest"
)

// hugeApp makes jellyfin's folder measure far more than any disk has free, with a
// sparse file: the size walk reads apparent sizes, so it costs no real disk.
func hugeApp(t *testing.T, appsDir string) {
	t.Helper()
	f, err := os.Create(filepath.Join(appsDir, "jellyfin", "db", "library.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(8 << 40); err != nil { // 8 TiB
		t.Skipf("cannot make a sparse file here: %v", err)
	}
}

func hasCall(f *backuptest.Fake, prefix string) bool {
	for _, c := range f.Calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// The mestio Immich case: an app that does not fit beside itself on a box backing up
// offsite. The local destination is left out of that one backup, and the update goes
// ahead with the repository's.
func TestUpdateTargetsLeavesOutALocalDestinationTheAppDoesNotFit(t *testing.T) {
	r, cfg := newRegistry(t)
	hugeApp(t, cfg.AppsDir())
	local, remote := backuptest.NewLocalLike(apps.EngineLocal), backuptest.NewRemote("kopia")
	set := backup.New(local, remote)
	if err := set.SetWriters([]string{apps.EngineLocal, "kopia"}); err != nil {
		t.Fatal(err)
	}
	r.Engines = set

	targets, dropped, err := r.UpdateTargets("jellyfin")
	if err != nil {
		t.Fatalf("UpdateTargets: %v", err)
	}
	if len(targets) != 1 || targets[0].ID() != "kopia" {
		t.Errorf("targets = %v, want the repository alone", targets)
	}
	if !strings.Contains(dropped, "this server's disk") {
		t.Errorf("dropped = %q, want it to say the local copy was left out", dropped)
	}
}

// With the local disk the only destination, an app that does not fit is refused, with
// the numbers.
func TestUpdateTargetsRefusesWhenNoDestinationCanTakeTheBackup(t *testing.T) {
	r, cfg := newRegistry(t)
	hugeApp(t, cfg.AppsDir())
	r.Engines = backup.New(backuptest.NewLocalLike(apps.EngineLocal))

	_, _, err := r.UpdateTargets("jellyfin")
	var room *apps.NoRoomError
	if !errors.As(err, &room) || room.Needed <= room.Free {
		t.Fatalf("err = %v, want a NoRoomError with needed > free", err)
	}
}

// Backups switched off is the owner's answer, not a failure: the update goes ahead
// without one (the server turns this into a warning).
func TestUpdateTargetsWithNoDestinationEnabled(t *testing.T) {
	r, _ := newRegistry(t)
	set := backup.New(backuptest.NewLocalLike(apps.EngineLocal))
	if err := set.SetWriters(nil); err != nil {
		t.Fatal(err)
	}
	r.Engines = set

	if _, _, err := r.UpdateTargets("jellyfin"); !errors.Is(err, apps.ErrNoBackupEngine) {
		t.Fatalf("err = %v, want ErrNoBackupEngine", err)
	}
}

// While an update holds the app, the nightly run skips it and a restore is refused —
// either would stop and restart the app under the update.
func TestReserveUpdateKeepsBackupsAndRestoresOff(t *testing.T) {
	r, _ := newRegistry(t)
	release, err := r.ReserveUpdate("jellyfin")
	if err != nil {
		t.Fatalf("ReserveUpdate: %v", err)
	}
	if _, err := r.BackupTracked(context.Background(), "jellyfin", "", false, nil); !errors.Is(err, apps.ErrBackupInFlight) {
		t.Errorf("BackupTracked during an update = %v, want ErrBackupInFlight (the nightly reads it as skipped)", err)
	}
	if _, err := r.ReserveUpdate("jellyfin"); !errors.Is(err, apps.ErrUpdating) {
		t.Errorf("second ReserveUpdate = %v, want ErrUpdating", err)
	}
	release()
	if _, err := r.BackupTracked(context.Background(), "jellyfin", "", false, nil); err != nil {
		t.Errorf("BackupTracked after the update = %v, want it to run", err)
	}
}

// A failed update is undone by restoring its backup over the app, and nothing of the
// failed state is kept: no leftover archive beside the backups.
func TestRestoreOverPutsTheBackupBackAndKeepsNothingOfTheFailure(t *testing.T) {
	r, cfg := newRegistry(t)
	res, err := r.Backup(context.Background(), "jellyfin", "", false, nil)
	name, err := oneName(t, res, err)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	db := filepath.Join(cfg.AppsDir(), "jellyfin", "db", "data.sqlite")
	if err := os.WriteFile(db, []byte("broken by the update"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := r.RestoreOver(context.Background(), "jellyfin", name); err != nil {
		t.Fatalf("RestoreOver: %v", err)
	}
	if got, _ := os.ReadFile(db); string(got) != "rows" {
		t.Errorf("data = %q after the restore, want the backed-up state", got)
	}
	if left := apps.ListBackups(cfg.BackupsDir(), "jellyfin"); len(left) != 0 {
		t.Errorf("archives after the restore = %+v, want none — no leftover of the failed state", left)
	}
}

// From a repository the backup is written over the app in place: no undo snapshot of
// the failed state first, and no copy fetched beside it, so no room is needed.
func TestRestoreOverFromARepositoryRestoresInPlaceWithoutAnUndo(t *testing.T) {
	r, _ := newRegistry(t)
	remote := backuptest.NewRemote("kopia")
	remote.Seed("jellyfin", "2026-01-01_120000")
	r.Engines = backup.New(remote)

	if err := r.RestoreOver(context.Background(), "jellyfin", "2026-01-01_120000"); err != nil {
		t.Fatalf("RestoreOver: %v", err)
	}
	if !hasCall(remote, "restore-in-place:jellyfin/2026-01-01_120000") {
		t.Errorf("calls = %v, want an in-place restore", remote.Calls)
	}
	if hasCall(remote, "snapshot:") || hasCall(remote, "materialize:") {
		t.Errorf("calls = %v, want no undo snapshot and no fetch", remote.Calls)
	}
}

// A backup written to several destinations is several chances to put the app back.
func TestRestoreOverTriesTheNextDestinationWhenOneFails(t *testing.T) {
	r, _ := newRegistry(t)
	first, second := backuptest.NewRemote("kopia"), backuptest.NewRemote("rclone")
	first.Seed("jellyfin", "2026-01-01_120000")
	second.Seed("jellyfin", "2026-01-01_120000")
	first.RestoreErr = errors.New("repository unreachable")
	set := backup.New(first, second)
	if err := set.SetWriters([]string{"kopia", "rclone"}); err != nil {
		t.Fatal(err)
	}
	r.Engines = set

	if err := r.RestoreOver(context.Background(), "jellyfin", "2026-01-01_120000"); err != nil {
		t.Fatalf("RestoreOver: %v", err)
	}
	if !hasCall(second, "restore-in-place:jellyfin/2026-01-01_120000") {
		t.Errorf("second destination calls = %v, want the restore to fall through to it", second.Calls)
	}
}
