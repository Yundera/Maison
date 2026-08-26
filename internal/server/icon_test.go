package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/yundera/maison/internal/appicon"
	"github.com/yundera/maison/internal/config"
)

// The app list points every installed tile at /api/apps/<app>/icon, so that route
// has to serve the file in the app's folder — with the content type the browser
// needs to render it in an <img>.
func TestAppIconIsServedFromTheAppFolder(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "AppData", "jellyfin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A one-pixel PNG's magic bytes are enough: what is served is the file.
	if err := os.WriteFile(filepath.Join(dir, appicon.FileBase+".png"), []byte("\x89PNG\r\n\x1a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An app with a folder but no icon: nothing to serve, and nothing to fetch
	// either (no icon URL), so the boot backfill leaves it alone.
	if err := os.MkdirAll(filepath.Join(root, "AppData", "plain"), 0o755); err != nil {
		t.Fatal(err)
	}

	h := New(config.Config{DataRoot: root}, fstest.MapFS{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/apps/jellyfin/icon", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/png") {
		t.Errorf("content-type = %q, want image/png", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("cache-control = %q, want no-cache (revalidate, so an update's icon shows)", cc)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/apps/plain/icon", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("an app with no copied icon: status = %d, want 404", rec.Code)
	}
}
