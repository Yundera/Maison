package kopia

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/engine"
)

// These are contract tests against kopia's actual CLI — JSON field names, the tag
// prefix asymmetry, ignore-rule anchoring, exit codes. Every one of them is a place
// kopia can change under us silently, which is the whole reason they run the real
// binary rather than a mock of it.
//
// They need a Docker daemon and a writable directory the daemon can bind-mount, so
// they skip rather than fail where either is missing. There is no CI test job today;
// when there is one, these turn on with no change.
func skipUnlessEngine(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable")
	}
	if hostRoot() == "" {
		t.Skip("MAISON_TEST_HOST_ROOT not set: the daemon cannot bind-mount the test tree")
	}
}

// hostRoot is the host's spelling of the directory the tests work in. The daemon
// resolves bind sources on the host, so a t.TempDir() inside this container is not
// reachable; the caller supplies a directory that is visible to both.
func hostRoot() string { return os.Getenv("MAISON_TEST_HOST_ROOT") }

const testPassword = "test-repo-password"

// newRepo builds a Provider over a fresh filesystem repository.
//
// The layout mirrors a real box exactly — data root, AppData, AppDataShared/backup/
// kopia — because the exclusion test below depends on those relative positions.
func newRepo(t *testing.T) (*Provider, config.Config) {
	t.Helper()
	skipUnlessEngine(t)

	// A directory under the shared root, unique per test, cleaned up afterwards.
	dataRoot, err := os.MkdirTemp(hostRootLocal(), "kopia-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dataRoot) })

	cfg := config.Config{
		DataRoot: dataRoot,
		// The daemon needs the host's spelling of the same directory.
		DataHostPath: filepath.Join(hostRoot(), filepath.Base(dataRoot)),
		PUID:         "0",
		PGID:         "0",
	}
	engineDir := cfg.BackupEngineDir(ID)
	for _, d := range []string{
		filepath.Join(cfg.AppsDir(), "jellyfin", "db"),
		filepath.Join(cfg.DataRoot, "Documents"),
		filepath.Join(engineDir, "cache"),
		filepath.Join(engineDir, "logs"),
		filepath.Join(cfg.DataRoot, "repo"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(cfg.AppsDir(), "jellyfin", "docker-compose.yml"), "services: {}\n")
	write(filepath.Join(cfg.AppsDir(), "jellyfin", "db", "data.sqlite"), "rows")
	write(filepath.Join(cfg.DataRoot, "Documents", "notes.txt"), "user notes")
	write(filepath.Join(engineDir, "repository.password"), testPassword)
	write(filepath.Join(engineDir, "cache", "blob"), "cache junk")
	write(filepath.Join(engineDir, "logs", "kopia.log"), "log junk")

	p := New(cfg)

	// Create the repository the way the host-side script would, pinning the identity
	// so snapshots are filed under a stable user@host.
	r := engine.New(cfg)
	if _, err := r.Run(context.Background(), engine.Spec{
		Image:    DefaultImage,
		Name:     "maison-kopia-test-create-" + filepath.Base(dataRoot),
		Hostname: "pcs-test",
		Network:  engine.NetworkNone,
		Mounts:   []engine.Mount{r.DataMount(false)},
		Secrets:  map[string]string{"KOPIA_PASSWORD": testPassword},
		Args: []string{
			"repository", "create", "filesystem",
			"--path=" + filepath.Join(cfg.DataRoot, "repo"),
			"--override-hostname=pcs-test", "--override-username=maison",
			"--cache-directory=" + filepath.Join(engineDir, "cache"),
			"--config-file=" + p.configFile(),
		},
		Timeout: 5 * time.Minute,
	}, nil); err != nil {
		t.Fatalf("create repository: %v", err)
	}
	return p, cfg
}

// hostRootLocal is this container's view of the shared directory.
func hostRootLocal() string {
	if v := os.Getenv("MAISON_TEST_LOCAL_ROOT"); v != "" {
		return v
	}
	return os.TempDir()
}

const stamp1 = "2026-02-02_120000"
const stamp2 = "2026-02-03_120000"

// The repository config is where the host-side connect leaves the identity, and
// where Maison reads it from rather than computing its own — two sides computing it
// independently is how one repository becomes two lineages that never see each other.
func TestStatusReadsTheIdentityFromTheRepositoryConfig(t *testing.T) {
	p, _ := newRepo(t)
	st := p.Status(context.Background())
	if !st.Connected {
		t.Fatalf("Status = %+v, want connected", st)
	}
	if st.Host != "pcs-test" || st.User != "maison" {
		t.Errorf("identity = %s@%s, want maison@pcs-test", st.User, st.Host)
	}
	if st.Type != "filesystem" {
		t.Errorf("Type = %q, want %q", st.Type, "filesystem")
	}
}

// A box whose host-side setup has not run is "not configured", not broken.
func TestStatusOnAnUnconfiguredBox(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	st := New(cfg).Status(context.Background())
	if st.Connected {
		t.Fatal("an unconfigured box reported a connected repository")
	}
	if st.Detail == "" {
		t.Error("Status gave no reason, so the UI has nothing to show")
	}
	if _, err := New(cfg).List(context.Background(), "jellyfin"); err == nil {
		t.Error("List should report the engine is not configured")
	}
}

// The round trip that everything else rests on: a backup is written, listed under
// the stamp Maison chose, and restored with its contents intact.
func TestSnapshotCommitListMaterializeRoundTrip(t *testing.T) {
	p, cfg := newRepo(t)
	ctx := context.Background()
	src := AppSource("jellyfin")

	for _, pass := range []int{1, 2} {
		if err := p.SnapshotSource(ctx, src, stamp1, pass, nil); err != nil {
			t.Fatalf("snapshot pass %d: %v", pass, err)
		}
	}
	b, err := p.CommitSource(ctx, src, stamp1, nil)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if b.Name != stamp1 || b.Tier != apps.TierRemote || b.Engine != ID {
		t.Fatalf("committed %+v, want name %q, tier %q, engine %q", b, stamp1, apps.TierRemote, ID)
	}
	if b.Size == 0 {
		t.Error("committed backup has no size; rootEntry.summ.size did not decode")
	}

	// Exactly one row: the torn first pass must not be listed.
	got, err := p.List(ctx, "jellyfin")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Name != stamp1 {
		t.Fatalf("List = %+v, want exactly the committed backup", got)
	}

	if err := p.Materialize(ctx, "jellyfin", stamp1, nil); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	dst := filepath.Join(cfg.BackupsDir(), "jellyfin", stamp1, "db", "data.sqlite")
	if body, err := os.ReadFile(dst); err != nil || string(body) != "rows" {
		t.Fatalf("materialised data = %q (%v), want %q", body, err, "rows")
	}
}

// THE test for the user-data set. `/AppData/` must exclude the app tree without
// swallowing `AppDataShared/`, whose engine configuration is deliberately backed up
// — and each engine's cache and logs must stay out, or a multi-gigabyte cache ships
// offsite nightly for data that is rebuilt on demand.
func TestUserDataExclusionsAreAnchoredCorrectly(t *testing.T) {
	p, _ := newRepo(t)
	ctx := context.Background()
	src := UserDataSource()

	if err := p.EnsurePolicy(ctx, src, DefaultRetention()); err != nil {
		t.Fatalf("EnsurePolicy: %v", err)
	}
	if err := p.SnapshotSource(ctx, src, stamp1, 2, nil); err != nil {
		t.Fatalf("SnapshotSource: %v", err)
	}
	got, err := p.ListSource(ctx, src)
	if err != nil || len(got) != 1 {
		t.Fatalf("ListSource = %+v (%v), want one snapshot", got, err)
	}

	listed := p.contents(t, "userdata")
	for _, want := range []string{"Documents/notes.txt", "AppDataShared/backup/kopia/repository.password"} {
		if !strings.Contains(listed, want) {
			t.Errorf("%s is missing from the user-data snapshot:\n%s", want, listed)
		}
	}
	for _, unwanted := range []string{"AppData/jellyfin", "cache/blob", "logs/kopia.log"} {
		if strings.Contains(listed, unwanted) {
			t.Errorf("%s was backed up but must be excluded:\n%s", unwanted, listed)
		}
	}
}

// contents lists every path in the newest snapshot of a source, for assertions.
func (p *Provider) contents(t *testing.T, app string) string {
	t.Helper()
	if app == "userdata" {
		app = userDataApp
	}
	snaps, err := p.snapshots(context.Background(), app)
	if err != nil || len(snaps) == 0 {
		t.Fatalf("snapshots(%s) = %v (%v)", app, snaps, err)
	}
	out, err := p.run(context.Background(), nil, 2*time.Minute, "ls", "-lr", snaps[len(snaps)-1].ID)
	if err != nil {
		t.Fatalf("kopia ls: %v", err)
	}
	return string(out)
}

// Tags come back from --json under a "tag:" prefix they were not written with. A
// filter built from the read spelling silently matches nothing, so this pins the
// asymmetry rather than leaving it to be rediscovered.
func TestTagsRoundTripThroughTheJSONPrefix(t *testing.T) {
	p, _ := newRepo(t)
	ctx := context.Background()
	if err := p.SnapshotSource(ctx, AppSource("jellyfin"), stamp1, 2, nil); err != nil {
		t.Fatalf("SnapshotSource: %v", err)
	}
	snaps, err := p.snapshots(ctx, "jellyfin")
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots = %+v (%v)", snaps, err)
	}
	raw, _ := json.Marshal(snaps[0].Tags)
	if !strings.Contains(string(raw), jsonTagPrefix+tagStamp) {
		t.Errorf("tags came back as %s, want keys prefixed with %q", raw, jsonTagPrefix)
	}
	if got := snaps[0].tag(tagStamp); got != stamp1 {
		t.Errorf("tag(%s) = %q, want %q", tagStamp, got, stamp1)
	}
}

// A stamp read back from a repository is untrusted input. Validating it on the way
// in is what lets the traversal guard elsewhere stay exactly as strict as it is.
func TestListDropsSnapshotsWithAnUnusableStamp(t *testing.T) {
	p, _ := newRepo(t)
	ctx := context.Background()

	// Written directly, bypassing SnapshotSource's own check.
	if _, err := p.run(ctx, nil, 5*time.Minute,
		"snapshot", "create", p.sourcePath(AppSource("jellyfin")),
		"--tags", tagApp+":jellyfin",
		"--tags", tagStamp+":../../etc/passwd",
		"--tags", tagPass+":2",
	); err != nil {
		t.Fatalf("seeding a bad snapshot: %v", err)
	}
	got, err := p.List(ctx, "jellyfin")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List = %+v, want the unparsable stamp dropped", got)
	}
}

func TestSnapshotSourceRejectsABadStamp(t *testing.T) {
	p, _ := newRepo(t)
	if err := p.SnapshotSource(context.Background(), AppSource("jellyfin"), "not-a-stamp", 1, nil); err == nil {
		t.Fatal("SnapshotSource accepted a name that is not a stamp")
	}
}

// An interrupted backup must leave nothing a later List could offer for restore.
func TestAbortRemovesBothPasses(t *testing.T) {
	p, _ := newRepo(t)
	ctx := context.Background()
	src := AppSource("jellyfin")

	for _, pass := range []int{1, 2} {
		if err := p.SnapshotSource(ctx, src, stamp1, pass, nil); err != nil {
			t.Fatalf("snapshot pass %d: %v", pass, err)
		}
	}
	if err := p.AbortSource(ctx, src, stamp1); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	snaps, err := p.snapshots(ctx, "jellyfin")
	if err != nil {
		t.Fatalf("snapshots: %v", err)
	}
	if len(snaps) != 0 {
		t.Fatalf("Abort left %d snapshots behind", len(snaps))
	}
}

// The commit point: after it, only the consistent pass is reachable.
func TestCommitDropsTheTornFirstPass(t *testing.T) {
	p, _ := newRepo(t)
	ctx := context.Background()
	src := AppSource("jellyfin")

	for _, pass := range []int{1, 2} {
		if err := p.SnapshotSource(ctx, src, stamp1, pass, nil); err != nil {
			t.Fatalf("snapshot pass %d: %v", pass, err)
		}
	}
	if _, err := p.CommitSource(ctx, src, stamp1, nil); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	snaps, err := p.snapshots(ctx, "jellyfin")
	if err != nil {
		t.Fatalf("snapshots: %v", err)
	}
	if len(snaps) != 1 || snaps[0].tag(tagPass) != "2" {
		t.Fatalf("after commit, snapshots = %d, want only the consistent pass", len(snaps))
	}
}

// Deleting one backup must not touch another.
func TestDeleteRemovesOnlyTheNamedBackup(t *testing.T) {
	p, _ := newRepo(t)
	ctx := context.Background()
	src := AppSource("jellyfin")

	for _, s := range []string{stamp1, stamp2} {
		if err := p.SnapshotSource(ctx, src, s, 2, nil); err != nil {
			t.Fatalf("snapshot %s: %v", s, err)
		}
	}
	if err := p.Delete(ctx, "jellyfin", stamp1); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err := p.List(ctx, "jellyfin")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Name != stamp2 {
		t.Fatalf("List = %+v, want only %s", got, stamp2)
	}
}

// Progress is decoration: a kopia release that restyles its output must not be able
// to fail a backup.
func TestProgressParsingIsToleran(t *testing.T) {
	cases := map[string]float64{
		`| 1 hashing, 0 hashed (100.8 MB), 2 cached (16 B), uploaded 95.6 MB, estimated 125.8 MB (80.1%) 0s left`: 80.1,
		`* 0 hashing, 1 hashed (125.8 MB), estimated 125.8 MB (100.0%) 0s left`:                                   100,
		`Snapshotting maison@pcs-test:/DATA/AppData/jellyfin ...`:                                                 apps.PctUnknown,
		``: apps.PctUnknown,
	}
	for line, want := range cases {
		var got apps.Event
		emitLine(func(e apps.Event) { got = e }, line)
		if got.Pct != want {
			t.Errorf("emitLine(%q).Pct = %v, want %v", line, got.Pct, want)
		}
		if got.Message != line {
			t.Errorf("emitLine(%q) lost the message: %q", line, got.Message)
		}
	}
}

// The single most dangerous thing in this package: an in-place restore of the user-data
// set must never be able to delete AppData/.
//
// The mechanism it relies on is --delete-extra, which is what makes a restore a restore
// rather than a merge. Aimed at the data root it would delete everything the snapshot
// does not contain — and AppData/ is excluded from this snapshot by policy, so it would
// take every app's data with it. RestoreUserData therefore restores entry by entry and
// never names the data root as a target.
//
// This test exists because that property is invisible in the code: both spellings look
// like "restore the user data", and only one of them destroys the box.
func TestUserDataRestoreNeverTargetsTheDataRoot(t *testing.T) {
	p, cfg := newRepo(t)
	ctx := context.Background()
	src := UserDataSource()

	if err := p.EnsurePolicy(ctx, src, DefaultRetention()); err != nil {
		t.Fatalf("EnsurePolicy: %v", err)
	}
	if err := p.SnapshotSource(ctx, src, stamp1, 2, nil); err != nil {
		t.Fatalf("SnapshotSource: %v", err)
	}

	appFile := filepath.Join(cfg.AppsDir(), "jellyfin", "db", "data.sqlite")
	notes := filepath.Join(cfg.DataRoot, "Documents", "notes.txt")
	extra := filepath.Join(cfg.DataRoot, "Documents", "since-the-backup.txt")
	fresh := filepath.Join(cfg.DataRoot, "MadeLater")

	// Drift the tree the way a user would between the backup and the restore.
	if err := os.WriteFile(appFile, []byte("rows written after the backup"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(notes); err != nil { // deleted since
		t.Fatal(err)
	}
	if err := os.WriteFile(extra, []byte("new"), 0o644); err != nil { // created since
		t.Fatal(err)
	}
	if err := os.MkdirAll(fresh, 0o755); err != nil { // a whole new top-level tree
		t.Fatal(err)
	}

	// Everything the snapshot holds except the fixture's own repository. newRepo puts a
	// *filesystem* repository at ${DataRoot}/repo so the tests need no bucket, which puts
	// it inside the user-data set — a layout no real deployment has, and one where the
	// restore would be reading from the very directory it is writing to. Naming the rest
	// exercises the same per-entry loop.
	all, err := p.entries(ctx, mustSnapshotID(t, p, stamp1))
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, e := range all {
		if e.name != "repo" {
			want = append(want, e.name)
		}
	}
	if err := p.RestoreUserData(ctx, stamp1, apps.UserDataRestoreOpts{Entries: want}, nil); err != nil {
		t.Fatalf("RestoreUserData: %v", err)
	}

	// The property this test exists for.
	got, err := os.ReadFile(appFile)
	if err != nil {
		t.Fatalf("an in-place user-data restore deleted app data: %v", err)
	}
	if string(got) != "rows written after the backup" {
		t.Errorf("app data = %q; the restore must not touch AppData at all", got)
	}

	// Within a restored entry it is an exact restore, not a merge.
	if b, err := os.ReadFile(notes); err != nil || string(b) != "user notes" {
		t.Errorf("Documents/notes.txt = %q (%v); a deleted file must come back", b, err)
	}
	if _, err := os.Stat(extra); !os.IsNotExist(err) {
		t.Error("a file created after the backup survived the restore; --delete-extra did not apply")
	}

	// A top-level tree that did not exist at snapshot time is left alone, rather than
	// silently deleted. See RestoreUserData for why that boundary is where it is.
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a top-level directory made after the backup was deleted: %v", err)
	}
}

// entries is parsed out of `kopia ls -l`, which has no --json, so the field layout is a
// contract with the CLI exactly like the tag prefix is.
func TestEntriesReadsTopLevelNamesAndDirFlags(t *testing.T) {
	p, _ := newRepo(t)
	ctx := context.Background()
	src := UserDataSource()

	if err := p.EnsurePolicy(ctx, src, DefaultRetention()); err != nil {
		t.Fatalf("EnsurePolicy: %v", err)
	}
	if err := p.SnapshotSource(ctx, src, stamp1, 2, nil); err != nil {
		t.Fatalf("SnapshotSource: %v", err)
	}
	id, err := p.snapshotID(ctx, userDataApp, stamp1)
	if err != nil {
		t.Fatal(err)
	}

	got, err := p.entries(ctx, id)
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	byName := map[string]bool{}
	for _, e := range got {
		byName[e.name] = e.dir
	}
	if dir, ok := byName["Documents"]; !ok || !dir {
		t.Errorf("entries = %+v; want Documents present and marked a directory", got)
	}
	// The exclusion holds here too: what is not in the snapshot cannot be a target.
	if _, ok := byName["AppData"]; ok {
		t.Error("AppData appeared in the snapshot's entries; it must be excluded")
	}
	for _, e := range got {
		if strings.ContainsAny(e.name, "/\\") || e.name == "" {
			t.Errorf("entry %q would not be safe to join onto a path", e.name)
		}
	}
}

// mustSnapshotID resolves the user-data snapshot for a stamp.
func mustSnapshotID(t *testing.T, p *Provider, stamp string) string {
	t.Helper()
	id, err := p.snapshotID(context.Background(), userDataApp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// AppDataShared holds the engines' own configuration and is in the snapshot on purpose,
// so that recovering any engine's backup returns the credentials for the others. In
// place that is the one directory whose live copy is more current than the backup by
// definition — and it is being read by the engine doing the restore.
func TestInPlaceRestoreLeavesTheEngineConfigurationAlone(t *testing.T) {
	p, cfg := newRepo(t)
	ctx := context.Background()
	src := UserDataSource()

	if err := p.EnsurePolicy(ctx, src, DefaultRetention()); err != nil {
		t.Fatalf("EnsurePolicy: %v", err)
	}
	if err := p.SnapshotSource(ctx, src, stamp1, 2, nil); err != nil {
		t.Fatalf("SnapshotSource: %v", err)
	}

	// Something written into the engine's directory after the backup was taken. A
	// --delete-extra restore of AppDataShared would remove it, because the snapshot does
	// not contain it — which is precisely what must not happen to the directory holding
	// the credentials the running restore is using. (The password file itself is not the
	// probe: rewriting it would lock the engine out of its own repository, which is the
	// same failure this guard exists to prevent, arriving early.)
	marker := filepath.Join(cfg.BackupEngineDir(ID), "connected-since-the-backup")
	if err := os.WriteFile(marker, []byte("newer than the snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := p.RestoreUserData(ctx, stamp1, apps.UserDataRestoreOpts{Entries: []string{"AppDataShared"}}, nil); err != nil {
		t.Fatalf("RestoreUserData: %v", err)
	}

	if _, err := os.Stat(marker); err != nil {
		t.Errorf("an in-place restore rolled back the engine's own directory (%v); it must "+
			"leave the credentials the running restore is using alone", err)
	}
}

// The credentials file is written by a shell script on the host, so its format is a
// contract with something this package cannot compile against: comments, blank lines
// and the padding `=` of a base64 secret all have to survive the parse.
func TestCredentialsFileParsing(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	p := New(cfg)
	dir := cfg.BackupEngineDir(ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	// An absent file is the filesystem-repository case and must not be an error.
	got, err := p.credentials()
	if err != nil {
		t.Fatalf("credentials() on a box with no file: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("credentials() = %v, want empty", got)
	}

	body := "# Generated by ensure-backup-config.sh - DO NOT EDIT.\n" +
		"\n" +
		"AWS_ACCESS_KEY_ID=0031abc\n" +
		"AWS_SECRET_ACCESS_KEY=K003zz+/base64==\n"
	if err := os.WriteFile(filepath.Join(dir, "credentials.env"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err = p.credentials()
	if err != nil {
		t.Fatalf("credentials(): %v", err)
	}
	// Split on the FIRST `=` only: a base64 secret carries its own padding, and
	// splitting on all of them silently truncates the credential to "K003zz+/base64".
	if got["AWS_SECRET_ACCESS_KEY"] != "K003zz+/base64==" {
		t.Errorf("secret = %q, want the padding preserved", got["AWS_SECRET_ACCESS_KEY"])
	}
	if got["AWS_ACCESS_KEY_ID"] != "0031abc" {
		t.Errorf("key id = %q", got["AWS_ACCESS_KEY_ID"])
	}
	if len(got) != 2 {
		t.Errorf("credentials() = %v, want exactly the two keys", got)
	}
}

// The kopia image ships KOPIA_CACHE_DIRECTORY=/app/cache and KOPIA_LOG_DIR=/app/logs,
// and they outrank --cache-directory and --log-dir. /app is root's, so an engine
// running as PUID cannot create either and every command fails before it reaches the
// repository. Both must land beside the rest of the engine's state.
func TestEngineEnvOverridesTheImagesBakedInPaths(t *testing.T) {
	cfg := config.Config{DataRoot: "/DATA"}
	p := New(cfg)
	env := p.engineEnv()

	if got, want := env["KOPIA_CACHE_DIRECTORY"], "/DATA/AppDataShared/backup/kopia/cache"; got != want {
		t.Errorf("KOPIA_CACHE_DIRECTORY = %q, want %q", got, want)
	}
	if got, want := env["KOPIA_LOG_DIR"], "/DATA/AppDataShared/backup/kopia/logs"; got != want {
		t.Errorf("KOPIA_LOG_DIR = %q, want %q", got, want)
	}
	for k, v := range env {
		if strings.HasPrefix(v, "/app") {
			t.Errorf("%s still points inside the image at %q", k, v)
		}
	}
}
