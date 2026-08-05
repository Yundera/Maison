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

	// Keep is the tiered retention the engine is asked to apply.
	Keep Keep `json:"keep"`

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
			s.cur = loaded
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
	return c
}
