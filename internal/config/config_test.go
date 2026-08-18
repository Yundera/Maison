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
