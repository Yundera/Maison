// Package server wires the HTTP router: REST API, WebSocket hub, and the
// embedded single-page app.
package server

import (
	"context"
	"io/fs"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/yundera/maison/internal/brand"
	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/appstore"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/dockerx"
	"github.com/yundera/maison/internal/installer"
	"github.com/yundera/maison/internal/live"
	"github.com/yundera/maison/internal/system"
	"github.com/yundera/maison/internal/usersettings"
)

// Server holds shared dependencies for the HTTP handlers.
type Server struct {
	cfg       config.Config
	uiFS      fs.FS
	collector *system.Collector
	hub       *live.Hub
	dx        *dockerx.Client
	apps      *apps.Registry
	store     *appstore.Manager
	installer *installer.Installer
	settings  *usersettings.Store
}

// New builds the root HTTP handler. A nil-Docker environment still serves the
// dashboard (apps endpoints report the connection error).
func New(cfg config.Config, uiFS fs.FS) http.Handler {
	collector := system.NewCollector(cfg.DataRoot)
	settings := usersettings.New(filepath.Join(cfg.StateDir(), "settings.json"))

	// Apps are published on the operator's additional domains, and they edit that
	// list at runtime — so the Config every layer below copies reads it live rather
	// than snapshotting it at boot. See internal/caddyroutes.
	cfg.Domains = settings.Domains

	s := &Server{
		cfg:       cfg,
		uiFS:      uiFS,
		collector: collector,
		hub:       live.NewHub(collector),
		settings:  settings,
	}

	if dx, err := dockerx.New(); err != nil {
		log.Printf("docker: %v (app management disabled)", err)
	} else {
		if err := dx.Ping(context.Background()); err != nil {
			log.Printf("docker: cannot reach daemon: %v", err)
		}
		s.dx = dx
		s.apps = apps.New(cfg, dx)
		// Rebroadcast the app list whenever a lifecycle op enters/leaves its busy
		// state, so tiles show/hide the "…" overlay live (docs/app-model.md).
		s.apps.OnChange = s.broadcastApps
		// Uninstall progress advances far more often than the busy set changes
		// (a zipped archive reports every chunk), so it gets the same throttle
		// the installer's does.
		s.apps.OnProgress = throttle(300*time.Millisecond, s.broadcastApps)
		s.hub.AppsSnapshot = s.appsSnapshot
		s.watchDocker()
	}

	// App store + installer (independent of Docker connectivity for browsing).
	// Persisted store sources (if any) take precedence over the env default.
	initialURLs := cfg.StoreURLs
	if ss := s.settings.Get().StoreSources; len(ss) > 0 {
		initialURLs = ss
	}
	s.store = appstore.New(initialURLs, filepath.Join(cfg.StateDir(), "appstore"))
	s.store.StartAutoRefresh(context.Background(), time.Hour)
	s.installer = installer.New(cfg, s.store, s.dx)
	// Rebroadcast the app list as install progress advances so the tile's
	// Download/Start bars move live. Pull events are frequent, so throttle.
	s.installer.OnUpdate = throttle(300*time.Millisecond, s.broadcastApps)

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)

	r.Get("/ping", s.handlePing)
	r.Get("/ws", s.hub.ServeWS)

	r.Route("/api", func(r chi.Router) {
		r.Get("/system/stats", s.handleSystemStats)
		r.Get("/apps", s.handleListApps)
		r.Get("/apps/{id}/config", s.handleGetConfig)
		r.Put("/apps/{id}/config", s.handlePutConfig)
		r.Get("/apps/{id}/override/form", s.handleGetOverrideForm)
		r.Put("/apps/{id}/override/form", s.handlePutOverrideForm)
		r.Post("/apps/{id}/override/validate", s.handleValidateOverride)
		r.Get("/apps/{id}/effective", s.handleEffectiveConfig)
		r.Get("/apps/{id}/env", s.handleGetEnv)
		r.Put("/apps/{id}/env", s.handlePutEnv)
		r.Put("/apps/{id}/webui", s.handlePutWebUI)
		r.Get("/apps/{id}/tips", s.handleRenderTips)
		r.Put("/apps/{id}/tips", s.handlePutTips)
		r.Get("/apps/{id}/update", s.handleCheckUpdate)
		r.Post("/apps/{id}/update", s.handleApplyUpdate)
		r.Get("/apps/{id}/services", s.handleAppServices)
		r.Get("/apps/{id}/reachable", s.handleReachable)
		r.Get("/apps/{id}/logs", s.handleAppLogs)
		r.Get("/apps/{id}/stats", s.handleAppStats)
		r.Post("/apps/{id}/dismiss", s.handleDismissApp)
		r.Post("/apps/{id}/{action}", s.handleAppAction)
		r.Delete("/apps/{id}", s.handleUninstallApp)

		r.Get("/store", s.handleStore)
		r.Get("/store/sources", s.handleStoreSources)
		r.Post("/store/sources", s.handleAddStoreSource)
		r.Delete("/store/sources", s.handleRemoveStoreSource)
		r.Post("/store/sources/refresh", s.handleRefreshStoreSource)
		r.Get("/store/app/{id}", s.handleStoreApp)
		r.Get("/store/{id}/backups", s.handleStoreBackups)
		r.Post("/store/{id}/install", s.handleInstall)

		r.Get("/settings", s.handleGetSettings)
		r.Put("/settings", s.handlePutSettings)
		r.Put("/settings/domains", s.handlePutDomains)
		r.Get("/settings/appenv", s.handleGetAppEnv)
		r.Put("/settings/appenv", s.handlePutAppEnv)
	})

	// The launch page (see launch.go) is served on the dashboard origin: a tile
	// click for a port-published app opens /launch?app=<id>, which holds the user
	// through start/readiness instead of a browser error page. Registered before
	// the SPA catch-all so it wins.
	r.Get("/launch", s.handleLaunch)

	r.Handle("/*", spaHandler(uiFS))

	return s.rootHandler(r)
}

// rootHandler marks every response with the X-Maison header (so the launch
// gate can tell Maison's catch-all apart from a real app, same-origin) and
// dispatches by Host: our own dashboard hosts get the dashboard router; any
// other host is an app gateway host we are catching for while the app is down,
// so it gets the launch gate. With Docker unavailable there is no app registry,
// so everything falls back to the dashboard.
func (s *Server) rootHandler(dashboard http.Handler) http.Handler {
	gate := s.gateRouter()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(brand.Header, "1")
		if s.apps == nil || s.isDashboardHost(r.Host) {
			dashboard.ServeHTTP(w, r)
			return
		}
		gate.ServeHTTP(w, r)
	})
}

func (s *Server) handlePing(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleSystemStats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.collector.Sample())
}

// listApps returns the reconciled app list with any in-flight installs and
// uninstalls overlaid so their tiles carry live progress. Shared by the REST
// endpoint and the live "apps" channel so both reflect them identically (a page
// reload mid-install shows progress without waiting for a broadcast).
func (s *Server) listApps(ctx context.Context) []apps.App {
	list, _ := s.apps.List(ctx)
	if s.installer != nil {
		list = overlayInstalls(list, s.installer.Installs())
	}
	return overlayUninstalls(list, s.apps.Uninstalls())
}

// appsSnapshot returns the current app list for the live "apps" channel.
func (s *Server) appsSnapshot() any {
	if s.apps == nil {
		return []any{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if list := s.listApps(ctx); list != nil {
		return list
	}
	return []any{}
}

// overlayInstalls stamps install progress onto matching app tiles, and appends a
// placeholder tile for any install whose on-disk/Docker footprint doesn't exist
// yet (the brief window before the compose dir is written). Returns the list
// re-sorted by display name.
func overlayInstalls(list []apps.App, installs []installer.InstallState) []apps.App {
	if len(installs) == 0 {
		return list
	}
	byID := make(map[string]int, len(list))
	for i, a := range list {
		byID[a.ID] = i
	}
	for _, st := range installs {
		if i, ok := byID[st.ID]; ok {
			list[i].Installing = st.Phase != "error"
			list[i].Download = st.Download
			list[i].Start = st.Start
			list[i].Phase = st.Phase
			list[i].InstallError = st.Error
			continue
		}
		list = append(list, apps.App{
			ID:           st.ID,
			Name:         st.Name,
			Icon:         st.Icon,
			Status:       apps.StatusStopped,
			Managed:      true,
			Installing:   st.Phase != "error",
			Download:     st.Download,
			Start:        st.Start,
			Phase:        st.Phase,
			InstallError: st.Error,
		})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}

// overlayUninstalls stamps uninstall progress onto matching app tiles. Unlike
// installs there is never a placeholder to append: the app's folder is what
// makes the tile, and it only disappears at the very last step — so an uninstall
// that is still running (or that failed) always has a tile to land on.
func overlayUninstalls(list []apps.App, uninstalls []apps.UninstallState) []apps.App {
	if len(uninstalls) == 0 {
		return list
	}
	byID := make(map[string]int, len(list))
	for i, a := range list {
		byID[a.ID] = i
	}
	for _, st := range uninstalls {
		i, ok := byID[st.ID]
		if !ok {
			continue
		}
		list[i].Uninstalling = st.Phase != apps.PhaseError
		list[i].Remove = st.Remove
		list[i].Archive = st.Archive
		list[i].Phase = st.Phase
		list[i].UninstallError = st.Error
	}
	return list
}

// throttle returns a function that runs fn at most once per d, always running a
// trailing call so the final state is not dropped.
func throttle(d time.Duration, fn func()) func() {
	var mu sync.Mutex
	var timer *time.Timer
	var last time.Time
	return func() {
		mu.Lock()
		defer mu.Unlock()
		if timer != nil {
			return // a trailing call is already scheduled
		}
		if wait := d - time.Since(last); wait > 0 {
			timer = time.AfterFunc(wait, func() {
				mu.Lock()
				timer = nil
				last = time.Now()
				mu.Unlock()
				fn()
			})
			return
		}
		last = time.Now()
		fn()
	}
}

// broadcastApps pushes the current app list to "apps" channel subscribers. The
// snapshot is built lazily: with no subscriber there is nothing to send and
// reconciling the list is pure waste (see live.Hub.BroadcastLazy).
func (s *Server) broadcastApps() {
	s.hub.BroadcastLazy(live.ChannelApps, s.appsSnapshot)
}

// watchDocker rebroadcasts the app list on container events, debounced.
func (s *Server) watchDocker() {
	go func() {
		trigger := make(chan struct{}, 1)
		go s.dx.WatchContainers(context.Background(), func() {
			select {
			case trigger <- struct{}{}:
			default:
			}
		})
		for range trigger {
			time.Sleep(400 * time.Millisecond) // debounce bursts
			s.broadcastApps()
		}
	}()
}
