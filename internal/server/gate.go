package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yundera/maison/internal/brand"
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
// (rather than an app gateway host it is catching for). With no gateway domain
// configured, every request is treated as the dashboard (the gate is inert).
func (s *Server) isDashboardHost(hostport string) bool {
	if s.cfg.AppDomain() == "" {
		return true
	}
	host := hostport
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	host = strings.ToLower(host)
	switch host {
	case "", "localhost", "127.0.0.1",
		s.cfg.AppDomain(),
		brand.Slug+"-"+s.cfg.AppDomain(),
		"casaos-" + s.cfg.AppDomain():
		return true
	}
	return false
}

// gateRouter handles requests that arrive on an app gateway host while the app
// is down (i.e. via the catch-all). Everything except the control endpoints
// renders the launch page.
func (s *Server) gateRouter() http.Handler {
	m := chi.NewRouter()
	m.Get(brand.GateRoot+"/reachable", s.gateReachable)
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
