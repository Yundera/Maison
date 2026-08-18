// Package backupconfig persists the deployment's backup settings.
//
// It is deliberately NOT part of internal/usersettings, which is wrong for this in
// three separate ways: its merge silently drops any field not named in it and is
// "non-zero wins", so a plain bool can never express false; its writer replaces the
// file in place, outside its own mutex, so a crash mid-write truncates it and the
// next boot silently reverts to defaults; and the dashboard auto-saves that whole
// blob on a keystroke debounce, so typing in an unrelated settings field would
// round-trip the backup configuration through the lossy merge.
//
// This store replaces the whole document, holds the lock across the write, and
// writes through a temporary — the same discipline the backup code itself uses.
package backupconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/yundera/maison/internal/notify"
)

// Config is everything the operator can decide about backups.
type Config struct {
	// Enabled turns the schedule on. Backups can always be taken by hand.
	Enabled bool `json:"enabled"`

	// Engine is the user's chosen engine ID, or "" to follow whatever the deployment
	// provisioned. Storing only the override — never the provisioned value — is what
	// lets a box keep tracking its provisioning, and makes "reset to default" a
	// matter of clearing the field. The nightly host-side script re-renders the
	// provisioned side; if the two shared a field it would overwrite the user's
	// choice every morning.
	Engine string `json:"engine,omitempty"`

	// Hour and Minute are local wall-clock time.
	Hour   int `json:"hour"`
	Minute int `json:"minute"`

	// UserData includes everything under the data root that is not an app.
	UserData bool `json:"user_data"`

	// Mode is the box-wide retention intent, and Keep / Count / MaxAgeDays are the
	// parameters the chosen mode reads. Empty means the box has no opinion and
	// follows whatever the deployment provisioned — the same "store only the
	// override" discipline as Engine above, for the same reason.
	//
	// Clearing the mode clears its parameter with it. An inherited layer that still
	// carried tiers would be indistinguishable from a user who had typed them, which
	// is exactly the ambiguity Mode exists to remove.
	Mode Mode `json:"mode,omitempty"`

	// Keep is the tiered retention the engine is asked to apply.
	Keep Keep `json:"keep"`

	// Count is read under ModeCount, MaxAgeDays under ModeAge.
	Count      int `json:"count,omitempty"`
	MaxAgeDays int `json:"max_age_days,omitempty"`

	// Engines holds per-engine overrides, keyed by the engine's permanent ID.
	//
	// Retention is per-engine because engines differ in what expiry their storage can
	// even survive (see apps.RetentionModel), and because an engine dropped from the
	// picker keeps expiring the backups it already wrote. With one shared set of
	// numbers, switching kopia → rclone → kopia would quietly rewrite the first
	// engine's intent while its snapshots were still ageing out under it.
	//
	// Entries for engines this build does not know are preserved untouched: engine IDs
	// are permanent, and dropping the settings of an engine the user might switch back
	// to is a silent loss of a choice they made.
	Engines map[string]EngineSettings `json:"engines,omitempty"`

	// SMTP is where failure alerts go. Empty means no alerting — which is a real
	// choice a user can make, and the reason this is not validated into existence.
	SMTP notify.SMTP `json:"smtp,omitempty"`

	// KeepLocal is how many on-disk archives of an app to retain. It is separate
	// from Keep because local archives cost real disk while remote ones cost a
	// quota, and because it is Maison that enforces it rather than the engine.
	KeepLocal int `json:"keep_local"`
}

// Keep is grandfather-father-son retention.
type Keep struct {
	Latest  int `json:"latest"`
	Daily   int `json:"daily"`
	Weekly  int `json:"weekly"`
	Monthly int `json:"monthly"`
	Annual  int `json:"annual"`
}

// Defaults are what a box runs with before anyone touches the settings.
//
// The schedule is 03:30 rather than 03:00 because the app-store refresh already
// runs at 03:00; a backup that walks the whole data root while the store is
// unpacking into it is a collision worth thirty minutes to avoid.
func Defaults() Config {
	return Config{
		Enabled:   false,
		Hour:      3,
		Minute:    30,
		UserData:  true,
		Keep:      Keep{Latest: 2, Daily: 7, Weekly: 4, Monthly: 12},
		KeepLocal: 2,
	}
}

// Store is the persisted configuration.
type Store struct {
	path string
	mu   sync.RWMutex
	cur  Config
}

// New reads the file if it exists. A malformed file falls back to defaults rather
// than failing the boot: backups not being configured is recoverable, a dashboard
// that will not start is not.
func New(path string) *Store {
	s := &Store{path: path, cur: Defaults()}
	if b, err := os.ReadFile(path); err == nil {
		var loaded Config
		if json.Unmarshal(b, &loaded) == nil {
			// Normalised on read as well as on write: a file that predates modes has to
			// resolve correctly on a box where nobody ever opens the settings page.
			s.cur = sane(migrate(loaded))
		}
	}
	return s
}

// Get returns the current configuration.
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur
}

// Set replaces the configuration wholesale — there is no merge, which is precisely
// what makes a plain bool safe here and what stops a field added later from being
// silently dropped by a marshaller that has not been taught about it.
//
// The write holds the lock and goes through a temporary, so two concurrent callers
// cannot land their file writes in the opposite order to their in-memory updates,
// and an interrupted write cannot leave a truncated file behind.
func (s *Store) Set(c Config) error {
	c = sane(c)
	s.mu.Lock()
	defer s.mu.Unlock()

	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".partial"
	// 0600: this file names the engine and may grow credentials-adjacent fields; the
	// rest of Maison's state is 0644 and this deliberately is not.
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	s.cur = c
	return nil
}

// migrate reads a settings file written before retention had modes.
//
// Such a file carries tiers and no mode, and the two cases are not the same thing.
// Tiers equal to the preset are what Defaults() writes on a box nobody has ever
// configured, so they are read as "never decided" — the box goes on following
// whatever the deployment provisions, which is what an untouched box should do.
// Anything else is a number a user actually typed, and is preserved as a custom
// override. Inferring the other way round would pin every box in the fleet to
// today's default the moment it first wrote its settings file.
//
// It runs on load rather than inside sane, because sane runs on every write and this
// is not idempotent: once tiers have been read as a mode, re-reading them would keep
// re-deciding a question the mode has already answered.
func migrate(c Config) Config {
	if c.Mode == ModeInherit && c.Keep != (Keep{}) && c.Keep != SmartKeep() {
		c.Mode = ModeCustom
	}
	return c
}

// sane clamps values that would otherwise produce a schedule that never fires or a
// retention policy that keeps nothing.
func sane(c Config) Config {
	if c.Hour < 0 || c.Hour > 23 {
		c.Hour = Defaults().Hour
	}
	if c.Minute < 0 || c.Minute > 59 {
		c.Minute = Defaults().Minute
	}
	if c.Keep.Latest < 1 {
		c.Keep.Latest = 1
	}
	if c.KeepLocal < 0 {
		c.KeepLocal = 0
	}
	c.Count = max(c.Count, 0)
	c.MaxAgeDays = max(c.MaxAgeDays, 0)
	if !c.Mode.Valid() {
		c.Mode = ModeInherit
	}
	for id, es := range c.Engines {
		if !es.Mode.Valid() {
			es.Mode = ModeInherit
		}
		es.Keep.Latest = max(es.Keep.Latest, 0)
		es.Count = max(es.Count, 0)
		es.MaxAgeDays = max(es.MaxAgeDays, 0)
		es.UploadLimitMB = max(es.UploadLimitMB, 0)
		if es.KeepLocal != nil && *es.KeepLocal < 0 {
			zero := 0
			es.KeepLocal = &zero
		}
		c.Engines[id] = es
	}
	return c
}
