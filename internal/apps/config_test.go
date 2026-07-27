package apps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yundera/maison/internal/config"
)

// seedTipsApp writes a store-shipped base compose carrying x-casaos tips.
func seedTipsApp(t *testing.T, appsDir, id string) string {
	t.Helper()
	dir := filepath.Join(appsDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	base := "services: {}\nx-casaos:\n  tips:\n    custom: store tips\n"
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSetTipsWritesXComposeAppOverrideAndLeavesBaseAlone(t *testing.T) {
	r := New(config.Config{DataRoot: t.TempDir()}, nil)
	if err := os.MkdirAll(r.cfg.AppsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := seedTipsApp(t, r.cfg.AppsDir(), "jellyfin")

	if err := r.SetTips("jellyfin", "my note ${PUID}"); err != nil {
		t.Fatal(err)
	}

	over, err := os.ReadFile(filepath.Join(dir, "docker-compose.override.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(over), "x-compose-app:") || !strings.Contains(string(over), "my note") {
		t.Fatalf("tips not in the override's x-compose-app block:\n%s", over)
	}
	if strings.Contains(string(over), "x-casaos") {
		t.Fatalf("override must not carry x-casaos tips:\n%s", over)
	}
	base, _ := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	if !strings.Contains(string(base), "store tips") {
		t.Fatalf("base compose was mutated:\n%s", base)
	}

	cfg, err := r.GetConfig("jellyfin")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tips != "my note ${PUID}" {
		t.Fatalf("override tips should win over the store's, got %q", cfg.Tips)
	}
}

func TestSetTipsEmptyFallsBackToStoreTips(t *testing.T) {
	r := New(config.Config{DataRoot: t.TempDir()}, nil)
	if err := os.MkdirAll(r.cfg.AppsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := seedTipsApp(t, r.cfg.AppsDir(), "jellyfin")

	if err := r.SetTips("jellyfin", "my note"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetTips("jellyfin", "  "); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "docker-compose.override.yml")); !os.IsNotExist(err) {
		t.Fatal("an override holding only cleared tips should be removed")
	}
	cfg, err := r.GetConfig("jellyfin")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tips != "store tips" {
		t.Fatalf("cleared tips should fall back to the store's, got %q", cfg.Tips)
	}
}
