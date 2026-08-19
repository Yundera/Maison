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

	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/domains"
)

// newDomainsServer builds a server on a scratch data root whose .env.app is the
// one below: a deployment with a domain, an IP, and a seeded admin password.
func newDomainsServer(t *testing.T) http.Handler {
	t.Helper()
	cfg := config.Config{DataRoot: t.TempDir()}
	if err := os.MkdirAll(cfg.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	env := "APP_NET=pcs\nAPP_DOMAIN=box.example.com\nAPP_PUBLIC_IP_DASH=10-0-0-1\nAPP_DEFAULT_PASSWORD=hunter2-hunter2\n"
	if err := os.WriteFile(filepath.Join(cfg.StateDir(), ".env.app"), []byte(env), 0o644); err != nil {
		t.Fatal(err)
	}
	return New(cfg, fstest.MapFS{})
}

func callDomains(t *testing.T, h http.Handler, method, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, "/api/settings/domains", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestDomainsViewCarriesThePrimaryDomain pins what the settings panel needs in
// order to say that a domain is *added*: the token an app's own route is written
// with, and what that token resolves to here. Without them the list reads as a
// domain switcher.
//
// It also pins what must NOT travel. .env.app carries the seeded admin password;
// the domains endpoint reports only the routing variables a host template can
// interpolate.
func TestDomainsViewCarriesThePrimaryDomain(t *testing.T) {
	h := newDomainsServer(t)

	code, body := callDomains(t, h, http.MethodGet, "")
	if code != http.StatusOK {
		t.Fatalf("GET -> %d %s", code, body)
	}

	var got struct {
		PrimaryToken string            `json:"primaryToken"`
		PrimaryHost  string            `json:"primaryHost"`
		Vars         map[string]string `json:"vars"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.PrimaryToken != "${APP_DOMAIN}" {
		t.Errorf("primaryToken = %q, want ${APP_DOMAIN}", got.PrimaryToken)
	}
	if got.PrimaryHost != "box.example.com" {
		t.Errorf("primaryHost = %q, want box.example.com", got.PrimaryHost)
	}
	if got.Vars["APP_PUBLIC_IP_DASH"] != "10-0-0-1" {
		t.Errorf("vars missing the routing family: %v", got.Vars)
	}
	for k := range got.Vars {
		if strings.Contains(strings.ToUpper(k), "PASSWORD") {
			t.Errorf("vars leaks %s out of .env.app", k)
		}
	}
	if strings.Contains(body, "hunter2") {
		t.Errorf("the seeded password reached the domains endpoint: %s", body)
	}
}

// TestPutDomainsCleansTheEditorsInput covers the two things the key/value
// directives editor can hand back that must not reach an app's override: a blank
// row someone opened and abandoned, and two entries fighting over one host.
func TestPutDomainsCleansTheEditorsInput(t *testing.T) {
	h := newDomainsServer(t)

	code, body := callDomains(t, h, http.MethodPut,
		`[{"name":"lan","domain":"lan.example.com","directives":{"import":" gateway_tls ","  ":"  "}}]`)
	if code != http.StatusOK {
		t.Fatalf("PUT -> %d %s", code, body)
	}
	var got struct {
		Domains []domains.Domain `json:"domains"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Domains) != 1 {
		t.Fatalf("domains = %v, want one entry", got.Domains)
	}
	if d := got.Domains[0].Directives; len(d) != 1 || d["import"] != "gateway_tls" {
		t.Errorf("directives = %v, want the blank row dropped and the value trimmed", d)
	}

	// A second entry on a host that is already listed would generate twice onto
	// the same route.
	code, body = callDomains(t, h, http.MethodPut,
		`[{"name":"a","domain":"x.example"},{"name":"b","domain":"x.example"}]`)
	if code != http.StatusBadRequest || !strings.Contains(body, "already in the list") {
		t.Errorf("duplicate host -> %d %s; want 400", code, body)
	}

	// The rejected save must not have replaced the accepted one.
	_, body = callDomains(t, h, http.MethodGet, "")
	if !strings.Contains(body, "lan.example.com") {
		t.Errorf("a rejected PUT changed the stored list: %s", body)
	}
}
