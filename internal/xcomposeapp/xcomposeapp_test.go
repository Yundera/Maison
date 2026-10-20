package xcomposeapp

import (
	"strings"
	"testing"
	"time"
)

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
		{"APP_DOMAIN placeholder (caddy_0 spelling)", App{WebUIHost: "terminal-${APP_DOMAIN}"}, "watch.nsl.sh", "https://terminal-watch.nsl.sh/"},
		{"APP_DOMAIN placeholder but no domain -> unreachable", App{WebUIHost: "terminal-${APP_DOMAIN}"}, "", ""},
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
	a, err := Parse(map[string]any{"webui-host": "j-${domain}", "title": map[string]any{"en_us": "Jellyfin"}})
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

func TestParseSteps(t *testing.T) {
	a, err := Parse(map[string]any{
		"steps": []any{
			map[string]any{"name": "seed", "at": "setup", "script": "echo seeding"},
			map[string]any{"name": "tune", "at": "started", "run": "always", "file": "scripts/tune.sh",
				"images": []any{"alpine:3.20"}, "timeout": "30s"},
		},
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(a.Steps) != 2 {
		t.Fatalf("steps = %+v", a.Steps)
	}
	seed, tune := a.Steps[0], a.Steps[1]
	if seed.Name != "seed" || seed.At != AtSetup || seed.RunMode() != RunOnce || seed.Script.Script != "echo seeding" {
		t.Fatalf("seed = %+v", seed)
	}
	if tune.RunMode() != RunAlways || tune.File != "scripts/tune.sh" || len(tune.Images) != 1 || tune.TimeoutOr(0) != 30*time.Second {
		t.Fatalf("tune = %+v", tune)
	}
	if errs := ValidateSteps(a.Steps); len(errs) != 0 {
		t.Fatalf("valid steps refused: %v", errs)
	}
	if seed.Lenient {
		t.Fatal("Lenient must never come from YAML")
	}
}

func TestValidateStepsRefusesAuthoringErrors(t *testing.T) {
	ok := Script{Script: "true"}
	cases := map[string][]Step{
		"no name":           {{At: AtSetup, Script: ok}},
		"no at":             {{Name: "a", Script: ok}},
		"unknown at":        {{Name: "a", At: "pre_up", Script: ok}},
		"unknown run":       {{Name: "a", At: AtSetup, Run: "twice", Script: ok}},
		"nothing to run":    {{Name: "a", At: AtSetup}},
		"script and file":   {{Name: "a", At: AtSetup, Script: Script{Script: "true", File: "scripts/a.sh"}}},
		"file outside":      {{Name: "a", At: AtSetup, Script: Script{File: "../a.sh"}}},
		"file not scripts/": {{Name: "a", At: AtSetup, Script: Script{File: "seed/a.sh"}}},
		"bad timeout":       {{Name: "a", At: AtSetup, Script: Script{Script: "true", Timeout: "soon"}}},
		"duplicate name":    {{Name: "a", At: AtSetup, Script: ok}, {Name: "a", At: AtStarted, Script: ok}},
	}
	for name, steps := range cases {
		if errs := ValidateSteps(steps); len(errs) == 0 {
			t.Errorf("%s: accepted", name)
		}
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

// `parent` rides in the same block and, like `view`, does not raise the schema
// version — an app declaring it installs on a build that has never heard of it.
func TestParseParent(t *testing.T) {
	a, err := Parse(map[string]any{"parent": "jellyfin"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.Parent != "jellyfin" {
		t.Fatalf("parent = %q, want %q", a.Parent, "jellyfin")
	}
	if a, err := Parse(map[string]any{"schema_version": SchemaVersion, "parent": "jellyfin"}); err != nil || a.Parent != "jellyfin" {
		t.Fatalf("parent at the current schema version: %q, %v", a.Parent, err)
	}
}

// An unknown view is a cosmetic mistake, not a reason to refuse the app: it
// falls back to the ordinary grid, which is where the app would have been
// anyway.
func TestNormalizeView(t *testing.T) {
	cases := map[string]string{
		"system":    ViewSystem,
		"  System ": ViewSystem,
		"hidden":    ViewApps, // retired: a tile is never hidden any more
		"Service":   ViewService,
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

// DeclaredView is what lets the app listing derive a view: it must tell an
// explicit `apps` from saying nothing, and treat an unknown value as nothing.
func TestDeclaredView(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"apps", ViewApps, true},
		{" SERVICE", ViewService, true},
		{"system", ViewSystem, true},
		{"hidden", "", false},
		{"", "", false},
		{"dashboard", "", false},
	}
	for _, tc := range cases {
		got, ok := DeclaredView(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("DeclaredView(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestDeclaresWebUI(t *testing.T) {
	cases := []struct {
		name string
		app  *App
		want bool
	}{
		{"nil block", nil, false},
		{"empty block", &App{}, false},
		{"webui-host", &App{WebUIHost: "app-${domain}"}, true},
		{"routes only", &App{Routes: []Route{{UpstreamPort: "8080"}}}, true},
		{"path alone is not an address", &App{WebUIPath: "/admin"}, false},
		{"port alone is not an address", &App{WebUIPort: "8443"}, false},
	}
	for _, tc := range cases {
		if got := tc.app.DeclaresWebUI(); got != tc.want {
			t.Errorf("%s: DeclaresWebUI() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The exclusion list has to survive the map[string]any round trip composefile puts
// the extension block through, and a build that does not understand `backup` must
// still read the rest of the block — which is what not raising SchemaVersion buys.
func TestParseBackupExclude(t *testing.T) {
	a, err := Parse(map[string]any{
		"schema_version": 2,
		"title":          map[string]any{"en_us": "Jellyfin"},
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
	a, err := Parse(map[string]any{"title": map[string]any{"en_us": "Jellyfin"}})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.Backup.Exclude != nil {
		t.Fatalf("exclude = %v, want nil", a.Backup.Exclude)
	}
	if a.Backup.Skip {
		t.Fatal("skip is set on an app that declared no backup block")
	}
}

// `skip` has to survive the same map[string]any round trip `exclude` does, and the
// two have to coexist: an app that is skipped entirely still declares what it would
// have left out, because the declaration is also documentation and because a skip can
// be lifted without the exclusions having to be rediscovered.
//
// The false case is asserted from an app that declares `skip: false` explicitly, not
// only from the absent one. The polarity is what makes the field safe on a build that
// predates it (see the Backup field comment), so there must be no path on which an
// unset or ignored `skip` reads as true.
func TestParseBackupSkip(t *testing.T) {
	a, err := Parse(map[string]any{
		"schema_version": 2,
		"title":          map[string]any{"en_us": "Kopia"},
		"backup": map[string]any{
			"skip":    true,
			"exclude": []any{"**/cache/", "**/logs/"},
		},
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !a.Backup.Skip {
		t.Fatal("skip = false, want true")
	}
	if want := "**/cache/ **/logs/"; strings.Join(a.Backup.Exclude, " ") != want {
		t.Fatalf("exclude = %v, want %q", a.Backup.Exclude, want)
	}
	if a.Title.Value() != "Kopia" {
		t.Fatalf("the rest of the block was lost: title = %q", a.Title.Value())
	}

	off, err := Parse(map[string]any{
		"schema_version": 2,
		"backup":         map[string]any{"skip": false, "exclude": []any{"cache/"}},
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if off.Backup.Skip {
		t.Fatal("skip = true on an app that declared skip: false")
	}
}

func TestParseLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		block                   map[string]any
		wantStop, wantUninstall bool
	}{
		{"absent allows both", map[string]any{"schema_version": 2}, true, true},
		{"empty block allows both", map[string]any{"schema_version": 2, "lifecycle": map[string]any{}}, true, true},
		{"system view alone allows both", map[string]any{"schema_version": 2, "view": "system"}, true, true},
		{"locked", map[string]any{"schema_version": 2, "lifecycle": map[string]any{"stoppable": false, "uninstallable": false}}, false, false},
		{"stoppable but not uninstallable", map[string]any{"schema_version": 2, "lifecycle": map[string]any{"uninstallable": false}}, true, false},
		{"explicit true", map[string]any{"schema_version": 2, "lifecycle": map[string]any{"stoppable": true, "uninstallable": true}}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := Parse(tc.block)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := a.Lifecycle.CanStop(); got != tc.wantStop {
				t.Errorf("CanStop = %v, want %v", got, tc.wantStop)
			}
			if got := a.Lifecycle.CanUninstall(); got != tc.wantUninstall {
				t.Errorf("CanUninstall = %v, want %v", got, tc.wantUninstall)
			}
		})
	}
}

// updatable is independent of the other two, and defaults to true like them.
func TestParseLifecycleUpdatable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block map[string]any
		want  bool
	}{
		{"absent", map[string]any{"schema_version": 2}, true},
		{"empty block", map[string]any{"schema_version": 2, "lifecycle": map[string]any{}}, true},
		{"stop and uninstall locked", map[string]any{"schema_version": 2, "lifecycle": map[string]any{"stoppable": false, "uninstallable": false}}, true},
		{"declared false", map[string]any{"schema_version": 2, "lifecycle": map[string]any{"updatable": false}}, false},
		{"explicit true", map[string]any{"schema_version": 2, "lifecycle": map[string]any{"updatable": true}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := Parse(tc.block)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := a.Lifecycle.CanUpdate(); got != tc.want {
				t.Errorf("CanUpdate = %v, want %v", got, tc.want)
			}
			if !a.Lifecycle.CanStop() && tc.name == "declared false" {
				t.Errorf("updatable: false must not restrict Stop")
			}
		})
	}
}

func TestParseCredentials(t *testing.T) {
	a, err := Parse(map[string]any{
		"schema_version": 2,
		"title":          map[string]any{"en_us": "Outline"},
		"credentials": []any{
			map[string]any{
				"label":    map[string]any{"en_us": "Administrator"},
				"username": "admin",
				"password": "${ADMIN_PASSWORD}",
				"note":     map[string]any{"en_us": "Change it", "fr_fr": "Changez-le"},
			},
			// A bare number must type as written, not cost the block; a label with no
			// en_us is refused and dropped, the entry kept.
			map[string]any{"label": map[string]any{"fr_fr": "Lecteur"}, "username": 1000, "password": 123456},
			// Not a mapping: decoded empty, never an error.
			"oops",
		},
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.Title.Value() != "Outline" {
		t.Fatalf("the rest of the block was lost: title = %q", a.Title.Value())
	}
	if len(a.Credentials) != 3 {
		t.Fatalf("credentials = %d, want 3", len(a.Credentials))
	}
	c := a.Credentials[0]
	if c.Label.Value() != "Administrator" || c.Username != "admin" || c.Password != "${ADMIN_PASSWORD}" || c.Note.Value() != "Change it" {
		t.Fatalf("first credential = %+v", c)
	}
	c = a.Credentials[1]
	if c.Label != nil || c.Username != "1000" || c.Password != "123456" || c.Note.Value() != "" {
		t.Fatalf("second credential = %+v", c)
	}
	if len(a.Problems) != 1 || !strings.HasPrefix(a.Problems[0], "credentials[1].label: ") {
		t.Fatalf("problems = %v, want the en_us-less label refused", a.Problems)
	}
	if c := a.Credentials[2]; c.Username != "" || c.Password != "" || c.Label != nil {
		t.Fatalf("scalar entry = %+v, want empty", a.Credentials[2])
	}
}

// A change is env, or a script entry; anything else — including the retired
// `apply: hook` — leaves the credential initial-only rather than failing the block.
func TestParseCredentialChange(t *testing.T) {
	cred := func(change map[string]any) map[string]any {
		return map[string]any{"username": "admin", "password": "${P}", "change": change}
	}
	a, err := Parse(map[string]any{
		"credentials": []any{
			cred(map[string]any{"apply": "env"}),
			cred(map[string]any{"apply": "script", "script": "compose exec app set-password", "timeout": "1m"}),
			cred(map[string]any{"apply": "script", "file": "scripts/passwd.sh"}),
			cred(map[string]any{"apply": "hook", "hook": "echo old form"}),
			cred(map[string]any{"apply": "script"}),
		},
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c := a.Credentials
	if c[0].Change == nil || c[0].Change.Apply != ApplyEnv {
		t.Errorf("env: %+v", c[0].Change)
	}
	if c[1].Change == nil || c[1].Change.Apply != ApplyScript || c[1].Change.Script.Script == "" || c[1].Change.Timeout != "1m" {
		t.Errorf("inline script: %+v", c[1].Change)
	}
	if c[2].Change == nil || c[2].Change.File != "scripts/passwd.sh" {
		t.Errorf("file script: %+v", c[2].Change)
	}
	if c[3].Change != nil || c[4].Change != nil {
		t.Errorf("unusable changes kept: %+v / %+v", c[3].Change, c[4].Change)
	}
}
