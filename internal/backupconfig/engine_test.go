package backupconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func ptr(n int) *int { return &n }

func TestEffectiveFallsAllTheWayToTheCompiledDefault(t *testing.T) {
	got := Defaults().Effective("kopia", Provisioned{})
	if got.Mode != ModeSmart || got.Source != SourceDefault {
		t.Fatalf("mode %q from %q, want smart from default", got.Mode, got.Source)
	}
	if got.Keep != SmartKeep() {
		t.Errorf("keep %+v, want the smart preset %+v", got.Keep, SmartKeep())
	}
	if got.KeepLocal != 2 {
		t.Errorf("keep local %d, want 2", got.KeepLocal)
	}
}

// The layers, most specific last: each one takes over from the one before it.
func TestEffectiveLayerPrecedence(t *testing.T) {
	prov := Provisioned{Settings: EngineSettings{Mode: ModeAge, MaxAgeDays: 90}}

	c := Defaults()
	if got := c.Effective("kopia", prov); got.Source != SourceProvisioned || got.MaxAge != 90*24*time.Hour {
		t.Fatalf("provisioned layer ignored: %+v", got)
	}

	c.Mode, c.Count = ModeCount, 5
	if got := c.Effective("kopia", prov); got.Source != SourceBox || got.Count != 5 {
		t.Fatalf("box setting did not override provisioning: %+v", got)
	}

	c.Engines = map[string]EngineSettings{"kopia": {Mode: ModeCustom, Keep: Keep{Latest: 1, Daily: 3}}}
	got := c.Effective("kopia", prov)
	if got.Source != SourceEngine || got.Keep.Daily != 3 {
		t.Fatalf("engine override did not win: %+v", got)
	}
	// …and only for the engine it names.
	if other := c.Effective("local", prov); other.Source != SourceBox || other.Count != 5 {
		t.Fatalf("engine override leaked to another engine: %+v", other)
	}
}

// "Reset to default" clears the override rather than writing today's default value —
// which is the whole reason the layers exist, so it gets its own test.
func TestClearingAnOverrideResumesFollowingTheDeployment(t *testing.T) {
	prov := Provisioned{Settings: EngineSettings{Mode: ModeCount, Count: 3}}
	c := Defaults()
	c.Engines = map[string]EngineSettings{"kopia": {Mode: ModeAll}}
	if got := c.Effective("kopia", prov); got.Mode != ModeAll {
		t.Fatalf("override not applied: %+v", got)
	}
	c.Engines["kopia"] = EngineSettings{}
	got := c.Effective("kopia", prov)
	if got.Source != SourceProvisioned || got.Count != 3 {
		t.Fatalf("cleared override did not fall through to provisioning: %+v", got)
	}
}

func TestModesResolveToTiersThatCannotDeleteMoreThanTheModeMeans(t *testing.T) {
	c := Defaults()

	c.Mode, c.Count = ModeCount, 4
	if got := c.Effective("kopia", Provisioned{}); got.Keep != (Keep{Latest: 4}) {
		t.Errorf("count mode resolved to %+v, want only Latest=4", got.Keep)
	}

	// An age is not expressible as a tier. Resolving it to zeroed tiers would tell an
	// engine's own policy engine to delete the entire history the age was keeping, so
	// the tiers are wide open and the planner applies the age.
	c.Mode, c.MaxAgeDays = ModeAge, 30
	got := c.Effective("kopia", Provisioned{})
	if got.MaxAge != 30*24*time.Hour {
		t.Errorf("max age %v, want 720h", got.MaxAge)
	}
	if got.Keep.Daily != KeepAll || got.Keep.Latest != KeepAll {
		t.Errorf("age mode resolved to tiers that expire things: %+v", got.Keep)
	}

	c.Mode = ModeAll
	if got := c.Effective("kopia", Provisioned{}); got.Keep.Monthly != KeepAll {
		t.Errorf("keep-all resolved to %+v", got.Keep)
	}

	// Zero counts elsewhere are legitimate ("no weeklies"); zero Latest is not.
	c.Mode, c.Keep = ModeCustom, Keep{Daily: 5}
	if got := c.Effective("kopia", Provisioned{}); got.Keep.Latest != 1 || got.Keep.Weekly != 0 {
		t.Errorf("custom tiers clamped wrongly: %+v", got.Keep)
	}
}

func TestKeepLocalResolvesSeparatelyFromTheMode(t *testing.T) {
	c := Defaults()
	c.Engines = map[string]EngineSettings{"kopia": {KeepLocal: ptr(0)}}
	got := c.Effective("kopia", Provisioned{})
	if got.KeepLocal != 0 {
		t.Errorf("keep local %d, want the explicit 0", got.KeepLocal)
	}
	// Keeping no local copies is a choice about disk, not about retention: the mode
	// still comes from the layer that decided it.
	if got.Mode != ModeSmart {
		t.Errorf("mode %q, want the inherited smart", got.Mode)
	}
	if other := c.Effective("local", Provisioned{}); other.KeepLocal != 2 {
		t.Errorf("keep local leaked across engines: %d", other.KeepLocal)
	}
}

func TestLockedFieldsAreReportedNotEnforced(t *testing.T) {
	got := Defaults().Effective("kopia", Provisioned{
		Settings: EngineSettings{Mode: ModeSmart},
		Locked:   []string{"mode"},
	})
	if !got.Locks("mode") || got.Locks("keep_local") {
		t.Fatalf("locked reported as %v", got.Locked)
	}
}

func TestUploadLimitAndUnmanagedComeFromTheMostSpecificLayerThatSetsThem(t *testing.T) {
	c := Defaults()
	c.Engines = map[string]EngineSettings{"kopia": {Unmanaged: true}}
	prov := Provisioned{Settings: EngineSettings{UploadLimitMB: 20}}
	got := c.Effective("kopia", prov)
	if !got.Unmanaged {
		t.Error("unmanaged flag lost")
	}
	if got.UploadLimitMB != 20 {
		t.Errorf("upload limit %d, want the provisioned 20", got.UploadLimitMB)
	}
}

// A settings file written before modes existed has tiers and no mode. Tiers equal to
// the preset are what an untouched box already holds, so they must not pin it.
func TestLegacyFileMigration(t *testing.T) {
	for name, tc := range map[string]struct {
		in       Config
		wantMode Mode
	}{
		"untouched box tracks its provisioning": {Config{Keep: SmartKeep(), KeepLocal: 2}, ModeInherit},
		"typed tiers are preserved as custom":   {Config{Keep: Keep{Latest: 2, Daily: 30}, KeepLocal: 2}, ModeCustom},
		"no tiers at all":                       {Config{KeepLocal: 2}, ModeInherit},
	} {
		if got := migrate(tc.in).Mode; got != tc.wantMode {
			t.Errorf("%s: mode %q, want %q", name, got, tc.wantMode)
		}
	}

	// A mode from a newer build falls back to the layer below rather than failing.
	if got := sane(migrate(Config{Mode: Mode("quarterly"), Keep: SmartKeep()})).Mode; got != ModeInherit {
		t.Errorf("unknown mode resolved to %q, want inherit", got)
	}
}

func TestStoreRoundTripKeepsUnknownEngines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.json")
	s := New(path)
	c := s.Get()
	c.Mode = ModeSmart
	c.Engines = map[string]EngineSettings{
		"rclone": {Mode: ModeAge, MaxAgeDays: 45, KeepLocal: ptr(0)},
	}
	if err := s.Set(c); err != nil {
		t.Fatal(err)
	}

	// Engine IDs are permanent, so a build that has never heard of rclone must still
	// carry its settings through: the user can switch back.
	back := New(path).Get()
	got, ok := back.Engines["rclone"]
	if !ok || got.Mode != ModeAge || got.MaxAgeDays != 45 || got.KeepLocal == nil || *got.KeepLocal != 0 {
		t.Fatalf("per-engine settings did not survive the round trip: %+v", back.Engines)
	}

	// keep_local must serialise as an explicit 0 rather than vanishing, or "keep none
	// locally" would read back as "not set".
	raw := map[string]any{}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	engines, _ := raw["engines"].(map[string]any)
	rclone, _ := engines["rclone"].(map[string]any)
	if _, ok := rclone["keep_local"]; !ok {
		t.Errorf("keep_local was omitted from the file: %s", b)
	}
}

func TestSaneClampsPerEngineValues(t *testing.T) {
	got := sane(Config{
		Keep:    SmartKeep(),
		Engines: map[string]EngineSettings{"kopia": {Mode: Mode("nope"), Count: -3, MaxAgeDays: -1, KeepLocal: ptr(-5)}},
	}).Engines["kopia"]
	if got.Mode != ModeInherit || got.Count != 0 || got.MaxAgeDays != 0 || *got.KeepLocal != 0 {
		t.Fatalf("negative per-engine values survived: %+v", got)
	}
}

// sane runs on every write, so applying it twice has to change nothing. The
// migration deliberately lives outside it for exactly this reason: reading tiers as a
// mode is a decision, and a decision re-made on every save is a setting that drifts.
func TestSaneIsIdempotent(t *testing.T) {
	for name, c := range map[string]Config{
		"defaults":   Defaults(),
		"empty":      {},
		"legacy":     migrate(Config{Keep: Keep{Latest: 2, Daily: 30}, KeepLocal: 1}),
		"per engine": {Mode: ModeAll, Engines: map[string]EngineSettings{"kopia": {Mode: ModeCount, Count: 3, KeepLocal: ptr(0)}}},
	} {
		once := sane(c)
		twice := sane(once)
		if !reflect.DeepEqual(once, twice) {
			t.Errorf("%s: sane is not idempotent:\n once: %+v\ntwice: %+v", name, once, twice)
		}
	}
}
