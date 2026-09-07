package server

import (
	"path/filepath"
	"testing"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/backup"
	"github.com/yundera/maison/internal/backup/backuptest"
	"github.com/yundera/maison/internal/backupconfig"
)

func newTriggerSet(t *testing.T) (*backup.Set, *backupconfig.Store) {
	t.Helper()
	set := backup.New(
		backuptest.NewLocalLike(apps.EngineLocal),
		backuptest.NewRemote("kopia"),
	)
	return set, backupconfig.New(filepath.Join(t.TempDir(), "backup.json"))
}

func writers(set *backup.Set, tr apps.Trigger) []string {
	var out []string
	for _, p := range set.Writers(tr) {
		out = append(out, p.ID())
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A box that has never opened the settings page carries no ticks at all, and must keep
// writing exactly where it was provisioned to.
//
// This is the whole migration. Read the other way — no ticks meaning "no destination
// for anything" — every existing box would silently stop backing up on upgrade, which
// is the one outcome this change must not produce.
func TestNoTicksAnywhereKeepsWritingWhereTheBoxAlreadyDid(t *testing.T) {
	set, store := newTriggerSet(t)
	if err := store.Set(backupconfig.Config{Engine: "kopia"}); err != nil {
		t.Fatal(err)
	}
	applyEngineSettings(set, store)

	for _, tr := range apps.Triggers {
		if got := writers(set, tr); !eq(got, []string{"kopia"}) {
			t.Errorf("%s writers = %v, want the engine the box was already using", tr, got)
		}
	}
}

// An unknown chosen engine falls back to the local one rather than leaving the box
// with nowhere to write.
func TestAnUnknownChosenEngineFallsBackToLocal(t *testing.T) {
	set, store := newTriggerSet(t)
	if err := store.Set(backupconfig.Config{Engine: "rclone"}); err != nil {
		t.Fatal(err)
	}
	applyEngineSettings(set, store)

	if got := writers(set, apps.TriggerSchedule); !eq(got, []string{apps.EngineLocal}) {
		t.Errorf("writers = %v, want the local engine", got)
	}
}

// The first tick anyone sets switches the box off the legacy single writer — including
// for the triggers they did NOT tick, which is what makes "archive uninstalls here but
// do not run the schedule here" expressible at all.
func TestOneTickSwitchesTheBoxToThePerEngineModel(t *testing.T) {
	set, store := newTriggerSet(t)
	yes, no := true, false
	if err := store.Set(backupconfig.Config{
		Engine: "kopia",
		Engines: map[string]backupconfig.EngineSettings{
			apps.EngineLocal: {Uninstall: &yes, Schedule: &no},
		},
	}); err != nil {
		t.Fatal(err)
	}
	applyEngineSettings(set, store)

	if got := writers(set, apps.TriggerUninstall); !eq(got, []string{apps.EngineLocal}) {
		t.Errorf("uninstall writers = %v, want the engine that was ticked for it", got)
	}
	// Nothing was ticked for the schedule, and that is now a stated answer rather than
	// a gap the legacy writer fills.
	if got := writers(set, apps.TriggerSchedule); len(got) != 0 {
		t.Errorf("schedule writers = %v, want none — no engine was ticked for it", got)
	}
}

// Both engines ticked is the point of the feature: one backup, two destinations.
func TestBothEnginesCanReceiveTheSameTrigger(t *testing.T) {
	set, store := newTriggerSet(t)
	yes := true
	if err := store.Set(backupconfig.Config{
		Engines: map[string]backupconfig.EngineSettings{
			apps.EngineLocal: {Schedule: &yes},
			"kopia":          {Schedule: &yes},
		},
	}); err != nil {
		t.Fatal(err)
	}
	applyEngineSettings(set, store)

	// Registration order, which puts the local engine first — load bearing for a
	// fan-out backup, whose cheap local pass should not sit behind an upload inside the
	// downtime window.
	if got := writers(set, apps.TriggerSchedule); !eq(got, []string{apps.EngineLocal, "kopia"}) {
		t.Errorf("schedule writers = %v, want both in registration order", got)
	}
}
