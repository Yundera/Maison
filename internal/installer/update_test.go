package installer

import (
	"context"
	"errors"
	"strings"
	"testing"
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
