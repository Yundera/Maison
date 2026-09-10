package installer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yundera/maison/internal/incident"
)

// What a failed update leaves behind, and the order an update does things in. Both
// come out of the FileBrowser update on watch.nsl.sh (2026-09-10): its init steps ran
// while the old container still held the database, the update was rolled back, and the
// rollback put back a version that could not start — with nothing in the incident
// register and nothing in the log.

func recordReports(in *Installer) *[]incident.Report {
	got := &[]incident.Report{}
	in.Report = func(r incident.Report) { *got = append(*got, r) }
	return got
}

var initFailed = errors.New(`init "add-admin": exit status 1`)

// Rolled back and running is still a failed update: the app is on the old version and
// the store keeps offering the one that just failed.
func TestARolledBackUpdateIsStillAnIncident(t *testing.T) {
	in := &Installer{
		RollBack:      func(context.Context, string, string) error { return nil },
		VerifyRunning: func(context.Context, string) error { return nil },
	}
	got := recordReports(in)

	if _, err := in.rollBack(context.Background(), "filebrowser", UpdateResult{Backup: "2026-09-10_123728"}, initFailed); err == nil {
		t.Fatal("a rolled-back update reported success")
	}
	if len(*got) != 1 {
		t.Fatalf("reports = %+v, want exactly one", *got)
	}
	r := (*got)[0]
	if r.ID != "app.update:filebrowser" || r.Severity != incident.Warning {
		t.Errorf("report = %s/%s, want app.update:filebrowser as a warning", r.ID, r.Severity)
	}
	if !strings.Contains(r.Detail, "add-admin") {
		t.Errorf("detail %q does not carry the failure that caused the rollback", r.Detail)
	}
}

// Put back is not running. The restore returned the old version, the old version
// could not open its own data, and that is the worst outcome short of losing it.
func TestARollBackThatDoesNotComeBackUpIsCritical(t *testing.T) {
	in := &Installer{
		RollBack: func(context.Context, string, string) error { return nil },
		VerifyRunning: func(context.Context, string) error {
			return errors.New("filebrowser keeps restarting (last exit code 1)")
		},
	}
	got := recordReports(in)

	res, err := in.rollBack(context.Background(), "filebrowser", UpdateResult{Backup: "2026-09-10_123728"}, initFailed)
	if err == nil {
		t.Fatal("a rollback that left the app down reported success")
	}
	for _, want := range []string{"not running", "keeps restarting", "add-admin"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	// The files were put back — that part worked — but the UI must not show it as fine.
	if !res.RolledBack || res.Warning == "" {
		t.Errorf("result = %+v, want rolled back with a warning", res)
	}
	if len(*got) != 1 || (*got)[0].Severity != incident.Critical || !strings.Contains((*got)[0].Detail, "keeps restarting") {
		t.Errorf("reports = %+v, want one critical naming why the app is down", *got)
	}
}

func TestAnUpdateThatCannotBeUndoneIsCritical(t *testing.T) {
	in := &Installer{}
	got := recordReports(in)

	if _, err := in.rollBack(context.Background(), "jellyfin", UpdateResult{}, errors.New("compose up failed")); err == nil {
		t.Fatal("expected an error")
	}
	if len(*got) != 1 || (*got)[0].Severity != incident.Critical {
		t.Errorf("reports = %+v, want one critical", *got)
	}
}

// updateFixture is an installed jellyfin pointed at a local store that ships a newer
// compose, with every update hook recording what it was asked to do.
func updateFixture(t *testing.T) (*Installer, string, string, *[]string) {
	t.Helper()
	srv := refStoreServer(t, refStoreZip(t, "jellyfin", "2"))
	installed := "name: jellyfin\nservices:\n  app:\n    image: example/jellyfin:1\n"
	in, dir := refInstaller(t, "jellyfin", installed)
	if _, err := in.SetUpdateRef(context.Background(), "jellyfin", srv.URL+"/store.zip/-/Apps/jellyfin"); err != nil {
		t.Fatalf("SetUpdateRef: %v", err)
	}

	steps := &[]string{}
	in.BackupBeforeUpdate = func(context.Context, string) (string, error) {
		*steps = append(*steps, "backup")
		return "2026-01-01_000000", nil
	}
	in.StopBeforeUpdate = func(context.Context, string) error {
		*steps = append(*steps, "stop")
		return nil
	}
	in.RollBack = func(context.Context, string, string) error {
		*steps = append(*steps, "rollback")
		return nil
	}
	in.VerifyRunning = func(context.Context, string) error { return nil }
	return in, dir, installed, steps
}

// The order is the fix. Stopped after the rollback point, because taking the rollback
// point restarts the app; stopped before the new compose is written and converged,
// because the new version's init steps open the data the old containers hold.
func TestApplyUpdateStopsTheOldVersionBetweenTheRollbackPointAndTheWrite(t *testing.T) {
	in, dir, installed, steps := updateFixture(t)
	stop := in.StopBeforeUpdate
	in.StopBeforeUpdate = func(ctx context.Context, project string) error {
		raw, _ := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
		if string(raw) != installed {
			t.Error("the new compose was written before the old version was stopped")
		}
		return stop(ctx, project)
	}

	// There is no Docker here, so bringing the new version up fails — which is exactly
	// the path that has to put the app back.
	if _, err := in.ApplyUpdate(context.Background(), "jellyfin"); err == nil {
		t.Fatal("an update that could not start reported success")
	}
	if got := strings.Join(*steps, " → "); got != "backup → stop → rollback" {
		t.Errorf("steps = %s, want backup → stop → rollback", got)
	}
}

// A stop that fails has changed nothing on disk, so there is nothing to roll back.
func TestApplyUpdateWritesNothingWhenTheStopFails(t *testing.T) {
	in, dir, installed, steps := updateFixture(t)
	in.StopBeforeUpdate = func(context.Context, string) error {
		*steps = append(*steps, "stop")
		return errors.New("daemon went away")
	}

	_, err := in.ApplyUpdate(context.Background(), "jellyfin")
	if err == nil || !strings.Contains(err.Error(), "daemon went away") {
		t.Fatalf("err = %v, want the stop failure", err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "docker-compose.yml")); string(raw) != installed {
		t.Errorf("compose was rewritten after a failed stop:\n%s", raw)
	}
	if got := strings.Join(*steps, " → "); got != "backup → stop" {
		t.Errorf("steps = %s, want backup → stop and no rollback", got)
	}
}
