package xcomposeapp

import "testing"

func TestWebURL(t *testing.T) {
	cases := []struct {
		name   string
		app    App
		domain string
		want   string
	}{
		{"gateway default scheme+path", App{WebUIHost: "jellyfin-${domain}"}, "app.localhost", "https://jellyfin-app.localhost/"},
		{"gateway with path", App{WebUIHost: "jellyfin-${domain}", WebUIPath: "/web/"}, "app.localhost", "https://jellyfin-app.localhost/web/"},
		{"uppercase placeholder (caddy-label style)", App{WebUIHost: "nc-${DOMAIN}"}, "example.com", "https://nc-example.com/"},
		{"direct host+port+scheme", App{WebUIHost: "nas.example.com", WebUIScheme: "http", WebUIPort: "8096"}, "", "http://nas.example.com:8096/"},
		{"literal host no placeholder, empty domain ok", App{WebUIHost: "nas.local"}, "", "https://nas.local/"},
		{"path missing leading slash", App{WebUIHost: "a-${domain}", WebUIPath: "app"}, "d", "https://a-d/app"},
		{"query-string path", App{WebUIHost: "t-${domain}", WebUIPath: "/?hash=x"}, "d", "https://t-d/?hash=x"},
		{"no host -> no url", App{}, "app.localhost", ""},
		{"domain placeholder but no domain -> unreachable", App{WebUIHost: "j-${domain}"}, "", ""},
		{"unknown placeholder -> unreachable", App{WebUIHost: "j-${weird}"}, "app.localhost", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.app.WebURL(c.domain); got != c.want {
				t.Fatalf("WebURL(%q) = %q, want %q", c.domain, got, c.want)
			}
		})
	}
}

func TestParseVersionGate(t *testing.T) {
	if _, err := Parse(map[string]any{"schema_version": SchemaVersion + 1, "webui-host": "x"}); err != ErrUnsupportedVersion {
		t.Fatalf("future schema_version: got err %v, want ErrUnsupportedVersion", err)
	}
	a, err := Parse(map[string]any{"webui-host": "j-${domain}", "title": "Jellyfin"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.Title.Value() != "Jellyfin" {
		t.Fatalf("title = %q", a.Title.Value())
	}
	if _, err := Parse(nil); err != ErrNoExtension {
		t.Fatalf("nil: got %v, want ErrNoExtension", err)
	}
}

func TestParseFolders(t *testing.T) {
	a, err := Parse(map[string]any{
		"folders": []any{
			"/DATA/AppData/${AppID}/config", // bare-path shorthand
			map[string]any{
				"path":      "/DATA/Media",
				"user":      1000, // YAML types this as an int; must survive as text
				"group":     "media",
				"mode":      "0775",
				"recursive": true,
			},
		},
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(a.Folders) != 2 {
		t.Fatalf("got %d folders, want 2", len(a.Folders))
	}
	if got := a.Folders[0]; got.Path != "/DATA/AppData/${AppID}/config" || got.Recursive || got.Mode != "" {
		t.Fatalf("shorthand folder = %+v", got)
	}
	want := Folder{Path: "/DATA/Media", User: "1000", Group: "media", Mode: "0775", Recursive: true}
	if a.Folders[1] != want {
		t.Fatalf("folder = %+v, want %+v", a.Folders[1], want)
	}
}

func TestParseHooks(t *testing.T) {
	a, err := Parse(map[string]any{
		"hooks": map[string]any{
			"pre_install": "echo installing",
			"pre_up":      "echo starting",
			"post_up":     "echo started",
		},
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := Hooks{PreInstall: "echo installing", PreUp: "echo starting", PostUp: "echo started"}
	if a.Hooks != want {
		t.Fatalf("hooks = %+v, want %+v", a.Hooks, want)
	}
}

func TestParseView(t *testing.T) {
	a, err := Parse(map[string]any{"view": "system"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.View != ViewSystem {
		t.Fatalf("view = %q, want %q", a.View, ViewSystem)
	}
}

// An unknown view is a cosmetic mistake, not a reason to refuse the app: it
// falls back to the ordinary grid, which is where the app would have been
// anyway.
func TestNormalizeView(t *testing.T) {
	cases := map[string]string{
		"system":    ViewSystem,
		"  System ": ViewSystem,
		"HIDDEN":    ViewHidden,
		"apps":      ViewApps,
		"":          ViewApps,
		"dashboard": ViewApps,
	}
	for in, want := range cases {
		if got := NormalizeView(in); got != want {
			t.Errorf("NormalizeView(%q) = %q, want %q", in, got, want)
		}
	}
}

// The exclusion list has to survive the map[string]any round trip composefile puts
// the extension block through, and a build that does not understand `backup` must
// still read the rest of the block — which is what not raising SchemaVersion buys.
func TestParseBackupExclude(t *testing.T) {
	a, err := Parse(map[string]any{
		"schema_version": 2,
		"title":          "Jellyfin",
		"backup": map[string]any{
			"exclude": []any{"cache/", "/DATA/AppData/${AppID}/transcodes", "**/thumbs/"},
		},
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"cache/", "/DATA/AppData/${AppID}/transcodes", "**/thumbs/"}
	if len(a.Backup.Exclude) != len(want) {
		t.Fatalf("exclude = %v, want %v", a.Backup.Exclude, want)
	}
	for i := range want {
		if a.Backup.Exclude[i] != want[i] {
			t.Fatalf("exclude[%d] = %q, want %q", i, a.Backup.Exclude[i], want[i])
		}
	}
	if a.Title.Value() != "Jellyfin" {
		t.Fatalf("the rest of the block was lost: title = %q", a.Title.Value())
	}
}

// An app that declares nothing must produce a zero BackupSpec rather than anything a
// caller has to guard — every backup path holds this value unconditionally.
func TestParseWithoutBackupBlock(t *testing.T) {
	a, err := Parse(map[string]any{"title": "Jellyfin"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.Backup.Exclude != nil {
		t.Fatalf("exclude = %v, want nil", a.Backup.Exclude)
	}
}
