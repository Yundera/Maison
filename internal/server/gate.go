package server

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yundera/maison/internal/brand"
	"github.com/yundera/maison/internal/routing"
)

// The launch gate is the launch page in "gate" mode (see launch.go).
//
// When an app is down, the mesh-router-caddy catch-all reverse-proxies its
// gateway host (`<app>-<domain>`) to Maison, preserving the original Host. We
// detect that we are standing in for an app host (rather than serving our own
// dashboard) and serve the launch page there. Because it is served on the app's
// OWN origin, its readiness redirect is simply a reload of "/", which — once the
// app is up — caddy routes to the real app instead of back to us.
//
// Apps that are already up never hit this path: caddy routes their host straight
// to the app and Maison is not involved.

// isDashboardHost reports whether host addresses Maison's own dashboard
// (rather than an app gateway host it is catching for). With no domain to route
// on, every request is treated as the dashboard (the gate is inert).
//
// The dashboard is reached at `maison` (and the legacy `casaos`) on every domain of
// the routing setting — the nip.io and sslip.io names as well as the gateway one —
// and at the bare primary domain.
func (s *Server) isDashboardHost(hostport string) bool {
	eff := s.cfg.Routing()
	if eff.Renderer != routing.Labels || s.cfg.AppDomain() == "" {
		return true
	}
	host := strings.ToLower(hostOnly(hostport))
	switch host {
	case "", "localhost", "127.0.0.1", s.cfg.AppDomain():
		return true
	}
	resolve := s.deployResolver()
	for _, name := range []string{brand.Slug, "casaos"} {
		for _, h := range routing.Hosts(eff, name, resolve) {
			if h == host {
				return true
			}
		}
	}
	return false
}

// seeHost records the address the dashboard is first opened at, for a ports-mode
// box that has no url-host of its own: it is what APP_URL then names. Loopback, and a
// bare container name a sibling calls the API by, say nothing about the box's
// address and are not recorded.
func (s *Server) seeHost(hostport string) {
	cur := s.settings.Get()
	if cur.Routing != nil && cur.Routing.URLHost != "" {
		return
	}
	if s.cfg.Routing().Renderer != routing.Ports {
		return
	}
	host := strings.ToLower(hostOnly(hostport))
	ip := net.ParseIP(host)
	switch {
	case host == "" || host == "localhost":
		return
	case ip != nil && ip.IsLoopback():
		return
	case ip == nil && !strings.Contains(host, "."):
		return
	}
	s.settings.SeeHost(host)
}

// hostOnly strips the port from a Host header, IPv6 brackets included.
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return strings.Trim(h, "[]")
	}
	return strings.Trim(hostport, "[]")
}

// deployResolver expands ${VAR} references with the deployment's .env.app — what a
// domain template resolves to on this box.
func (s *Server) deployResolver() routing.Resolver {
	var env map[string]string
	if s.cfg.AppEnv != nil {
		env = s.cfg.AppEnv()
	}
	return routing.EnvResolver(env)
}

// gateRouter handles requests that arrive on an app gateway host while the app
// is down (i.e. via the catch-all). Everything except the control endpoints
// renders the launch page.
func (s *Server) gateRouter() http.Handler {
	m := chi.NewRouter()
	m.Get(brand.GateRoot+"/reachable", s.gateReachable)
	// The launch page shows the app's icon, and the app list gives it as a path on
	// whatever origin the page was served from — here, the app's own host. Serving
	// it from the gate too keeps that one URL right in both modes.
	m.Get("/api/apps/{id}/icon", s.handleAppIcon)
	m.Post(brand.GateRoot+"/start", s.gateStart)
	m.Handle("/*", http.HandlerFunc(s.gatePage))
	return m
}

// gateReachable is the app-origin probe: resolve the app from the request Host
// and report whether it can be opened yet.
func (s *Server) gateReachable(w http.ResponseWriter, r *http.Request) {
	app, ok := s.apps.FindByHost(r.Context(), r.Host, s.cfg.AppDomain())
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown app host"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.apps.Reach(ctx, app))
}

func (s *Server) gateStart(w http.ResponseWriter, r *http.Request) {
	app, ok := s.apps.FindByHost(r.Context(), r.Host, s.cfg.AppDomain())
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown app host"})
		return
	}
	if err := s.apps.EnsureStarted(r.Context(), app.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.broadcastApps()
	writeJSON(w, http.StatusOK, map[string]string{"status": "starting", "id": app.ID})
}

// gatePage serves the launch page in gate mode. Its deep-links point at the
// dashboard host when a domain is configured, and are omitted otherwise.
func (s *Server) gatePage(w http.ResponseWriter, _ *http.Request) {
	boot := launchBoot{
		Mode:      "gate",
		Reachable: brand.GateRoot + "/reachable",
		Start:     brand.GateRoot + "/start",
	}
	if d := s.cfg.AppDomain(); d != "" {
		host := "https://" + brand.Slug + "-" + d
		boot.Dashboard = &host
	}
	writeLaunchPage(w, boot)
}
