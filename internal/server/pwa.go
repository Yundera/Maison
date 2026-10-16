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
// redirect, every browser that installed the worker would hold a stale bounce to a
// setup page, from cache, on every network hiccup, with no way to clear it. Being
// outside the gate is a property of the route table rather than of isNavigation's
// current definition — and it keeps those fetches from costing a call to the
// deployment's check.
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
// pattern as launchHTML.
const serviceWorkerJS = `/* ` + brand.Name + `'s service worker.
 *
 * ── THE ONE RULE ───────────────────────────────────────────────────────────────
 * It NEVER serves a cached document for a navigation, and NEVER stores one.
 *
 * internal/server/onboarding.go puts a gate in front of the dashboard: while the
 * deployment says a first-run step is owed, every navigation is redirected to its
 * setup page instead of the app, and that file's doc comment is explicit that there
 * is no path around it. A worker answering navigations from cache would BE that
 * path. Two ways it goes wrong, and this file is shaped so neither is possible:
 *
 *   - The Cache API ignores Cache-Control: no-store, so a worker that cached
 *     navigation responses would pin the setup redirect and keep replaying it long
 *     after setup finished, pointing at a stale setup URL, with no way out — or
 *     pin the dashboard and skip the step the deployment is still asking for.
 *   - Navigation preload requests carry the browser's own Accept: text/html, so a
 *     preload response is the redirect too. Preload is therefore deliberately
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
// launch page it is self-contained — it has to render with the server that serves
// every other asset unreachable, so it cannot so much as link an icon. The mark is
// inlined as SVG.
//
// English only, matching its sibling: it is served by Go and cannot reach the
// Svelte i18n bundle.
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
  /* The bare shell, no tile: it reads on both the dark and the light background. */
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
      <!-- web/brand/favicon.svg (the flat shell) plus a size, inlined because this page
           has to render with the server that would serve /icons/ unreachable. -->
      <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100" width="76" height="76"><path d="M37.59 59.55 L37.59 59.53 L37.58 59.51 L37.58 59.50 L37.57 59.48 L37.57 59.46 L37.57 59.44 L37.57 59.43 L37.56 59.41 L37.56 59.39 L37.56 59.37 L37.57 59.36 L37.57 59.34 L37.57 59.32 L37.57 59.30 L37.57 59.28 L37.58 59.27 L37.58 59.25 L37.59 59.23 L37.60 59.21 L37.60 59.19 L37.61 59.18 L37.62 59.16 L37.63 59.14 L37.64 59.12 L37.65 59.11 L37.66 59.09 L37.67 59.07 L37.68 59.06 L37.69 59.04 L37.71 59.03 L37.72 59.01 L37.73 59.00 L37.75 58.98 L37.77 58.97 L37.78 58.96 L37.80 58.95 L37.82 58.93 L37.83 58.92 L37.85 58.91 L37.87 58.90 L37.89 58.89 L37.91 58.88 L37.93 58.87 L37.95 58.87 L37.97 58.86 L37.99 58.85 L38.02 58.85 L38.04 58.84 L38.06 58.84 L38.08 58.84 L38.10 58.83 L38.13 58.83 L38.15 58.83 L38.17 58.83 L38.20 58.83 L38.22 58.83 L38.25 58.84 L38.27 58.84 L38.29 58.84 L38.32 58.85 L38.34 58.86 L38.36 58.86 L38.39 58.87 L38.41 58.88 L38.43 58.89 L38.46 58.90 L38.48 58.91 L38.50 58.92 L38.52 58.94 L38.54 58.95 L38.57 58.96 L38.59 58.98 L38.61 59.00 L38.63 59.01 L38.65 59.03 L38.67 59.05 L38.68 59.07 L38.70 59.09 L38.72 59.11 L38.73 59.13 L38.75 59.16 L38.77 59.18 L38.78 59.20 L38.79 59.23 L38.81 59.25 L38.82 59.28 L38.83 59.30 L38.84 59.33 L38.85 59.36 L38.86 59.39 L38.86 59.41 L38.87 59.44 L38.88 59.47 L38.88 59.50 L38.88 59.53 L38.89 59.56 L38.89 59.59 L38.89 59.62 L38.89 59.65 L38.88 59.68 L38.88 59.71 L38.88 59.75 L38.87 59.78 L38.86 59.81 L38.86 59.84 L38.85 59.87 L38.84 59.90 L38.83 59.93 L38.82 59.96 L38.80 59.99 L38.79 60.02 L38.77 60.05 L38.75 60.08 L38.74 60.11 L38.72 60.13 L38.70 60.16 L38.68 60.19 L38.65 60.21 L38.63 60.24 L38.61 60.26 L38.58 60.29 L38.55 60.31 L38.53 60.33 L38.50 60.36 L38.47 60.38 L38.44 60.40 L38.41 60.42 L38.38 60.43 L38.34 60.45 L38.31 60.47 L38.28 60.48 L38.24 60.49 L38.21 60.51 L38.17 60.52 L38.13 60.53 L38.10 60.54 L38.06 60.54 L38.02 60.55 L37.98 60.55 L37.94 60.56 L37.90 60.56 L37.86 60.56 L37.82 60.56 L37.78 60.56 L37.74 60.55 L37.70 60.55 L37.66 60.54 L37.62 60.53 L37.58 60.52 L37.54 60.51 L37.50 60.50 L37.46 60.48 L37.42 60.47 L37.38 60.45 L37.34 60.43 L37.30 60.41 L37.27 60.39 L37.23 60.37 L37.19 60.34 L37.16 60.32 L37.12 60.29 L37.09 60.26 L37.05 60.23 L37.02 60.20 L36.99 60.17 L36.96 60.13 L36.93 60.10 L36.90 60.06 L36.87 60.02 L36.85 59.98 L36.82 59.94 L36.80 59.90 L36.78 59.86 L36.76 59.81 L36.74 59.77 L36.72 59.72 L36.70 59.68 L36.69 59.63 L36.67 59.58 L36.66 59.53 L36.65 59.48 L36.65 59.44 L36.64 59.38 L36.63 59.33 L36.63 59.28 L36.63 59.23 L36.63 59.18 L36.63 59.13 L36.64 59.07 L36.65 59.02 L36.65 58.97 L36.66 58.91 L36.68 58.86 L36.69 58.81 L36.71 58.76 L36.73 58.71 L36.75 58.65 L36.77 58.60 L36.79 58.55 L36.82 58.50 L36.85 58.45 L36.88 58.40 L36.91 58.36 L36.94 58.31 L36.98 58.26 L37.01 58.22 L37.05 58.17 L37.09 58.13 L37.14 58.09 L37.18 58.05 L37.23 58.01 L37.27 57.97 L37.32 57.94 L37.37 57.90 L37.43 57.87 L37.48 57.84 L37.54 57.81 L37.59 57.78 L37.65 57.76 L37.71 57.73 L37.77 57.71 L37.83 57.69 L37.89 57.67 L37.96 57.66 L38.02 57.64 L38.09 57.63 L38.15 57.62 L38.22 57.62 L38.29 57.61 L38.35 57.61 L38.42 57.61 L38.49 57.62 L38.56 57.62 L38.63 57.63 L38.70 57.64 L38.77 57.65 L38.83 57.67 L38.90 57.69 L38.97 57.71 L39.04 57.73 L39.11 57.76 L39.17 57.78 L39.24 57.82 L39.31 57.85 L39.37 57.89 L39.43 57.92 L39.50 57.96 L39.56 58.01 L39.62 58.05 L39.68 58.10 L39.74 58.15 L39.79 58.21 L39.85 58.26 L39.90 58.32 L39.95 58.38 L40.00 58.44 L40.05 58.50 L40.09 58.57 L40.14 58.64 L40.18 58.71 L40.22 58.78 L40.25 58.85 L40.29 58.93 L40.32 59.01 L40.35 59.08 L40.37 59.16 L40.40 59.25 L40.42 59.33 L40.43 59.41 L40.45 59.50 L40.46 59.58 L40.47 59.67 L40.48 59.76 L40.48 59.84 L40.48 59.93 L40.48 60.02 L40.47 60.11 L40.46 60.20 L40.45 60.29 L40.43 60.38 L40.41 60.47 L40.39 60.56 L40.36 60.65 L40.33 60.74 L40.30 60.83 L40.26 60.92 L40.22 61.00 L40.18 61.09 L40.13 61.17 L40.08 61.26 L40.03 61.34 L39.97 61.42 L39.91 61.50 L39.85 61.58 L39.79 61.65 L39.72 61.73 L39.65 61.80 L39.57 61.87 L39.49 61.94 L39.41 62.00 L39.33 62.06 L39.24 62.12 L39.16 62.18 L39.07 62.23 L38.97 62.28 L38.88 62.33 L38.78 62.38 L38.68 62.42 L38.58 62.46 L38.47 62.49 L38.37 62.52 L38.26 62.55 L38.15 62.58 L38.04 62.60 L37.93 62.61 L37.81 62.63 L37.70 62.63 L37.59 62.64 L37.47 62.64 L37.35 62.64 L37.24 62.63 L37.12 62.62 L37.00 62.60 L36.88 62.58 L36.77 62.55 L36.65 62.52 L36.53 62.49 L36.42 62.45 L36.30 62.41 L36.19 62.36 L36.07 62.31 L35.96 62.26 L35.85 62.20 L35.74 62.13 L35.63 62.07 L35.53 61.99 L35.42 61.92 L35.32 61.84 L35.22 61.75 L35.12 61.66 L35.03 61.57 L34.94 61.47 L34.85 61.37 L34.76 61.27 L34.68 61.16 L34.60 61.05 L34.53 60.93 L34.46 60.82 L34.39 60.69 L34.33 60.57 L34.27 60.44 L34.21 60.31 L34.16 60.18 L34.11 60.04 L34.07 59.91 L34.03 59.77 L34.00 59.62 L33.97 59.48 L33.95 59.33 L33.93 59.19 L33.92 59.04 L33.92 58.89 L33.91 58.74 L33.92 58.59 L33.93 58.43 L33.94 58.28 L33.96 58.13 L33.99 57.97 L34.02 57.82 L34.06 57.67 L34.10 57.51 L34.15 57.36 L34.20 57.21 L34.26 57.06 L34.33 56.91 L34.40 56.76 L34.47 56.62 L34.56 56.48 L34.65 56.33 L34.74 56.20 L34.84 56.06 L34.94 55.93 L35.05 55.80 L35.17 55.67 L35.29 55.54 L35.41 55.42 L35.54 55.31 L35.68 55.20 L35.82 55.09 L35.96 54.98 L36.11 54.88 L36.27 54.79 L36.42 54.70 L36.58 54.62 L36.75 54.54 L36.92 54.46 L37.09 54.40 L37.27 54.34 L37.45 54.28 L37.63 54.23 L37.82 54.19 L38.00 54.15 L38.19 54.12 L38.39 54.09 L38.58 54.08 L38.77 54.07 L38.97 54.06 L39.17 54.07 L39.37 54.08 L39.57 54.09 L39.77 54.12 L39.97 54.15 L40.17 54.19 L40.37 54.24 L40.57 54.29 L40.77 54.36 L40.97 54.42 L41.16 54.50 L41.36 54.59 L41.55 54.68 L41.74 54.78 L41.93 54.88 L42.11 55.00 L42.30 55.12 L42.47 55.25 L42.65 55.38 L42.82 55.52 L42.99 55.67 L43.15 55.83 L43.31 55.99 L43.46 56.16 L43.61 56.33 L43.75 56.52 L43.89 56.70 L44.02 56.90 L44.14 57.10 L44.26 57.30 L44.37 57.51 L44.48 57.73 L44.57 57.95 L44.66 58.17 L44.74 58.40 L44.82 58.63 L44.89 58.87 L44.94 59.11 L44.99 59.36 L45.04 59.60 L45.07 59.85 L45.09 60.11 L45.11 60.36 L45.12 60.62 L45.11 60.88 L45.10 61.14 L45.08 61.40 L45.05 61.66 L45.01 61.92 L44.96 62.19 L44.90 62.45 L44.83 62.71 L44.75 62.97 L44.66 63.23 L44.56 63.48 L44.45 63.74 L44.34 63.99 L44.21 64.24 L44.07 64.48 L43.93 64.73 L43.77 64.97 L43.60 65.20 L43.43 65.43 L43.25 65.65 L43.05 65.87 L42.85 66.09 L42.64 66.29 L42.42 66.49 L42.19 66.69 L41.96 66.88 L41.71 67.06 L41.46 67.23 L41.20 67.39 L40.94 67.55 L40.66 67.69 L40.38 67.83 L40.10 67.96 L39.81 68.08 L39.51 68.19 L39.20 68.29 L38.89 68.38 L38.58 68.46 L38.26 68.52 L37.94 68.58 L37.61 68.63 L37.28 68.66 L36.95 68.68 L36.62 68.69 L36.28 68.69 L35.94 68.68 L35.60 68.65 L35.26 68.61 L34.91 68.56 L34.57 68.50 L34.23 68.43 L33.89 68.34 L33.55 68.24 L33.21 68.12 L32.88 68.00 L32.54 67.86 L32.21 67.71 L31.89 67.54 L31.57 67.37 L31.25 67.18 L30.94 66.98 L30.63 66.76 L30.33 66.54 L30.03 66.30 L29.75 66.05 L29.47 65.79 L29.19 65.51 L28.93 65.23 L28.67 64.94 L28.43 64.63 L28.19 64.31 L27.96 63.99 L27.75 63.65 L27.54 63.31 L27.35 62.95 L27.17 62.59 L26.99 62.21 L26.84 61.83 L26.69 61.45 L26.56 61.05 L26.44 60.65 L26.34 60.24 L26.24 59.82 L26.17 59.40 L26.11 58.98 L26.06 58.55 L26.03 58.11 L26.01 57.68 L26.01 57.24 L26.02 56.79 L26.05 56.35 L26.10 55.90 L26.16 55.46 L26.24 55.01 L26.34 54.56 L26.45 54.12 L26.58 53.67 L26.72 53.23 L26.88 52.79 L27.06 52.36 L27.26 51.93 L27.47 51.50 L27.69 51.08 L27.94 50.66 L28.20 50.25 L28.47 49.85 L28.77 49.46 L29.07 49.07 L29.40 48.69 L29.74 48.32 L30.09 47.97 L30.46 47.62 L30.84 47.28 L31.24 46.96 L31.65 46.65 L32.07 46.35 L32.51 46.06 L32.96 45.79 L33.42 45.54 L33.89 45.29 L34.37 45.07 L34.87 44.86 L35.37 44.67 L35.89 44.49 L36.41 44.33 L36.95 44.19 L37.49 44.07 L38.03 43.96 L38.59 43.88 L39.15 43.81 L39.71 43.77 L40.28 43.74 L40.86 43.74 L41.44 43.75 L42.02 43.79 L42.60 43.84 L43.18 43.92 L43.77 44.02 L44.35 44.14 L44.93 44.28 L45.51 44.45 L46.09 44.63 L46.66 44.84 L47.23 45.07 L47.80 45.32 L48.36 45.59 L48.91 45.88 L49.45 46.20 L49.99 46.53 L50.52 46.89 L51.03 47.27 L51.54 47.66 L52.04 48.08 L52.52 48.52 L52.99 48.98 L53.45 49.45 L53.89 49.95 L54.31 50.46 L54.72 51.00 L55.12 51.55 L55.49 52.11 L55.85 52.70 L56.19 53.30 L56.51 53.91 L56.81 54.54 L57.09 55.19 L57.34 55.84 L57.58 56.51 L57.79 57.19 L57.98 57.89 L58.14 58.59 L58.28 59.31 L58.40 60.03 L58.49 60.76 L58.55 61.50 L58.59 62.24 L58.60 62.99 L58.59 63.74 L58.55 64.50 L58.48 65.26 L58.38 66.02 L58.26 66.78 L58.11 67.55 L57.93 68.31 L57.72 69.07 L57.48 69.82 L57.22 70.57 L56.93 71.32 L56.60 72.06 L56.25 72.79 L55.88 73.51 L55.47 74.22 L55.04 74.93 L54.58 75.62 L54.09 76.30 L53.58 76.96 L53.04 77.61 L52.47 78.24 L51.88 78.86 L51.26 79.46 L50.61 80.04 L49.95 80.60 L49.26 81.14 L48.54 81.66 L47.80 82.16 L47.05 82.63 L46.27 83.07 L45.47 83.50 L44.65 83.89 L43.81 84.26 L42.96 84.60 L42.09 84.91 L41.20 85.19 L40.29 85.45 L39.38 85.67 L38.45 85.86 L37.51 86.01 L36.56 86.14 L35.59 86.23 L34.62 86.29 L33.65 86.31 L32.66 86.30 L31.67 86.25 L30.68 86.17 L29.69 86.05 L28.69 85.89 L27.70 85.70 L26.70 85.48 L25.71 85.21 L24.72 84.91 L23.74 84.57 L22.77 84.20 L21.80 83.78 L20.84 83.33 L19.90 82.85 L18.96 82.33 L18.04 81.77 L17.14 81.17 L16.25 80.54 L15.37 79.88 L14.52 79.18 L13.69 78.44 L12.88 77.67 L12.09 76.87 L11.33 76.04 L10.59 75.17 L9.88 74.28 L9.20 73.35 L8.55 72.39 L7.92 71.41 L7.33 70.40 L6.78 69.36 L6.25 68.29 L5.76 67.20 L5.31 66.09 L4.90 64.96 L4.52 63.80 L4.19 62.63 L3.89 61.43 L3.63 60.22 L3.42 59.00 L3.25 57.75 L3.12 56.50 L3.04 55.24 L3.00 53.96 L3.01 52.68 L3.06 51.39 L3.16 50.09 L3.30 48.79 L3.50 47.49 L3.74 46.19 L4.03 44.90 L4.36 43.60 L4.75 42.31 L5.18 41.03 L5.66 39.75 L6.19 38.48 L6.77 37.23 L7.39 35.99 L8.07 34.77 L8.79 33.56 L9.55 32.38 L10.37 31.21 L11.23 30.07 L12.13 28.95 L13.09 27.86 L14.08 26.79 L15.12 25.76 L16.20 24.76 L17.32 23.79 L18.48 22.85 L19.69 21.95 L20.93 21.09 L22.21 20.27 L23.52 19.49 L24.87 18.76 L26.26 18.06 L27.68 17.42 L29.12 16.82 L30.60 16.27 L32.10 15.77 L33.63 15.32 L35.19 14.92 L36.76 14.58 L38.36 14.29 L39.98 14.05 L41.61 13.87 L43.26 13.75 L44.93 13.69 L46.60 13.69 L48.28 13.75 L49.97 13.86 L51.67 14.04 L53.37 14.28 L55.07 14.59 L56.76 14.95 L58.46 15.38 L60.14 15.87 L61.82 16.42 L63.49 17.04 L65.14 17.72 L66.78 18.46 L68.41 19.26 L70.01 20.13 L71.59 21.06 L73.14 22.05 L74.67 23.10 L76.17 24.21 L77.64 25.39 L79.07 26.62 L80.47 27.90 L81.83 29.25 L83.14 30.65 L84.42 32.11 L85.65 33.61 L86.83 35.18 L87.97 36.79 L89.05 38.45 L90.08 40.16 L91.05 41.91 L91.96 43.71 L92.82 45.56 L93.61 47.44 L94.34 49.36 L95.01 51.32 L95.61 53.31 L96.14 55.33 L96.61 57.39 L97.00 59.47 Q77.66 66.43 58.31 59.47 Z" fill="#27AAE1"></path><path d="M67.23 18.67 L67.59 18.85 L67.95 19.03 L68.31 19.21 L68.67 19.40 L69.03 19.59 L69.38 19.78 L69.74 19.98 L70.09 20.18 L70.45 20.38 L70.80 20.59 L71.15 20.80 L71.50 21.01 L71.85 21.22 L72.20 21.44 L72.55 21.66 L72.89 21.88 L73.23 22.11 L73.58 22.34 L73.92 22.57 L74.26 22.81 L74.60 23.05 L74.93 23.29 L75.27 23.53 L75.60 23.78 L75.93 24.03 L76.26 24.29 L76.59 24.54 L76.92 24.80 L77.25 25.07 L77.57 25.33 L77.89 25.60 L78.21 25.87 L78.53 26.14 L78.85 26.42 L79.17 26.70 L79.48 26.98 L79.79 27.27 L80.10 27.56 L80.41 27.85 L80.72 28.14 L81.02 28.44 L81.32 28.74 L81.63 29.04 L81.92 29.35 L82.22 29.66 L82.51 29.97 L82.81 30.28 L83.10 30.60 L83.39 30.92 L83.67 31.24 L83.96 31.56 L84.24 31.89 L84.52 32.22 L84.79 32.55 L85.07 32.89 L85.34 33.23 L85.61 33.57 L85.88 33.91 L86.14 34.25 L86.41 34.60 L86.67 34.95 L86.93 35.31 L87.18 35.66 L87.44 36.02 L87.69 36.38 L87.93 36.74 L88.18 37.11 L88.42 37.48 L88.66 37.85 L88.90 38.22 L89.14 38.60 L89.37 38.97 L89.60 39.35 L89.83 39.74 L90.05 40.12 L90.27 40.51 L90.49 40.90 L90.71 41.29 L90.92 41.68 L91.13 42.08 L91.34 42.47 L91.55 42.87 L91.75 43.28 L91.95 43.68 L92.14 44.09 L92.34 44.50 L92.53 44.91 L92.71 45.32 L92.90 45.73 L93.08 46.15 L93.26 46.57 L93.43 46.99 L93.60 47.41 L93.77 47.84 L93.94 48.26 L94.10 48.69 L94.26 49.12 L94.41 49.55 L94.57 49.99 L94.72 50.42 L94.86 50.86 L95.01 51.30 L95.15 51.74 L95.28 52.18 L95.42 52.63 L95.54 53.07 L95.67 53.52 L95.79 53.97 L95.91 54.42 L96.03 54.87 L96.14 55.32 L96.25 55.78 L96.36 56.24 L96.46 56.69 L96.56 57.15 L96.65 57.61 L96.75 58.08 L96.83 58.54 L96.92 59.00 L97.00 59.47 Q77.66 66.43 58.31 59.47 L58.31 59.47 L58.28 59.31 L58.25 59.15 L58.22 58.99 L58.19 58.83 L58.16 58.67 L58.12 58.52 L58.09 58.36 L58.05 58.20 L58.02 58.05 L57.98 57.89 L57.94 57.73 L57.90 57.58 L57.85 57.42 L57.81 57.27 L57.77 57.12 L57.72 56.97 L57.67 56.81 L57.62 56.66 L57.58 56.51 L57.53 56.36 L57.47 56.21 L57.42 56.06 L57.37 55.91 L57.31 55.77 L57.26 55.62 L57.20 55.47 L57.14 55.33 L57.08 55.18 L57.02 55.04 L56.96 54.89 L56.90 54.75 L56.84 54.61 L56.77 54.47 L56.71 54.32 L56.64 54.18 L56.57 54.04 L56.51 53.91 L56.44 53.77 L56.37 53.63 L56.29 53.49 L56.22 53.36 L56.15 53.22 L56.07 53.09 L56.00 52.95 L55.92 52.82 L55.85 52.69 L55.77 52.56 L55.69 52.43 L55.61 52.30 L55.53 52.17 L55.45 52.04 L55.36 51.91 L55.28 51.79 L55.19 51.66 L55.11 51.54 L55.02 51.41 L54.94 51.29 L54.85 51.17 L54.76 51.04 L54.67 50.92 L54.58 50.80 L54.49 50.69 L54.40 50.57 L54.30 50.45 L54.21 50.34 L54.12 50.22 L54.02 50.11 L53.92 49.99 L53.83 49.88 L53.73 49.77 L53.63 49.66 L53.53 49.55 L53.43 49.44 L53.33 49.33 L53.23 49.22 L53.13 49.12 L53.03 49.01 L52.92 48.91 L52.82 48.81 L52.71 48.70 L52.61 48.60 L52.50 48.50 L52.40 48.40 L52.29 48.31 L52.18 48.21 L52.07 48.11 L51.96 48.02 L51.85 47.92 L51.74 47.83 L51.63 47.74 L51.52 47.65 L51.41 47.56 L51.30 47.47 L51.18 47.38 L51.07 47.29 L50.96 47.21 L50.84 47.12 L50.73 47.04 L50.61 46.95 L50.49 46.87 L50.38 46.79 L50.26 46.71 L50.14 46.63 L50.02 46.55 L49.91 46.48 L49.79 46.40 L49.67 46.33 L49.55 46.25 L49.43 46.18 L49.31 46.11 L49.19 46.04 L49.06 45.97 L48.94 45.90 L48.82 45.83 L48.70 45.77 L48.57 45.70 L48.45 45.64 L48.33 45.57 L48.20 45.51 L48.08 45.45 Q51.76 27.85 67.23 18.67 Z" fill="#EE2A7B"></path></svg>
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

// escapeAttr escapes a value for a double-quoted HTML attribute.
func escapeAttr(s string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	).Replace(s)
}
