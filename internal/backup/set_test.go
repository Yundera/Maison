package backup

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

const (
	older = "2026-01-01_120000"
	newer = "2026-02-02_120000"
)

func names(bs []apps.Backup) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Name
	}
	return out
}

// The write engine is a choice; reading must not follow it. This is the rule that
// keeps a user's existing backups reachable after they switch engines — without it
// they are all still there and none of them can be found.
func TestReadingIgnoresTheSelectedEngine(t *testing.T) {
	local := backuptest.NewLocalLike(apps.EngineLocal)
	remote := backuptest.NewRemote("kopia")
	local.Seed("jellyfin", older)
	remote.Seed("jellyfin", newer)

	s := New(local, remote)
	if err := s.SetWriter("kopia"); err != nil {
		t.Fatalf("SetWriter: %v", err)
	}

	// Selected engine is kopia, but the local archive must still be listed...
	got := names(s.List(context.Background(), "jellyfin"))
	if len(got) != 2 || got[0] != newer || got[1] != older {
		t.Fatalf("List = %v, want newest-first [%s %s]", got, newer, older)
	}

	// ...and still be reachable for restore, from the engine that actually has it.
	p, _, err := s.Locate(context.Background(), "jellyfin", older)
	if err != nil {
		t.Fatalf("Locate(%s): %v", older, err)
	}
	if p.ID() != apps.EngineLocal {
		t.Fatalf("Locate(%s) dispatched to %q, want the engine holding it (%q)", older, p.ID(), apps.EngineLocal)
	}
}

// The same backup in two engines is one backup to the user: one row, marked as
// being in both.
func TestUnionDedupesAndMarksBothTiers(t *testing.T) {
	local := backuptest.NewLocalLike(apps.EngineLocal)
	remote := backuptest.NewRemote("kopia")
	local.Seed("jellyfin", newer)
	remote.Seed("jellyfin", newer)

	got := New(local, remote).List(context.Background(), "jellyfin")
	if len(got) != 1 {
		t.Fatalf("List returned %d rows for one backup held twice: %v", len(got), names(got))
	}
	if got[0].Tier != apps.TierBoth {
		t.Errorf("Tier = %q, want %q", got[0].Tier, apps.TierBoth)
	}
	// The row should name the engine the restore will actually come from.
	if got[0].Engine != apps.EngineLocal {
		t.Errorf("Engine = %q, want the instant-restore engine %q", got[0].Engine, apps.EngineLocal)
	}
}

// Among engines holding the same backup, the one that can restore it without
// copying wins — otherwise a backup that is sitting on the local disk would be
// downloaded again.
func TestLocatePrefersInstantRestore(t *testing.T) {
	remote := backuptest.NewRemote("kopia")
	local := backuptest.NewLocalLike(apps.EngineLocal)
	remote.Seed("jellyfin", newer)
	local.Seed("jellyfin", newer)

	// Registered remote-first, so a naive "first match wins" would pick the wrong one.
	p, _, err := New(remote, local).Locate(context.Background(), "jellyfin", newer)
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if p.ID() != apps.EngineLocal {
		t.Fatalf("Locate picked %q, want %q (instant restore)", p.ID(), apps.EngineLocal)
	}
}

// An unconfigured remote engine is the normal state of a box whose host-side setup
// has not run. It must contribute nothing rather than empty the page.
func TestUnconfiguredEngineDoesNotBreakListing(t *testing.T) {
	local := backuptest.NewLocalLike(apps.EngineLocal)
	local.Seed("jellyfin", newer)
	remote := backuptest.NewRemote("kopia")
	remote.ListErr = apps.ErrNotConfigured

	got := New(local, remote).List(context.Background(), "jellyfin")
	if len(got) != 1 || got[0].Name != newer {
		t.Fatalf("List = %v, want the local archive to survive an unconfigured engine", names(got))
	}
}

// A broken repository should degrade the page, not empty it — same reasoning as
// above, but for a real failure rather than an expected state.
func TestBrokenEngineDoesNotBreakListing(t *testing.T) {
	local := backuptest.NewLocalLike(apps.EngineLocal)
	local.Seed("jellyfin", newer)
	remote := backuptest.NewRemote("kopia")
	remote.ListErr = errors.New("repository unreachable")

	if got := New(local, remote).List(context.Background(), "jellyfin"); len(got) != 1 {
		t.Fatalf("List = %v, want the local archive to survive a broken engine", names(got))
	}
}

// Deleting a row the user sees once must delete the backup everywhere it is, or the
// row reappears on the next refresh with no explanation.
func TestDeleteRemovesFromEveryEngineHoldingIt(t *testing.T) {
	local := backuptest.NewLocalLike(apps.EngineLocal)
	remote := backuptest.NewRemote("kopia")
	local.Seed("jellyfin", newer)
	remote.Seed("jellyfin", newer)

	s := New(local, remote)
	if err := s.Delete(context.Background(), "jellyfin", newer); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := s.List(context.Background(), "jellyfin"); len(got) != 0 {
		t.Fatalf("after Delete, List = %v, want empty", names(got))
	}
	for _, f := range []*backuptest.Fake{local, remote} {
		if !strings.Contains(strings.Join(f.Calls, " "), "delete:jellyfin/"+newer) {
			t.Errorf("engine %s was not asked to delete: calls = %v", f.ID(), f.Calls)
		}
	}
}

func TestDeleteUnknownBackupIsAnError(t *testing.T) {
	s := New(backuptest.NewLocalLike(apps.EngineLocal))
	if err := s.Delete(context.Background(), "jellyfin", newer); err == nil {
		t.Fatal("Delete of a backup no engine holds should fail")
	}
}

// Writing somewhere other than where the user asked is how someone ends up
// believing their data is offsite when it is not, so an unknown engine must be a
// refusal rather than a fallback.
func TestSetWriterRefusesUnknownEngine(t *testing.T) {
	s := New(backuptest.NewLocalLike(apps.EngineLocal))
	if err := s.SetWriter("kopia"); err == nil {
		t.Fatal("SetWriter accepted an unregistered engine")
	}
	if got := s.Writer().ID(); got != apps.EngineLocal {
		t.Fatalf("Writer = %q after a refused SetWriter, want it unchanged (%q)", got, apps.EngineLocal)
	}
}

// The first engine registered is the write target, which makes the always-present
// local engine the default without anyone having to configure it.
func TestFirstRegisteredEngineIsTheDefaultWriter(t *testing.T) {
	s := New(backuptest.NewLocalLike(apps.EngineLocal), backuptest.NewRemote("kopia"))
	if got := s.Writer().ID(); got != apps.EngineLocal {
		t.Fatalf("default Writer = %q, want %q", got, apps.EngineLocal)
	}
}

// Re-registering a reconfigured engine must not move it, or the picker reorders
// itself under the user for no visible reason.
func TestReRegisterKeepsPosition(t *testing.T) {
	s := New(backuptest.NewLocalLike(apps.EngineLocal), backuptest.NewRemote("kopia"))
	s.Register(backuptest.NewRemote("kopia"))
	if got := s.IDs(); len(got) != 2 || got[0] != apps.EngineLocal || got[1] != "kopia" {
		t.Fatalf("IDs = %v, want [local kopia]", got)
	}
}

// The global page has to work on a box where nothing is installed and nothing is on
// the data disk. That is the rebuilt-box case — a fresh machine with the repository
// reconnected — and it is the one the page matters most in, because the repository is
// the only thing left that knows the app existed.
func TestListAllSeesAppsThatOnlyExistRemotely(t *testing.T) {
	local := backuptest.NewLocalLike(apps.EngineLocal)
	remote := backuptest.NewRemote("kopia")
	remote.Seed("jellyfin", older)
	remote.Seed("nextcloud", newer)

	// appsDir is a directory with nothing in it: no app is installed.
	got := New(local, remote).ListAll(context.Background(), t.TempDir(), t.TempDir())

	if len(got) != 2 {
		t.Fatalf("ListAll returned %d groups (%+v); want jellyfin and nextcloud", len(got), got)
	}
	for _, g := range got {
		if !g.Orphan {
			t.Errorf("%s: Orphan = false; nothing is installed, so every group is an orphan", g.App)
		}
		if len(g.Backups) != 1 {
			t.Errorf("%s: %d backups; want 1", g.App, len(g.Backups))
		}
	}
}

// An app with archives on the disk *and* snapshots in a repository is one app, and
// the group must carry the union rather than whichever engine answered first.
func TestListAllMergesEnginesIntoOneGroupPerApp(t *testing.T) {
	local := backuptest.NewLocalLike(apps.EngineLocal)
	remote := backuptest.NewRemote("kopia")
	local.Seed("jellyfin", older)
	remote.Seed("jellyfin", newer)

	got := New(local, remote).ListAll(context.Background(), t.TempDir(), t.TempDir())

	if len(got) != 1 {
		t.Fatalf("ListAll returned %d groups; want 1", len(got))
	}
	if g := names(got[0].Backups); len(g) != 2 || g[0] != newer || g[1] != older {
		t.Errorf("backups = %v; want both, newest first", g)
	}
}

// A repository that cannot be reached must degrade the page, not empty it: the
// archives on the local disk are still there and still restorable.
func TestListAllSurvivesABrokenEngine(t *testing.T) {
	local := backuptest.NewLocalLike(apps.EngineLocal)
	remote := backuptest.NewRemote("kopia")
	local.Seed("jellyfin", older)
	remote.ListErr = errors.New("repository unreachable")

	got := New(local, remote).ListAll(context.Background(), t.TempDir(), t.TempDir())

	if len(got) != 1 || got[0].App != "jellyfin" {
		t.Fatalf("ListAll = %+v; want jellyfin from the working engine", got)
	}
}

// Orphan is the flag the page keys its "uninstalled" tag off, so it has to follow the
// app folder rather than the backup's engine.
func TestListAllMarksOnlyMissingAppsAsOrphans(t *testing.T) {
	remote := backuptest.NewRemote("kopia")
	remote.Seed("jellyfin", older)
	remote.Seed("nextcloud", older)

	appsDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(appsDir, "jellyfin"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, g := range New(remote).ListAll(context.Background(), t.TempDir(), appsDir) {
		want := g.App == "nextcloud"
		if g.Orphan != want {
			t.Errorf("%s: Orphan = %v; want %v", g.App, g.Orphan, want)
		}
	}
}

// A name that is not a name is a malformed request, not a backup that happens to be
// missing — and it must be refused before any engine is handed it, since this is the
// one guard that does not depend on every future engine remembering to check.
func TestLocateAndDeleteRefuseNamesThatAreNotNames(t *testing.T) {
	s := New(backuptest.NewLocalLike(apps.EngineLocal))

	for _, c := range []struct{ app, stamp, want string }{
		{"jellyfin", "notes", "not a backup name"},
		{"../etc", older, "invalid app name"},
		{"jellyfin", "../../etc/passwd", "not a backup name"},
	} {
		if _, _, err := s.Locate(context.Background(), c.app, c.stamp); err == nil ||
			!strings.Contains(err.Error(), c.want) {
			t.Errorf("Locate(%q, %q) = %v; want %q", c.app, c.stamp, err, c.want)
		}
		if err := s.Delete(context.Background(), c.app, c.stamp); err == nil ||
			!strings.Contains(err.Error(), c.want) {
			t.Errorf("Delete(%q, %q) = %v; want %q", c.app, c.stamp, err, c.want)
		}
	}
}

// seedDir writes a small app-shaped tree, so a measured folder archive has a size
// that is worth asserting on rather than 0.
func seedDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"docker-compose.yml": "services: {}\n",
		".env":               "PUID=1000\n",
		"db/data.sqlite":     "rows",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// ListAll over the real local engine, on a real directory tree.
//
// The fakes above pin the merge rules; this pins that the thing the page actually
// runs on a box still measures folder archives and still marks an uninstalled app as
// an orphan. It replaces a test that called apps.ListAll directly — that function is
// gone, because a second way to list every backup is a second way to miss the ones a
// remote engine holds.
func TestListAllOverTheRealLocalEngine(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{DataRoot: root}
	seedDir(t, filepath.Join(cfg.AppsDir(), "jellyfin")) // installed
	seedDir(t, filepath.Join(cfg.BackupsDir(), "jellyfin", newer))
	// sonarr has an archive but no app folder: uninstalled, and reachable only here.
	seedDir(t, filepath.Join(cfg.BackupsDir(), "sonarr", older))

	got := New(apps.NewLocalProvider(cfg)).ListAll(context.Background(), cfg.BackupsDir(), cfg.AppsDir())

	if len(got) != 2 {
		t.Fatalf("ListAll = %+v; want jellyfin and sonarr", got)
	}
	if got[0].App != "jellyfin" || got[0].Orphan {
		t.Errorf("jellyfin = %+v; want Orphan=false", got[0])
	}
	if got[1].App != "sonarr" || !got[1].Orphan {
		t.Errorf("sonarr = %+v; want Orphan=true", got[1])
	}
	// seedDir writes 13 + 10 + 4 bytes, nested one directory deep — so this also pins
	// that the walk recurses. Unmeasured folder archives would leave every size at 0,
	// and a list of "—" cannot answer what is eating the disk.
	const want = 13 + 10 + 4
	if got[0].Total != want || got[0].Backups[0].Size != want {
		t.Errorf("jellyfin measured %d (row %d); want %d",
			got[0].Total, got[0].Backups[0].Size, want)
	}
}

// A remote-only backup has no folder on this disk. Measuring must leave the size the
// engine reported alone rather than walking a path that is not there and calling the
// backup empty.
func TestMeasureLeavesRemoteSizesAlone(t *testing.T) {
	remote := backuptest.NewRemote("kopia")
	remote.Seed("jellyfin", older)

	got := New(remote).ListAll(context.Background(), t.TempDir(), t.TempDir())
	if len(got) != 1 || len(got[0].Backups) != 1 {
		t.Fatalf("ListAll = %+v; want one remote backup", got)
	}
	if got[0].Backups[0].Tier != apps.TierRemote {
		t.Errorf("tier = %q; want %q", got[0].Backups[0].Tier, apps.TierRemote)
	}
}

// The global page must cost one call per engine, not one per app. For a remote engine
// each call is a subprocess against the repository, so a per-app shape would turn
// opening the page on a box with twenty apps into twenty of them.
func TestListAllAsksEachEngineOnce(t *testing.T) {
	remote := backuptest.NewRemote("kopia")
	for _, app := range []string{"jellyfin", "nextcloud", "sonarr", "radarr"} {
		remote.Seed(app, older)
	}

	got := New(remote).ListAll(context.Background(), t.TempDir(), t.TempDir())

	if len(got) != 4 {
		t.Fatalf("ListAll returned %d groups; want 4", len(got))
	}
	if remote.ListAllCalls != 1 {
		t.Errorf("engine was asked %d times for %d apps; want exactly 1",
			remote.ListAllCalls, len(got))
	}
}

// Listing an app name that no app can have must not reach an engine: for a remote one
// the name becomes a subprocess argument, and for the local one a path.
func TestListRefusesAnAppNameThatIsNotOne(t *testing.T) {
	remote := backuptest.NewRemote("kopia")
	remote.Seed("jellyfin", older)
	s := New(remote)

	for _, bad := range []string{"../etc", ".backups", "a/b", ""} {
		if got := s.List(context.Background(), bad); got != nil {
			t.Errorf("List(%q) = %+v; want nothing", bad, got)
		}
	}
}
