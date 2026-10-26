// Package usersettings persists the operator's dashboard preferences
// (wallpaper, language, widget visibility) to a JSON file under the data root.
package usersettings

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/yundera/maison/internal/notify"
	"github.com/yundera/maison/internal/requirements"
	"github.com/yundera/maison/internal/routing"
	"github.com/yundera/maison/internal/secrets"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// Settings is the persisted preference set.
type Settings struct {
	Wallpaper    string          `json:"wallpaper"`
	Language     string          `json:"language"`
	Widgets      map[string]bool `json:"widgets"`
	StoreSources []string        `json:"store_sources,omitempty"`

	// StoreNames is the operator's own name for a store, by canonical store URL.
	// Only renamed stores appear; a store absent from the map is displayed under
	// the name it gives itself in its store.json, which is the default.
	//
	// A map beside the URL list rather than a richer StoreSources entry, so the
	// on-disk shape of store_sources — a plain array of URLs — stays what every
	// box already has and what an operator editing settings.json by hand expects.
	StoreNames map[string]string `json:"store_names,omitempty"`

	// Routing is how the box reaches its apps: the renderer and its parameters
	// (internal/routing). Nil means the deployment's default (routing.Effective) —
	// labels on its own domain when it has one, published ports when it has none.
	//
	// A POINTER, for the same reason SMTP is: merge reads a zero value as "not
	// supplied". It travels only through its own endpoint and the admin socket; the
	// dashboard's generic settings PUT never carries it (see the server).
	Routing *routing.Setting `json:"routing,omitempty"`

	// SeenHost is the host the dashboard was first opened at, recorded only while a
	// ports-mode box has no url-host of its own: it is the best answer Maison — in a
	// container, unable to see the host's addresses — has to "what address do the
	// apps have?". It fills APP_URL and nothing else.
	SeenHost string `json:"seen_host,omitempty"`

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

	// OpenInNewTab is how a tile click opens an app: a new tab (the default, and
	// what Maison has always done) or the current one.
	//
	// The current tab is what makes the browser's Back button work — an app opened
	// in place returns to the dashboard the way every other page does, instead of
	// leaving a tab per app behind. The launch gate already navigates with
	// location.replace, so Back skips it and lands on the dashboard either way.
	//
	// A POINTER, for the same reason MetricsHistory is: merge reads a zero value as
	// "not supplied", so a plain bool could be turned off and never on again.
	OpenInNewTab *bool `json:"open_in_new_tab,omitempty"`

	// MetricsHistory switches the resource-history sampler on and off. It is the
	// one thing Maison measures when nobody is looking at the dashboard, so it is
	// the one thing worth being able to turn off.
	//
	// A POINTER, not a bool, and that is load-bearing: merge treats a zero value as
	// "not supplied", so a plain bool could be turned on and then never off again —
	// `false` would be indistinguishable from an absent field. See merge.
	MetricsHistory *bool `json:"metrics_history,omitempty"`

	// Capabilities overrides what Maison detects about the box, by capability name
	// (public-ip, gpu-nvidia, gpu-dri) → auto | yes | no. Absent means auto. The
	// store hides an app whose `requires` the box effectively lacks, so this is how
	// an operator who forwarded ports says "yes, inbound connections reach me" — and
	// a deployment that knows (Yundera) can manage it. See internal/requirements.
	Capabilities map[string]string `json:"capabilities,omitempty"`

	// Feedback and Onboarding are the deployment's: set only through Manage (see
	// managed.go), never by a user edit. Nil is the normal state of a standalone box.
	Feedback   *Feedback   `json:"feedback,omitempty"`
	Onboarding *Onboarding `json:"onboarding,omitempty"`

	// Managed names the settings the deployment owns, and who that is, by setting
	// name. It is the API's view of ownership: on disk the flag sits inside each
	// wrapped field instead (managed.go), and an incoming edit never sets it.
	Managed map[string]string `json:"managed,omitempty"`
}

// Defaults returns the initial settings.
func Defaults() Settings {
	return Settings{
		Wallpaper: "/wallpapers/default_wallpaper.jpg",
		Language:  "en_us",
		Widgets:   map[string]bool{"clock": true, "system": true, "storage": true},
		// On: the cost is a sparse file that reaches 1.65 MiB after thirty days and
		// one cheap reading a minute, and the alternative is a graph that is empty
		// the first time anyone looks for it.
		MetricsHistory: boolPtr(true),
		// New tab: the behaviour every existing box already has, so upgrading does
		// not silently change what a click does.
		OpenInNewTab: boolPtr(true),
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
	loaded, migrated, err := decodeFile(b)
	if err != nil {
		log.Printf("usersettings: %s unparseable: %v (running on defaults)", path, err)
		return s
	}
	owners := loaded.Managed
	s.cur = merge(Defaults(), loaded)
	s.cur.Managed = owners
	if migrated {
		// The old domains list became the routing setting: write it back once, so
		// the file says what the box runs on.
		if err := s.write(s.cur); err != nil {
			log.Printf("usersettings: could not write the migrated routing setting: %v", err)
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

// Routing returns the stored routing setting, nil when there is none. The server
// turns it into the effective one for config.Config, read live, so an app coming
// up after a settings change is routed on the setting as it stands now.
func (s *Store) Routing() *routing.Setting { return s.Get().Routing }

// ErrManaged is a user edit of a setting the deployment owns.
var ErrManaged = errors.New("managed by the deployment")

// SetRouting replaces the routing setting — nil goes back to the deployment's
// default. It is the routing editor's write: unlike Set, it can clear the field,
// and it refuses rather than drops an edit of a managed setting, because the editor
// is the only thing sending it.
func (s *Store) SetRouting(r *routing.Setting) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, managed := s.cur.Managed["routing"]; managed {
		return ErrManaged
	}
	next := s.cur
	next.Routing = r
	if err := s.write(next); err != nil {
		return err
	}
	s.cur = next
	return nil
}

// SetCapabilities replaces the capability overrides — nil goes back to auto for
// every capability. Like SetRouting it refuses an edit of a managed setting.
func (s *Store) SetCapabilities(c map[string]string) error {
	if err := ValidateCapabilities(c); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, managed := s.cur.Managed["capabilities"]; managed {
		return ErrManaged
	}
	next := s.cur
	next.Capabilities = c
	if err := s.write(next); err != nil {
		return err
	}
	s.cur = next
	return nil
}

// ValidateCapabilities checks every key is a known capability and every value an
// override requirements understands.
func ValidateCapabilities(c map[string]string) error {
	for k, v := range c {
		if !xcomposeapp.KnownCapability(k) {
			return fmt.Errorf("capabilities: unknown capability %q (known: %s)", k, strings.Join(xcomposeapp.Capabilities, ", "))
		}
		if !requirements.ValidOverride(v) {
			return fmt.Errorf("capabilities: %s: %q is not auto, yes or no", k, v)
		}
	}
	return nil
}

// SeeHost records host as the address the dashboard is opened at — the first one
// only. A dashboard reached under two names (a LAN address and a domain) would
// otherwise flip the apps' address back and forth, and rewrite this file on every
// request that did. Setting url-host is how the operator picks another. Cheap to
// call on every request: it writes once.
func (s *Store) SeeHost(host string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur.SeenHost != "" {
		return
	}
	next := s.cur
	next.SeenHost = host
	if err := s.write(next); err != nil {
		log.Printf("usersettings: record dashboard host: %v", err)
		return
	}
	s.cur = next
}

// Set persists new settings.
//
// The incoming set is merged onto the *current* settings, not onto the defaults:
// callers send the fields they are editing, and a field they leave out has to keep
// the value it has. Merging onto Defaults() instead silently reset every omitted
// field — the dashboard's own PUT /api/settings carries only wallpaper, language
// and widgets, so adding a store source and then changing the wallpaper
// dropped the store source. Clearing a field still works, by sending it as an
// explicit empty value ([] rather than absent).
//
// A field the deployment manages is dropped from n before the merge (see
// dropManaged): only Manage and Release change a managed setting.
//
// The write happens under the lock, so two edits land on disk in the order they
// were applied in memory.
func (s *Store) Set(n Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dropManaged(&n, s.cur.Managed)
	next := merge(s.cur, n)
	next.Managed = s.cur.Managed
	if err := s.write(next); err != nil {
		return err
	}
	s.cur = next
	return nil
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

// write renders one settings document to disk: atomically (temp file + rename, so
// a crash mid-write never leaves half a document) and 0600, since it can hold an
// SMTP login. Secrets themselves live in secrets.env; this file holds references
// to them, but a box upgraded from before that may still carry a literal password
// until the server moves it across.
func (s *Store) write(v Settings) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := encodeFile(v)
	if err != nil {
		return err
	}
	return secrets.WriteFileAtomic(s.path, append(b, '\n'), 0o600)
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
	if in.StoreNames != nil {
		base.StoreNames = in.StoreNames
	}
	if in.Routing != nil {
		base.Routing = in.Routing
	}
	if in.SeenHost != "" {
		base.SeenHost = in.SeenHost
	}
	if in.MetricsHistory != nil {
		base.MetricsHistory = in.MetricsHistory
	}
	if in.OpenInNewTab != nil {
		base.OpenInNewTab = in.OpenInNewTab
	}
	if in.SMTP != nil {
		base.SMTP = in.SMTP
	}
	if in.Capabilities != nil {
		base.Capabilities = in.Capabilities
	}
	if in.Feedback != nil {
		base.Feedback = in.Feedback
	}
	if in.Onboarding != nil {
		base.Onboarding = in.Onboarding
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
//
// A MANAGED transport is not layered at all: the deployment's value is the whole
// answer, and the env-provided one (prov) contributes only the From/To it would
// fill in for an address the managed value left empty. The layering below is the
// transition path for a deployment that still provides its relay through SMTP_*.
func (s Settings) EffectiveSMTP(prov notify.SMTP) notify.SMTP {
	if _, managed := s.Managed["smtp"]; managed && s.SMTP != nil {
		out := *s.SMTP
		if out.From == "" {
			out.From = prov.From
		}
		if out.To == "" {
			out.To = prov.To
		}
		return out
	}
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
