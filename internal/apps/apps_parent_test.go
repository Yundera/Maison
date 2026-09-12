package apps

import (
	"testing"

	"github.com/yundera/maison/internal/xcomposeapp"
)

// buildApp only records what the compose declared: whether it resolves depends on
// what else is installed, which one app's metadata cannot say.
func TestBuildAppKeepsTheDeclaredParent(t *testing.T) {
	app := buildApp("bazarr", nil, &xcomposeapp.App{Parent: "  sonarr  "}, "", true, StatusRunning, nil)
	if app.Parent != "sonarr" {
		t.Errorf("parent = %q, want %q", app.Parent, "sonarr")
	}
	if app := buildApp("sonarr", nil, &xcomposeapp.App{}, "", true, StatusRunning, nil); app.Parent != "" {
		t.Errorf("parent = %q, want empty", app.Parent)
	}
}

// Every way a parent can fail to resolve ends the same way: an ordinary app. An
// extension must never vanish because its maintainer named an app this box does
// not have.
func TestResolveParents(t *testing.T) {
	cases := []struct {
		name string
		in   []App
		want map[string]string
	}{
		{
			name: "resolvable parent is kept",
			in:   []App{{ID: "jellyfin"}, {ID: "jellyseerr", Parent: "jellyfin"}},
			want: map[string]string{"jellyfin": "", "jellyseerr": "jellyfin"},
		},
		{
			name: "absent parent is dropped",
			in:   []App{{ID: "jellyseerr", Parent: "jellyfin"}},
			want: map[string]string{"jellyseerr": ""},
		},
		{
			name: "self-reference is dropped",
			in:   []App{{ID: "jellyfin", Parent: "jellyfin"}},
			want: map[string]string{"jellyfin": ""},
		},
		{
			name: "case is ignored and the id is canonicalised",
			in:   []App{{ID: "jellyfin"}, {ID: "jellyseerr", Parent: "Jellyfin"}},
			want: map[string]string{"jellyfin": "", "jellyseerr": "jellyfin"},
		},
		{
			// Nesting is one level: an extension cannot itself be extended, so the
			// grandchild is an ordinary app rather than a tile with nowhere to go.
			name: "chains collapse to one level",
			in:   []App{{ID: "a"}, {ID: "b", Parent: "a"}, {ID: "c", Parent: "b"}},
			want: map[string]string{"a": "", "b": "a", "c": ""},
		},
		{
			// The chain rule reads declared parents, so a cycle cannot survive it —
			// and cannot depend on which of the two was listed first either.
			name: "a two-app cycle is broken both ways",
			in:   []App{{ID: "a", Parent: "b"}, {ID: "b", Parent: "a"}},
			want: map[string]string{"a": "", "b": ""},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolveParents(tc.in)
			for _, a := range tc.in {
				if want := tc.want[a.ID]; a.Parent != want {
					t.Errorf("%s: parent = %q, want %q", a.ID, a.Parent, want)
				}
			}
		})
	}
}
