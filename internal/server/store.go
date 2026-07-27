package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/appstore"
)

type storeResponse struct {
	Apps       []*appstore.CatalogApp `json:"apps"`
	Categories []string               `json:"categories"`
	Recommend  []string               `json:"recommend"`
}

func (s *Server) handleStore(w http.ResponseWriter, _ *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, storeResponse{
		Apps:       s.store.Catalog(),
		Categories: s.store.Categories(),
		Recommend:  s.store.Recommend(),
	})
}

// --- App-store source management (add/remove custom stores) ---

func (s *Server) handleStoreSources(w http.ResponseWriter, _ *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string][]string{"sources": s.store.URLs()})
}

func (s *Server) handleAddStoreSource(w http.ResponseWriter, r *http.Request) {
	url, ok := decodeURL(w, r)
	if !ok {
		return
	}
	urls := s.store.URLs()
	for _, u := range urls {
		if u == url {
			s.applySources(w, r.Context(), urls) // already present
			return
		}
	}
	s.applySources(w, r.Context(), append(urls, url))
}

func (s *Server) handleRemoveStoreSource(w http.ResponseWriter, r *http.Request) {
	url, ok := decodeURL(w, r)
	if !ok {
		return
	}
	var kept []string
	for _, u := range s.store.URLs() {
		if u != url {
			kept = append(kept, u)
		}
	}
	s.applySources(w, r.Context(), kept)
}

// handleRefreshStoreSource force re-downloads a single store and rebuilds the
// catalog (one reload per store, triggered from the source list).
func (s *Server) handleRefreshStoreSource(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store unavailable"})
		return
	}
	url, ok := decodeURL(w, r)
	if !ok {
		return
	}
	rc, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	_ = s.store.RefreshStore(rc, url)
	writeJSON(w, http.StatusOK, map[string][]string{"sources": s.store.URLs()})
}

func decodeURL(w http.ResponseWriter, r *http.Request) (string, bool) {
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.URL) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url required"})
		return "", false
	}
	return strings.TrimSpace(body.URL), true
}

// applySources updates the store URLs, persists them, refreshes the catalog, and
// returns the new source list.
func (s *Server) applySources(w http.ResponseWriter, ctx context.Context, urls []string) {
	s.store.SetURLs(urls)
	cur := s.settings.Get()
	cur.StoreSources = urls
	_ = s.settings.Set(cur)

	rc, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	_ = s.store.Refresh(rc)

	writeJSON(w, http.StatusOK, map[string][]string{"sources": s.store.URLs()})
}

// handleStoreApp returns one store app. The optional ?store=<zip url> pins the
// lookup to that store — which need not be a configured source, so a deep link
// can address an app in a store the user has never added (the UI warns before
// installing one). Without it, the merged catalog answers: first store wins.
func (s *Server) handleStoreApp(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store unavailable"})
		return
	}
	app, _, err := s.store.GetFrom(r.Context(), storeParam(r), chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, app)
}

// storeParam reads the optional ?store=<zip url> pin shared by the store app,
// backups and install endpoints. Empty means "use the merged catalog".
func storeParam(r *http.Request) string {
	return strings.TrimSpace(r.URL.Query().Get("store"))
}

// handleStoreBackups lists the uninstall archives of a store app, so the store
// can offer "install from backup" next to a fresh install. The compose project
// name is resolved server-side (it can come from the compose file's own `name:`,
// which the client cannot see).
func (s *Server) handleStoreBackups(w http.ResponseWriter, r *http.Request) {
	if s.installer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "install unavailable"})
		return
	}
	project, err := s.installer.ProjectFor(r.Context(), storeParam(r), chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	backups := apps.ListBackups(s.cfg.AppsDir(), project)
	if backups == nil {
		backups = []apps.Backup{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": project, "backups": backups})
}

// handleInstall starts a detached install and returns immediately. Progress is
// not streamed on this request: the install runs on a background context (so
// closing the store panel never cancels it) and its progress rides the live
// "apps" channel as Download/Start bars on the app's tile (see appsSnapshot).
//
// An optional {"from_backup": "<archive name>"} body reinstalls the app on top of
// one of its uninstall archives instead of on a clean slate.
func (s *Server) handleInstall(w http.ResponseWriter, r *http.Request) {
	if s.installer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "install unavailable"})
		return
	}
	var body struct {
		FromBackup string `json:"from_backup"`
	}
	// A body is optional here: a plain install posts nothing at all.
	_ = json.NewDecoder(r.Body).Decode(&body)

	project, err := s.installer.StartInstall(r.Context(), storeParam(r), chi.URLParam(r, "id"), strings.TrimSpace(body.FromBackup))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.broadcastApps()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started", "id": project})
}
