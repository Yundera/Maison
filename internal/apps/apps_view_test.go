package apps

import (
	"testing"

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
		{"hidden app", "hidden", xcomposeapp.ViewHidden, false},
		{"ordinary app", "", xcomposeapp.ViewApps, false},
		{"unknown view falls back", "platform", xcomposeapp.ViewApps, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := buildApp("x", nil, &xcomposeapp.App{View: tc.view}, "", true, StatusRunning, nil)
			if app.View != tc.wantView {
				t.Errorf("view = %q, want %q", app.View, tc.wantView)
			}
			if app.Protected != tc.protected {
				t.Errorf("protected = %v, want %v", app.Protected, tc.protected)
			}
		})
	}
}

// An app with no x-compose-app block at all — every store app today — lands in
// the ordinary grid and stays uninstallable.
func TestBuildAppDefaultsToTheAppsView(t *testing.T) {
	app := buildApp("x", &xcasaos.StoreInfo{}, nil, "", false, StatusRunning, nil)
	if app.View != xcomposeapp.ViewApps || app.Protected {
		t.Fatalf("view = %q, protected = %v; want %q / false", app.View, app.Protected, xcomposeapp.ViewApps)
	}
}
