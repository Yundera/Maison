package server

import (
	"encoding/json"
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

// The engine routes must work on a box with no Docker and no repository: an
// unconfigured deployment has to render a settings page that explains itself, not a
// 503. They also sit one character from /api/backups, so this pins that both
// resolve to their own handler rather than one swallowing the other.
func TestBackupEngineRoutesAreReachableWithoutDockerOrARepository(t *testing.T) {
	h := New(config.Config{DataRoot: t.TempDir()}, fstest.MapFS{})

	for _, tc := range []struct {
		method, path string
		body         string
		wantStatus   int
		wantBody     string
	}{
		{"GET", "/api/backup/status", "", http.StatusOK, `"active"`},
		{"GET", "/api/backups", "", http.StatusOK, `"apps"`},
		{"PUT", "/api/backup/config", `{"enabled":false,"hour":4,"minute":15}`, http.StatusOK, `"hour":4`},
		{"PUT", "/api/backup/config", `{"engine":"nosuchengine"}`, http.StatusBadRequest, "unknown backup engine"},
		{"POST", "/api/backup/email-key", "", http.StatusBadRequest, "no mail server configured"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.wantStatus {
			t.Errorf("%s %s = %d, want %d (body %s)", tc.method, tc.path, rec.Code, tc.wantStatus, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), tc.wantBody) {
			t.Errorf("%s %s body = %s, want it to contain %q", tc.method, tc.path, rec.Body.String(), tc.wantBody)
		}
	}
}

// The user-data restore route sits under /backups/, where the next segment is otherwise
// an app name. Static beats a parameter in chi, which is what stops this being read as
// a restore of an app called "userdata" — the same precedence the /apps/{id}/{action}
// routes rely on above, and worth pinning for the same reason.
func TestUserDataRestoreRouteBeatsTheAppWildcard(t *testing.T) {
	h := New(config.Config{DataRoot: t.TempDir()}, fstest.MapFS{})

	req := httptest.NewRequest(http.MethodPost, "/api/backups/userdata/restore",
		strings.NewReader(`{"name":"2026-01-01_000000"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// The box has the local engine and no repository, so the restore is refused — but by
	// the user-data handler, which says so in the engine's terms. handleRestoreOrphan
	// would have reported a missing backup for an app instead.
	body := rec.Body.String()
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/backups/userdata/restore = %d %s; want 400", rec.Code, body)
	}
	if !strings.Contains(body, "engine") {
		t.Errorf("body = %s; want the user-data handler's refusal, not an app restore", body)
	}
}

// The global page must describe the user-data set even on a box that cannot back it up,
// because "no backups" and "this engine will never do this" look identical otherwise.
func TestGlobalBackupsDescribesTheUserDataSet(t *testing.T) {
	h := New(config.Config{DataRoot: t.TempDir()}, fstest.MapFS{})

	req := httptest.NewRequest(http.MethodGet, "/api/backups", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/backups = %d %s; want 200", rec.Code, rec.Body.String())
	}

	var got struct {
		UserData struct {
			Available bool     `json:"available"`
			Reason    string   `json:"reason"`
			Source    string   `json:"source"`
			Excluded  []string `json:"excluded"`
		} `json:"user_data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.UserData.Available {
		t.Error("the local engine cannot back up the user-data set; available must be false")
	}
	if got.UserData.Reason == "" {
		t.Error("an unavailable set must say why")
	}
	// The exclusions are what make a restore that did not bring something back
	// diagnosable, so they are part of the payload rather than folklore.
	if len(got.UserData.Excluded) == 0 || got.UserData.Source == "" {
		t.Errorf("user_data = %+v; want the source and its exclusions", got.UserData)
	}
}
