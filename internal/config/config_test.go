package config

import (
	"path/filepath"
	"testing"
)

func TestStateDirDefaultsToMaisonsOwnAppFolder(t *testing.T) {
	c := Config{DataRoot: "/DATA"}
	if got, want := c.StateDir(), filepath.Join("/DATA", "AppData", "maison"); got != want {
		t.Errorf("StateDir() = %q, want %q", got, want)
	}
}

// STATE_DIR moves everything Maison owns — settings, store cache, and the
// .env.app it reads the deployment's variables from.
func TestStateDirOverride(t *testing.T) {
	c := Config{DataRoot: "/DATA", StateDirPath: "/var/lib/maison"}
	if got, want := c.StateDir(), "/var/lib/maison"; got != want {
		t.Errorf("StateDir() = %q, want %q", got, want)
	}
}

func TestIsProtected(t *testing.T) {
	c := Config{ProtectedApps: []string{"maison", "CasaOS"}}

	cases := []struct {
		name    string
		storeID string
		project string
		want    bool
	}{
		{"store id matches", "maison", "my-dashboard", true},
		{"project name matches when no store id", "", "maison", true},
		{"match is case-insensitive", "casaos", "whatever", true},
		{"unrelated app", "nextcloud", "nextcloud", false},
		{"empty identifiers never match", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.IsProtected(tc.storeID, tc.project); got != tc.want {
				t.Fatalf("IsProtected(%q, %q) = %v, want %v", tc.storeID, tc.project, got, tc.want)
			}
		})
	}

	if (Config{}).IsProtected("maison", "maison") {
		t.Fatal("nothing is protected when PROTECTED_APPS is unset")
	}
}
