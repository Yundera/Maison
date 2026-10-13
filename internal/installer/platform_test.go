package installer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yundera/maison/internal/config"
)

// A store app must not install over a stack the box's platform deployed — the managed
// PCS's own kopia — while reinstalling an ordinary app over its own folder still works.
func TestInstallRefusesAPlatformOwnedFolder(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	in := &Installer{cfg: cfg}
	write := func(project, compose string) {
		t.Helper()
		dir := filepath.Join(cfg.AppsDir(), project)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(compose), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := in.refusePlatformOwned("fresh"); err != nil {
		t.Errorf("fresh install refused: %v", err)
	}

	write("kopia", "name: kopia\nservices:\n  a:\n    image: x\nx-compose-app:\n  lifecycle:\n    uninstallable: false\n")
	if err := in.refusePlatformOwned("kopia"); !errors.Is(err, ErrPlatformOwned) {
		t.Errorf("platform stack: %v, want ErrPlatformOwned", err)
	}

	write("spliit", "name: spliit\nservices:\n  a:\n    image: x\n")
	if err := in.refusePlatformOwned("spliit"); err != nil {
		t.Errorf("reinstall over an ordinary app refused: %v", err)
	}
}
