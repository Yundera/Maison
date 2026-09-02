package usersettings

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yundera/maison/internal/domains"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	return New(filepath.Join(t.TempDir(), "settings.json"))
}

// A caller sends the fields it is editing, so a field it leaves out has to survive.
// Merging onto the defaults instead of onto the current settings reset every omitted
// field: the dashboard's PUT /api/settings carries only wallpaper, language, widgets
// and domains, so adding a store source and then changing the wallpaper dropped the
// store source off the box.
func TestSetKeepsFieldsTheCallerDidNotSend(t *testing.T) {
	s := newStore(t)

	// The store panel's read-modify-write, as internal/server/store.go does it.
	cur := s.Get()
	cur.StoreSources = []string{"https://example.test/store.zip"}
	if err := s.Set(cur); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// The dashboard's own settings PUT, which knows nothing about store sources.
	if err := s.Set(Settings{Wallpaper: "/wallpapers/blue.jpg"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got := s.Get()
	if len(got.StoreSources) != 1 {
		t.Fatalf("store sources = %v, want the one that was added", got.StoreSources)
	}
	if got.Wallpaper != "/wallpapers/blue.jpg" {
		t.Errorf("wallpaper = %q, want the one just set", got.Wallpaper)
	}
	// Fields nobody has ever set still read as their defaults.
	if got.Language != Defaults().Language {
		t.Errorf("language = %q, want the default", got.Language)
	}
}

// The other half: a field must still be clearable. Absent means "leave it alone",
// but an explicit empty list means "remove everything" — that is how the domains
// editor removes the last domain.
func TestSetClearsOnAnExplicitEmptyList(t *testing.T) {
	s := newStore(t)

	cur := s.Get()
	cur.Domains = []domains.Domain{{Name: "sslip", Domain: "${APP_PUBLIC_IP_DASH}.sslip.io"}}
	if err := s.Set(cur); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if len(s.Get().Domains) != 1 {
		t.Fatalf("domains = %v, want the one that was added", s.Get().Domains)
	}

	cur = s.Get()
	cur.Domains = []domains.Domain{}
	if err := s.Set(cur); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := s.Get().Domains; len(got) != 0 {
		t.Errorf("domains = %v, want none", got)
	}
}

// Settings persist across a restart, and a file written before a field existed
// still loads — the load path merges onto the defaults, which is what supplies the
// missing field.
func TestNewLoadsWhatSetWrote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")

	s := New(path)
	cur := s.Get()
	cur.StoreSources = []string{"https://example.test/store.zip"}
	cur.Wallpaper = "/wallpapers/blue.jpg"
	if err := s.Set(cur); err != nil {
		t.Fatalf("Set: %v", err)
	}

	reloaded := New(path).Get()
	if len(reloaded.StoreSources) != 1 || reloaded.Wallpaper != "/wallpapers/blue.jpg" {
		t.Fatalf("reloaded = %+v, want what was persisted", reloaded)
	}
	if reloaded.Widgets == nil {
		t.Error("widgets = nil, want the defaults to fill in a field the file never set")
	}
}

// The history toggle is a *bool precisely so it can be turned back off. A plain
// bool would hit merge's "a zero value means not supplied" rule and the off
// switch would silently do nothing — which is the bug this test exists to catch
// if anyone ever simplifies the field.
func TestHistoryToggleCanBeTurnedOffAgain(t *testing.T) {
	s := newStore(t)
	if !s.HistoryEnabled() {
		t.Fatal("history should default to enabled")
	}

	off := false
	if err := s.Set(Settings{MetricsHistory: &off}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if s.HistoryEnabled() {
		t.Fatal("history still enabled after being switched off")
	}

	on := true
	if err := s.Set(Settings{MetricsHistory: &on}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !s.HistoryEnabled() {
		t.Fatal("history still disabled after being switched back on")
	}
}

// An edit that does not mention the toggle must not reset it — the dashboard's own
// PUT /api/settings carries wallpaper and widgets only.
func TestUnrelatedEditKeepsTheHistoryToggle(t *testing.T) {
	s := newStore(t)
	off := false
	if err := s.Set(Settings{MetricsHistory: &off}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set(Settings{Wallpaper: "/wallpapers/other.jpg"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if s.HistoryEnabled() {
		t.Fatal("changing the wallpaper re-enabled history")
	}
}

// A settings file written before the field existed must keep history on rather
// than reading its absence as "off".
func TestSettingsFilePredatingTheFieldDefaultsToEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"wallpaper":"/w.jpg","language":"fr_fr"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !New(path).HistoryEnabled() {
		t.Error("history disabled by an upgrade from a file that predates the setting")
	}
}

// A box nobody has ever configured must still say what it is running on. Before
// seeding, an untouched box carried no settings.json at all, so the only way to
// answer "what wallpaper, which widgets?" was to read this package.
func TestNewSeedsTheFileWhenItIsAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	New(path)

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no file after New: %v", err)
	}
	// Read back through a second store rather than by unmarshalling here: what
	// matters is that the seeded document loads as the defaults, not that it holds
	// any particular bytes.
	if got := New(path).Get(); got.Wallpaper != Defaults().Wallpaper || got.Language != Defaults().Language {
		t.Errorf("seeded file loads as %+v, want the defaults", got)
	}
}

// The seed must never reach a file that already exists. An unparseable one is the
// only copy of whatever the user configured, and overwriting it with defaults would
// destroy the evidence needed to recover it.
func TestNewLeavesAMalformedFileAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	const broken = `{"wallpaper": "/wallpapers/mine.jpg"` // truncated on purpose
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := New(path).Get(); got.Wallpaper != Defaults().Wallpaper {
		t.Errorf("wallpaper %q, want the in-memory default", got.Wallpaper)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != broken {
		t.Errorf("file rewritten to %q, want it untouched", b)
	}
}

// Empty, not rendered: a seeded file must not pin the box to today's widget set, so a
// widget added in a later version still appears on a box nobody has configured.
func TestTheSeedIsAnEmptyDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	New(path)

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != "{}" {
		t.Errorf("seeded %q, want an empty document", b)
	}
	if got := New(path).Get(); !reflect.DeepEqual(got, Defaults()) {
		t.Errorf("empty document loads as %+v, want the defaults %+v", got, Defaults())
	}
}
