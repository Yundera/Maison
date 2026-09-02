// Package usersettings persists the operator's dashboard preferences
// (wallpaper, language, widget visibility) to a JSON file under the data root.
package usersettings

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/yundera/maison/internal/domains"
	"github.com/yundera/maison/internal/notify"
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

	// SMTP is where Maison's outbound mail goes — today, the backup failure and
	// recovery alerts.
	//
	// It lives here rather than in backupconfig, where it started, because it is a
	// property of the box and not of the backup schedule: the next thing Maison needs
	// to mail — a certificate about to expire, a disk filling up — would otherwise
	// have to reach into the backup configuration to find a relay. A move, not a
	// redesign: EffectiveSMTP below is the same resolution it always was, and
	// server.adoptLegacySMTP carries a box that configured it in the old place.
	//
	// A POINTER, for the same reason MetricsHistory is: merge treats a zero value as
	// "not supplied", and an SMTP block that has been deliberately emptied has to be
	// distinguishable from one that was never sent.
	SMTP *notify.SMTP `json:"smtp,omitempty"`

	// MetricsHistory switches the resource-history sampler on and off. It is the
	// one thing Maison measures when nobody is looking at the dashboard, so it is
	// the one thing worth being able to turn off.
	//
	// A POINTER, not a bool, and that is load-bearing: merge treats a zero value as
	// "not supplied", so a plain bool could be turned on and then never off again —
	// `false` would be indistinguishable from an absent field. See merge.
	MetricsHistory *bool `json:"metrics_history,omitempty"`
}

// Defaults returns the initial settings.
func Defaults() Settings {
	return Settings{
		Wallpaper: "/wallpapers/default_wallpaper.jpg",
		Language:  "en_us",
		Widgets:   map[string]bool{"clock": true, "system": true, "storage": true},
		// On: the cost is a sparse file that reaches 1.32 MiB after thirty days and
		// one cheap reading a minute, and the alternative is a graph that is empty
		// the first time anyone looks for it.
		MetricsHistory: boolPtr(true),
	}
}

// Store is a file-backed settings store.
type Store struct {
	path string
	mu   sync.RWMutex
	cur  Settings
}

// New loads settings from path, writing Defaults() there when the file does not
// exist yet.
//
// Seeding is what makes the state directory answer "what is this dashboard running
// on?". Before it, both stores were write-on-first-change, so an untouched box
// carried no settings.json at all and the effective configuration existed only in
// this package's source — which is a poor place for an operator to have to look.
//
// The seeded document is empty — `{}` — which this store has always read as "every
// field defaults", since a load merges the file onto Defaults(). An empty seed is
// what keeps a box tracking the defaults: a rendered one would pin it to the widget
// set, wallpaper and language of the day it first booted, so a widget added in a
// later version would never appear on it.
//
// ONLY A GENUINE ABSENCE IS SEEDED. A file that exists but does not parse is left
// exactly as it is: it is the only copy of whatever the user configured, and
// replacing it would destroy the evidence needed to get it back. The in-memory
// fallback to defaults is unchanged, so such a box still boots.
//
// A seed that fails is logged and otherwise ignored. The settings in memory are the
// same either way, and refusing to start over a file that could not be written would
// trade a cosmetic gap for an unusable dashboard.
func New(path string) *Store {
	s := &Store{path: path, cur: Defaults()}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if err := s.seed(); err != nil {
				log.Printf("usersettings: could not seed %s: %v", path, err)
			}
		} else {
			log.Printf("usersettings: %s unreadable: %v (running on defaults)", path, err)
		}
		return s
	}
	var loaded Settings
	if json.Unmarshal(b, &loaded) == nil {
		s.cur = merge(Defaults(), loaded)
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

	return s.write(cur)
}

// seed writes the empty document that says "this box has no opinion about anything".
//
// O_EXCL rather than a plain write: New reads and then writes, and two Maison
// processes booting against the same state directory must not have the second one
// blank the file the first has already started using.
func (s *Store) seed() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil // someone else got there first, which is the outcome we wanted
		}
		return err
	}
	defer f.Close()
	_, err = f.WriteString("{}\n")
	return err
}

// write renders one settings document to disk.
func (s *Store) write(v Settings) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
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
	if in.MetricsHistory != nil {
		base.MetricsHistory = in.MetricsHistory
	}
	if in.SMTP != nil {
		base.SMTP = in.SMTP
	}
	return base
}

// EffectiveSMTP resolves this box's mail settings over the deployment's.
//
// It is the same "store only the override" rule the retention layering follows (see
// backupconfig.Effective), applied to the one other thing the deployment provisions
// and the user may want to change.
//
// The TRANSPORT TRAVELS TOGETHER — host, port, credentials, security — for the reason
// Mode carries its own parameter there: a host set on this box with credentials
// inherited from the deployment is a login sent to the wrong server. From and To
// resolve on their own, because "mail from somewhere else" and "mail someone else"
// are independent choices, and the recipient is the one a user actually changes.
func (s Settings) EffectiveSMTP(prov notify.SMTP) notify.SMTP {
	out := prov
	if s.SMTP == nil {
		return out
	}
	if s.SMTP.Host != "" {
		out.Host, out.Port = s.SMTP.Host, s.SMTP.Port
		out.User, out.Pass = s.SMTP.User, s.SMTP.Pass
		out.Security = s.SMTP.Security
	}
	if s.SMTP.From != "" {
		out.From = s.SMTP.From
	}
	if s.SMTP.To != "" {
		out.To = s.SMTP.To
	}
	return out
}

// EffectiveSMTP resolves the current settings against the deployment's transport.
func (s *Store) EffectiveSMTP(prov notify.SMTP) notify.SMTP { return s.Get().EffectiveSMTP(prov) }

func boolPtr(b bool) *bool { return &b }

// HistoryEnabled reports whether the resource-history sampler should run. Absent
// means on, so a settings file written before the field existed keeps the default
// rather than silently disabling history on upgrade.
func (s *Store) HistoryEnabled() bool {
	v := s.Get().MetricsHistory
	return v == nil || *v
}
