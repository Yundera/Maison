package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// The launch page is the single screen a user sees between clicking a tile and
// the app opening. It owns the entire wait — starting the stack, polling for
// readiness, and, when something is wrong, explaining it with an action instead
// of a browser error page. It never navigates to the app until the backend probe
// confirms a real response, so a raw connection error or 502 can only appear
// behind a deliberate "Open anyway".
//
// One template, two modes (chosen by the injected boot config):
//
//   - "dashboard": served at /launch?app=<id> on Maison's own origin. Polls
//     /api/apps/<id>/reachable (a server-side probe — the browser can't read a
//     cross-origin app's status) and, when ready, redirects to the app URL. This
//     is how port-published apps launch, and any app opened from a tile.
//
//   - "gate": served by the host-based catch-all when a request lands on a down
//     app's own gateway host (see rootHandler). Polls /__maison/reachable and,
//     when ready, reloads "/" — same origin, now the real app.
//
// Both share the phase machine and copy below; only the endpoints and the
// redirect target differ. The phases come from apps.Reach.

// launchBoot is injected into the page as window.__CDL. It is the only thing that
// differs between the two modes.
type launchBoot struct {
	Mode      string `json:"mode"`          // "dashboard" | "gate"
	App       string `json:"app,omitempty"` // app id (dashboard mode; gate resolves by host)
	Reachable string `json:"reachable"`     // poll endpoint
	Start     string `json:"start"`         // start action endpoint
	// Dashboard is the base URL for the settings/logs deep-links. A pointer so the
	// three cases stay distinct: nil (omitted) hides the deep-link buttons, ""
	// means same origin (dashboard mode), a URL points at the dashboard host
	// (gate mode, when a domain is configured).
	Dashboard *string `json:"dashboard,omitempty"`
}

// handleLaunch serves the dashboard-origin launch page for /launch?app=<id>.
func (s *Server) handleLaunch(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("app"))
	if id == "" || s.apps == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	sameOrigin := "" // deep-links go to our own origin
	writeLaunchPage(w, launchBoot{
		Mode:      "dashboard",
		App:       id,
		Reachable: "/api/apps/" + id + "/reachable",
		Start:     "/api/apps/" + id + "/start",
		Dashboard: &sameOrigin,
	})
}

// handleReachable is the dashboard-origin probe: resolve the app by id and report
// whether it can be opened yet.
func (s *Server) handleReachable(w http.ResponseWriter, r *http.Request) {
	if s.apps == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "docker unavailable"})
		return
	}
	id := chi.URLParam(r, "id")
	app, ok := s.apps.Get(r.Context(), id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown app"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.apps.Reach(ctx, app))
}

// writeLaunchPage renders the shared template with boot injected.
func writeLaunchPage(w http.ResponseWriter, boot launchBoot) {
	blob, _ := json.Marshal(boot)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(strings.Replace(launchHTML, "/*__BOOT__*/", "window.__CDL="+string(blob), 1)))
}

// launchHTML is the self-contained launch page. It carries no external assets so
// it keeps working while an app host flips from Maison's catch-all to the real
// app under it. The copy and behaviour per phase live entirely in the script.
const launchHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Starting…</title>
<style>
  :root { color-scheme: light dark; }
  * { box-sizing: border-box; }
  body {
    margin: 0; min-height: 100vh; display: grid; place-items: center;
    font-family: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
    background: #0f1115; color: #e8ebf0;
  }
  @media (prefers-color-scheme: light) { body { background: #f3f5f8; color: #1b2330; } }
  .card { text-align: center; padding: 2rem; max-width: 23rem; width: 100%; }
  .icon {
    width: 84px; height: 84px; margin: 0 auto 1.25rem; border-radius: 20px;
    display: grid; place-items: center; overflow: hidden;
    background: #2f6df6; color: #fff; font-size: 2.3rem; font-weight: 600;
    box-shadow: 0 8px 24px rgba(0,0,0,0.35);
  }
  .icon img { width: 100%; height: 100%; object-fit: cover; }
  h1 { font-size: 1.2rem; margin: 0 0 0.4rem; font-weight: 640; text-wrap: balance; }
  .status { font-size: 0.92rem; opacity: 0.72; min-height: 1.3em; }
  .status.err { color: #ff6b6f; opacity: 1; }
  .spinner {
    width: 34px; height: 34px; margin: 1.5rem auto 0;
    border: 3px solid rgba(128,128,128,0.25); border-top-color: #2f6df6;
    border-radius: 50%; animation: spin 0.8s linear infinite;
  }
  @keyframes spin { to { transform: rotate(360deg); } }
  @media (prefers-reduced-motion: reduce) { .spinner { animation-duration: 2s; } }
  .mark { width: 42px; height: 42px; margin: 1.4rem auto 0; border-radius: 50%;
    display: grid; place-items: center; font-size: 1.4rem; font-weight: 800; }
  .mark.ok { background: #16a34a; color: #04140a; }
  .mark.crit { background: rgba(255,107,111,0.16); color: #ff6b6f; }
  .mark.warn { background: rgba(251,191,36,0.16); color: #fbbf24; }
  .actions { display: flex; flex-wrap: wrap; gap: 0.55rem; justify-content: center; margin: 1.4rem 0 0; }
  .btn { padding: 0.55rem 0.95rem; border-radius: 9px; font: inherit; font-size: 0.86rem;
    font-weight: 600; cursor: pointer; border: 1px solid transparent; }
  .btn.primary { background: #2f6df6; color: #fff; }
  .btn.ghost { background: rgba(232,235,240,0.08); color: inherit; border-color: rgba(232,235,240,0.14); }
  @media (prefers-color-scheme: light) { .btn.ghost { background: rgba(20,30,50,0.05); border-color: rgba(20,30,50,0.12); } }
  .btn:focus-visible { outline: 2px solid #2f6df6; outline-offset: 2px; }
  .late { margin-top: 1.2rem; font-size: 0.85rem; opacity: 0; transition: opacity 0.3s; }
  .late.show { opacity: 0.85; }
  .late a { color: #2f6df6; cursor: pointer; text-decoration: underline; text-underline-offset: 3px; }
  .late .note { display: block; margin-top: 0.35rem; font-size: 0.76rem; opacity: 0.7; }
  .bg { display: inline-flex; align-items: center; gap: 0.5rem; margin: 1.1rem auto 0;
    padding: 0.3rem 0.75rem; border-radius: 999px; font-size: 0.75rem; opacity: 0.7;
    border: 1px solid rgba(232,235,240,0.14); }
  .bg .pulse { width: 7px; height: 7px; border-radius: 50%; background: #2f6df6; animation: pulse 1.4s ease-in-out infinite; }
  @keyframes pulse { 0%,100% { opacity: .35; } 50% { opacity: 1; } }
</style>
</head>
<body>
  <div class="card">
    <div class="icon" id="icon"><span id="letter">·</span></div>
    <h1 id="head">Starting your app…</h1>
    <div class="status" id="status">Waking it up…</div>
    <div id="mark"></div>
    <div id="actions" class="actions"></div>
    <div class="late" id="late"><a id="anyway">Open anyway →</a><span class="note">The app may not have finished loading.</span></div>
    <div class="bg" id="bg" style="display:none"><span class="pulse"></span>Still checking in the background</div>
  </div>
<script>
/*__BOOT__*/;
(function () {
  var boot = window.__CDL || {};
  var $ = function (id) { return document.getElementById(id); };
  var started = Date.now();
  var app = { id: boot.app || '', name: '', icon: '' };
  var redirectURL = boot.mode === 'gate' ? '/' : '';
  var startKicked = false, done = false, timer = null;

  function esc(s) { var d = document.createElement('div'); d.textContent = s == null ? '' : s; return d.innerHTML; }

  function setIcon(a) {
    if (a.icon) {
      var img = new Image();
      img.onload = function () { $('icon').innerHTML = ''; $('icon').appendChild(img); };
      img.src = a.icon;
    } else {
      $('letter').textContent = (a.name || '?').charAt(0).toUpperCase();
    }
  }

  // Deep-link back to the dashboard's settings/logs for an app. In dashboard mode
  // the base is our own origin; in gate mode it is the configured dashboard host,
  // and when that is unknown the deep-link buttons are simply omitted.
  function deepLink(panel) {
    if (boot.dashboard == null) return null;
    return boot.dashboard + '/?app=' + encodeURIComponent(app.id) + '&panel=' + panel;
  }

  // Redirect target for a port-only app, built browser-side because only the
  // browser knows which host it reached Maison on. Gateway apps carry a full url.
  function targetFrom(res) {
    if (boot.mode === 'gate') return '/';
    if (res.url) return res.url;
    if (res.port) return (res.scheme || 'http') + '://' + location.hostname + ':' + res.port + (res.index || '');
    return '/';
  }

  function goTo(url) { if (done) return; done = true; if (timer) clearTimeout(timer); location.replace(url); }

  var COPY = {
    starting:     function () { return { head: 'Starting ' + app.name + '…', sub: 'Waking it up…', spin: true }; },
    initializing: function () { return { head: 'Setting ' + app.name + ' up…', sub: 'First launch can take a few minutes.', spin: true }; },
    waiting:      function () { return { head: 'Almost ready…', sub: 'Waiting for ' + app.name + ' to respond…', spin: true }; },
    tls:          function () { return { head: 'Securing the connection…', sub: 'The certificate is still being issued — this clears on its own.', spin: true }; },
    ready:        function () { return { head: 'Ready — opening…', sub: 'Taking you to ' + app.name + '.', mark: 'ok', markGlyph: '✓' }; },
    bad_gateway:  function () { return {
      head: app.name + ' isn’t responding',
      sub: 'The app started, but its web service isn’t answering. If this keeps up, check its logs or reinstall it.',
      mark: 'crit', markGlyph: '!', keepBg: true,
      actions: [ ['View logs','logs','primary'], ['Open settings','settings','ghost'], ['Retry','retry','ghost'] ], anyway: true };
    },
    app_error:    function () { return {
      head: app.name + ' returned an error',
      sub: 'The app is running but responded with an error. Its logs usually say why.',
      mark: 'crit', markGlyph: '!', keepBg: true,
      actions: [ ['View logs','logs','primary'], ['Retry','retry','ghost'] ], anyway: true };
    },
    no_url:       function () { return {
      head: app.name + ' has no web page to open',
      sub: 'It doesn’t publish a web UI on a reachable address. You can add a port in Settings.',
      mark: 'warn', markGlyph: '!', stop: true,
      actions: [ ['Open settings','settings','primary'] ] };
    },
    start_error:  function (msg) { return {
      head: 'Couldn’t start ' + app.name,
      sub: msg || 'The app could not be started.', mark: 'crit', markGlyph: '!',
      actions: [ ['Retry','retry','ghost'] ] };
    }
  };

  // The "still starting" phases get an Open-anyway escape hatch and softened copy
  // once we cross the patience threshold — stretched on a first boot.
  var WAITY = { starting: 1, initializing: 1, waiting: 1, tls: 1 };
  function threshold() { return app.firstBoot ? 180000 : 45000; }

  function render(phase, extra) {
    var c = (COPY[phase] || COPY.waiting)(extra);
    var slow = WAITY[phase] && (Date.now() - started > threshold());

    $('head').textContent = c.head;
    $('status').textContent = slow ? 'This is taking longer than usual.' : c.sub;
    $('status').className = 'status' + (c.mark === 'crit' ? ' err' : '');

    $('mark').innerHTML = '';
    if (c.spin) { var s = document.createElement('div'); s.className = 'spinner'; $('mark').appendChild(s); }
    else if (c.mark) { var m = document.createElement('div'); m.className = 'mark ' + c.mark; m.textContent = c.markGlyph || ''; $('mark').appendChild(m); }

    var acts = c.actions || [];
    $('actions').innerHTML = '';
    acts.forEach(function (a) {
      var label = a[0], action = a[1], style = a[2];
      if ((action === 'logs' || action === 'settings') && deepLink(action === 'logs' ? 'logs' : 'settings') == null) return;
      var b = document.createElement('button');
      b.className = 'btn ' + style; b.textContent = label;
      b.onclick = function () { doAction(action); };
      $('actions').appendChild(b);
    });

    $('late').classList.toggle('show', !!(c.anyway || slow));
    $('bg').style.display = c.keepBg ? 'inline-flex' : 'none';
  }

  function doAction(action) {
    if (action === 'retry') { started = Date.now(); startKicked = false; done = false; kickAndLoop(); return; }
    if (action === 'logs') { var l = deepLink('logs'); if (l) location.href = l; return; }
    if (action === 'settings') { var s = deepLink('settings'); if (s) location.href = s; return; }
  }
  $('anyway').addEventListener('click', function () { goTo(redirectURL || '/'); });

  function fetchReach() {
    return fetch(boot.reachable, { cache: 'no-store', headers: { 'Accept': 'application/json' } })
      .then(function (r) { return r.ok ? r.json() : null; })
      .catch(function () { return null; });
  }

  // A port-only app has no server-probeable URL; once its containers are up we
  // confirm readiness with a no-cors ping from the browser (which — unlike
  // Maison — can reach the published host port). An opaque resolve means the
  // port is listening.
  function clientPing(res) {
    if (boot.mode !== 'dashboard' || res.url || !res.port) return Promise.resolve(false);
    return fetch(targetFrom(res), { mode: 'no-cors', cache: 'no-store' })
      .then(function () { return true; }).catch(function () { return false; });
  }

  function step() {
    if (done) return;
    fetchReach().then(function (res) {
      if (done) return;
      if (!res) { render('waiting'); return schedule(); }
      if (res.app) { app.name = res.app.name || app.name; app.icon = res.app.icon || app.icon; app.id = res.app.id || app.id; }
      if (typeof res.first_boot === 'boolean') app.firstBoot = res.first_boot;
      redirectURL = targetFrom(res);
      document.title = 'Starting ' + app.name + '…';
      setIcon(app);

      if (res.phase === 'ready') return goTo(redirectURL);

      if (res.phase === 'waiting') {
        return clientPing(res).then(function (up) {
          if (up) return goTo(redirectURL);
          render('waiting'); schedule();
        });
      }
      render(res.phase);
      if (res.phase !== 'no_url') schedule();
    });
  }

  // Poll cadence: gentle backoff so a long boot doesn't hammer the backend.
  function schedule() {
    var age = Date.now() - started;
    var delay = age > 30000 ? 4000 : age > 12000 ? 2500 : 1500;
    timer = setTimeout(step, delay);
  }

  function kickStart() {
    if (startKicked) return Promise.resolve();
    startKicked = true;
    return fetch(boot.start, { method: 'POST', cache: 'no-store' })
      .then(function (r) {
        if (r.ok) return;
        return r.json().catch(function () { return {}; }).then(function (j) { throw new Error(j.error || 'start failed'); });
      })
      .catch(function (e) { render('start_error', e && e.message); /* keep polling; a transient failure self-heals */ });
  }

  // First look decides whether we even need to start the stack; then poll.
  function kickAndLoop() {
    fetchReach().then(function (res) {
      if (res && res.app) { app.name = res.app.name || app.name; app.icon = res.app.icon || app.icon; app.id = res.app.id || app.id; }
      if (res && typeof res.first_boot === 'boolean') app.firstBoot = res.first_boot;
      setIcon(app);
      if (res && res.phase === 'ready') { redirectURL = targetFrom(res); return goTo(redirectURL); }
      if (res && res.phase === 'no_url') { render('no_url'); return; }
      var needsStart = !res || res.container == null || res.container.total === 0 || res.container.running < res.container.total;
      (needsStart ? kickStart() : Promise.resolve()).then(step);
    });
  }

  kickAndLoop();
})();
</script>
</body>
</html>`
