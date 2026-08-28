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
