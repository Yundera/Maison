package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/secrets"
	"github.com/yundera/maison/internal/usersettings"
)

const spaMarker = "<html>the dashboard</html>"

// checkToken is the bearer the test deployment's check expects.
const checkToken = "ob-token-123"

// fakeCheck is the deployment's onboarding check: it answers whatever the test last
// set, counts how often it was asked, and records the headers of the last request.
type fakeCheck struct {
	*httptest.Server
	hits atomic.Int32

	mu      sync.Mutex
	handler http.HandlerFunc
	auth    string
	accept  string
}

func newFakeCheck(t *testing.T) *fakeCheck {
	t.Helper()
	fc := &fakeCheck{}
	fc.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fc.hits.Add(1)
		fc.mu.Lock()
		fc.auth, fc.accept = r.Header.Get("Authorization"), r.Header.Get("Accept")
		h := fc.handler
		fc.mu.Unlock()
		if h == nil {
			writeJSON(w, http.StatusOK, map[string]any{"show": false})
			return
		}
		h(w, r)
	}))
	t.Cleanup(fc.Close)
	return fc
}

// answer makes the check answer {"show": show, "url": setupURL}.
func (fc *fakeCheck) answer(show bool, setupURL string) {
	fc.set(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"show": show, "url": setupURL})
	})
}

func (fc *fakeCheck) set(h http.HandlerFunc) {
	fc.mu.Lock()
	fc.handler = h
	fc.mu.Unlock()
}

func (fc *fakeCheck) headers() (auth, accept string) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.auth, fc.accept
}

// newGatedServer builds a server on a scratch data root, its onboarding setting
// pointed at checkURL with a secret-reference token — exactly what a deployment
// writes. An empty checkURL leaves the setting out: the standalone box. The SPA it
// serves is a marker, so a test can tell "the dashboard answered" by body alone.
func newGatedServer(t *testing.T, checkURL string) http.Handler {
	t.Helper()
	cfg := config.Config{DataRoot: t.TempDir()}
	if err := os.MkdirAll(cfg.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if checkURL != "" {
		sec := secrets.New(filepath.Join(cfg.StateDir(), secrets.FileName))
		if err := sec.Set("onboarding_token", checkToken); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(map[string]string{"check_url": checkURL, "token": secrets.Ref("onboarding_token")})
		st := usersettings.New(filepath.Join(cfg.StateDir(), "settings.json"))
		if err := st.Manage("onboarding", "yundera", raw, sec.Has); err != nil {
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

// TestGateIsInertWithoutTheSetting is the case every standalone Maison is in, and
// the one that must never regress: no onboarding setting, no behaviour change.
func TestGateIsInertWithoutTheSetting(t *testing.T) {
	rec := get(t, newGatedServer(t, ""), "/", navigation)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), spaMarker) {
		t.Errorf("with no onboarding setting the dashboard must be served; got %d %q", rec.Code, rec.Body.String())
	}
}

// TestGateRedirectsWhenTheCheckSaysShow covers the point of the feature: an owner who
// lands on the dashboard while setup is owed is sent to the deployment's page — and
// the check was asked the way the contract says, with the bearer token.
func TestGateRedirectsWhenTheCheckSaysShow(t *testing.T) {
	fc := newFakeCheck(t)
	fc.answer(true, "https://admin.box.example/")
	h := newGatedServer(t, fc.URL)

	for _, path := range []string{"/", "/some/deep/link", "/index.html"} {
		rec := get(t, h, path, navigation)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("GET %s: status %d, want 303 (body %q)", path, rec.Code, rec.Body.String())
		}
		loc, err := url.Parse(rec.Header().Get("Location"))
		if err != nil || loc.Scheme+"://"+loc.Host+loc.Path != "https://admin.box.example/" {
			t.Errorf("GET %s: Location %q, want the setup URL", path, rec.Header().Get("Location"))
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("GET %s: Cache-Control %q — a cached redirect would outlive the setup", path, got)
		}
	}
	auth, accept := fc.headers()
	if auth != "Bearer "+checkToken {
		t.Errorf("Authorization = %q, want the resolved bearer token", auth)
	}
	if accept != "application/json" {
		t.Errorf("Accept = %q, want application/json", accept)
	}
}

// TestGateIsNeverCached. The check is asked on every navigation: a
// show:true is never remembered, and neither is a show:false.
func TestGateIsNeverCached(t *testing.T) {
	fc := newFakeCheck(t)
	fc.answer(true, "https://admin.box.example/")
	h := newGatedServer(t, fc.URL)

	if rec := get(t, h, "/", navigation); rec.Code != http.StatusSeeOther {
		t.Fatalf("first navigation: %d, want 303", rec.Code)
	}
	fc.answer(false, "https://admin.box.example/")
	if rec := get(t, h, "/", navigation); !strings.Contains(rec.Body.String(), spaMarker) {
		t.Fatalf("after the check said no: got %d %q, want the dashboard", rec.Code, rec.Body.String())
	}
	fc.answer(true, "https://admin.box.example/")
	if rec := get(t, h, "/", navigation); rec.Code != http.StatusSeeOther {
		t.Fatalf("after the check said show again: %d, want 303", rec.Code)
	}
	if n := fc.hits.Load(); n != 3 {
		t.Errorf("the check was asked %d times for 3 navigations", n)
	}
}

// TestGateFailsOpen. Every one of these is a broken or absent answer, and redirecting
// on any of them would lock the owner out of their own dashboard behind a page that
// may not be there.
func TestGateFailsOpen(t *testing.T) {
	for _, c := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"server error", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}},
		{"unauthorized", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no", http.StatusUnauthorized)
		}},
		{"not json", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>login</html>"))
		}},
		{"show without a url", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"show": true})
		}},
		{"relative url", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"show": true, "url": "/setup"})
		}},
		// The answer becomes a Location on the dashboard's own origin.
		{"script scheme", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"show": true, "url": "javascript:alert(1)"})
		}},
		// A redirect is not followed: a login page in front of a misconfigured check is
		// not an answer.
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://login.example/", http.StatusFound)
		}},
		{"too slow", func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(3 * time.Second):
			case <-r.Context().Done():
			}
			writeJSON(w, http.StatusOK, map[string]any{"show": true, "url": "https://admin.box.example/"})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			fc := newFakeCheck(t)
			fc.set(c.h)
			h := newGatedServer(t, fc.URL)
			start := time.Now()
			rec := get(t, h, "/", navigation)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), spaMarker) {
				t.Errorf("got %d %q (Location %q), want the dashboard", rec.Code, rec.Body.String(), rec.Header().Get("Location"))
			}
			if d := time.Since(start); d > 2500*time.Millisecond {
				t.Errorf("the page waited %v for the check", d)
			}
		})
	}

	// And a check nobody is listening on.
	fc := newFakeCheck(t)
	dead := fc.URL
	fc.Close()
	if rec := get(t, newGatedServer(t, dead), "/", navigation); !strings.Contains(rec.Body.String(), spaMarker) {
		t.Errorf("unreachable check: got %d, want the dashboard", rec.Code)
	}
}

// TestGateLeavesEverythingElseAlone. Only browser navigations are intercepted — and
// the navigation test runs before the check is asked, so the API, the SPA's assets
// and POSTs never cost the deployment a request.
func TestGateLeavesEverythingElseAlone(t *testing.T) {
	fc := newFakeCheck(t)
	fc.answer(true, "https://admin.box.example/")
	h := newGatedServer(t, fc.URL)

	if body := get(t, h, "/ping", nil).Body.String(); !strings.Contains(body, `"ok"`) {
		t.Errorf("/ping went through the gate; got %q", body)
	}
	// An asset fetch carries no text/html in Accept, so it is not a navigation.
	if body := get(t, h, "/assets/app.js", map[string]string{"Accept": "*/*"}).Body.String(); !strings.Contains(body, "// bundle") {
		t.Errorf("an asset request was redirected; got %q", body)
	}
	// Nor is a fetch of the SPA without text/html.
	if rec := get(t, h, "/", map[string]string{"Accept": "application/json"}); rec.Code == http.StatusSeeOther {
		t.Error("a non-HTML request was redirected")
	}
	// POST is never a navigation, whatever it accepts.
	req := httptest.NewRequest(http.MethodPost, "/api/apps/x/start", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusSeeOther {
		t.Error("a POST was treated as a navigation")
	}
	if n := fc.hits.Load(); n != 0 {
		t.Errorf("the check was asked %d times for requests that are not navigations", n)
	}
}

// TestReturnCarriesTheHostTheOwnerUsed. A box answers on its domain and on the
// nip.io/sslip.io fallbacks at once; the far side has to send the browser back to
// the one actually in use, so the address is taken from the request and not from
// configuration.
func TestReturnCarriesTheHostTheOwnerUsed(t *testing.T) {
	fc := newFakeCheck(t)
	fc.answer(true, "https://admin.box.example/?step=1")
	h := newGatedServer(t, fc.URL)

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

			u, err := url.Parse(rec.Header().Get("Location"))
			if rec.Code != http.StatusSeeOther || err != nil {
				t.Fatalf("status %d, Location %q", rec.Code, rec.Header().Get("Location"))
			}
			if got := u.Query().Get("return"); got != c.want {
				t.Errorf("return = %q, want %q", got, c.want)
			}
			if u.Query().Get("step") != "1" || u.Scheme+"://"+u.Host+u.Path != "https://admin.box.example/" {
				t.Errorf("the setup URL was rewritten: %q", u)
			}
		})
	}
}
