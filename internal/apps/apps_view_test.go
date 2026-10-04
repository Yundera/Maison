package apps

import (
	"testing"

	"github.com/yundera/maison/internal/dockerx"
	"github.com/yundera/maison/internal/xcasaos"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// Protection is derived from the view in exactly one place, so a tile can never
// be shown in the System grid while the API still lets it be stopped.
func TestBuildAppDerivesProtectedFromView(t *testing.T) {
	cases := []struct {
		name      string
		view      string
		wantView  string
		protected bool
	}{
		{"system app", "system", xcomposeapp.ViewSystem, true},
		{"retired hidden view falls back", "hidden", xcomposeapp.ViewApps, false},
		{"service app", "service", xcomposeapp.ViewService, false},
		{"ordinary app", "", xcomposeapp.ViewApps, false},
		{"unknown view falls back", "platform", xcomposeapp.ViewApps, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ca := &xcomposeapp.App{View: tc.view, WebUIHost: "x-${domain}"}
			app := buildApp("x", nil, ca, "", true, StatusRunning, nil)
			if app.View != tc.wantView {
				t.Errorf("view = %q, want %q", app.View, tc.wantView)
			}
			if app.Protected != tc.protected {
				t.Errorf("protected = %v, want %v", app.Protected, tc.protected)
			}
		})
	}
}

// An app with only an x-casaos block that names a web UI — the legacy CasaOS
// shape — lands in the ordinary grid and stays uninstallable.
func TestBuildAppDefaultsToTheAppsView(t *testing.T) {
	app := buildApp("x", &xcasaos.StoreInfo{WebUIPort: "8080"}, nil, "", false, StatusRunning, nil)
	if app.View != xcomposeapp.ViewApps || app.Protected {
		t.Fatalf("view = %q, protected = %v; want %q / false", app.View, app.Protected, xcomposeapp.ViewApps)
	}
}

// With no declared view, the grid follows whether the app declares a web UI —
// and a declared view, `apps` included, always wins over the derivation.
func TestBuildAppDerivesServiceView(t *testing.T) {
	smb := map[string][]dockerx.Port{"samba": {{Private: 445, Public: 445}}}
	web := map[string][]dockerx.Port{"web": {{Private: 80, Public: 8080}}}
	cases := []struct {
		name  string
		si    *xcasaos.StoreInfo
		ca    *xcomposeapp.App
		ports map[string][]dockerx.Port
		want  string
	}{
		{"x-casaos with no web fields", &xcasaos.StoreInfo{Category: "Cloud"}, nil, smb, xcomposeapp.ViewService},
		{"x-casaos webui_port", &xcasaos.StoreInfo{WebUIPort: "80"}, nil, nil, xcomposeapp.ViewApps},
		{"x-casaos port_map", &xcasaos.StoreInfo{PortMap: "8081"}, nil, nil, xcomposeapp.ViewApps},
		{"x-casaos hostname", &xcasaos.StoreInfo{Hostname: "app.example.com"}, nil, nil, xcomposeapp.ViewApps},
		{"x-casaos index alone", &xcasaos.StoreInfo{Index: "/admin"}, nil, nil, xcomposeapp.ViewService},
		{"x-compose-app webui-host", nil, &xcomposeapp.App{WebUIHost: "app-${domain}"}, nil, xcomposeapp.ViewApps},
		{"x-compose-app routes", nil, &xcomposeapp.App{Routes: []xcomposeapp.Route{{UpstreamPort: "80"}}}, nil, xcomposeapp.ViewApps},
		{"x-compose-app webui-path alone", nil, &xcomposeapp.App{WebUIPath: "/"}, nil, xcomposeapp.ViewService},
		{"web UI from either block counts", &xcasaos.StoreInfo{WebUIPort: "80"}, &xcomposeapp.App{}, nil, xcomposeapp.ViewApps},
		{"explicit apps wins", nil, &xcomposeapp.App{View: "apps"}, nil, xcomposeapp.ViewApps},
		{"explicit system wins", nil, &xcomposeapp.App{View: "system"}, nil, xcomposeapp.ViewSystem},
		{"retired hidden view is derived", nil, &xcomposeapp.App{View: "hidden"}, nil, xcomposeapp.ViewService},
		{"unknown view is derived", nil, &xcomposeapp.App{View: "platform"}, nil, xcomposeapp.ViewService},
		{"no metadata, published port", nil, nil, web, xcomposeapp.ViewApps},
		{"no metadata, no published port", nil, nil, map[string][]dockerx.Port{"db": {{Private: 5432}}}, xcomposeapp.ViewService},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := buildApp("x", tc.si, tc.ca, "", true, StatusRunning, tc.ports)
			if app.View != tc.want {
				t.Errorf("view = %q, want %q", app.View, tc.want)
			}
			if app.Protected != (tc.want == xcomposeapp.ViewSystem) {
				t.Errorf("protected = %v for view %q", app.Protected, app.View)
			}
		})
	}
}

// The derivation must not depend on a domain being configured: a webui-host that
// cannot resolve yet still declares a web UI, so the app stays in the app grid.
func TestBuildAppUnresolvedHostStaysAnApp(t *testing.T) {
	app := buildApp("x", nil, &xcomposeapp.App{WebUIHost: "x-${domain}"}, "", true, StatusRunning, nil)
	if app.URL != "" || app.View != xcomposeapp.ViewApps {
		t.Fatalf("url = %q, view = %q; want no url, %q", app.URL, app.View, xcomposeapp.ViewApps)
	}
}

// A service's published port is its non-HTTP listener (Samba's 445): it must not
// be turned into a click URL the way an app's published web port is.
func TestBuildAppServiceGetsNoPortURL(t *testing.T) {
	smb := map[string][]dockerx.Port{"samba": {{Private: 445, Public: 445}}}
	app := buildApp("samba", &xcasaos.StoreInfo{Category: "Cloud"}, nil, "", true, StatusRunning, smb)
	if app.View != xcomposeapp.ViewService || app.Port != "" {
		t.Fatalf("view = %q, port = %q; want %q and no port", app.View, app.Port, xcomposeapp.ViewService)
	}
	web := map[string][]dockerx.Port{"web": {{Private: 80, Public: 8080}}}
	if got := buildApp("hand", nil, nil, "", false, StatusRunning, web).Port; got != "8080" {
		t.Fatalf("hand-made stack port = %q, want 8080", got)
	}
}
