package stackup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yundera/maison/internal/dockerx"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// failingRunner fails the run whose image is failOn, and records every spec.
type failingRunner struct {
	specs  []dockerx.RunSpec
	failOn string
}

func (f *failingRunner) RunOnce(_ context.Context, spec dockerx.RunSpec) (string, error) {
	f.specs = append(f.specs, spec)
	if spec.Image == f.failOn {
		return "", errors.New("exited 1: cannot convert")
	}
	return "", nil
}

func withFailingRunner(t *testing.T, f *failingRunner) {
	t.Helper()
	prev := newRunner
	newRunner = func() (Runner, error) { return f, nil }
	t.Cleanup(func() { newRunner = prev })
}

func threeSteps() MigratePlan {
	return MigratePlan{From: 1, To: 4, Steps: []xcomposeapp.Migration{
		{ID: 2, Name: xcomposeapp.Localized{"en_us": "pg-14-to-16"}, Image: "pgautoupgrade:16",
			Volumes: []string{"/DATA/AppData/${AppID}/db:/var/lib/postgresql/data"}},
		{ID: 3, Image: "convert:3"},
		{ID: 4, Image: "convert:4"},
	}}
}

func TestRunMigrationsInOrderWithAMarkerPerStep(t *testing.T) {
	f := &failingRunner{}
	withFailingRunner(t, f)
	cfg, dir := dataRootApp(t, "odoo", "")
	var seen []string
	err := RunMigrations(context.Background(), cfg, "odoo", dir, threeSteps(), map[string]string{},
		func(i, n int, m xcomposeapp.Migration) { seen = append(seen, m.Label()) })
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(seen, ","); got != "pg-14-to-16,step 3,step 4" {
		t.Errorf("progress = %s", got)
	}
	if len(f.specs) != 3 || f.specs[0].Image != "pgautoupgrade:16" || f.specs[2].Image != "convert:4" {
		t.Fatalf("ran %+v", f.specs)
	}
	// The daemon resolves a bind source, so it is spelled as the host spells it.
	if b := f.specs[0].Binds; len(b) != 1 || b[0] != "/host/DATA/AppData/odoo/db:/var/lib/postgresql/data" {
		t.Errorf("binds = %v", b)
	}
	if v, _, _ := ReadDataVersion(dir); v != 4 {
		t.Errorf("marker = %d, want 4", v)
	}
}

// A failure stops the run with the marker at the last step that succeeded — the
// rollback point puts it back, but the log and the folder say how far it got.
func TestRunMigrationsStopsAtTheFailingStep(t *testing.T) {
	f := &failingRunner{failOn: "convert:3"}
	withFailingRunner(t, f)
	cfg, dir := dataRootApp(t, "odoo", "")
	err := RunMigrations(context.Background(), cfg, "odoo", dir, threeSteps(), map[string]string{}, nil)
	if err == nil || !strings.Contains(err.Error(), "data migration 3") {
		t.Fatalf("err = %v", err)
	}
	if len(f.specs) != 2 {
		t.Errorf("ran %d steps, want the run to stop at the failing one", len(f.specs))
	}
	if v, _, _ := ReadDataVersion(dir); v != 2 {
		t.Errorf("marker = %d, want 2", v)
	}
}

func TestRunMigrationsRefusesABlockedStep(t *testing.T) {
	cfg, dir := dataRootApp(t, "odoo", "")
	plan := MigratePlan{From: 1, To: 2, Steps: []xcomposeapp.Migration{{ID: 2, Blocked: xcomposeapp.Localized{"en_us": "by hand"}}}}
	if err := RunMigrations(context.Background(), cfg, "odoo", dir, plan, nil, nil); err == nil {
		t.Fatal("a blocked step ran")
	}
	if _, ok, _ := ReadDataVersion(dir); ok {
		t.Error("a refused step wrote the marker")
	}
}

// A script step runs from .migrate/ in the hook environment, and is told where the
// data is going.
func TestRunMigrationsScriptStep(t *testing.T) {
	fixtureHookBin(t)
	cfg, dir := hookTestApp(t)
	os.MkdirAll(filepath.Join(dir, MigrateDir, "migrations"), 0o755)
	os.WriteFile(filepath.Join(dir, MigrateDir, "migrations", "0002.sh"),
		[]byte(`echo "$MIGRATION_ID $DATA_VERSION_FROM $DATA_VERSION_TO /DATA" > seen`), 0o644)
	plan := MigratePlan{From: 1, To: 2, Steps: []xcomposeapp.Migration{{ID: 2, Script: "migrations/0002.sh"}}}
	if err := RunMigrations(context.Background(), cfg, "app", dir, plan, nil, nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "seen"))
	if err != nil {
		t.Fatal(err)
	}
	// /DATA is rewritten to the host path, as in any hook.
	if got := strings.TrimSpace(string(b)); got != "2 1 2 /hostroot" {
		t.Errorf("script saw %q", got)
	}
	if v, _, _ := ReadDataVersion(dir); v != 2 {
		t.Errorf("marker = %d", v)
	}
}

func TestRunMigrationsScriptFailureFailsTheStep(t *testing.T) {
	fixtureHookBin(t)
	cfg, dir := hookTestApp(t)
	os.MkdirAll(filepath.Join(dir, MigrateDir, "migrations"), 0o755)
	os.WriteFile(filepath.Join(dir, MigrateDir, "migrations", "x.sh"), []byte("exit 3"), 0o644)
	plan := MigratePlan{From: 1, To: 2, Steps: []xcomposeapp.Migration{{ID: 2, Script: "migrations/x.sh", Timeout: "5s"}}}
	if err := RunMigrations(context.Background(), cfg, "app", dir, plan, nil, nil); err == nil {
		t.Fatal("a script exiting 3 was recorded as applied")
	}
	if _, ok, _ := ReadDataVersion(dir); ok {
		t.Error("a failed step wrote the marker")
	}
}

// Migrations run after variables and before files: a create-if-absent default must
// not take the path a migration is about to move the user's real data into.
func TestConvergeRunsMigrationsBeforeFiles(t *testing.T) {
	cfg, dir := dataRootApp(t, "app", "")
	spec := Spec{Files: []xcomposeapp.File{{Path: "/DATA/AppData/${AppID}/config.yml", Content: "default"}}}
	target := filepath.Join(dir, "config.yml")
	migrate := func(context.Context, map[string]string) error {
		if _, err := os.Stat(target); err == nil {
			t.Error("files ran before the migration")
		}
		return os.WriteFile(target, []byte("migrated"), 0o644)
	}
	if _, err := converge(context.Background(), cfg, "app", dir, spec, nil, migrate); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "migrated" {
		t.Errorf("config.yml = %q, want the migration's", b)
	}
}

// In update mode the marker must be where the plan starts; anything else is a
// folder the plan was not made for.
func TestUpWithMigrateRefusesAMarkerOffThePlan(t *testing.T) {
	cfg, dir := dataRootApp(t, "odoo", "")
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(composeV3), 0o644)
	WriteDataVersion(dir, 2)
	plan := MigratePlan{From: 1, To: 3}
	err := UpWith(context.Background(), cfg, "odoo", dir, []string{filepath.Join(dir, "docker-compose.yml")}, UpOptions{Migrate: &plan})
	if err == nil || !strings.Contains(err.Error(), "not 1") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".env")); !os.IsNotExist(err) {
		t.Error("a refused update still wrote the app's .env")
	}
}
