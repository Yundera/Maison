package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/yundera/maison/internal/config"
)

func newChangeServer(t *testing.T) http.Handler {
	t.Helper()
	cfg := config.Config{DataRoot: t.TempDir()}
	dir := filepath.Join(cfg.AppsDir(), "samba")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	base := `services: {}
x-compose-app:
  secrets:
    SMB_PASSWORD: password:24
  credentials:
    - label: SMB
      username: admin
      password: ${SMB_PASSWORD}
      change: {apply: env, policy: {min_length: 10}}
    - label: Initial
      username: admin
      password: ${SMB_PASSWORD}
`
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SMB_PASSWORD=old-password-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return New(cfg, fstest.MapFS{})
}

func TestGenerateCredentialEndpoint(t *testing.T) {
	h := newChangeServer(t)
	rec := callCreds(t, h, http.MethodPost, "/api/apps/samba/credentials/0/generate", "")
	if rec.Code == http.StatusServiceUnavailable {
		t.Skip("docker client unavailable in this environment")
	}
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("generate -> %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	var got struct{ Password string }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Password) != 24 {
		t.Fatalf("generate body %s (%v)", rec.Body, err)
	}

	if rec := callCreds(t, h, http.MethodPost, "/api/apps/samba/credentials/1/generate", ""); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("initial-only credential -> %d, want 422", rec.Code)
	}
	if rec := callCreds(t, h, http.MethodPost, "/api/apps/samba/credentials/7/generate", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown index -> %d, want 404", rec.Code)
	}
}

func TestChangeCredentialEndpointRefusesPolicy(t *testing.T) {
	h := newChangeServer(t)
	rec := callCreds(t, h, http.MethodPost, "/api/apps/samba/credentials/0/change", `{"password":"sh$rt"}`)
	if rec.Code == http.StatusServiceUnavailable {
		t.Skip("docker client unavailable in this environment")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("change -> %d %s, want 400", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.Contains(body, "sh$rt") || strings.Contains(body, "old-password-1") {
		t.Errorf("response leaks a value: %s", body)
	}
	if !strings.Contains(body, `"too_short"`) || !strings.Contains(body, `"forbidden"`) {
		t.Errorf("rules missing: %s", body)
	}
}
