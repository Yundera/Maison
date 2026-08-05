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

	"github.com/yundera/maison/internal/apps"
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
func TestBackupUsesTheConfiguredEngine(t *testing.T) {
	r, _ := newRegistry(t)
	fake := backuptest.NewRemote("kopia")
	r.Engine = fake

	name, err := r.Backup(context.Background(), "jellyfin", false, nil)
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
			r.Engine = fake

			if _, err := r.Backup(context.Background(), "jellyfin", false, nil); err == nil {
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
	r.Engine = fake

	name, err := r.Backup(context.Background(), "jellyfin", false, nil)
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

	name, err := r.Backup(context.Background(), "jellyfin", false, nil)
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
