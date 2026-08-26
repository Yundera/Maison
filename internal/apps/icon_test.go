package apps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/yundera/maison/internal/appicon"
	"github.com/yundera/maison/internal/config"
)

// seedIconApp writes a managed app whose compose declares an icon URL.
func seedIconApp(t *testing.T, appsDir, id, iconURL string) string {
	t.Helper()
	dir := filepath.Join(appsDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	base := "services: {}\nx-casaos:\n  icon: " + iconURL + "\n"
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Apps installed before Maison kept a local icon have none on disk, and nothing
// else would ever put one there — the copy is otherwise only taken at install and
// refreshed at update.
func TestEnsureIconsBackfillsAnAppWithNoCopy(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte("PNG"))
	}))
	defer srv.Close()

	r := New(config.Config{DataRoot: t.TempDir()}, nil)
	dir := seedIconApp(t, r.cfg.AppsDir(), "jellyfin", srv.URL+"/icon.png")

	r.EnsureIcons(context.Background())
	if got, err := os.ReadFile(filepath.Join(dir, appicon.FileBase+".png")); err != nil || string(got) != "PNG" {
		t.Fatalf("icon = %q / %v, want the fetched bytes", got, err)
	}
	if u := r.localIcon("jellyfin"); u != "/api/apps/jellyfin/icon" {
		t.Fatalf("localIcon = %q, want the API path", u)
	}

	// Idempotent: an app that already has its copy is not re-downloaded.
	r.EnsureIcons(context.Background())
	if hits != 1 {
		t.Fatalf("fetched %d times, want once", hits)
	}
}

// An app with no copy keeps the icon URL from its compose — the tile renders as
// it always did.
func TestLocalIconIsEmptyWithoutACopy(t *testing.T) {
	r := New(config.Config{DataRoot: t.TempDir()}, nil)
	seedIconApp(t, r.cfg.AppsDir(), "jellyfin", "https://cdn.example/icon.png")
	if u := r.localIcon("jellyfin"); u != "" {
		t.Fatalf("localIcon = %q, want none", u)
	}
}

// IconPath's answer is handed to a file server, so an id that is not a plain app
// name gets no path at all.
func TestIconPathRefusesAnIdThatIsNotAnAppName(t *testing.T) {
	r := New(config.Config{DataRoot: t.TempDir()}, nil)
	for _, id := range []string{"", "..", "../../etc", `a\b`, "a/b"} {
		if p := r.IconPath(id); p != "" {
			t.Errorf("IconPath(%q) = %q, want none", id, p)
		}
	}
}
