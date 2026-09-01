package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/onboarding"
)

const spaMarker = "<html>the dashboard</html>"

// newGatedServer builds a server on a scratch data root, optionally armed with an
// onboarding file. The SPA it serves is a marker, so a test can tell "the
// dashboard answered" from "the interstitial answered" by body alone.
func newGatedServer(t *testing.T, setupURL string) http.Handler {
	t.Helper()
	cfg := config.Config{DataRoot: t.TempDir()}
	if err := os.MkdirAll(cfg.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if setupURL != "" {
		file := `{"url":"` + setupURL + `"}`
		if err := os.WriteFile(onboarding.Path(cfg), []byte(file), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return New(cfg, fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte(spaMarker)},
		"assets/app.js": &fstest.MapFile{Data: []byte("// bundle")},
	})
}

func get(t *testing.T, h http.Handler, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

var navigation = map[string]string{"Accept": "text/html,application/xhtml+xml,*/*;q=0.8"}

// TestGateIsInertWithoutTheFile is the case every standalone Maison is in, and
// the one that must never regress: no onboarding file, no behaviour change.
func TestGateIsInertWithoutTheFile(t *testing.T) {
	rec := get(t, newGatedServer(t, ""), "/", navigation)
	if !strings.Contains(rec.Body.String(), spaMarker) {
		t.Errorf("with no onboarding file the dashboard must be served; got %q", rec.Body.String())
	}
}

// TestGateInterceptsNavigations covers the point of the feature: an owner who
// lands on the dashboard while setup is still owed is sent to it instead.
func TestGateInterceptsNavigations(t *testing.T) {
	h := newGatedServer(t, "https://admin.box.example/")

	for _, path := range []string{"/", "/some/deep/link", "/index.html"} {
		body := get(t, h, path, navigation).Body.String()
		if strings.Contains(body, spaMarker) {
			t.Errorf("GET %s reached the dashboard around the gate", path)
		}
		if !strings.Contains(body, "Continue setup") {
			t.Errorf("GET %s did not render the interstitial; got %q", path, body)
		}
	}
}

// TestGateHasNoWayPast is the property that makes it a gate rather than a route.
// The failure it guards against is not an attacker — whoever is here is already
// authenticated by the gate in front of Maison — but an owner who dismisses the
// prompt once and stays on borrowed credentials forever.
func TestGateHasNoWayPast(t *testing.T) {
	body := strings.ToLower(get(t, newGatedServer(t, "https://admin.box.example/"), "/", navigation).Body.String())
	for _, escape := range []string{"skip", "later", "dismiss", "remind"} {
		if strings.Contains(body, escape) {
			t.Errorf("the interstitial offers a way past the gate: found %q", escape)
		}
	}
}

// TestGateLeavesEverythingElseAlone. Only browser navigations are intercepted:
// the API, the WebSocket and the SPA's own assets go through untouched, so the
// wrapper cannot break a request it was never meant to see.
func TestGateLeavesEverythingElseAlone(t *testing.T) {
	h := newGatedServer(t, "https://admin.box.example/")

	if body := get(t, h, "/ping", nil).Body.String(); !strings.Contains(body, `"ok"`) {
		t.Errorf("/ping went through the gate; got %q", body)
	}
	// An asset fetch carries no text/html in Accept, so it is not a navigation.
	if body := get(t, h, "/assets/app.js", map[string]string{"Accept": "*/*"}).Body.String(); !strings.Contains(body, "// bundle") {
		t.Errorf("an asset request got the interstitial; got %q", body)
	}
	// POST is never a navigation, whatever it accepts.
	req := httptest.NewRequest(http.MethodPost, "/api/apps/x/start", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "Continue setup") {
		t.Error("a POST was treated as a navigation")
	}
}

// TestReturnCarriesTheHostTheOwnerUsed. A box answers on its domain and on the
// nip.io/sslip.io fallbacks at once; the far side has to send the browser back to
// the one actually in use, so the address is taken from the request and not from
// configuration.
func TestReturnCarriesTheHostTheOwnerUsed(t *testing.T) {
	h := newGatedServer(t, "https://admin.box.example/")

	for _, c := range []struct {
		name    string
		headers map[string]string
		host    string
		want    string
	}{
		{
			name:    "forwarded by the gateway",
			headers: map[string]string{"Accept": "text/html", "X-Forwarded-Proto": "https"},
			host:    "maison.box.example",
			want:    "https://maison.box.example/",
		},
		{
			name:    "a fallback hostname",
			headers: map[string]string{"Accept": "text/html", "X-Forwarded-Proto": "https"},
			host:    "maison-10-0-0-1.sslip.io",
			want:    "https://maison-10-0-0-1.sslip.io/",
		},
		{
			name:    "only the first hop counts",
			headers: map[string]string{"Accept": "text/html", "X-Forwarded-Proto": "https, http"},
			host:    "maison.box.example",
			want:    "https://maison.box.example/",
		},
		{
			name:    "nothing forwarded",
			headers: map[string]string{"Accept": "text/html"},
			host:    "localhost:8080",
			want:    "http://localhost:8080/",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Host = c.host
			for k, v := range c.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			href := hrefFrom(t, rec.Body.String())
			u, err := url.Parse(href)
			if err != nil {
				t.Fatalf("unparseable href %q: %v", href, err)
			}
			if got := u.Query().Get("return"); got != c.want {
				t.Errorf("return = %q, want %q", got, c.want)
			}
			if u.Scheme+"://"+u.Host+u.Path != "https://admin.box.example/" {
				t.Errorf("the setup URL was rewritten: %q", href)
			}
		})
	}
}

// TestSetupURLIsEscapedIntoTheAttribute. The URL is rendered into an anchor, and
// it arrives via a file plus a request-supplied Host — neither of which this
// package writes.
func TestSetupURLIsEscapedIntoTheAttribute(t *testing.T) {
	h := newGatedServer(t, "https://admin.box.example/?a=1&b=2")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = `evil"><script>alert(1)</script>`
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("the Host header broke out of the href attribute")
	}
	if strings.Contains(body, "?a=1&b=2") {
		t.Error("the ampersand in the setup URL was not escaped for the attribute")
	}
}

// hrefFrom pulls the interstitial's single link out of the rendered page.
func hrefFrom(t *testing.T, body string) string {
	t.Helper()
	const marker = `class="btn" href="`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no setup link in the page: %q", body)
	}
	rest := body[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		t.Fatalf("unterminated href in the page: %q", body)
	}
	return strings.NewReplacer("&amp;", "&", "&quot;", `"`, "&#39;", "'", "&lt;", "<", "&gt;", ">").Replace(rest[:j])
}
