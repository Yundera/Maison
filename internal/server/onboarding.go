package server

import (
	"net/http"
	"net/url"
	"strings"
)

// The onboarding gate stands in front of the dashboard while the deployment
// says the owner still has a first-run step to complete (see internal/onboarding
// for how it is asked).
//
// It is a GATE, not a route: there is no path that reaches the dashboard around
// it, because what it protects against is an owner who never gets round to it.
// Whether the owner may put the step off is the deployment's call, not Maison's —
// the deployment's page can offer "later" and simply answer show:false until the
// reminder is due again.
//
// It is NOT a security boundary and must not be mistaken for one. Whoever
// reaches Maison has already been authenticated by the gate in front of it; this
// only decides which page they land on.

// onboardingGate wraps the SPA with the setup redirect.
//
// Only browser navigations are intercepted — a GET or HEAD that asks for HTML —
// and that test comes FIRST, before anything is asked of the deployment: the
// SPA's own assets, /api, /ws and the app-gate host (the last three registered
// outside this wrapper) must never cost an outbound call.
//
// A navigation the deployment says "show" for is answered with a 303 to its page.
// A redirect rather than a page of our own, because the deployment's page IS the
// explanation: it is on the box's own domain, it says where the owner is and why,
// and it sends them back here (`return`) when they are done. An interstitial in
// between was one more click on the way to the same place. The 303 is no-store,
// so neither the browser nor anything in between remembers it once setup is done.
//
// Anything else — no check configured, a check that is slow, down, or answers
// nonsense — serves the dashboard. Fail open; see internal/onboarding.
func (s *Server) onboardingGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isNavigation(r) {
			next.ServeHTTP(w, r)
			return
		}
		setupURL, show := s.onboardingCheck().Ask(r.Context())
		if !show {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, withReturn(setupURL, r), http.StatusSeeOther)
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
// typed. A fixed value in the setting would bounce them onto a different host at
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
