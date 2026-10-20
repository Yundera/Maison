package installer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yundera/maison/internal/appstore"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/incident"
	"github.com/yundera/maison/internal/stackup"
)

// The rollback path is the interesting half of an update, and it is reachable
// without a store or a Docker daemon: rollBack is where the ordering and the
// error-reporting decisions live, and both are easy to get subtly wrong.

func TestRollBackReportsBothFailures(t *testing.T) {
	in := &Installer{
		RollBack: func(context.Context, string, string) error { return errors.New("disk is full") },
	}
	cause := errors.New("compose up failed")

	res, err := in.rollBack(context.Background(), "jellyfin", UpdateResult{Backup: "2026-01-01_000000"}, cause)

	if err == nil {
		t.Fatal("a failed rollback must surface as an error")
	}
	// Both, because the operator needs to know the update failed *and* that the app
	// is now in neither state — reporting only one of those hides the worse half.
	for _, want := range []string{"compose up failed", "disk is full"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if res.RolledBack {
		t.Error("a failed rollback reported itself as having rolled back")
	}
	if res.Warning == "" {
		t.Error("a failed rollback left no warning for the UI")
	}
}

func TestRollBackSucceedsAndStillReportsTheCause(t *testing.T) {
	var restored string
	in := &Installer{
		RollBack: func(_ context.Context, _, name string) error { restored = name; return nil },
	}
	cause := errors.New("compose up failed")

	res, err := in.rollBack(context.Background(), "jellyfin", UpdateResult{Backup: "2026-01-01_000000"}, cause)

	if restored != "2026-01-01_000000" {
		t.Errorf("restored %q, want the rollback point taken before the update", restored)
	}
	if !res.RolledBack {
		t.Error("a successful rollback was not reported")
	}
	// The update still failed. Returning nil here would render as a successful
	// update on a tile whose app is running the *old* version.
	if err == nil || !errors.Is(err, cause) {
		t.Fatalf("err = %v, want it to wrap the original failure", err)
	}
	if res.Applied {
		t.Error("a rolled-back update reported itself as applied")
	}
}

// With no rollback point there is nothing to undo, and saying so is better than a
// bare "compose up failed" that leaves the operator wondering what state the app is
// in.
func TestRollBackWithoutABackupPointSaysSo(t *testing.T) {
	in := &Installer{RollBack: func(context.Context, string, string) error { return nil }}
	_, err := in.rollBack(context.Background(), "jellyfin", UpdateResult{}, errors.New("compose up failed"))
	if err == nil || !strings.Contains(err.Error(), "could not be undone") {
		t.Fatalf("err = %v, want it to say the update could not be undone", err)
	}
}

// A box with no Docker has no rollback hooks wired at all. That must not panic.
func TestRollBackWithNoHookWired(t *testing.T) {
	in := &Installer{}
	_, err := in.rollBack(context.Background(), "jellyfin", UpdateResult{Backup: "2026-01-01_000000"}, errors.New("boom"))
	if err == nil {
		t.Fatal("expected an error")
	}
}

// --- the store reference in the override --------------------------------------

// refFixture makes an app folder holding just an override, and an Installer that
// reads from it.
func refFixture(t *testing.T, override string) (*Installer, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "AppData", "jellyfin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if override != "" {
		if err := os.WriteFile(filepath.Join(dir, "docker-compose.override.yml"), []byte(override), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &Installer{cfg: config.Config{DataRoot: root}}, dir
}

func TestUpdateRefRoundTripsThroughTheOverride(t *testing.T) {
	in, dir := refFixture(t, "")
	want := appstore.ParseRef("github.com/Yundera/AppStore/archive/main.zip/-/catalog/apps/Jellyfin")

	if err := writeUpdateRef(dir, want); err != nil {
		t.Fatalf("writeUpdateRef: %v", err)
	}
	// One key, holding the whole reference — the same string the store's own deep
	// link carries, which is what makes it pasteable.
	raw, err := os.ReadFile(filepath.Join(dir, "docker-compose.override.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "store-ref: github.com/Yundera/AppStore/archive/main.zip/-/catalog/apps/Jellyfin") {
		t.Errorf("override does not carry the locator:\n%s", raw)
	}

	if got := in.readUpdateRef("jellyfin"); got != want {
		t.Errorf("read back %+v, want %+v", got, want)
	}
}

// An app installed before the single-locator form still updates from where it came
// from: the three-field spelling is read when store-ref is absent.
func TestUpdateRefReadsTheSupersededFields(t *testing.T) {
	in, _ := refFixture(t, `x-compose-app:
  store: https://github.com/Yundera/AppStore/archive/main.zip
  store-app-id: Jellyfin
  store-apps-path: catalog/apps
`)
	got := in.readUpdateRef("jellyfin")
	want := appstore.Ref{
		URL:      "https://github.com/Yundera/AppStore/archive/main.zip",
		AppsPath: "catalog/apps",
		ID:       "Jellyfin",
	}
	if got != want {
		t.Errorf("read %+v, want %+v", got, want)
	}
}

// Writing must not leave the old fields behind: two records of where an app
// updates from, one of them stale, sends a retargeted app back to the store it was
// moved off.
func TestWriteUpdateRefDropsTheSupersededFields(t *testing.T) {
	in, dir := refFixture(t, `x-compose-app:
  store: https://github.com/Yundera/AppStore/archive/main.zip
  store-app-id: Jellyfin
  webui-host: jellyfin-${domain}
services:
  jellyfin:
    mem_limit: 2g
`)
	moved := appstore.ParseRef("git.example.org/lab/-/archive/main/lab.zip/-/Apps/Jellyfin")
	if err := writeUpdateRef(dir, moved); err != nil {
		t.Fatalf("writeUpdateRef: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "docker-compose.override.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"store-app-id:", "store-apps-path:", "store: "} {
		if strings.Contains(string(raw), gone) {
			t.Errorf("override still carries %q:\n%s", gone, raw)
		}
	}
	// Everything else the operator put in the override survives the rewrite.
	for _, kept := range []string{"webui-host:", "mem_limit:"} {
		if !strings.Contains(string(raw), kept) {
			t.Errorf("override lost %q:\n%s", kept, raw)
		}
	}
	if got := in.readUpdateRef("jellyfin"); got != moved {
		t.Errorf("read back %+v, want the store it was moved to (%+v)", got, moved)
	}
}

// No reference at all is not an error — it is an app Maison merely discovered, and
// the Update tab says so rather than failing.
func TestReadUpdateRefWithoutAnOverride(t *testing.T) {
	in, _ := refFixture(t, "")
	if got := in.readUpdateRef("jellyfin"); got.ID != "" {
		t.Errorf("read %+v, want the zero reference", got)
	}
}

// --- the data plan --------------------------------------------------------------

func dataPlanFixture(t *testing.T, installed string, marker int) (*Installer, string, *[]map[string]string) {
	t.Helper()
	in, dir := refFixture(t, "")
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(installed), 0o644); err != nil {
		t.Fatal(err)
	}
	if marker > 0 {
		if err := stackup.WriteDataVersion(dir, marker); err != nil {
			t.Fatal(err)
		}
	}
	var args []map[string]string
	in.Report = func(r incident.Report) { args = append(args, r.Args) }
	// A wired rollback point, as on a real box: without one any step is refused.
	in.BackupBeforeUpdate = func(context.Context, string, func(UpdateState)) (string, string, error) { return "rp", "", nil }
	return in, dir, &args
}

const (
	plainCompose   = "services: {app: {image: odoo:18.0}}\n"
	migrateCompose = "services: {app: {image: odoo:19.0}}\nx-compose-app: {data_version: 2, migrations: [{id: 2, name: {en_us: openupgrade}, image: openupgrade:19}]}\n"
)

func TestPlanLetsAnOrdinaryUpdateThrough(t *testing.T) {
	in, dir, reported := dataPlanFixture(t, plainCompose, 0)
	plan, err := in.planUpdateData("odoo", dir, []byte("services: {app: {image: odoo:18.1}}\n"), UpdateOptions{})
	if err != nil || len(plan.Steps) != 0 || plan.From != 1 || plan.To != 1 {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	if len(*reported) != 0 {
		t.Errorf("incidents raised: %v", *reported)
	}
}

func TestPlanCarriesPendingSteps(t *testing.T) {
	in, dir, _ := dataPlanFixture(t, plainCompose, 0)
	plan, err := in.planUpdateData("odoo", dir, []byte(migrateCompose), UpdateOptions{})
	if err != nil || plan.From != 1 || plan.To != 2 || len(plan.Steps) != 1 || plan.Steps[0].Label() != "openupgrade" {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	if imgs := migrationImages(plan); len(imgs) != 1 || imgs[0] != "openupgrade:19" {
		t.Errorf("images to pull = %v", imgs)
	}
}

func TestPlanRefusals(t *testing.T) {
	cases := []struct {
		name      string
		installed string
		marker    int
		next      string
		opts      UpdateOptions
		noHook    bool
		reason    string
	}{
		{name: "blocked step", installed: plainCompose,
			next:   "services: {app: {image: odoo:19.0}}\nx-compose-app: {data_version: 2, migrations: [{id: 2, blocked: {en_us: Needs OpenUpgrade.}}]}\n",
			reason: DataRefusedBlocked},
		{name: "data newer than the store", installed: plainCompose, marker: 3, next: plainCompose, reason: DataRefusedNewer},
		{name: "malformed manifest", installed: plainCompose,
			next:   "services: {app: {image: odoo:19.0}}\nx-compose-app: {data_version: 3, migrations: [{id: 3, image: x:1}]}\n",
			reason: DataRefusedStoreError},
		{name: "update without backup", installed: plainCompose, next: migrateCompose,
			opts: UpdateOptions{NoBackup: true}, reason: DataRefusedNoBackup},
		{name: "no rollback hook wired", installed: plainCompose, next: migrateCompose,
			noHook: true, reason: DataRefusedNoBackup},
	}
	for _, c := range cases {
		in, dir, reported := dataPlanFixture(t, c.installed, c.marker)
		if c.noHook {
			in.BackupBeforeUpdate = nil
		}
		_, err := in.planUpdateData("odoo", dir, []byte(c.next), c.opts)
		var refused *DataRefusedError
		if !errors.As(err, &refused) || refused.Reason != c.reason {
			t.Errorf("%s: err = %v, want reason %s", c.name, err, c.reason)
			continue
		}
		if len(*reported) != 1 || (*reported)[0]["reason"] != c.reason {
			t.Errorf("%s: incidents %v, want reason %s", c.name, *reported, c.reason)
		}
	}
}

// The baseline comes from the compose being replaced: an install with no marker
// whose compose already declares data_version 2 is at 2, not 1.
func TestPlanBaselineIsTheInstalledCompose(t *testing.T) {
	in, dir, _ := dataPlanFixture(t, migrateCompose, 0)
	plan, err := in.planUpdateData("app", dir, []byte(strings.Replace(migrateCompose, "odoo:19.0", "odoo:19.1", 1)), UpdateOptions{})
	if err != nil || plan.From != 2 || len(plan.Steps) != 0 {
		t.Fatalf("plan = %+v, %v; want no steps from 2", plan, err)
	}
}

func TestDescribeDataStates(t *testing.T) {
	_, dir, _ := dataPlanFixture(t, plainCompose, 0)
	cases := map[string]struct{ next, state string }{
		"ordinary": {"services: {app: {image: odoo:18.1}}\n", StateAvailable},
		"steps":    {migrateCompose, StateAvailable},
		"blocked":  {"services: {}\nx-compose-app: {data_version: 2, migrations: [{id: 2, blocked: {en_us: by hand}}]}\n", StateBlocked},
		"broken":   {"services: {}\nx-compose-app: {data_version: 0}\n", StateError},
	}
	for name, c := range cases {
		dp, state, _ := describeData(dir, []byte(c.next))
		if state != c.state {
			t.Errorf("%s: state %s, want %s", name, state, c.state)
		}
		if name == "steps" && (len(dp.Migrations) != 1 || dp.DataFrom != 1 || dp.DataTo != 2) {
			t.Errorf("steps: %+v", dp)
		}
		if name == "ordinary" && (dp.DataTo != 0 || dp.Migrations != nil) {
			t.Errorf("ordinary update describes data: %+v", dp)
		}
		if name == "blocked" && dp.Blocked == "" {
			t.Error("blocked: no message")
		}
	}
}

func TestWriteMigrationsReplacesTheCopy(t *testing.T) {
	_, dir := refFixture(t, "")
	stale := filepath.Join(dir, stackup.MigrateDir, "old.sh")
	os.MkdirAll(filepath.Dir(stale), 0o755)
	os.WriteFile(stale, []byte("exit 0"), 0o644)
	if err := writeMigrations(nil, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a script the store no longer ships survived the update")
	}
}

// An update that moves an app off the shared password onto a seeded secret must
// carry the old value over: the app's database was initialised with it. The update
// path once handed EnsureSecrets no prior .env, so the secret was generated afresh
// and ghost and docmost lost their databases (yunderalabs, 2026-10-17).
func TestApplyUpdateSeedsSecretsFromTheInstalledEnv(t *testing.T) {
	compose := "name: ghost\n" +
		"services:\n" +
		"  app:\n" +
		"    image: example/ghost:2\n" +
		"    environment:\n" +
		"      DB_PASSWORD: ${GHOST_DB_PASSWORD}\n" +
		"x-compose-app:\n" +
		"  schema_version: 2\n" +
		"  title:\n" +
		"    en_us: ghost\n" +
		"  secrets:\n" +
		"    GHOST_DB_PASSWORD: ${APP_DEFAULT_PASSWORD}|alnum:32\n"
	srv := refStoreServer(t, storeZipOf(t, "ghost", compose))
	in, dir := refInstaller(t, "ghost", "name: ghost\nservices:\n  app:\n    image: example/ghost:1\n")
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("APP_DEFAULT_PASSWORD=the-old-one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := in.SetUpdateRef(context.Background(), "ghost", srv.URL+"/store.zip/-/Apps/ghost"); err != nil {
		t.Fatalf("SetUpdateRef: %v", err)
	}
	in.RollBack = func(context.Context, string, string) error { return nil }

	_, _ = in.ApplyUpdate(context.Background(), "ghost", UpdateOptions{NoBackup: true}) // no Docker: start fails after the secrets

	env, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), "GHOST_DB_PASSWORD=the-old-one\n") {
		t.Errorf("seeded secret not carried over from the installed .env:\n%s", env)
	}
}
