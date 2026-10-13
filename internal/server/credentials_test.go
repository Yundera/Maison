package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/config"
)

// newCredentialsServer seeds one managed app whose x-compose-app declares a
// credential pointing at a value in its .env.
func newCredentialsServer(t *testing.T) http.Handler {
	t.Helper()
	cfg := config.Config{DataRoot: t.TempDir()}
	dir := filepath.Join(cfg.AppsDir(), "outline")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	base := "services: {}\nx-compose-app:\n  credentials:\n    - label: Administrator\n      username: admin\n      password: ${ADMIN_PASSWORD}\n"
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("ADMIN_PASSWORD=hunter2-hunter2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return New(cfg, fstest.MapFS{})
}

func callCreds(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestShowCredentialsRendersAndDismissRoundTrips(t *testing.T) {
	h := newCredentialsServer(t)

	rec := callCreds(t, h, http.MethodPost, "/api/apps/outline/credentials/show", "")
	if rec.Code == http.StatusServiceUnavailable {
		t.Skip("docker client unavailable in this environment")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("show -> %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("credentials must be served no-store")
	}
	var got struct {
		Credentials []apps.RenderedCredential `json:"credentials"`
		Dismissed   bool                      `json:"dismissed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Credentials) != 1 || got.Credentials[0].Password != "hunter2-hunter2" || got.Dismissed {
		t.Fatalf("show = %+v", got)
	}

	if rec := callCreds(t, h, http.MethodPut, "/api/apps/outline/credentials/dismissed", `{"dismissed":true}`); rec.Code != http.StatusOK {
		t.Fatalf("dismiss -> %d %s", rec.Code, rec.Body)
	}
	rec = callCreds(t, h, http.MethodPost, "/api/apps/outline/credentials/show", "")
	if !strings.Contains(rec.Body.String(), `"dismissed":true`) {
		t.Fatalf("dismissed not recorded: %s", rec.Body)
	}

	if rec := callCreds(t, h, http.MethodPost, "/api/apps/missing/credentials/show", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown app -> %d, want 404", rec.Code)
	}
}

// The tile list goes over the websocket and into the browser's localStorage: it
// may say an app HAS credentials, never what they are.
func TestAppListCarriesNoCredentialValues(t *testing.T) {
	h := newCredentialsServer(t)

	rec := callCreds(t, h, http.MethodGet, "/api/apps", "")
	if rec.Code == http.StatusServiceUnavailable {
		t.Skip("docker client unavailable in this environment")
	}
	body := rec.Body.String()
	if strings.Contains(body, "hunter2") {
		t.Fatalf("a credential value reached the app list: %s", body)
	}
	if !strings.Contains(body, `"credentials_pending":true`) {
		t.Fatalf("the list should flag pending credentials: %s", body)
	}
}
