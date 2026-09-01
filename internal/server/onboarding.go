package server

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/yundera/maison/internal/brand"
	"github.com/yundera/maison/internal/onboarding"
)

// The onboarding gate stands in front of the dashboard while the deployment
// still owes a first-run setup (see internal/onboarding for what arms it).
//
// It is a GATE, not a route: there is no path that reaches the dashboard around
// it and no way to dismiss it, because the thing it is protecting against is an
// owner who never gets round to it. On a PCS the setup step is claiming the
// box's local account, and an owner who skips it stays permanently dependent on
// the provider's SSO to reach a server that is supposed to be theirs. The same
// reasoning put the admin app's own wizard around its app shell instead of on a
// route it could be navigated away from.
//
// It is NOT a security boundary and must not be mistaken for one. Whoever
// reaches Maison has already been authenticated by the gate in front of it; this
// only decides which page they land on.

// onboardingGate wraps the SPA with the setup interstitial.
//
// Only browser navigations are intercepted — a GET or HEAD that asks for HTML.
// Everything else falls through untouched, which keeps the interstitial out of
// the SPA's own asset responses and leaves /api, /ws and the app-gate host (all
// registered outside this wrapper) alone. Blocking those would buy nothing: with
// the document replaced, nothing is left to call them.
func (s *Server) onboardingGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setupURL, pending := onboarding.Pending(s.cfg)
		if !pending || !isNavigation(r) {
			next.ServeHTTP(w, r)
			return
		}
		writeOnboardingPage(w, withReturn(setupURL, r))
	})
}

// isNavigation reports whether a request is a browser asking for a page, as
// opposed to the SPA fetching one of its own assets.
func isNavigation(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// withReturn adds the dashboard's own address to the setup URL as `return`, so
// the far side can send the browser back where it started.
//
// Taken from the request rather than from configuration on purpose: a box
// answers on several hostnames at once — its domain plus the nip.io and
// sslip.io fallbacks — and only the browser knows which one the owner actually
// typed. A fixed value in the file would bounce them onto a different host at
// the end of setup, possibly one their current session does not cover.
//
// A far side that honours this must validate it before redirecting; an
// unchecked `return` on an internet-facing host is an open redirect.
func withReturn(setupURL string, r *http.Request) string {
	u, err := url.Parse(setupURL)
	if err != nil {
		return setupURL
	}
	q := u.Query()
	q.Set("return", requestOrigin(r))
	u.RawQuery = q.Encode()
	return u.String()
}

// requestOrigin reconstructs the absolute address this request arrived at.
// Maison is always reached through a reverse proxy that terminates TLS, so the
// scheme comes from the forwarded header first and the connection only decides
// when nothing forwarded one.
func requestOrigin(r *http.Request) string {
	scheme := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
	if i := strings.IndexByte(scheme, ','); i >= 0 {
		scheme = strings.TrimSpace(scheme[:i]) // first hop wins
	}
	if scheme != "http" && scheme != "https" {
		scheme = "http"
		if r.TLS != nil {
			scheme = "https"
		}
	}
	host := r.Host
	if fwd := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); fwd != "" {
		if i := strings.IndexByte(fwd, ','); i >= 0 {
			fwd = strings.TrimSpace(fwd[:i])
		}
		host = fwd
	}
	return scheme + "://" + host + "/"
}

// writeOnboardingPage renders the interstitial with the setup URL injected.
func writeOnboardingPage(w http.ResponseWriter, setupURL string) {
	page := strings.ReplaceAll(onboardingHTML, "__SETUP_URL__", escapeAttr(setupURL))
	page = strings.ReplaceAll(page, "__BRAND__", escapeAttr(brand.Name))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// 200, not a redirect: the owner is shown where they are being sent and why,
	// and clicks. A silent cross-host bounce from the address they typed to one
	// they have never seen reads as a hijack, and it takes their back button with
	// it.
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(page))
}

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

// onboardingHTML is the self-contained interstitial. No external assets and no
// script: it has to render on a box whose dashboard the owner has never loaded,
// and its whole job is one link.
//
// Deliberately no "skip", "later" or "dismiss" — see the note at the top of this
// file.
const onboardingHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Finish setting up your server</title>
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
  .icon {
    width: 84px; height: 84px; margin: 0 auto 1.35rem; border-radius: 20px;
    display: grid; place-items: center;
    background: #2f6df6; color: #fff;
    box-shadow: 0 8px 24px rgba(0,0,0,0.35);
  }
  .icon svg { width: 42px; height: 42px; }
  h1 { font-size: 1.2rem; margin: 0 0 0.5rem; font-weight: 640; text-wrap: balance; }
  p { font-size: 0.92rem; line-height: 1.5; opacity: 0.72; margin: 0; text-wrap: pretty; }
  .btn {
    display: inline-block; margin-top: 1.6rem; padding: 0.62rem 1.15rem;
    border-radius: 9px; font-size: 0.9rem; font-weight: 600;
    background: #2f6df6; color: #fff; text-decoration: none;
  }
  .btn:focus-visible { outline: 2px solid #2f6df6; outline-offset: 3px; }
  .note { display: block; margin-top: 1rem; font-size: 0.78rem; opacity: 0.55; }
</style>
</head>
<body>
  <main class="card">
    <div class="icon" aria-hidden="true">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
           stroke-linecap="round" stroke-linejoin="round">
        <path d="M3 10.5 12 3l9 7.5"></path>
        <path d="M5 9.5V20a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V9.5"></path>
        <path d="M10 21v-6h4v6"></path>
      </svg>
    </div>
    <h1>Finish setting up your server</h1>
    <p>A few steps are still needed before your __BRAND__ dashboard is ready. It only takes a minute.</p>
    <a class="btn" href="__SETUP_URL__">Continue setup &rarr;</a>
    <span class="note">You will come back here when it is done.</span>
  </main>
</body>
</html>`
