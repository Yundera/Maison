package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/yundera/maison/internal/config"
)

// /api/store/{id}/asset/* sits among /store/app/{id}, /store/{id}/backups and
// /store/{id}/install, and it is the only one of them with a wildcard tail. This
// pins that each still reaches its own handler — the symptom of getting it wrong
// is a store page whose images come back as JSON.
//
// The server is built with no store configured, so every one of these 404s;
// *which* 404 comes back is what identifies the handler that ran. The JSON store
// handlers say so in a body, the asset route answers a bare image 404.
func TestStoreAssetRouteDoesNotCollide(t *testing.T) {
	h := New(config.Config{DataRoot: t.TempDir()}, fstest.MapFS{})

	for _, c := range []struct {
		path     string
		wantCode int
		wantBody string // "" means: no body at all, which is http.NotFound's
	}{
		{"/api/store/demo/asset/icon.png", http.StatusNotFound, ""},
		{"/api/store/demo/asset/assets/screenshot-1.png", http.StatusNotFound, ""},
		{"/api/store/demo/asset/icon.png?store=example.test%2Fs.zip", http.StatusNotFound, ""},
		// The neighbours still answer as themselves.
		{"/api/store/app/demo", http.StatusNotFound, `"error"`},
		{"/api/store/demo/backups", http.StatusNotFound, `"error"`},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))

		if rec.Code != c.wantCode {
			t.Errorf("GET %s -> %d, want %d", c.path, rec.Code, c.wantCode)
		}
		body := strings.TrimSpace(rec.Body.String())
		if c.wantBody == "" {
			if strings.Contains(body, `"error"`) {
				t.Errorf("GET %s: answered by a JSON store handler (%q)", c.path, body)
			}
		} else if !strings.Contains(body, c.wantBody) {
			t.Errorf("GET %s -> %q, want a response containing %q", c.path, body, c.wantBody)
		}
	}
}
