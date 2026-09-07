// Package apps_test holds the tests that exercise the Registry through the backup
// engine seam.
//
// They live in an external test package because the fake engine (internal/backup/
// backuptest) imports internal/apps, so a test inside package apps could not import
// it without a cycle. The cost is that only exported identifiers are reachable,
// which is why this file seeds its own fixture rather than reusing seedApp.
package apps_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/backup"
	"github.com/yundera/maison/internal/backup/backuptest"
	"github.com/yundera/maison/internal/config"
)

// newRegistry builds a Registry over a temp DATA_ROOT with no Docker client, so the
// on-disk half of a backup runs without a daemon. With dx nil the app never reads as
// running, so no stop and no restart happen — see the note at the end of this file.
func newRegistry(t *testing.T) (*apps.Registry, config.Config) {
	t.Helper()
	cfg := config.Config{DataRoot: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(cfg.AppsDir(), "jellyfin", "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"docker-compose.yml": "services: {}\n",
		".env":               "PUID=1000\n",
		"db/data.sqlite":     "rows",
	} {
		if err := os.WriteFile(filepath.Join(cfg.AppsDir(), "jellyfin", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return apps.New(cfg, nil), cfg
}

// The whole point of the seam: a configured engine receives the backup, in the
// two-pass-then-commit order the registry owns.
// oneName is the single backup name a test with one engine expects. BackupTo returns a
// row per destination; a test that registered one engine is entitled to say so rather
// than indexing a slice at every call site.
func oneName(t *testing.T, res []apps.Result, err error) (string, error) {
	t.Helper()
	if err != nil {
		return "", err
	}
	if len(res) != 1 {
		t.Fatalf("expected one engine result, got %d", len(res))
	}
	return res[0].Name, res[0].Err
}

// setWriters points every trigger at one engine — what SetWriter used to mean, which is
// still what a test wanting "backups go here" is asking for.
func setWriters(set *backup.Set, ids ...string) error {
	for _, t := range apps.Triggers {
		if err := set.SetWriters(t, ids); err != nil {
			return err
		}
	}
	return nil
}

func TestBackupUsesTheConfiguredEngine(t *testing.T) {
	r, _ := newRegistry(t)
	fake := backuptest.NewRemote("kopia")
	r.Engines = backup.New(fake)

	res, err := r.Backup(context.Background(), "jellyfin", "", false, nil)
	name, err := oneName(t, res, err)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	got := strings.Join(fake.Calls, " ")
	for _, want := range []string{"snapshot:jellyfin/" + name + "#1", "snapshot:jellyfin/" + name + "#2", "commit:jellyfin/" + name} {
		if !strings.Contains(got, want) {
			t.Errorf("engine calls = %v, missing %q", fake.Calls, want)
		}
	}
	if len(fake.Calls) != 3 {
		t.Errorf("engine calls = %v, want exactly two passes and a commit", fake.Calls)
	}
	// Pass 1 must come before pass 2, or the stopped pass is not the last word.
	if i, j := strings.Index(got, "#1"), strings.Index(got, "#2"); i > j {
		t.Errorf("passes ran out of order: %v", fake.Calls)
	}
}

// Nothing the engine staged is durable until Commit, so a failure anywhere before
// it must discard that work rather than leave something a later List could offer
// for restore.
func TestBackupDiscardsIncompleteWork(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*backuptest.Fake)
	}{
		{"a pass fails", func(f *backuptest.Fake) { f.SnapshotErr = errors.New("repository unreachable") }},
		{"the commit fails", func(f *backuptest.Fake) { f.CommitErr = errors.New("repository unreachable") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newRegistry(t)
			fake := backuptest.NewRemote("kopia")
			tc.apply(fake)
			r.Engines = backup.New(fake)

			if _, err := r.Backup(context.Background(), "jellyfin", "", false, nil); err == nil {
				t.Fatal("Backup should have failed")
			}
			if !strings.Contains(strings.Join(fake.Calls, " "), "abort:jellyfin/") {
				t.Errorf("engine was not asked to discard the incomplete backup: %v", fake.Calls)
			}
			if got, _ := fake.List(context.Background(), "jellyfin"); len(got) != 0 {
				t.Errorf("a failed backup left %d listable backups behind", len(got))
			}
		})
	}
}

// A successful backup must not be discarded — the mirror image of the test above,
// and the one that catches an Abort fired on the happy path.
func TestBackupKeepsWhatItCommitted(t *testing.T) {
	r, _ := newRegistry(t)
	fake := backuptest.NewRemote("kopia")
	r.Engines = backup.New(fake)

	res, err := r.Backup(context.Background(), "jellyfin", "", false, nil)
	name, err := oneName(t, res, err)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if strings.Contains(strings.Join(fake.Calls, " "), "abort:") {
		t.Errorf("a successful backup was discarded: %v", fake.Calls)
	}
	got, _ := fake.List(context.Background(), "jellyfin")
	if len(got) != 1 || got[0].Name != name {
		t.Fatalf("engine holds %+v, want the committed backup %q", got, name)
	}
	// The name Backup reports is the engine's, not one the registry guessed — which
	// is what lets the local engine call its zip artefact "<stamp>.zip".
	if got[0].Engine != "kopia" {
		t.Errorf("committed backup Engine = %q, want %q", got[0].Engine, "kopia")
	}
}

// With no engine configured, a Registry behaves exactly as Maison always has: the
// built-in local engine writes an archive to .backups/.
func TestBackupWithoutAnEngineUsesTheLocalOne(t *testing.T) {
	r, cfg := newRegistry(t)

	res, err := r.Backup(context.Background(), "jellyfin", "", false, nil)
	name, err := oneName(t, res, err)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.BackupsDir(), "jellyfin", name, "db", "data.sqlite")); err != nil {
		t.Fatalf("local archive missing: %v", err)
	}
	got := apps.ListBackups(cfg.BackupsDir(), "jellyfin")
	if len(got) != 1 || got[0].Engine != apps.EngineLocal || got[0].Tier != apps.TierLocal {
		t.Fatalf("ListBackups = %+v, want one local-tier archive", got)
	}
}

// NOTE: the invariant that the app is restarted when the engine fails *after* the
// stop is not covered here, and cannot be until there is a fake for the Docker
// layer — with dx nil, isRunning reports false, so nothing is ever stopped. The
// deferred restart is registered before the abort defer specifically so that
// bringing the app back up takes precedence over cleanup; that ordering is asserted
// by reading the code, not by a test, until dockerx grows a seam of its own.

// StartRestore is what the UI calls, and it used to gate on the data disk: an app's
// archive had to exist under .backups/ before the restore was allowed to start. On a
// box configured for a remote engine that rejected every backup it had — the restore
// path underneath dispatches on where a backup actually is and would have handled it
// fine, but nothing could reach it. The precondition has to ask the engines the same
// question the restore will.
func TestStartRestoreAcceptsABackupThatOnlyExistsRemotely(t *testing.T) {
	r, _ := newRegistry(t)
	fake := backuptest.NewRemote("kopia")
	fake.Seed("jellyfin", "2026-01-01_120000")
	r.Engines = backup.New(fake)

	if err := r.StartRestore(context.Background(), "jellyfin", "", "2026-01-01_120000"); err != nil {
		t.Fatalf("StartRestore on a remote-only backup: %v", err)
	}
	// StartRestore detaches: the restore outlives the call, and it writes into the tree
	// t.TempDir() is about to delete. Joining it is not politeness, it is the difference
	// between a deterministic test and one that fails when the machine is busy.
	waitRestore(t, r, "jellyfin")
}

// waitRestore blocks until a detached backup or restore of `id` has settled. A finished
// one is removed from the tracked set; a failed one stays behind with PhaseError.
func waitRestore(t *testing.T, r *apps.Registry, id string) apps.BackupState {
	t.Helper()
	for i := 0; i < 400; i++ {
		found := false
		var st apps.BackupState
		for _, s := range r.Backups() {
			if s.ID == id {
				found, st = true, s
			}
		}
		if !found || st.Phase == apps.PhaseError {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the restore never finished")
	return apps.BackupState{}
}

// The mirror image: the precondition still has to refuse. A restore is destructive —
// the live folder is archived and replaced — so "no engine has this" must fail before
// anything is touched, not halfway through.
func TestStartRestoreRefusesWhatNoEngineHas(t *testing.T) {
	r, _ := newRegistry(t)
	r.Engines = backup.New(backuptest.NewRemote("kopia"))

	for _, name := range []string{"2026-01-01_120000", "notes"} {
		if err := r.StartRestore(context.Background(), "jellyfin", "", name); err == nil {
			t.Errorf("StartRestore(%q) succeeded; no engine holds it", name)
		}
	}
}

// The free-space guard has to be evaluated against the engine the backup is actually
// going to, not the default one.
//
// A remote engine streams and needs no local room, so it reports Needed:0 and always
// "enough". If a manual backup aimed at the local engine were estimated against a
// remote default, the guard would be skipped for the one case that genuinely needs a
// full second copy on disk — and filling the data root does not merely fail the
// backup, it fails every app still writing to that disk.
func TestEstimateFollowsTheTargetEngineNotTheDefault(t *testing.T) {
	r, _ := newRegistry(t)
	// Default is remote; the local engine is registered and selectable as a target.
	remote := backuptest.NewRemote("kopia")
	set := backup.New(remote, apps.NewLocalProvider(config.Config{DataRoot: t.TempDir()}))
	if err := setWriters(set, "kopia"); err != nil {
		t.Fatal(err)
	}
	r.Engines = set

	def, err := r.EstimateBackup("jellyfin", "", false)
	if err != nil {
		t.Fatalf("EstimateBackup(default): %v", err)
	}
	if !def.Streamed || def.Needed != 0 {
		t.Errorf("default (remote) estimate = %+v; want streamed with nothing needed locally", def)
	}

	local, err := r.EstimateBackup("jellyfin", apps.EngineLocal, false)
	if err != nil {
		t.Fatalf("EstimateBackup(local): %v", err)
	}
	if local.Streamed || local.Needed == 0 {
		t.Errorf("local estimate = %+v; want real local space to be required", local)
	}

	// An engine nobody registered is refused rather than silently falling back to the
	// default — writing somewhere other than where the user asked is the failure this
	// whole seam exists to prevent.
	if _, err := r.EstimateBackup("jellyfin", "nosuchengine", false); err == nil {
		t.Error("EstimateBackup with an unknown engine succeeded")
	}
	if err := r.StartBackup("jellyfin", "nosuchengine", false); err == nil {
		t.Error("StartBackup with an unknown engine succeeded")
	}
}

// --- Uninstall through the engine seam -------------------------------------
//
// An uninstall used to be the one write that never reached an engine: it renamed the
// app folder into .backups and called that the backup. On a box whose default engine
// writes offsite that produced nothing offsite — while the settings page said the
// opposite — so the data an uninstall was supposed to be protecting died with the
// disk. These pin the sequence that replaced it.

// The whole point: an uninstall is a backup through the default engine, and the app is
// offered to it wholesale because its folder is about to cease existing.
func TestUninstallBacksUpThroughTheDefaultEngine(t *testing.T) {
	r, cfg := newRegistry(t)
	fake := backuptest.NewRemote("kopia")
	r.Engines = backup.New(fake)

	name, err := r.Uninstall(context.Background(), "jellyfin", false, nil)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	// One pass, not two. A stopped app has no live pass to be incremental against, and
	// the pass that is committed is pass 2 — committing a pass-1 snapshot would discard
	// it as the torn throwaway it normally is.
	want := []string{
		"snapshot:jellyfin/" + name + "#2+consume",
		"commit:jellyfin/" + name,
	}
	if got := strings.Join(fake.Calls, " "); got != strings.Join(want, " ") {
		t.Errorf("engine calls = %q; want %q", got, strings.Join(want, " "))
	}

	// A streaming engine reads the folder and leaves it, so removing it is the
	// registry's job. Skipping that would leave the app's data on the disk the user
	// just asked to reclaim.
	if _, err := os.Stat(filepath.Join(cfg.AppsDir(), "jellyfin")); !os.IsNotExist(err) {
		t.Errorf("app folder survived an uninstall through a streaming engine")
	}
}

// The ordering rule, from the failure side: until the backup is committed, nothing is
// destroyed. A failure at either step leaves the app exactly as it was — installed,
// with its data — because the alternative is an app that is gone and a backup that
// never happened.
func TestUninstallLeavesTheAppIntactWhenTheEngineFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*backuptest.Fake)
	}{
		{"snapshot fails", func(f *backuptest.Fake) { f.SnapshotErr = errors.New("repository unreachable") }},
		{"commit fails", func(f *backuptest.Fake) { f.CommitErr = errors.New("upload never confirmed") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, cfg := newRegistry(t)
			fake := backuptest.NewRemote("kopia")
			tc.set(fake)
			r.Engines = backup.New(fake)

			if _, err := r.Uninstall(context.Background(), "jellyfin", false, nil); err == nil {
				t.Fatal("Uninstall succeeded; the engine failed")
			}
			appDir := filepath.Join(cfg.AppsDir(), "jellyfin")
			if _, err := os.Stat(appDir); err != nil {
				t.Fatalf("app folder was destroyed after a failed backup: %v", err)
			}
			if body, err := os.ReadFile(filepath.Join(appDir, "db", "data.sqlite")); err != nil || string(body) != "rows" {
				t.Errorf("app data = %q, %v; want it untouched", body, err)
			}
			// And nothing durable was left behind that a later list could offer.
			if got := r.Engines.List(context.Background(), "jellyfin"); len(got) != 0 {
				t.Errorf("List = %+v; a failed uninstall must leave no backup", got)
			}
			if !strings.Contains(strings.Join(fake.Calls, " "), "abort:jellyfin/") {
				t.Errorf("engine calls = %v; want the incomplete backup aborted", fake.Calls)
			}
		})
	}
}

// It must not quietly fall back to the local disk when the chosen engine cannot be
// reached. That is the tempting behaviour and the wrong one: it puts the data somewhere
// other than where the user was told it goes, at the exact moment the tile disappears
// and stops being able to say so.
func TestUninstallDoesNotFallBackToLocalWhenTheEngineFails(t *testing.T) {
	r, cfg := newRegistry(t)
	fake := backuptest.NewRemote("kopia")
	fake.SnapshotErr = errors.New("repository unreachable")
	set := backup.New(fake, apps.NewLocalProvider(cfg))
	if err := setWriters(set, "kopia"); err != nil {
		t.Fatal(err)
	}
	r.Engines = set

	if _, err := r.Uninstall(context.Background(), "jellyfin", false, nil); err == nil {
		t.Fatal("Uninstall succeeded; the chosen engine failed")
	}
	if got := set.ListIn(context.Background(), apps.EngineLocal, "jellyfin"); len(got) != 0 {
		t.Errorf("local engine holds %+v; a failed offsite uninstall must not silently write here", got)
	}
}

// --- The store's install-from-backup path ----------------------------------
//
// This path used to read the data disk directly rather than going through the set —
// the same "writes go through the engine set, reads do not" bug the Backups page had.
// The effect was that a box whose backups live in a repository could not reinstall an
// app on top of any of them: the picker was empty, and had it not been, the install
// would have looked for a folder that was never there.

func TestRestoreForInstallReachesABackupOnlyARepositoryHolds(t *testing.T) {
	r, cfg := newRegistry(t)
	fake := backuptest.NewRemote("kopia")
	fake.MaterializeInto = cfg.BackupsDir()
	fake.Seed("jellyfin", "2026-01-01_120000")
	r.Engines = backup.New(fake)

	// The case that matters: the app is not installed. That is what a store reinstall
	// is, and it is why this cannot go through Restore, which stops a live app and
	// archives the folder it replaces.
	appDir := filepath.Join(cfg.AppsDir(), "jellyfin")
	if err := os.RemoveAll(appDir); err != nil {
		t.Fatal(err)
	}

	if err := r.RestoreForInstall(context.Background(), "jellyfin", "kopia", "2026-01-01_120000", nil); err != nil {
		t.Fatalf("RestoreForInstall: %v", err)
	}

	if body, err := os.ReadFile(filepath.Join(appDir, "db", "data.sqlite")); err != nil || string(body) != "rows" {
		t.Errorf("restored app data = %q, %v; want the repository's copy in place", body, err)
	}
	if !strings.Contains(strings.Join(fake.Calls, " "), "materialize:jellyfin/2026-01-01_120000") {
		t.Errorf("engine calls = %v; want the backup fetched from the repository", fake.Calls)
	}
}

// The picker offers a row per engine, so the request names one — and it has to be
// honoured. Two engines can hold the same stamp and they are different backups;
// installing the other one is not what was clicked.
func TestRestoreForInstallUsesTheEngineTheRowNamed(t *testing.T) {
	r, cfg := newRegistry(t)
	first := backuptest.NewRemote("kopia")
	second := backuptest.NewRemote("restic")
	for _, f := range []*backuptest.Fake{first, second} {
		f.MaterializeInto = cfg.BackupsDir()
		f.Seed("jellyfin", "2026-01-01_120000")
	}
	r.Engines = backup.New(first, second)

	if err := os.RemoveAll(filepath.Join(cfg.AppsDir(), "jellyfin")); err != nil {
		t.Fatal(err)
	}
	if err := r.RestoreForInstall(context.Background(), "jellyfin", "restic", "2026-01-01_120000", nil); err != nil {
		t.Fatalf("RestoreForInstall: %v", err)
	}

	if strings.Contains(strings.Join(first.Calls, " "), "materialize:") {
		t.Errorf("kopia was asked for a backup the user picked out of restic: %v", first.Calls)
	}
	if !strings.Contains(strings.Join(second.Calls, " "), "materialize:jellyfin/2026-01-01_120000") {
		t.Errorf("restic calls = %v; want the picked backup fetched from it", second.Calls)
	}
}

// And it must refuse rather than write over a live app. RestoreBackup carries that
// guard; this pins that the install path still goes through it, because the install
// that follows is deliberately non-destructive on top of what it finds and would
// otherwise merge a backup into a running app's folder.
func TestRestoreForInstallRefusesAnAppThatIsInstalled(t *testing.T) {
	r, cfg := newRegistry(t)
	fake := backuptest.NewRemote("kopia")
	fake.MaterializeInto = cfg.BackupsDir()
	fake.Seed("jellyfin", "2026-01-01_120000")
	r.Engines = backup.New(fake)

	if err := r.RestoreForInstall(context.Background(), "jellyfin", "kopia", "2026-01-01_120000", nil); err == nil {
		t.Fatal("RestoreForInstall succeeded over an installed app")
	}
	if body, err := os.ReadFile(filepath.Join(cfg.AppsDir(), "jellyfin", ".env")); err != nil || string(body) != "PUID=1000\n" {
		t.Errorf("live app was modified = %q, %v", body, err)
	}
}

// An engine that streams to a repository never walks the app folder, so the only way
// it can honour an app's exclusions is by being handed them. This is the half of the
// chain the local engine's own tests cannot see: the registry reading the app's
// compose and passing the parsed rules down, on both passes, in their canonical
// spelling.
func TestBackupHandsTheAppsExclusionsToTheEngine(t *testing.T) {
	r, cfg := newRegistry(t)
	compose := "services: {}\nx-compose-app:\n  schema_version: 2\n  backup:\n    exclude:\n      - cache/\n      - \"**/thumbs/\"\n"
	if err := os.WriteFile(filepath.Join(cfg.AppsDir(), "jellyfin", "docker-compose.yml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := backuptest.NewRemote("kopia")
	r.Engines = backup.New(fake)

	if _, err := r.Backup(context.Background(), "jellyfin", "", false, nil); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	got := strings.Join(fake.Calls, " ")
	if n := strings.Count(got, "+exclude(cache/,**/thumbs/)"); n != 2 {
		t.Errorf("engine calls = %v; both passes must carry the declaration, saw %d", fake.Calls, n)
	}
}

// An app that declares nothing must reach the engine with nothing — the absence is
// what tells a repository engine to clear whatever rules it is still holding.
func TestBackupWithoutADeclarationCarriesNoExclusions(t *testing.T) {
	r, _ := newRegistry(t)
	fake := backuptest.NewRemote("kopia")
	r.Engines = backup.New(fake)

	if _, err := r.Backup(context.Background(), "jellyfin", "", false, nil); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if got := strings.Join(fake.Calls, " "); strings.Contains(got, "+exclude") {
		t.Errorf("engine calls = %v, want no exclusions", fake.Calls)
	}
}

// ── Fan-out ───────────────────────────────────────────────────────────────────────
//
// A backup can be written to several engines at once. These pin the parts of that
// which are not obvious from reading the loop.

// The whole reason fan-out lives inside one BackupWith rather than being a loop over
// it: the app is stopped ONCE for every destination.
//
// Looping outside would stop it per engine, and worse — the first operation's deferred
// restart would bring the app up while the second was still taking its "consistent"
// stopped pass, producing a torn snapshot that looks fine until someone restores it.
func TestFanOutStopsTheAppOnceForEveryEngine(t *testing.T) {
	r, cfg := newRegistry(t)
	local := backuptest.NewLocalLike(apps.EngineLocal)
	remote := backuptest.NewRemote("kopia")
	set := backup.New(local, remote)
	if err := setWriters(set, apps.EngineLocal, "kopia"); err != nil {
		t.Fatal(err)
	}
	r.Engines = set
	_ = cfg

	res, err := r.Backup(context.Background(), "jellyfin", "", false, nil)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("got %d engine results, want one per engine: %+v", len(res), res)
	}
	for _, got := range res {
		if got.Err != nil {
			t.Errorf("%s: %v", got.Engine, got.Err)
		}
		if got.Name == "" {
			t.Errorf("%s produced no backup name", got.Engine)
		}
	}
	// Both engines saw both passes against the SAME stamp: one backup, two destinations.
	for _, f := range []*backuptest.Fake{local, remote} {
		var passes int
		var stamps = map[string]bool{}
		for _, c := range f.Calls {
			if strings.HasPrefix(c, "snapshot:") {
				passes++
				stamps[strings.Split(strings.TrimPrefix(c, "snapshot:"), "#")[0]] = true
			}
		}
		if passes != 2 {
			t.Errorf("%s saw %d snapshot passes, want 2: %v", f.ID(), passes, f.Calls)
		}
		if len(stamps) != 1 {
			t.Errorf("%s was given %d stamps, want one shared across engines: %v", f.ID(), len(stamps), f.Calls)
		}
	}
	if res[0].Name != res[1].Name {
		t.Errorf("engines disagreed on the backup name: %q vs %q", res[0].Name, res[1].Name)
	}
}

// One destination failing does not throw away the copies that landed. Losing the
// offsite copy of an app is not a reason to also lose the local one.
func TestFanOutKeepsTheCopiesThatSucceeded(t *testing.T) {
	r, _ := newRegistry(t)
	local := backuptest.NewLocalLike(apps.EngineLocal)
	remote := backuptest.NewRemote("kopia")
	remote.SnapshotErr = errors.New("repository unreachable")
	set := backup.New(local, remote)
	if err := setWriters(set, apps.EngineLocal, "kopia"); err != nil {
		t.Fatal(err)
	}
	r.Engines = set

	res, err := r.Backup(context.Background(), "jellyfin", "", false, nil)
	// Not a whole-operation failure: something was written.
	if err != nil {
		t.Fatalf("Backup refused although the local copy succeeded: %v", err)
	}
	byEngine := map[string]apps.Result{}
	for _, got := range res {
		byEngine[got.Engine] = got
	}
	if got := byEngine[apps.EngineLocal]; got.Err != nil || got.Name == "" {
		t.Errorf("local copy = %+v, want it written", got)
	}
	if got := byEngine["kopia"]; got.Err == nil {
		t.Error("the unreachable repository was reported as having succeeded")
	}
	// And the engine that failed holds nothing: its staging was aborted.
	if got, _ := remote.List(context.Background(), "jellyfin"); len(got) != 0 {
		t.Error("the failed engine kept a backup it never committed")
	}
}

// Every destination failing IS a whole-operation failure. A backup that wrote nowhere
// and reported success would have the scheduler record a healthy run and the incident
// register stay quiet, on a box where nothing was saved.
func TestFanOutFailsWhenEveryEngineFails(t *testing.T) {
	r, _ := newRegistry(t)
	a := backuptest.NewRemote("kopia")
	b := backuptest.NewRemote("rclone")
	a.SnapshotErr = errors.New("repository unreachable")
	b.SnapshotErr = errors.New("bucket denied")
	set := backup.New(a, b)
	if err := setWriters(set, "kopia", "rclone"); err != nil {
		t.Fatal(err)
	}
	r.Engines = set

	if _, err := r.Backup(context.Background(), "jellyfin", "", false, nil); err == nil {
		t.Fatal("a backup that wrote nowhere reported success")
	}
}

// Nowhere to write is a refusal, not an empty success.
func TestBackupRefusesWithNoDestination(t *testing.T) {
	r, _ := newRegistry(t)
	set := backup.New(backuptest.NewLocalLike(apps.EngineLocal))
	if err := set.SetWriters(apps.TriggerSchedule, nil); err != nil {
		t.Fatal(err)
	}
	r.Engines = set

	if _, err := r.Backup(context.Background(), "jellyfin", "", false, nil); err == nil {
		t.Fatal("a backup with no configured destination reported success")
	}
}

// ── Fan-out uninstall ─────────────────────────────────────────────────────────────

// An uninstall archived to several engines is all-or-nothing: if any destination
// fails, the app stays installed and running and nothing is left half-done.
//
// The real local provider is used rather than a fake, because the thing under test is
// its rename — the local engine TAKES the app folder, which is what makes ordering and
// rollback load-bearing rather than academic.
func TestUninstallRefusesWhenADestinationFails(t *testing.T) {
	r, cfg := newRegistry(t)
	local := apps.NewLocalProvider(cfg)
	remote := backuptest.NewRemote("kopia")
	remote.CommitErr = errors.New("repository unreachable")
	set := backup.New(local, remote)
	if err := setWriters(set, apps.EngineLocal, "kopia"); err != nil {
		t.Fatal(err)
	}
	r.Engines = set

	_, err := r.Uninstall(context.Background(), "jellyfin", false, nil)
	if err == nil {
		t.Fatal("the uninstall went ahead although a destination failed")
	}
	if !strings.Contains(err.Error(), "kopia") {
		t.Errorf("error %q does not name the destination that failed", err)
	}
	// The way out has to be in the message: all-or-nothing means an engine nobody can
	// reach otherwise blocks every uninstall on the box with no hint why.
	if !strings.Contains(err.Error(), "Settings") {
		t.Errorf("error %q does not say how to get unstuck", err)
	}
	// The app is still there, with its data.
	if _, statErr := os.Stat(filepath.Join(cfg.AppsDir(), "jellyfin", "db", "data.sqlite")); statErr != nil {
		t.Fatalf("the app folder did not survive a refused uninstall: %v", statErr)
	}
	// And nothing was left in the archive for a retry to trip over.
	if got := apps.ListBackups(cfg.BackupsDir(), "jellyfin"); len(got) != 0 {
		t.Errorf("a refused uninstall left %d archive(s) behind: %+v", len(got), got)
	}
}

// The consuming engine goes LAST, and once it has committed the archive IS the user's
// data — the folder was renamed into it. Nothing after that may delete it.
//
// This is the case that would destroy data if the rollback were written as "undo every
// archive this attempt wrote": the local archive is not a spare copy.
func TestUninstallNeverRollsBackTheArchiveThatTookTheFolder(t *testing.T) {
	r, cfg := newRegistry(t)
	local := apps.NewLocalProvider(cfg)
	remote := backuptest.NewRemote("kopia")
	set := backup.New(local, remote)
	if err := setWriters(set, apps.EngineLocal, "kopia"); err != nil {
		t.Fatal(err)
	}
	r.Engines = set

	name, err := r.Uninstall(context.Background(), "jellyfin", false, nil)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if name == "" {
		t.Fatal("the uninstall produced no archive name")
	}
	// The reading engine ran first, while the folder still existed…
	var order []string
	for _, c := range remote.Calls {
		if strings.HasPrefix(c, "snapshot:") {
			order = append(order, c)
		}
	}
	if len(order) == 0 {
		t.Fatal("the repository never saw the app folder — it was consumed before it read it")
	}
	// …and the local archive, which took the folder, is still there.
	got := apps.ListBackups(cfg.BackupsDir(), "jellyfin")
	if len(got) != 1 {
		t.Fatalf("local archives after the uninstall = %d, want the one holding the app: %+v", len(got), got)
	}
	if _, statErr := os.Stat(filepath.Join(cfg.AppsDir(), "jellyfin")); !os.IsNotExist(statErr) {
		t.Errorf("the app folder is still in place after a committed uninstall: %v", statErr)
	}
}

// Two engines that both take the folder cannot both have it. Refused up front rather
// than discovered halfway through, when the second would rename a folder that the
// first has already moved — after the point of no return.
func TestUninstallRefusesTwoConsumingEngines(t *testing.T) {
	r, cfg := newRegistry(t)
	set := backup.New(apps.NewLocalProvider(cfg), backuptest.NewFake("mirror", apps.Caps{ConsumesSource: true}))
	if err := setWriters(set, apps.EngineLocal, "mirror"); err != nil {
		t.Fatal(err)
	}
	r.Engines = set

	_, err := r.Uninstall(context.Background(), "jellyfin", false, nil)
	if err == nil || !strings.Contains(err.Error(), "only one") {
		t.Fatalf("Uninstall error = %v, want a refusal naming the conflict", err)
	}
	if _, statErr := os.Stat(filepath.Join(cfg.AppsDir(), "jellyfin")); statErr != nil {
		t.Errorf("the app folder was touched by a refused uninstall: %v", statErr)
	}
}
