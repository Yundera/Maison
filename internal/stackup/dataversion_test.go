package stackup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yundera/maison/internal/incident"
)

func dataFixture(t *testing.T, compose string) string {
	t.Helper()
	dir := t.TempDir()
	if compose != "" {
		if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(compose), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const composeV3 = `services: {app: {image: a:1}}
x-compose-app:
  data_version: 3
  migrations:
    - {id: 2, script: "true"}
    - {id: 3, script: "true"}
`

func TestDataVersionMarkerRoundTrips(t *testing.T) {
	dir := dataFixture(t, "")
	if _, ok, err := ReadDataVersion(dir); ok || err != nil {
		t.Fatalf("empty folder: ok=%v err=%v", ok, err)
	}
	if err := WriteDataVersion(dir, 3); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := ReadDataVersion(dir); v != 3 || !ok || err != nil {
		t.Fatalf("got %d %v %v", v, ok, err)
	}
	// Nothing but the marker is left behind by the atomic write.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != DataVersionFile {
		t.Fatalf("folder holds %v", entries)
	}
}

func TestReadDataVersionRejectsGarbage(t *testing.T) {
	dir := dataFixture(t, "")
	for _, body := range []string{"three", "0", "-1", ""} {
		os.WriteFile(filepath.Join(dir, DataVersionFile), []byte(body), 0o644)
		if _, _, err := ReadDataVersion(dir); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
}

// The baseline rule: no marker means the data is where the compose on disk says,
// which is 1 for every compose that predates the feature.
func TestInstalledDataVersionBaseline(t *testing.T) {
	if _, ok, err := InstalledDataVersion(dataFixture(t, "")); ok || err != nil {
		t.Fatalf("empty folder: ok=%v err=%v", ok, err)
	}
	if v, ok, err := InstalledDataVersion(dataFixture(t, "services: {app: {image: a:1}}\n")); v != 1 || !ok || err != nil {
		t.Fatalf("plain compose: %d %v %v", v, ok, err)
	}
	dir := dataFixture(t, composeV3)
	if v, _, _ := InstalledDataVersion(dir); v != 3 {
		t.Fatalf("declared compose: %d, want 3", v)
	}
	WriteDataVersion(dir, 2)
	if v, _, _ := InstalledDataVersion(dir); v != 2 {
		t.Fatalf("the marker wins over the compose: got %d", v)
	}
}

func TestCheckDataVersion(t *testing.T) {
	// No marker: cannot fail, whatever the compose declares.
	dir := dataFixture(t, composeV3)
	if err := CheckDataVersion(dir); err != nil {
		t.Fatalf("no marker: %v", err)
	}
	WriteDataVersion(dir, 3)
	if err := CheckDataVersion(dir); err != nil {
		t.Fatalf("agreeing marker: %v", err)
	}
	for _, v := range []int{2, 4} {
		WriteDataVersion(dir, v)
		var dv *DataVersionError
		if err := CheckDataVersion(dir); !errors.As(err, &dv) || dv.Data != v || dv.Compose != 3 {
			t.Errorf("marker %d: err = %v", v, err)
		}
	}
}

// A compose installed under older manifest rules — here a migration name written as
// a plain string, before names were locale maps — still starts and still reports
// its version, so an update can move it to the store's current format.
func TestInstalledComposeIsNotRejudged(t *testing.T) {
	const legacy = `services: {app: {image: a:1}}
x-compose-app:
  data_version: 2
  migrations:
    - {id: 2, name: postgres-13-to-17, script: "true"}
`
	dir := dataFixture(t, legacy)
	if v, ok, err := InstalledDataVersion(dir); v != 2 || !ok || err != nil {
		t.Fatalf("no marker: %d %v %v", v, ok, err)
	}
	WriteDataVersion(dir, 2)
	if err := CheckDataVersion(dir); err != nil {
		t.Fatalf("agreeing marker: %v", err)
	}
}

// Up refuses before it touches anything: no .env synced, no folder created.
func TestUpRefusesMismatchedDataBeforeWriting(t *testing.T) {
	cfg := testConfig(t)
	dir := filepath.Join(cfg.AppsDir(), "odoo")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(composeV3), 0o644)
	WriteDataVersion(dir, 2)

	var reported []string
	cfg.Report = func(r incident.Report) { reported = append(reported, r.ID) }
	err := Up(context.Background(), cfg, "odoo", dir, []string{filepath.Join(dir, "docker-compose.yml")})
	var dv *DataVersionError
	if !errors.As(err, &dv) {
		t.Fatalf("err = %v, want a DataVersionError", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".env")); !os.IsNotExist(err) {
		t.Error("a refused start still wrote the app's .env")
	}
	if len(reported) != 1 || reported[0] != "app.data:odoo" {
		t.Errorf("incidents = %v", reported)
	}
}
