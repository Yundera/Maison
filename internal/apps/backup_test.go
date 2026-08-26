package apps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// read is a small helper: the tests care about file *contents* surviving a
// backup, since that is the whole contract.
func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestBackupFolderSnapshotsTheApp(t *testing.T) {
	r, appsDir, backupsDir := newTestRegistry(t)
	seedApp(t, filepath.Join(appsDir, "jellyfin"))

	name, err := r.Backup(context.Background(), "jellyfin", "", false, nil)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// The app is untouched — a backup is not an uninstall.
	if _, err := os.Stat(filepath.Join(appsDir, "jellyfin", ".env")); err != nil {
		t.Errorf("app folder damaged by a backup: %v", err)
	}
	snap := filepath.Join(backupsDir, "jellyfin", name)
	if got := read(t, filepath.Join(snap, "db", "data.sqlite")); got != "rows" {
		t.Errorf("snapshot data = %q, want %q", got, "rows")
	}
	// The compose and .env are in it too: that is what makes a Maison archive
	// restorable on its own, rather than a bag of data with no app around it.
	for _, f := range []string{"docker-compose.yml", ".env"} {
		if _, err := os.Stat(filepath.Join(snap, f)); err != nil {
			t.Errorf("%s missing from the snapshot: %v", f, err)
		}
	}
	if got := ListBackups(backupsDir, "jellyfin"); len(got) != 1 || got[0].Zip {
		t.Errorf("ListBackups = %+v; want one folder archive", got)
	}
}

func TestBackupZipLeavesNoStagingFolder(t *testing.T) {
	r, appsDir, backupsDir := newTestRegistry(t)
	seedApp(t, filepath.Join(appsDir, "jellyfin"))

	name, err := r.Backup(context.Background(), "jellyfin", "", true, nil)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if !strings.HasSuffix(name, ".zip") {
		t.Fatalf("name = %q; want a .zip", name)
	}

	// Both on disk at once is exactly what the free-space guard budgets for; both
	// still there afterwards is a leak that fills the disk one backup at a time.
	entries, err := os.ReadDir(filepath.Join(backupsDir, "jellyfin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != name {
		var got []string
		for _, e := range entries {
			got = append(got, e.Name())
		}
		t.Fatalf("backup dir = %v; want only %q", got, name)
	}

	// And it restores, which is the only proof the zip is whole.
	if err := os.RemoveAll(filepath.Join(appsDir, "jellyfin")); err != nil {
		t.Fatal(err)
	}
	if err := RestoreBackup(backupsDir, appsDir, "jellyfin", name); err != nil {
		t.Fatalf("RestoreBackup: %v", err)
	}
	if got := read(t, filepath.Join(appsDir, "jellyfin", "db", "data.sqlite")); got != "rows" {
		t.Errorf("restored data = %q, want %q", got, "rows")
	}
}

func TestBackupRefusesWhenTheDiskIsTooFull(t *testing.T) {
	r, appsDir, backupsDir := newTestRegistry(t)
	seedApp(t, filepath.Join(appsDir, "jellyfin"))

	// A free-space reading is only available for a real filesystem, so rather than
	// filling one we check the arithmetic the guard runs on: needed scales with the
	// artefact, and the zip case budgets for the snapshot and the zip together.
	folder, err := r.EstimateBackup("jellyfin", "", false)
	if err != nil {
		t.Fatalf("EstimateBackup: %v", err)
	}
	zipped, err := r.EstimateBackup("jellyfin", "", true)
	if err != nil {
		t.Fatalf("EstimateBackup: %v", err)
	}
	if folder.Size == 0 {
		t.Fatal("estimate measured the app folder as empty")
	}
	if zipped.Needed <= folder.Needed {
		t.Errorf("zip needs %d, folder needs %d; a zip must budget for both copies",
			zipped.Needed, folder.Needed)
	}

	// An app with no folder cannot be backed up, and says so before any work.
	if _, err := r.EstimateBackup("nosuchapp", "", false); err == nil {
		t.Error("estimating a missing app should fail")
	}
	if err := r.StartBackup("nosuchapp", "", false); err == nil {
		t.Error("backing up a missing app should fail")
	}
	if len(r.Backups()) != 0 {
		t.Errorf("a refused backup was tracked: %+v", r.Backups())
	}
	if entries, _ := os.ReadDir(backupsDir); len(entries) != 0 {
		t.Errorf("a refused backup left something on disk: %v", entries)
	}
}

func TestStartBackupTracksThenClearsItsProgress(t *testing.T) {
	r, appsDir, backupsDir := newTestRegistry(t)
	seedApp(t, filepath.Join(appsDir, "jellyfin"))

	var phases []string
	r.OnProgress = func() {
		for _, st := range r.Backups() {
			if len(phases) == 0 || phases[len(phases)-1] != st.Phase {
				phases = append(phases, st.Phase)
			}
		}
	}

	if err := r.StartBackup("jellyfin", "", false); err != nil {
		t.Fatalf("StartBackup: %v", err)
	}
	// Tracked from the moment it is asked for, so the tile carries progress without
	// waiting for the first event.
	if got := r.Backups(); len(got) != 1 || got[0].ID != "jellyfin" {
		t.Fatalf("Backups() = %+v; want one entry for jellyfin", got)
	}
	waitFor(t, "the backup to finish", func() bool { return len(r.Backups()) == 0 })

	if got := ListBackups(backupsDir, "jellyfin"); len(got) != 1 {
		t.Fatalf("ListBackups = %+v; want the archive", got)
	}
	// The copy pass must have been reported, or the tile's bar would jump straight
	// from empty to gone.
	if len(phases) == 0 || phases[0] != PhaseCopy {
		t.Errorf("phases = %v; want copy first", phases)
	}
}

func TestRestoreArchivesTheStateItReplaces(t *testing.T) {
	r, appsDir, backupsDir := newTestRegistry(t)
	app := filepath.Join(appsDir, "jellyfin")
	seedApp(t, app)

	name, err := r.Backup(context.Background(), "jellyfin", "", false, nil)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// Move the app on: the file the backup holds is changed, and another is added.
	if err := os.WriteFile(filepath.Join(app, "db", "data.sqlite"), []byte("newer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "later.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Stamps have one-second resolution, so a restore in the same second as the
	// backup would collide with it. Real use never does; the test must wait.
	time.Sleep(time.Second)
	if err := r.Restore(context.Background(), "jellyfin", "", name, nil); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if got := read(t, filepath.Join(app, "db", "data.sqlite")); got != "rows" {
		t.Errorf("restored data = %q, want %q", got, "rows")
	}
	// A restore is a replacement, not a merge: what the archive does not have is
	// gone from the live folder.
	if _, err := os.Stat(filepath.Join(app, "later.txt")); !os.IsNotExist(err) {
		t.Error("a file absent from the archive survived the restore")
	}
	// And the state it replaced was archived, so the restore is undoable. The
	// folder archive it restored *from* was consumed by the rename, so the count
	// stays at one.
	got := ListBackups(backupsDir, "jellyfin")
	if len(got) != 1 {
		t.Fatalf("ListBackups = %+v; want the pre-restore archive", got)
	}
	if s := read(t, filepath.Join(backupsDir, "jellyfin", got[0].Name, "db", "data.sqlite")); s != "newer" {
		t.Errorf("pre-restore archive holds %q, want %q", s, "newer")
	}
}

func TestMirrorCopiesOnlyTheDelta(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	seedApp(t, src)

	var firstPass int64
	if err := mirror(src, dst, func(_, total int64) { firstPass = total }); err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if firstPass == 0 {
		t.Fatal("first pass measured no work")
	}

	// Nothing changed: the second pass has nothing to do. This is what makes the
	// app's downtime short rather than proportional to its size.
	var secondPass int64 = -1
	if err := mirror(src, dst, func(_, total int64) { secondPass = total }); err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if secondPass != 0 {
		t.Errorf("unchanged second pass measured %d bytes; want 0", secondPass)
	}

	// Now change one file, add one, and delete one.
	if err := os.WriteFile(filepath.Join(src, "db", "data.sqlite"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "new.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(src, ".env")); err != nil {
		t.Fatal(err)
	}
	if err := mirror(src, dst, nil); err != nil {
		t.Fatalf("mirror: %v", err)
	}

	if got := read(t, filepath.Join(dst, "db", "data.sqlite")); got != "changed" {
		t.Errorf("changed file = %q, want %q", got, "changed")
	}
	if got := read(t, filepath.Join(dst, "new.txt")); got != "new" {
		t.Errorf("added file = %q, want %q", got, "new")
	}
	// A deletion must propagate, or the snapshot would hold data the app has
	// dropped — and a restore would bring it back.
	if _, err := os.Stat(filepath.Join(dst, ".env")); !os.IsNotExist(err) {
		t.Error("deleted file survived in the mirror")
	}
}

func TestMirrorPrunesDeletedSubtreesAndPartials(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	seedApp(t, src)
	if err := mirror(src, dst, nil); err != nil {
		t.Fatalf("mirror: %v", err)
	}

	// A whole directory disappears from the source...
	if err := os.RemoveAll(filepath.Join(src, "db")); err != nil {
		t.Fatal(err)
	}
	// ...and a previous run died mid-copy, leaving a temporary behind.
	if err := os.WriteFile(filepath.Join(dst, "stale.partial"), []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := mirror(src, dst, nil); err != nil {
		t.Fatalf("mirror: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dst, "db")); !os.IsNotExist(err) {
		t.Error("deleted subtree survived in the mirror")
	}
	if _, err := os.Stat(filepath.Join(dst, "stale.partial")); !os.IsNotExist(err) {
		t.Error("orphaned .partial survived in the mirror")
	}
}

func TestMirrorSkipsIrregularFiles(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	seedApp(t, src)
	// A dangling symlink stands in for the sockets and fifos apps leave in their
	// folders: not data, and fatal to a naive copy that tries to open them.
	if err := os.Symlink("/nonexistent/target", filepath.Join(src, "app.sock")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := mirror(src, dst, nil); err != nil {
		t.Fatalf("mirror over an irregular file: %v", err)
	}
	if got := read(t, filepath.Join(dst, "db", "data.sqlite")); got != "rows" {
		t.Errorf("data = %q, want %q", got, "rows")
	}
	if _, err := os.Lstat(filepath.Join(dst, "app.sock")); !os.IsNotExist(err) {
		t.Error("irregular file was copied into the snapshot")
	}
}

// End to end through the real local engine: the byte counts the engine reports have
// to survive all the way onto the tile's state, or none of the rest of this matters.
//
// The local engine is the strict case — it knows its total exactly, because mirror
// walks the tree before copying it — so anything missing here is Maison dropping it
// rather than the engine failing to say it.
func TestBackupCarriesByteCountsOntoTheTile(t *testing.T) {
	r, appsDir, _ := newTestRegistry(t)
	seedApp(t, filepath.Join(appsDir, "jellyfin"))

	var sawTotal, sawDone bool
	r.OnProgress = func() {
		for _, st := range r.Backups() {
			if st.Total > 0 {
				sawTotal = true
			}
			if st.Done > 0 {
				sawDone = true
			}
		}
	}

	if _, err := r.BackupTracked(context.Background(), "jellyfin", "", false, nil); err != nil {
		t.Fatalf("BackupTracked: %v", err)
	}
	if !sawTotal || !sawDone {
		t.Errorf("tile saw total=%v done=%v; want both reported during the backup", sawTotal, sawDone)
	}
	// A finished backup is not tracked any more — the tile drops the overlay rather
	// than keeping a full bar on it forever.
	if got := r.Backups(); len(got) != 0 {
		t.Errorf("Backups() = %+v after success, want the overlay cleared", got)
	}
}

// The whole-box run needs to see exactly what the tile sees, from one tracker — two
// would derive two slightly different estimates of the same bytes.
func TestASecondObserverGetsTheSameDerivedNumbers(t *testing.T) {
	r, appsDir, _ := newTestRegistry(t)
	seedApp(t, filepath.Join(appsDir, "jellyfin"))

	var mu sync.Mutex
	var mirrored []BackupEvent
	if _, err := r.BackupTracked(context.Background(), "jellyfin", "", false, func(ev BackupEvent) {
		mu.Lock()
		mirrored = append(mirrored, ev)
		mu.Unlock()
	}); err != nil {
		t.Fatalf("BackupTracked: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(mirrored) == 0 {
		t.Fatal("the second observer received nothing")
	}
	var withBytes int
	for _, ev := range mirrored {
		if ev.Total > 0 {
			withBytes++
		}
	}
	if withBytes == 0 {
		t.Error("the mirrored events carried no byte counts")
	}
	if last := mirrored[len(mirrored)-1]; last.Phase != PhaseDone {
		t.Errorf("last mirrored phase = %q, want %q — the observer must see the end", last.Phase, PhaseDone)
	}
}

// An app already being backed up is reported as such rather than backed up twice, so
// the whole-box run can tell "already in hand" apart from "went wrong".
func TestASecondBackupOfTheSameAppIsRefusedNotDuplicated(t *testing.T) {
	r, appsDir, _ := newTestRegistry(t)
	seedApp(t, filepath.Join(appsDir, "jellyfin"))

	if err := r.beginBackup("jellyfin"); err != nil {
		t.Fatalf("beginBackup: %v", err)
	}
	_, err := r.BackupTracked(context.Background(), "jellyfin", "", false, nil)
	if !errors.Is(err, ErrBackupInFlight) {
		t.Errorf("err = %v, want ErrBackupInFlight", err)
	}
}
