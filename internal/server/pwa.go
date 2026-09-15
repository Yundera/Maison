package server

// The PWA surface: the web app manifest, the service worker, and the page the
// worker shows when the box cannot be reached.
//
// All three are served from here rather than shipped as files in web/public/ for
// the same reason, and it is not a style preference. spaHandler answers an
// unknown path with index.html and HTTP 200 (see spa.go) — there is no 404. A
// manifest or a worker that went missing from the embedded tree would therefore
// be served as an HTML document with a success status, and the symptoms are
// uniformly baffling: the manifest "fails to parse", the worker registration
// fails with a MIME error, and neither leaves anything in a log anyone reads. A
// route in the binary either exists or does not compile.
//
// They also have to sit OUTSIDE the onboarding gate, which is why they are
// registered ahead of the SPA catch-all rather than inside it. None of them is a
// navigation, so the gate would pass them today on its own — but /offline.html is
// fetched and CACHED by the worker at install time, and the Cache API ignores
// Cache-Control: no-store. If that one fetch could ever come back as the setup
// interstitial, every browser that installed the worker would show a stale setup
// page, from cache, on every network hiccup, with no way to clear it. Being
// outside the gate is a property of the route table rather than of isNavigation's
// current definition.
//
// ── IF THIS FEATURE IS EVER REMOVED, READ THIS FIRST ────────────────────────────
// Do not simply delete the /sw.js route. Because the catch-all has no 404, the
// route's absence is answered with index.html and a 200 — and a 200 does not
// unregister anything. A browser that ever installed this worker would keep it,
// in control, forever. Replace the handler's body with the unregistering stub in
// swUnregisterJS below and leave the route in place for at least a year.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"strings"

	"github.com/yundera/maison/internal/brand"
)

// handleManifest serves the web app manifest.
//
// The explicit Content-Type is load-bearing. Go's builtin MIME table has no entry
// for .webmanifest, and the runtime image is Alpine without mailcap, so it has no
// /etc/mime.types either — a file served out of the embedded tree would be sniffed
// to text/plain in production while working perfectly on a developer's machine
// that does have mailcap installed. That divergence is the worst shape a bug can
// take, and this is one line.
func (s *Server) handleManifest(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json; charset=utf-8")
	// no-cache, not no-store: revalidate, so a changed icon set or name reaches an
	// already-installed app, but a repeat visit does not refetch it. Same reasoning
	// as the app-icon handler in apps.go.
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(manifestJSON())
}

// manifestJSON builds the manifest. Marshalled rather than templated: a brand
// name is a display string and may legitimately contain a quote or a backslash,
// which string substitution into JSON would turn into a parse error.
func manifestJSON() []byte {
	type icon struct {
		Src     string `json:"src"`
		Sizes   string `json:"sizes"`
		Type    string `json:"type"`
		Purpose string `json:"purpose,omitempty"`
	}
	type shortcut struct {
		Name  string `json:"name"`
		URL   string `json:"url"`
		Icons []icon `json:"icons,omitempty"`
	}
	tile := []icon{{Src: "/icons/icon-192.png", Sizes: "192x192", Type: "image/png"}}

	b, _ := json.MarshalIndent(struct {
		ID         string     `json:"id"`
		Name       string     `json:"name"`
		ShortName  string     `json:"short_name"`
		Desc       string     `json:"description"`
		StartURL   string     `json:"start_url"`
		Scope      string     `json:"scope"`
		Display    string     `json:"display"`
		Background string     `json:"background_color"`
		Theme      string     `json:"theme_color"`
		Launch     any        `json:"launch_handler"`
		Icons      []icon     `json:"icons"`
		Shortcuts  []shortcut `json:"shortcuts"`
	}{
		// Explicit, so that a later change to start_url cannot orphan every install
		// that already exists — identity is id, and it defaults to start_url.
		ID: "/",
		// brand.Name is the DISPLAY string, which is exactly what this is. Never
		// brand.Slug: its package doc forbids the identifier form in display use.
		Name:      brand.Name,
		ShortName: brand.Name,
		Desc:      "The dashboard for your server.",
		StartURL:  "/",
		Scope:     "/",
		Display:   "standalone",
		// Painted behind the icon on the splash screen, so it wants to match the
		// app's FIRST paint rather than its eventual look: app.css sets
		// body { background: #000 } and fades the wallpaper in over it. White here
		// would flash white and then drop to black.
		Background: "#000000",
		// Colours the Android status bar and the desktop title bar, so this one wants
		// what is actually at the top of the viewport — the TopBar, which is #fff.
		// Not --primary, which appears nowhere near the top edge.
		Theme: "#ffffff",
		// Chromium-only and ignored elsewhere, but it matters more here than usual:
		// the dashboard holds a WebSocket whose server-side sampling is gated on
		// having subscribers, so a second window means the box does every bit of
		// that work twice.
		Launch: map[string]string{"client_mode": "navigate-existing"},
		Icons: []icon{
			{Src: "/icons/icon.svg", Sizes: "any", Type: "image/svg+xml"},
			{Src: "/icons/icon-192.png", Sizes: "192x192", Type: "image/png"},
			{Src: "/icons/icon-512.png", Sizes: "512x512", Type: "image/png"},
			// Android crops an icon to the launcher's shape. Without a maskable
			// variant it pillarboxes the "any" icon inside a white circle instead.
			{Src: "/icons/icon-maskable-512.png", Sizes: "512x512", Type: "image/png", Purpose: "maskable"},
		},
		// Real routes — see web/src/lib/route.ts. They share the app icon rather than
		// shipping two more glyphs; Android draws a generic square for a shortcut with
		// no icon of its own.
		Shortcuts: []shortcut{
			{Name: "App store", URL: "/store", Icons: tile},
			{Name: "Settings", URL: "/settings/domain", Icons: tile},
		},
	}, "", "  ")
	return b
}

// handleServiceWorker serves the worker with its cache version substituted in.
func (s *Server) handleServiceWorker(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	// A registration's updateViaCache already defaults to bypassing the HTTP cache
	// for the top-level worker script — but that binds the browser, not the auth
	// gate or any proxy between it and this box. A worker pinned by an intermediary
	// is a release that never lands, so say it here too.
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = io.WriteString(w, strings.ReplaceAll(serviceWorkerJS, "__VERSION__", s.uiVersion))
}

// handleOffline serves the page the worker falls back to when the box is
// unreachable. It is the only document the worker ever stores.
func (s *Server) handleOffline(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = io.WriteString(w, strings.ReplaceAll(offlineHTML, "__BRAND__", escapeAttr(brand.Name)))
}

// immutableAssets serves Vite's content-hashed bundle.
//
// Registering it as its own route buys two things the catch-all cannot. The
// filename already contains a hash of the contents, so "immutable" is a fact
// rather than a bet and the browser can skip revalidation entirely — worth having
// because embed.FS reports a zero ModTime, so http.ServeContent emits neither an
// ETag nor a Last-Modified and every asset fetch today is a full 200.
//
// And it gives a missing chunk a real 404. Answered by the catch-all instead, a
// stale index.html asking for a bundle that no longer exists produces "Failed to
// load module script: expected a JavaScript module script but the server responded
// with a MIME type of text/html" — a message that reads like a build problem and
// is a routing one.
func immutableAssets(uiFS fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(uiFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := fs.Stat(uiFS, strings.TrimPrefix(r.URL.Path, "/")); err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		fileServer.ServeHTTP(w, r)
	})
}

// uiVersion fingerprints the entire embedded UI, naming the service worker's
// cache so that it changes exactly when the thing it caches changes.
//
// The whole tree, not just index.html. index.html names the content-hashed
// bundle, so hashing it alone would catch a code change — but the font, the
// images and the wallpapers live under web/public/, are NOT hashed by Vite, and
// are served to the worker cache-first. A release that swapped a wallpaper would
// leave the version unmoved and every existing install pinned to the old one.
//
// The Go-served documents that the worker also caches are folded in for the same
// reason. /offline.html is stored in that cache but does not live in uiFS, so a
// release that reworded it would otherwise leave the version unmoved and every
// installed browser showing the old text. serviceWorkerJS is included too: a
// browser notices a changed worker script by itself, but if the cache NAME did not
// move with it, activate would find nothing to drop and the stale entries would
// survive the update that was meant to replace them.
//
// fs.WalkDir walks in lexical order, so the result is stable across runs and
// across machines. About 1.7 MB of input, once, at boot.
func uiVersion(uiFS fs.FS) string {
	h := sha256.New()
	_, _ = io.WriteString(h, offlineHTML)
	_, _ = io.WriteString(h, serviceWorkerJS)
	_ = fs.WalkDir(uiFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable entry must not stop the walk
		}
		_, _ = io.WriteString(h, p+"\x00")
		f, err := uiFS.Open(p)
		if err != nil {
			return nil
		}
		defer func() { _ = f.Close() }()
		_, _ = io.Copy(h, f)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// swUnregisterJS is not served. It is the replacement body for
// handleServiceWorker if this feature is ever withdrawn — see the note at the top
// of this file for why deleting the route instead does not work.
const swUnregisterJS = `
self.addEventListener('install', function () { self.skipWaiting() })
self.addEventListener('activate', function (e) {
  e.waitUntil(
    self.registration.unregister()
      .then(function () { return self.clients.matchAll() })
      .then(function (cs) { cs.forEach(function (c) { c.navigate(c.url) }) })
  )
})
`

// serviceWorkerJS is the worker. __VERSION__ is substituted per request from the
// fingerprint of the embedded UI.
//
// Written without backticks so it can live in a Go raw string, following the same
// pattern as launchHTML and onboardingHTML.
const serviceWorkerJS = `/* ` + brand.Name + `'s service worker.
 *
 * ── THE ONE RULE ───────────────────────────────────────────────────────────────
 * It NEVER serves a cached document for a navigation, and NEVER stores one.
 *
 * internal/server/onboarding.go puts a gate in front of the dashboard: until the
 * deployment's first-run step is done, every navigation gets an interstitial
 * instead of the app, and that file's doc comment is explicit that there is no
 * path around it. A worker answering navigations from cache would BE that path.
 * Two ways it goes wrong, and this file is shaped so neither is possible:
 *
 *   - The Cache API ignores Cache-Control: no-store, so a worker that cached
 *     navigation responses would pin the interstitial and keep serving it long
 *     after setup finished, pointing at a stale setup URL, with no way out.
 *   - Navigation preload requests carry the browser's own Accept: text/html, so a
 *     preload response is the interstitial too. Preload is therefore deliberately
 *     never enabled; see the activate handler.
 *
 * The same argument covers the auth gate in front of this box: only the server
 * can know whether this browser still has a session.
 *
 * Nothing is lost by the rule. Maison is served BY the machine it manages, so a
 * cached shell with the box unreachable is an admin tool with no data and no
 * working action — which is the same reason app status is not cached. The speed
 * this worker buys is in the bundle, the font and the wallpaper, together well
 * over a megabyte that is currently refetched on every single load because
 * nothing in the embedded tree carries a validator. The document itself is 400
 * bytes.
 */

var VERSION = '__VERSION__'
var CACHE = 'maison-' + VERSION
var OFFLINE = '/offline.html'

/* The only thing precached, and the only document this worker will ever hold. It
 * is stored BY NAME at install time; it is never the response to a navigation. */
var PRECACHE = [OFFLINE]

/* An allowlist, not a denylist of /api. A denylist is one new top-level endpoint
 * away from silently caching live box state; anything not named here defaults to
 * untouched. Same reasoning as the hook-ABI allowlist in the Dockerfile. */
var CACHEABLE = ['/assets/', '/fonts/', '/img/', '/wallpapers/', '/icons/']

self.addEventListener('install', function (event) {
  /* No skipWaiting: with navigations network-only, a new release already reaches
   * the browser on the next load without the worker's help. Taking over early
   * would buy nothing and costs the usual mid-session-takeover bugs. */
  event.waitUntil(caches.open(CACHE).then(function (c) { return c.addAll(PRECACHE) }))
})

self.addEventListener('activate', function (event) {
  /* navigationPreload is deliberately NOT enabled here. It exists to overlap the
   * network with a cache lookup for navigations, and this worker never looks up a
   * navigation — enabling it would only add a second route by which the setup
   * interstitial could reach the cache. */
  event.waitUntil(dropOldCaches().then(function () { return self.clients.claim() }))
})

/* Prefix-scoped so it can never delete a cache belonging to something else. */
function dropOldCaches() {
  return caches.keys().then(function (keys) {
    return Promise.all(keys.map(function (k) {
      if (k.indexOf('maison-') === 0 && k !== CACHE) return caches.delete(k)
      return null
    }))
  })
}

self.addEventListener('fetch', function (event) {
  var req = event.request
  if (req.method !== 'GET') return
  if (req.mode === 'navigate') { event.respondWith(navigateOnly(req)); return }

  var url = new URL(req.url)
  /* App gateway hosts and the auth origin are separate origins with their own
   * gates and their own sessions. Never ours to answer. */
  if (url.origin !== self.location.origin) return
  if (!cacheablePath(url.pathname)) return

  event.respondWith(assetFirst(req))
})
/* Returning without calling respondWith is the "untouched" case: the browser then
 * behaves exactly as if no worker were installed. /api, /ws, /ping, /launch and
 * every route that does not exist yet land here. */

function cacheablePath(path) {
  for (var i = 0; i < CACHEABLE.length; i++) {
    if (path.indexOf(CACHEABLE[i]) === 0) return true
  }
  return false
}

/* Network-only, with a branded failure page. The cache is read ONLY in the catch,
 * and never written. Note what this means: a box that is up but answering with
 * the setup interstitial, a 500, or an auth redirect RESOLVES the fetch, so that
 * response is passed straight through untouched. Only a genuine network failure
 * reaches the offline page. */
function navigateOnly(req) {
  return fetch(req).catch(function () {
    return caches.match(OFFLINE).then(function (hit) { return hit || Response.error() })
  })
}

/* Cache-first with no revalidation: the cache name carries the UI version, so a
 * release busts the whole cache at once and a hit is by definition current. */
function assetFirst(req) {
  return caches.open(CACHE).then(function (cache) {
    return cache.match(req).then(function (hit) {
      if (hit) return hit
      /* Deliberately not caught: an asset that fails to load should fail exactly
       * as it does today, rather than being swallowed into a blank page. */
      return fetch(req).then(function (res) {
        if (cacheable(res)) cache.put(req, res.clone()).catch(function () {})
        return res
      })
    })
  })
}

/* The second, independent lock on what may be stored. */
function cacheable(res) {
  if (!res || res.status !== 200) return false
  /* Not opaque, and same-origin. */
  if (res.type !== 'basic') return false
  /* An expired session yields a redirect chain to the auth origin; res.redirected
   * is the reliable tell, and without this check a login page gets stored under an
   * asset URL and served until the version changes. */
  if (res.redirected) return false
  /* Redundant today — nothing in CACHEABLE serves HTML — and that is the point. If
   * a future edit ever routes a document through this path, it is still refused.
   * This is also what the SPA catch-all's 200-instead-of-404 would look like. */
  var ct = res.headers.get('Content-Type') || ''
  return ct.indexOf('text/html') === -1
}
`

// offlineHTML is what the worker shows when the box cannot be reached. Like the
// launch page and the onboarding interstitial it is self-contained — it has to
// render with the server that serves every other asset unreachable, so it cannot
// so much as link an icon. The mark is inlined as SVG.
//
// English only, matching its two siblings: it is served by Go and cannot reach
// the Svelte i18n bundle.
const offlineHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Can't reach your server</title>
<style>
  :root { color-scheme: light dark; }
  * { box-sizing: border-box; }
  body {
    margin: 0; min-height: 100vh; display: grid; place-items: center;
    font-family: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
    background: #0f1115; color: #e8ebf0;
  }
  @media (prefers-color-scheme: light) { body { background: #f3f5f8; color: #1b2330; } }
  .card { text-align: center; padding: 2rem; max-width: 24rem; width: 100%; }
  /* No radius or overflow here: the SVG below clips itself to the same corner
     proportion as the real icon. A radius on the wrapper is a second, different
     rounding on top of that, and it eats the outer edge of the amber circles. */
  .mark { width: 76px; height: 76px; margin: 0 auto 1.4rem; }
  h1 { font-size: 1.2rem; margin: 0 0 0.6rem; font-weight: 640; text-wrap: balance; }
  p { font-size: 0.92rem; line-height: 1.5; opacity: 0.75; margin: 0 0 0.6rem; }
  .host { font-weight: 600; opacity: 0.9; overflow-wrap: anywhere; }
  button {
    margin-top: 1.4rem; padding: 0.55rem 1.1rem; font: inherit; font-weight: 600;
    color: #fff; background: #2f6df6; border: 0; border-radius: 8px; cursor: pointer;
  }
  button:disabled { opacity: 0.6; cursor: default; }
</style>
</head>
<body>
  <div class="card">
    <div class="mark">
      <!-- The same geometry as web/brand/icon.svg, inlined because this page has to
           render with the server that would serve /icons/icon.svg unreachable. -->
      <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 192 192" width="76" height="76">
        <clipPath id="sq"><rect width="192" height="192" rx="36" ry="36"/></clipPath>
        <g clip-path="url(#sq)">
          <rect width="192" height="192" fill="#F8F6F5"/>
          <circle cx="54.7" cy="107.7" r="40.4" fill="#FFBD4B" opacity=".95"/>
          <circle cx="96" cy="92.9" r="55.4" fill="#C23800" opacity=".95"/>
          <circle cx="136.3" cy="107.7" r="40.4" fill="#FFBD4B" opacity=".95"/>
        </g>
      </svg>
    </div>
    <h1>Can't reach your server</h1>
    <p>__BRAND__ runs on your own machine, and this device cannot get to it right now.</p>
    <p>Check that the machine is powered on and connected, and that this device is
       on a network that can reach it.</p>
    <p class="host" id="host"></p>
    <button id="retry" type="button">Try again</button>
  </div>
<script>
  // The host is read here rather than injected by the server: the worker may serve
  // this same cached copy on any of the several hostnames a box answers on.
  document.getElementById('host').textContent = location.host
  var retry = document.getElementById('retry')
  function attempt() {
    retry.disabled = true
    // /ping rather than a blind reload, so a failed attempt does not flicker the
    // browser's own error page in and out.
    fetch('/ping', { cache: 'no-store' })
      .then(function (r) { if (r.ok) location.reload(); else retry.disabled = false })
      .catch(function () { retry.disabled = false })
  }
  retry.addEventListener('click', attempt)
  setInterval(attempt, 10000)
</script>
</body>
</html>
`
