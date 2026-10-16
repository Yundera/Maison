package apps

import (
	"testing"

	"github.com/yundera/maison/internal/composefile"
	"github.com/yundera/maison/internal/dockerx"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// meta resolves the metadata of a compose given as YAML, the way the registry does.
func meta(t *testing.T, compose string) *xcomposeapp.App {
	t.Helper()
	f, err := composefile.Parse([]byte(compose))
	if err != nil {
		t.Fatal(err)
	}
	ca, err := f.App()
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// The view is a grid and nothing else: what the operator may do to an app comes
// from its own `lifecycle` declaration, whatever grid it sits in.
func TestBuildAppTakesLifecycleFromTheDeclarationNotTheView(t *testing.T) {
	no := false
	locked := xcomposeapp.LifecycleSpec{Stoppable: &no, Uninstallable: &no}
	cases := []struct {
		name                     string
		view                     string
		lc                       xcomposeapp.LifecycleSpec
		wantView                 string
		stoppable, uninstallable bool
	}{
		{"system view alone", "system", xcomposeapp.LifecycleSpec{}, xcomposeapp.ViewSystem, true, true},
		{"system view, locked", "system", locked, xcomposeapp.ViewSystem, false, false},
		{"ordinary app, locked", "", locked, xcomposeapp.ViewApps, false, false},
		{"only uninstall refused", "system", xcomposeapp.LifecycleSpec{Uninstallable: &no}, xcomposeapp.ViewSystem, true, false},
		{"service app", "service", xcomposeapp.LifecycleSpec{}, xcomposeapp.ViewService, true, true},
		{"ordinary app", "", xcomposeapp.LifecycleSpec{}, xcomposeapp.ViewApps, true, true},
		{"unknown view falls back", "platform", xcomposeapp.LifecycleSpec{}, xcomposeapp.ViewApps, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ca := &xcomposeapp.App{View: tc.view, WebUIHost: "x-${domain}", Lifecycle: tc.lc}
			app := buildApp("x", ca, "", true, StatusRunning, nil)
			if app.View != tc.wantView {
				t.Errorf("view = %q, want %q", app.View, tc.wantView)
			}
			if app.Stoppable != tc.stoppable || app.Uninstallable != tc.uninstallable {
				t.Errorf("stoppable/uninstallable = %v/%v, want %v/%v",
					app.Stoppable, app.Uninstallable, tc.stoppable, tc.uninstallable)
			}
		})
	}
}

// lifecycle.updatable reaches the tile on its own, without touching the other two.
func TestBuildAppTakesUpdatableFromTheDeclaration(t *testing.T) {
	no := false
	ca := &xcomposeapp.App{View: "system", Lifecycle: xcomposeapp.LifecycleSpec{Updatable: &no}}
	app := buildApp("x", ca, "", true, StatusRunning, nil)
	if app.Updatable || !app.Stoppable || !app.Uninstallable {
		t.Fatalf("updatable/stoppable/uninstallable = %v/%v/%v, want false/true/true",
			app.Updatable, app.Stoppable, app.Uninstallable)
	}
	if plain := buildApp("y", &xcomposeapp.App{}, "", true, StatusRunning, nil); !plain.Updatable {
		t.Fatal("an app with no declaration must stay updatable")
	}
}

// An app with only an x-casaos block that names a web UI — the legacy CasaOS
// shape — lands in the ordinary grid and stays uninstallable.
func TestBuildAppDefaultsToTheAppsView(t *testing.T) {
	app := buildApp("x", meta(t, "x-casaos:\n  webui_port: \"8080\"\n"), "", false, StatusRunning, nil)
	if app.View != xcomposeapp.ViewApps || !app.Stoppable || !app.Uninstallable {
		t.Fatalf("view = %q, stoppable = %v, uninstallable = %v; want %q / true / true",
			app.View, app.Stoppable, app.Uninstallable, xcomposeapp.ViewApps)
	}
}

// With no declared view, the grid follows whether the app declares a web UI —
// and a declared view, `apps` included, always wins over the derivation.
func TestBuildAppDerivesServiceView(t *testing.T) {
	smb := map[string][]dockerx.Port{"samba": {{Private: 445, Public: 445}}}
	web := map[string][]dockerx.Port{"web": {{Private: 80, Public: 8080}}}
	cases := []struct {
		name string
		ca   string // compose YAML; "" means no metadata at all

		ports map[string][]dockerx.Port
		want  string
	}{
		{"x-casaos with no web fields", "x-casaos:\n  category: Cloud\n", smb, xcomposeapp.ViewService},
		{"x-casaos webui_port", "x-casaos:\n  webui_port: \"80\"\n", nil, xcomposeapp.ViewApps},
		{"x-casaos port_map", "x-casaos:\n  port_map: \"8081\"\n", nil, xcomposeapp.ViewApps},
		{"x-casaos hostname", "x-casaos:\n  hostname: app.example.com\n", nil, xcomposeapp.ViewApps},
		{"x-casaos index alone", "x-casaos:\n  index: /admin\n", nil, xcomposeapp.ViewService},
		{"x-compose-app webui-host", "x-compose-app:\n  webui-host: app-${domain}\n", nil, xcomposeapp.ViewApps},
		{"x-compose-app routes", "x-compose-app:\n  routes: [{upstream-port: 80}]\n", nil, xcomposeapp.ViewApps},
		{"x-compose-app webui-path alone", "x-compose-app:\n  webui-path: /\n", nil, xcomposeapp.ViewService},
		{"x-compose-app webui-port alone", "x-compose-app:\n  webui-port: \"8080\"\n", nil, xcomposeapp.ViewService},
		{"x-casaos beside x-compose-app is ignored", "x-casaos:\n  webui_port: \"80\"\nx-compose-app:\n  title: X\n", nil, xcomposeapp.ViewService},
		{"explicit apps wins", "x-compose-app:\n  view: apps\n", nil, xcomposeapp.ViewApps},
		{"explicit system wins", "x-compose-app:\n  view: system\n", nil, xcomposeapp.ViewSystem},
		{"retired hidden view is derived", "x-compose-app:\n  view: hidden\n", nil, xcomposeapp.ViewService},
		{"unknown view is derived", "x-compose-app:\n  view: platform\n", nil, xcomposeapp.ViewService},
		{"no metadata, published port", "", web, xcomposeapp.ViewApps},
		{"no metadata, no published port", "", map[string][]dockerx.Port{"db": {{Private: 5432}}}, xcomposeapp.ViewService},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ca *xcomposeapp.App
			if tc.ca != "" {
				ca = meta(t, tc.ca)
			}
			app := buildApp("x", ca, "", true, StatusRunning, tc.ports)
			if app.View != tc.want {
				t.Errorf("view = %q, want %q", app.View, tc.want)
			}
			if !app.Stoppable || !app.Uninstallable {
				t.Errorf("view %q restricted the lifecycle (stoppable %v, uninstallable %v)",
					app.View, app.Stoppable, app.Uninstallable)
			}
		})
	}
}

// The derivation must not depend on a domain being configured: a webui-host that
// cannot resolve yet still declares a web UI, so the app stays in the app grid.
func TestBuildAppUnresolvedHostStaysAnApp(t *testing.T) {
	app := buildApp("x", &xcomposeapp.App{WebUIHost: "x-${domain}"}, "", true, StatusRunning, nil)
	if app.URL != "" || app.View != xcomposeapp.ViewApps {
		t.Fatalf("url = %q, view = %q; want no url, %q", app.URL, app.View, xcomposeapp.ViewApps)
	}
}

// A service's published port is its non-HTTP listener (Samba's 445): it must not
// be turned into a click URL the way an app's published web port is.
func TestBuildAppServiceGetsNoPortURL(t *testing.T) {
	smb := map[string][]dockerx.Port{"samba": {{Private: 445, Public: 445}}}
	app := buildApp("samba", meta(t, "x-casaos:\n  category: Cloud\n"), "", true, StatusRunning, smb)
	if app.View != xcomposeapp.ViewService || app.Port != "" {
		t.Fatalf("view = %q, port = %q; want %q and no port", app.View, app.Port, xcomposeapp.ViewService)
	}
	web := map[string][]dockerx.Port{"web": {{Private: 80, Public: 8080}}}
	if got := buildApp("hand", nil, "", false, StatusRunning, web).Port; got != "8080" {
		t.Fatalf("hand-made stack port = %q, want 8080", got)
	}
}

// A CasaOS app is opened the way CasaOS opens it: hostname, port_map and index
// build the URL, an ${APP_DOMAIN} hostname resolving like a native webui-host, and
// without a hostname the browser's own host on port_map — where the published
// port bound to webui_port on the main service wins.
func TestBuildAppOpensCasaOSAppsLikeCasaOS(t *testing.T) {
	app := buildApp("x", meta(t, "x-casaos:\n  scheme: https\n  hostname: x-${APP_DOMAIN}\n  port_map: \"443\"\n  index: /web\n"), "box.example", true, StatusRunning, nil)
	if app.URL != "https://x-box.example/web" {
		t.Fatalf("url = %q", app.URL)
	}
	app = buildApp("x", meta(t, "x-casaos:\n  port_map: \"8081\"\n  index: /admin\n"), "box.example", true, StatusRunning, nil)
	if app.URL != "" || app.Port != "8081" || app.Scheme != "http" || app.Index != "/admin" {
		t.Fatalf("url/port/scheme/index = %q/%q/%q/%q", app.URL, app.Port, app.Scheme, app.Index)
	}
	ports := map[string][]dockerx.Port{
		"db":  {{Private: 5432, Public: 5432}},
		"web": {{Private: 6881, Public: 6881}, {Private: 8080, Public: 18080}},
	}
	app = buildApp("x", meta(t, "x-casaos:\n  main: web\n  webui_port: \"8080\"\n"), "", true, StatusRunning, ports)
	if app.Port != "18080" {
		t.Fatalf("port = %q, want the one bound to webui_port on main", app.Port)
	}
}

// One block or the other: a compose carrying both is read as x-compose-app, and
// nothing of its x-casaos block reaches the tile.
func TestBuildAppIgnoresXCasaOSBesideXComposeApp(t *testing.T) {
	ca := meta(t, "x-casaos:\n  title: {en_us: Legacy}\n  icon: https://e.x/legacy.png\n  category: Old\n  hostname: legacy.example\nx-compose-app:\n  webui-host: x-${domain}\n")
	app := buildApp("x", ca, "box.example", true, StatusRunning, nil)
	if app.Name != "x" || app.Icon != "" || app.Category != "" || app.URL != "https://x-box.example/" {
		t.Fatalf("name/icon/category/url = %q/%q/%q/%q", app.Name, app.Icon, app.Category, app.URL)
	}
}
