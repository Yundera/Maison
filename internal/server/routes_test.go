package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/yundera/maison/internal/config"
)

// TestBackupRoutesBeatTheActionCatchAll pins the one thing about the backup
// routes that is not obvious from reading them.
//
// `POST /api/apps/{id}/{action}` is a catch-all at the same depth as
// `POST /api/apps/{id}/backup` and `/restore`. chi resolves a static segment
// ahead of a parameter, so the specific routes win — but that is a property of
// the router, not of our code, and if it ever stopped holding, the symptom would
// be a confusing "unknown action" from a button that used to work.
//
// The server is built without a reachable daemon and with an empty app tree, so
// every handler here fails — which is what makes it a routing test: *which*
// failure comes back identifies which handler ran. Only handleAppAction ever
// says "unknown action".
func TestBackupRoutesBeatTheActionCatchAll(t *testing.T) {
	h := New(config.Config{DataRoot: t.TempDir()}, fstest.MapFS{})

	for _, c := range []struct {
		method, path string
		wantBody     string // a fragment only the intended handler produces
	}{
		{http.MethodPost, "/api/apps/jellyfin/backup", "no folder to back up"},
		{http.MethodPost, "/api/apps/jellyfin/restore", "not a backup name"},
		{http.MethodGet, "/api/apps/jellyfin/backups", "[]"},
		{http.MethodGet, "/api/apps/jellyfin/backups/estimate", "no folder to back up"},
		// The catch-all still works for what it is actually for.
		{http.MethodPost, "/api/apps/jellyfin/nonsense", "unknown action"},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		body := rec.Body.String()
		if !strings.Contains(body, c.wantBody) {
			t.Errorf("%s %s -> %d %s; want a response containing %q",
				c.method, c.path, rec.Code, body, c.wantBody)
		}
		if c.wantBody != "unknown action" && strings.Contains(body, "unknown action") {
			t.Errorf("%s %s: swallowed by the {action} catch-all", c.method, c.path)
		}
	}
}

// TestGlobalBackupRoutesAreReachable guards the other half of the design: the
// global surface must work for an app that no longer exists, so it must not sit
// behind the app registry or the Docker daemon.
func TestGlobalBackupRoutesAreReachable(t *testing.T) {
	h := New(config.Config{DataRoot: t.TempDir()}, fstest.MapFS{})

	req := httptest.NewRequest(http.MethodGet, "/api/backups", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/backups = %d %s; want 200", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"apps"`) {
		t.Errorf("GET /api/backups body = %s; want an apps list", rec.Body.String())
	}

	// Deleting is name-validated, not registry-backed, so a bad name is a 400
	// rather than a 404 from the SPA catch-all — proof the route exists.
	req = httptest.NewRequest(http.MethodDelete, "/api/backups/jellyfin/notes", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not a backup name") {
		t.Errorf("DELETE /api/backups/jellyfin/notes = %d %s; want 400 with a name error",
			rec.Code, rec.Body.String())
	}
}
