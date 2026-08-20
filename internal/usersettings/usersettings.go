// Package usersettings persists the operator's dashboard preferences
// (wallpaper, language, widget visibility) to a JSON file under the data root.
package usersettings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/yundera/maison/internal/domains"
)

// Settings is the persisted preference set.
type Settings struct {
	Wallpaper    string          `json:"wallpaper"`
	Language     string          `json:"language"`
	Widgets      map[string]bool `json:"widgets"`
	StoreSources []string        `json:"store_sources,omitempty"`

	// Domains are the additional domains every app is published on. Empty (the
	// default) means apps are reachable only at the deployment's primary domain,
	// exactly as their store compose routes them.
	Domains []domains.Domain `json:"domains,omitempty"`
}

// Defaults returns the initial settings.
func Defaults() Settings {
	return Settings{
		Wallpaper: "/wallpapers/default_wallpaper.jpg",
		Language:  "en_us",
		Widgets:   map[string]bool{"clock": true, "system": true, "storage": true},
	}
}

// Store is a file-backed settings store.
type Store struct {
	path string
	mu   sync.RWMutex
	cur  Settings
}

// New loads settings from path (creating defaults if absent).
func New(path string) *Store {
	s := &Store{path: path, cur: Defaults()}
	if b, err := os.ReadFile(path); err == nil {
		var loaded Settings
		if json.Unmarshal(b, &loaded) == nil {
			s.cur = merge(Defaults(), loaded)
		}
	}
	return s
}

// Get returns the current settings.
func (s *Store) Get() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur
}

// Domains returns the additional domains apps are published on. It is the live
// accessor config.Config carries, so an app coming up after a settings change is
// routed on the list as it stands now.
func (s *Store) Domains() []domains.Domain { return s.Get().Domains }

// Set persists new settings.
//
// The incoming set is merged onto the *current* settings, not onto the defaults:
// callers send the fields they are editing, and a field they leave out has to keep
// the value it has. Merging onto Defaults() instead silently reset every omitted
// field — the dashboard's own PUT /api/settings carries only wallpaper, language,
// widgets and domains, so adding a store source and then changing the wallpaper
// dropped the store source. Clearing a field still works, by sending it as an
// explicit empty value ([] rather than absent), which is how the domains editor
// removes the last domain.
func (s *Store) Set(n Settings) error {
	s.mu.Lock()
	s.cur = merge(s.cur, n)
	cur := s.cur
	s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o644)
}

// merge overlays the fields in carries onto base, leaving the rest of base alone.
// A zero value means "not supplied" rather than "set to zero", so an empty slice
// and an absent one are different: [] clears the list, absent keeps it.
//
// base is Defaults() when loading a file that may predate a field, and the current
// settings when applying an edit (see Set).
func merge(base, in Settings) Settings {
	if in.Wallpaper != "" {
		base.Wallpaper = in.Wallpaper
	}
	if in.Language != "" {
		base.Language = in.Language
	}
	if in.Widgets != nil {
		base.Widgets = in.Widgets
	}
	if in.StoreSources != nil {
		base.StoreSources = in.StoreSources
	}
	if in.Domains != nil {
		base.Domains = in.Domains
	}
	return base
}
