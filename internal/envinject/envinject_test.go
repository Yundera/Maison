package envinject

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yundera/maison/internal/config"
)

// A deployment that mounts the data folder elsewhere gets the host path as a
// symlink to the mount, so a script's $APP_DIR works in its own shell too.
func TestEnsureHostPathAlias(t *testing.T) {
	tmp := t.TempDir()
	data := filepath.Join(tmp, "DATA")
	if err := os.MkdirAll(filepath.Join(data, "AppData", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	host := filepath.Join(tmp, "mnt", "user", "appdata", "maison")
	cfg := config.Config{DataRoot: data, DataHostPath: host}
	if err := EnsureHostPathAlias(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(host, "AppData", "app")); err != nil {
		t.Fatalf("host path does not reach the data mount: %v", err)
	}
	// Idempotent: a second start finds its own link.
	if err := EnsureHostPathAlias(cfg); err != nil {
		t.Fatalf("second run: %v", err)
	}
	// Same path: nothing to do, nothing created.
	if err := EnsureHostPathAlias(config.Config{DataRoot: data, DataHostPath: data}); err != nil {
		t.Fatal(err)
	}
	// Someone else's directory at the host path is reported, never replaced.
	other := filepath.Join(tmp, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureHostPathAlias(config.Config{DataRoot: data, DataHostPath: other}); err == nil {
		t.Fatal("an unrelated directory at the host path was accepted")
	}
}

// ContainerPath and HostPath are inverses across the data mount.
func TestPathMapping(t *testing.T) {
	cfg := config.Config{DataRoot: "/DATA", DataHostPath: "/opt/maison/DATA"}
	for _, in := range []string{
		"/DATA/AppData/jellyfin/config",
		"${DATA_ROOT}/AppData/jellyfin/config",
		"/opt/maison/DATA/AppData/jellyfin/config",
	} {
		const wantContainer = "/DATA/AppData/jellyfin/config"
		got := ContainerPath(in, cfg)
		if got != wantContainer {
			t.Fatalf("ContainerPath(%q) = %q, want %q", in, got, wantContainer)
		}
		const wantHost = "/opt/maison/DATA/AppData/jellyfin/config"
		if back := HostPath(got, cfg); back != wantHost {
			t.Fatalf("HostPath(%q) = %q, want %q", got, back, wantHost)
		}
	}
	// A path outside the data mount is left alone — the caller decides what to do.
	if got := HostPath("/etc/hosts", cfg); got != "/etc/hosts" {
		t.Fatalf("HostPath(/etc/hosts) = %q", got)
	}
}
