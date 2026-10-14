package server

import (
	"path/filepath"
	"testing"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/backup"
	"github.com/yundera/maison/internal/backup/backuptest"
	"github.com/yundera/maison/internal/backupconfig"
)

func newEngineSet(t *testing.T) (*backup.Set, *backupconfig.Store) {
	t.Helper()
	set := backup.New(
		backuptest.NewLocalLike(apps.EngineLocal),
		backuptest.NewRemote("kopia"),
	)
	return set, backupconfig.New(filepath.Join(t.TempDir(), "backup.json"))
}

func writers(set *backup.Set) []string {
	var out []string
	for _, p := range set.Writers() {
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

// A box that has never opened the settings page states nothing, and must keep writing
// exactly where it was provisioned to.
//
// This is the whole migration. Read the other way — nothing stated meaning "no
// destination" — every existing box would silently stop backing up on upgrade, which
// is the one outcome this change must not produce.
func TestNothingStatedKeepsWritingWhereTheBoxAlreadyDid(t *testing.T) {
	set, store := newEngineSet(t)
	if err := store.Set(backupconfig.Config{Engine: "kopia"}); err != nil {
		t.Fatal(err)
	}
	applyEngineSettings(set, store)

	if got := writers(set); !eq(got, []string{"kopia"}) {
		t.Errorf("writers = %v, want the engine the box was already using", got)
	}
}

// An unknown chosen engine falls back to the local one rather than leaving the box
// with nowhere to write.
func TestAnUnknownChosenEngineFallsBackToLocal(t *testing.T) {
	set, store := newEngineSet(t)
	if err := store.Set(backupconfig.Config{Engine: "rclone"}); err != nil {
		t.Fatal(err)
	}
	applyEngineSettings(set, store)

	if got := writers(set); !eq(got, []string{apps.EngineLocal}) {
		t.Errorf("writers = %v, want the local engine", got)
	}
}

// The first engine anyone enables switches the box off the legacy single writer:
// the chosen engine no longer receives anything it was not enabled for.
func TestOneStatedEngineSwitchesTheBoxToThePerEngineModel(t *testing.T) {
	set, store := newEngineSet(t)
	yes := true
	if err := store.Set(backupconfig.Config{
		Engine:  "kopia",
		Engines: map[string]backupconfig.EngineSettings{apps.EngineLocal: {Enabled: &yes}},
	}); err != nil {
		t.Fatal(err)
	}
	applyEngineSettings(set, store)

	if got := writers(set); !eq(got, []string{apps.EngineLocal}) {
		t.Errorf("writers = %v, want only the engine that was enabled", got)
	}
}

// Both engines enabled is the point of the feature: one backup, two destinations.
func TestBothEnginesCanBeEnabled(t *testing.T) {
	set, store := newEngineSet(t)
	yes := true
	if err := store.Set(backupconfig.Config{
		Engines: map[string]backupconfig.EngineSettings{
			apps.EngineLocal: {Enabled: &yes},
			"kopia":          {Enabled: &yes},
		},
	}); err != nil {
		t.Fatal(err)
	}
	applyEngineSettings(set, store)

	// Registration order, which puts the local engine first — load bearing for a
	// fan-out backup, whose cheap local pass should not sit behind an upload inside the
	// downtime window.
	if got := writers(set); !eq(got, []string{apps.EngineLocal, "kopia"}) {
		t.Errorf("writers = %v, want both in registration order", got)
	}
}

// The local engine can be switched off like any other: a box backing up offsite does
// not have to keep a second copy of every app on its own disk.
func TestTheLocalEngineCanBeSwitchedOff(t *testing.T) {
	set, store := newEngineSet(t)
	yes, no := true, false
	if err := store.Set(backupconfig.Config{
		Engines: map[string]backupconfig.EngineSettings{
			apps.EngineLocal: {Enabled: &no},
			"kopia":          {Enabled: &yes},
		},
	}); err != nil {
		t.Fatal(err)
	}
	applyEngineSettings(set, store)

	if got := writers(set); !eq(got, []string{"kopia"}) {
		t.Errorf("writers = %v, want the repository alone", got)
	}
}

// A backup.json written before the merge keeps its meaning: an engine ticked for either
// trigger is enabled, one with both ticks off is not.
func TestLegacyTicksReadAsEnabled(t *testing.T) {
	set, store := newEngineSet(t)
	yes, no := true, false
	if err := store.Set(backupconfig.Config{
		Engines: map[string]backupconfig.EngineSettings{
			apps.EngineLocal: {Uninstall: &yes, Schedule: &no},
			"kopia":          {Uninstall: &no, Schedule: &no},
		},
	}); err != nil {
		t.Fatal(err)
	}
	applyEngineSettings(set, store)

	if got := writers(set); !eq(got, []string{apps.EngineLocal}) {
		t.Errorf("writers = %v, want the engine ticked for a trigger and not the one ticked for none", got)
	}
}

// What the settings page sends is folded into the one switch, and the ticks are not
// stored.
func TestFoldTriggersStoresOnlyEnabled(t *testing.T) {
	yes, no := true, false
	got := backupconfig.FoldTriggers(backupconfig.Config{
		Engines: map[string]backupconfig.EngineSettings{
			apps.EngineLocal: {Schedule: &no, Uninstall: &yes},
			"kopia":          {Enabled: &no, Schedule: &yes},
		},
	})
	local, kopia := got.Engines[apps.EngineLocal], got.Engines["kopia"]
	if local.Enabled == nil || !*local.Enabled || local.Schedule != nil || local.Uninstall != nil {
		t.Errorf("local = %+v, want enabled with no ticks left", local)
	}
	// A stated switch wins over the ticks a stale client sent alongside it.
	if kopia.Enabled == nil || *kopia.Enabled || kopia.Schedule != nil {
		t.Errorf("kopia = %+v, want disabled with no ticks left", kopia)
	}
}

// An engine enabled that the box does not have cannot be a writer, but it must be
// remembered as missing: an uninstall with no writers and nothing missing goes ahead
// without a backup, while one whose destination vanished has to refuse.
func TestAnEnabledEngineThatIsNotInstalledIsMissing(t *testing.T) {
	set, store := newEngineSet(t)
	yes, no := true, false
	if err := store.Set(backupconfig.Config{
		Engines: map[string]backupconfig.EngineSettings{
			apps.EngineLocal: {Enabled: &no},
			"rclone":         {Enabled: &yes},
		},
	}); err != nil {
		t.Fatal(err)
	}
	applyEngineSettings(set, store)

	if got := writers(set); len(got) != 0 {
		t.Errorf("writers = %v, want none", got)
	}
	if got := set.Missing(); !eq(got, []string{"rclone"}) {
		t.Errorf("missing = %v, want the enabled engine the box does not have", got)
	}

	// Switching it off is the user switching backups off: nothing missing any more.
	if err := store.Set(backupconfig.Config{
		Engines: map[string]backupconfig.EngineSettings{
			apps.EngineLocal: {Enabled: &no},
			"rclone":         {Enabled: &no},
		},
	}); err != nil {
		t.Fatal(err)
	}
	applyEngineSettings(set, store)
	if got := set.Missing(); len(got) != 0 {
		t.Errorf("missing = %v after switching it off, want none", got)
	}
}
