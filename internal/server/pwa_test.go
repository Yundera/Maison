package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/yundera/maison/internal/brand"
	"github.com/yundera/maison/internal/config"
)

// TestPWARoutesBeatTheSPACatchAll pins the reason these three are routes in the
// binary rather than files under web/public.
//
// spaHandler answers an unknown path with index.html and HTTP 200 — there is no
// 404 anywhere in it. So the symptom of losing one of these is not a missing file:
// it is a service worker that is silently an HTML document, and a manifest that
// "fails to parse" with nothing in any log. Worse, because a 200 is not a 404, a
// browser that already registered the worker does NOT unregister it — it keeps the
// one it has, in control, indefinitely.
func TestPWARoutesBeatTheSPACatchAll(t *testing.T) {
	h := newGatedServer(t, "")
	for _, c := range []struct{ path, wantType string }{
		{"/sw.js", "text/javascript"},
		{"/manifest.webmanifest", "application/manifest+json"},
		{"/offline.html", "text/html"},
	} {
		rec := get(t, h, c.path, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200", c.path, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, c.wantType) {
			t.Errorf("%s: Content-Type %q, want %q", c.path, got, c.wantType)
		}
		if strings.Contains(rec.Body.String(), spaMarker) {
			t.Errorf("%s: answered by the SPA fallback, not its own handler", c.path)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s: Cache-Control %q, want no-cache", c.path, got)
		}
	}
}

// TestPWARoutesAnswerHEAD.
//
// chi matches a method exactly, so an unregistered HEAD is not "GET without a
// body" — it misses the route entirely and lands on the catch-all, which answers
// index.html with a 200. A probe or a proxy issuing HEAD /manifest.webmanifest
// would be told Content-Type: text/html and see a success status, which is the
// same silently-wrong answer the routes exist to prevent. Caught by hand with
// `curl -I` during implementation, where every one of the three looked like HTML.
func TestPWARoutesAnswerHEAD(t *testing.T) {
	h := newGatedServer(t, "")
	for _, c := range []struct{ path, wantType string }{
		{"/sw.js", "text/javascript"},
		{"/manifest.webmanifest", "application/manifest+json"},
		{"/offline.html", "text/html"},
	} {
		req := httptest.NewRequest(http.MethodHead, c.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("HEAD %s: status %d, want 200", c.path, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, c.wantType) {
			t.Errorf("HEAD %s: Content-Type %q, want %q — answered by the SPA fallback?", c.path, got, c.wantType)
		}
	}
}

// TestThePWASurfaceIsNotBehindTheOnboardingGate is the most important test here.
//
// The worker fetches /offline.html at install time and stores it, and the Cache API
// ignores Cache-Control: no-store. If that one fetch could ever come back as the
// setup interstitial, every browser that installed the worker would show a stale
// setup page — from cache, pointing at a setup URL that may no longer exist — on
// every network hiccup, with no way to clear it.
//
// The gate passes these today because it only intercepts requests carrying
// text/html in Accept, and none of them normally does. This test asks with the
// browser's own navigation Accept header anyway, so that the property survives any
// future widening of isNavigation.
func TestThePWASurfaceIsNotBehindTheOnboardingGate(t *testing.T) {
	h := newGatedServer(t, "https://admin.box.example/setup")
	for _, path := range []string{"/sw.js", "/manifest.webmanifest", "/offline.html", "/assets/app.js"} {
		rec := get(t, h, path, navigation)
		if strings.Contains(rec.Body.String(), "admin.box.example") {
			t.Errorf("%s: got the onboarding interstitial", path)
		}
	}
	// And the gate is still doing its job for an actual navigation.
	if rec := get(t, h, "/", navigation); !strings.Contains(rec.Body.String(), "admin.box.example") {
		t.Error("/: expected the interstitial, got the dashboard")
	}
}

// TestServiceWorkerCarriesTheUIVersion covers the cache-busting story.
//
// If the version never moves, activate never drops the previous cache, and a
// release that changed a wallpaper or the font never reaches a browser holding the
// old one. The two trees here differ ONLY in a wallpaper — not in index.html — to
// pin that the whole embedded tree is fingerprinted: those files live under
// web/public, are not content-hashed by Vite, and are served to the worker
// cache-first, so hashing index.html alone would miss them.
func TestServiceWorkerCarriesTheUIVersion(t *testing.T) {
	build := func(wallpaper string) http.Handler {
		return New(config.Config{DataRoot: t.TempDir()}, fstest.MapFS{
			"index.html":             &fstest.MapFile{Data: []byte(spaMarker)},
			"assets/app.js":          &fstest.MapFile{Data: []byte("// bundle")},
			"wallpapers/default.jpg": &fstest.MapFile{Data: []byte(wallpaper)},
		})
	}
	first := get(t, build("one"), "/sw.js", nil).Body.String()
	second := get(t, build("two"), "/sw.js", nil).Body.String()

	if strings.Contains(first, "__VERSION__") {
		t.Error("/sw.js: placeholder not substituted")
	}
	if first == second {
		t.Error("/sw.js: identical for two different UI trees — the cache would never be dropped")
	}
}

// TestUIVersionCoversTheGoServedOfflinePage.
//
// /offline.html is stored in the worker's cache but does not live in uiFS, so a
// version derived from uiFS alone would not move when its wording changed, and
// every installed browser would keep showing the old text. Caught while testing by
// hand: a reworded offline page did not appear after a restart, because the cache
// name had not changed and activate had nothing to drop.
func TestUIVersionCoversTheGoServedOfflinePage(t *testing.T) {
	ui := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte(spaMarker)}}

	// The same walk uiVersion does, but over the tree ALONE. If the result matches,
	// the Go-served documents are not being folded in.
	h := sha256.New()
	_ = fs.WalkDir(ui, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		_, _ = io.WriteString(h, p+"\x00")
		f, _ := ui.Open(p)
		defer func() { _ = f.Close() }()
		_, _ = io.Copy(h, f)
		return nil
	})
	treeOnly := hex.EncodeToString(h.Sum(nil))[:12]

	if uiVersion(ui) == treeOnly {
		t.Error("uiVersion hashes only the UI tree: a reworded offline page would never reach an installed browser")
	}
	if uiVersion(ui) != uiVersion(ui) {
		t.Error("uiVersion is not deterministic")
	}
}

// TestServiceWorkerDoesNotPrecacheTheAppShell asserts on source text rather than
// behaviour, and that is worth saying out loud: the behaviour it guards can only be
// observed in a browser, so this is the cheap check that runs in CI and the real one
// lives in the manual pass.
//
// What it guards is not small. Precaching "/" or "/index.html", or enabling
// navigation preload, would each let the worker answer a navigation from cache —
// and the onboarding gate's whole contract (see onboarding.go) is that no path
// reaches the dashboard around it.
func TestServiceWorkerDoesNotPrecacheTheAppShell(t *testing.T) {
	if !strings.Contains(serviceWorkerJS, "var PRECACHE = [OFFLINE]") {
		t.Error("the precache list is no longer exactly the offline page")
	}
	if strings.Contains(serviceWorkerJS, "navigationPreload.enable") {
		t.Error("navigation preload enabled: preload requests carry Accept: text/html, so the response is the interstitial")
	}
}

// TestAssetsAreImmutableAndMissingOnesAre404.
//
// A missing hashed chunk answered by the catch-all surfaces as "Failed to load
// module script: expected a JavaScript module script but the server responded with
// a MIME type of text/html" — a message that reads like a build problem and is a
// routing one.
func TestAssetsAreImmutableAndMissingOnesAre404(t *testing.T) {
	h := newGatedServer(t, "")

	rec := get(t, h, "/assets/app.js", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("/assets/app.js: status %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("/assets/app.js: Cache-Control %q, want immutable", got)
	}

	missing := get(t, h, "/assets/index-DEADBEEF.js", nil)
	if missing.Code != http.StatusNotFound {
		t.Errorf("missing chunk: status %d, want 404", missing.Code)
	}
	if strings.Contains(missing.Body.String(), spaMarker) {
		t.Error("missing chunk: answered with index.html")
	}
}

// TestIndexIsNotCached — index.html names the content-hashed bundle, so a browser
// holding yesterday's copy asks for asset URLs that no longer exist and renders a
// blank page.
func TestIndexIsNotCached(t *testing.T) {
	rec := get(t, newGatedServer(t, ""), "/", nil)
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("/: Cache-Control %q, want no-cache", got)
	}
}

// TestManifestIsServedAsAManifest.
//
// The Content-Type is the whole point of serving this from Go: the builtin MIME
// table has no .webmanifest entry and the Alpine runtime image ships no
// /etc/mime.types, so a file served out of the embedded tree sniffs to text/plain
// in production while working on a developer's machine that has mailcap installed.
func TestManifestIsServedAsAManifest(t *testing.T) {
	rec := get(t, newGatedServer(t, ""), "/manifest.webmanifest", nil)

	var m struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		StartURL string `json:"start_url"`
		Scope    string `json:"scope"`
		Display  string `json:"display"`
		Icons    []struct {
			Src     string `json:"src"`
			Sizes   string `json:"sizes"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("manifest does not parse as JSON: %v", err)
	}
	if m.Name != brand.Name {
		t.Errorf("name = %q, want %q", m.Name, brand.Name)
	}
	for _, c := range []struct{ got, want, field string }{
		{m.ID, "/", "id"},
		{m.StartURL, "/", "start_url"},
		{m.Scope, "/", "scope"},
		{m.Display, "standalone", "display"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
		}
	}

	// Installability needs a 512 icon, and Android pillarboxes the "any" icon inside
	// a white circle unless a maskable one is offered.
	var has512, hasMaskable bool
	for _, i := range m.Icons {
		if i.Sizes == "512x512" {
			has512 = true
		}
		if i.Purpose == "maskable" {
			hasMaskable = true
		}
	}
	if !has512 {
		t.Error("no 512x512 icon: the browser will refuse to install")
	}
	if !hasMaskable {
		t.Error("no maskable icon")
	}
}
