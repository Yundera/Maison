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
	"github.com/yundera/maison/internal/routing"
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

func callRouting(t *testing.T, h http.Handler, method, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

type routingBody struct {
	Setting   *routing.Setting  `json:"setting"`
	Effective routing.Setting   `json:"effective"`
	Vars      map[string]string `json:"vars"`
	Preview   struct {
		Override string            `json:"override"`
		Vars     map[string]string `json:"vars"`
	} `json:"preview"`
}

// TestRoutingViewCarriesTheEffectiveSettingAndAPreview pins what the editor needs:
// what the box runs on with nothing stored (labels on its own domain), and what
// that generates for a sample app, rendered by the real generator.
//
// It also pins what must NOT travel. .env.app carries the seeded admin password;
// the endpoint reports only the routing variables a host template can interpolate.
func TestRoutingViewCarriesTheEffectiveSettingAndAPreview(t *testing.T) {
	h := newDomainsServer(t)

	code, body := callRouting(t, h, http.MethodGet, "/api/settings/routing", "")
	if code != http.StatusOK {
		t.Fatalf("GET -> %d %s", code, body)
	}
	var got routingBody
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.Setting != nil || got.Effective.Renderer != routing.Labels {
		t.Errorf("setting = %+v, effective = %+v; want none stored, labels in effect", got.Setting, got.Effective)
	}
	if !strings.Contains(got.Preview.Override, "caddy_0: myapp-${APP_DOMAIN}") {
		t.Errorf("preview does not show the generated route:\n%s", got.Preview.Override)
	}
	if got.Preview.Vars["APP_URL"] != "https://myapp-box.example.com" {
		t.Errorf("preview APP_URL = %q", got.Preview.Vars["APP_URL"])
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
		t.Errorf("the seeded password reached the routing endpoint: %s", body)
	}
}

// TestPutRoutingValidatesAndResets covers a save the editor cleaned up, one it must
// refuse without replacing what is stored, and going back to the default.
func TestPutRoutingValidatesAndResets(t *testing.T) {
	h := newDomainsServer(t)

	code, body := callRouting(t, h, http.MethodPut, "/api/settings/routing",
		`{"renderer":"labels","domains":[{"name":"lan","domain":"lan.example.com","labels":{"import":" gateway_tls ","  ":"  "}}]}`)
	if code != http.StatusOK {
		t.Fatalf("PUT -> %d %s", code, body)
	}
	var got routingBody
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.Setting == nil || len(got.Setting.Domains) != 1 {
		t.Fatalf("setting = %+v", got.Setting)
	}
	if d := got.Setting.Domains[0]; d.Domain != "{name}-lan.example.com" || len(d.Labels) != 1 || d.Labels["import"] != "gateway_tls" {
		t.Errorf("domain = %+v, want the placeholder added, the blank row dropped and the value trimmed", d)
	}

	code, body = callRouting(t, h, http.MethodPut, "/api/settings/routing", `{"renderer":"labels","domains":[]}`)
	if code != http.StatusBadRequest {
		t.Errorf("a labels setting with no domain -> %d %s; want 400", code, body)
	}
	_, body = callRouting(t, h, http.MethodGet, "/api/settings/routing", "")
	if !strings.Contains(body, "lan.example.com") {
		t.Errorf("a rejected PUT changed the stored setting: %s", body)
	}

	code, body = callRouting(t, h, http.MethodPut, "/api/settings/routing", `null`)
	if code != http.StatusOK || !strings.Contains(body, `"setting":null`) {
		t.Errorf("reset -> %d %s", code, body)
	}

	// The preview of an unsaved draft does not store it.
	code, body = callRouting(t, h, http.MethodPost, "/api/settings/routing/preview", `{"renderer":"ports","url-host":"192.168.1.10"}`)
	if code != http.StatusOK || !strings.Contains(body, "APP_PORT_MYAPP") {
		t.Errorf("preview -> %d %s", code, body)
	}
	_, body = callRouting(t, h, http.MethodGet, "/api/settings/routing", "")
	if !strings.Contains(body, `"setting":null`) {
		t.Errorf("a preview stored its draft: %s", body)
	}
}
