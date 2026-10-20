package stackup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yundera/maison/internal/xcomposeapp"
)

func scriptStep(id int, body string) xcomposeapp.Migration {
	return xcomposeapp.Migration{ID: id, Script: xcomposeapp.Script{Script: body}}
}

func threeSteps() MigratePlan {
	return MigratePlan{From: 1, To: 4, Steps: []xcomposeapp.Migration{
		{ID: 2, Name: xcomposeapp.Localized{"en_us": "pg-14-to-16"}, Script: xcomposeapp.Script{Script: `echo "$MIGRATION_ID" >> ran`}},
		scriptStep(3, `echo "$MIGRATION_ID" >> ran`),
		scriptStep(4, `echo "$MIGRATION_ID" >> ran`),
	}}
}

func ranSteps(t *testing.T, dir string) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(dir, "ran"))
	return strings.Join(strings.Fields(string(b)), ",")
}

func TestRunMigrationsInOrderWithAMarkerPerStep(t *testing.T) {
	fixtureHookBin(t)
	cfg, dir := hookTestApp(t)
	var seen []string
	err := RunMigrations(context.Background(), cfg, "app", dir, threeSteps(), map[string]string{},
		func(i, n int, m xcomposeapp.Migration) { seen = append(seen, m.Label()) })
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(seen, ","); got != "pg-14-to-16,step 3,step 4" {
		t.Errorf("progress = %s", got)
	}
	if got := ranSteps(t, dir); got != "2,3,4" {
		t.Fatalf("ran %s", got)
	}
	if v, _, _ := ReadDataVersion(dir); v != 4 {
		t.Errorf("marker = %d, want 4", v)
	}
}

// A failure stops the run with the marker at the last step that succeeded — the
// rollback point puts it back, but the log and the folder say how far it got.
func TestRunMigrationsStopsAtTheFailingStep(t *testing.T) {
	fixtureHookBin(t)
	cfg, dir := hookTestApp(t)
	plan := threeSteps()
	plan.Steps[1] = scriptStep(3, `echo "$MIGRATION_ID" >> ran; exit 1`)
	err := RunMigrations(context.Background(), cfg, "app", dir, plan, map[string]string{}, nil)
	if err == nil || !strings.Contains(err.Error(), "data migration 3") {
		t.Fatalf("err = %v", err)
	}
	if got := ranSteps(t, dir); got != "2,3" {
		t.Errorf("ran %s, want the run to stop at the failing step", got)
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

// A file step runs from .scripts/, in the shared script environment, and is told
// where the data is going. What it writes to $MAISON_OUTPUT reaches the converge.
func TestRunMigrationsFileStep(t *testing.T) {
	fixtureHookBin(t)
	cfg, dir := hookTestApp(t)
	os.MkdirAll(filepath.Join(dir, ScriptsStateDir), 0o755)
	os.WriteFile(filepath.Join(dir, ScriptsStateDir, "0002.sh"),
		[]byte(`echo "$MIGRATION_ID $DATA_VERSION_FROM $DATA_VERSION_TO $APP_DIR" > seen
echo "CONVERTED=yes" >> "$MAISON_OUTPUT"`), 0o644)
	plan := MigratePlan{From: 1, To: 2, Steps: []xcomposeapp.Migration{{ID: 2, Script: xcomposeapp.Script{File: "scripts/0002.sh"}}}}
	captures := map[string]string{}
	if err := RunMigrations(context.Background(), cfg, "app", dir, plan, captures, nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "seen"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(b)); got != "2 1 2 /hostroot/AppData/app" {
		t.Errorf("script saw %q", got)
	}
	if captures["CONVERTED"] != "yes" {
		t.Errorf("captures = %v, want the step's output", captures)
	}
	if v, _, _ := ReadDataVersion(dir); v != 2 {
		t.Errorf("marker = %d", v)
	}
}

func TestRunMigrationsScriptFailureFailsTheStep(t *testing.T) {
	fixtureHookBin(t)
	cfg, dir := hookTestApp(t)
	plan := MigratePlan{From: 1, To: 2, Steps: []xcomposeapp.Migration{{ID: 2, Script: xcomposeapp.Script{Script: "exit 3", Timeout: "5s"}}}}
	if err := RunMigrations(context.Background(), cfg, "app", dir, plan, map[string]string{}, nil); err == nil {
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
