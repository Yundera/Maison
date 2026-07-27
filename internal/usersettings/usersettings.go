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
func (s *Store) Set(n Settings) error {
	s.mu.Lock()
	s.cur = merge(Defaults(), n)
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

func merge(def, in Settings) Settings {
	if in.Wallpaper != "" {
		def.Wallpaper = in.Wallpaper
	}
	if in.Language != "" {
		def.Language = in.Language
	}
	if in.Widgets != nil {
		def.Widgets = in.Widgets
	}
	if in.StoreSources != nil {
		def.StoreSources = in.StoreSources
	}
	if in.Domains != nil {
		def.Domains = in.Domains
	}
	return def
}
