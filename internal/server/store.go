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
	"github.com/yundera/maison/internal/installer"
)

type storeResponse struct {
	Apps       []*appstore.CatalogApp `json:"apps"`
	Categories []string               `json:"categories"`
	Recommend  []string               `json:"recommend"`
	// Sources rides along so the panel can group the catalog by store in the order
	// the box has them configured, and name each group as the store names itself.
	// Deriving either from Apps would lose the order and, for a store that ships no
	// app at all, the store.
	Sources []appstore.Source `json:"sources"`
}

func (s *Server) handleStore(w http.ResponseWriter, _ *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store unavailable"})
		return
	}
	// CatalogAll, not Catalog: the browse is grouped by store, not merged across
	// them, so the payload carries every configured store's copy of an app rather
	// than only the one that won the id collision. Primary still marks the copy a
	// bare id resolves to, which is what the featured row and /store/<id> use —
	// see appstore.CatalogApp.
	writeJSON(w, http.StatusOK, storeResponse{
		Apps:       s.store.CatalogAll(),
		Categories: s.store.Categories(),
		Recommend:  s.store.Recommend(),
		Sources:    s.store.Sources(),
	})
}

// --- App-store source management (add/remove custom stores) ---

// sourcesResponse answers every source-list mutation. Warning is a *non-fatal*
// report: the sources were applied and the catalog rebuilt from whatever answered,
// but at least one store could not be fetched. It rides a 200 because the list in
// the UI must still update — an outright failure gets a non-2xx and an "error"
// instead (see handleRefreshStoreSource).
type sourcesResponse struct {
	// Each source with the name it gives itself in store.json, falling back to its
	// URL. A name is never derived from the URL: "owner/repo" is one forge's path
	// layout, it means nothing for a store served from anywhere else, and it
	// collapses two refs of the same repository into the same label.
	Sources []appstore.Source `json:"sources"`
	Warning string            `json:"warning,omitempty"`
}

// storeSyncTimeout bounds one download+extract pass over every configured store.
const storeSyncTimeout = 90 * time.Second

// detachedStoreCtx is the context a store sync runs on. It keeps the request's
// values but drops its cancellation: a refresh has already written to disk by the
// time the user closes the store panel, and cancelling mid-extract leaves the
// staging tree half-populated for the next pass to clean up. Installs detach for
// the same reason. The timeout still applies, so nothing runs unbounded.
func detachedStoreCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), storeSyncTimeout)
}

func (s *Server) handleStoreSources(w http.ResponseWriter, _ *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, sourcesResponse{Sources: s.store.Sources()})
}

func (s *Server) handleAddStoreSource(w http.ResponseWriter, r *http.Request) {
	url, ok := decodeURL(w, r)
	if !ok {
		return
	}
	urls := s.store.URLs()
	for _, u := range urls {
		if u == url {
			s.applySources(w, r, urls) // already present
			return
		}
	}
	s.applySources(w, r, append(urls, url))
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
	s.applySources(w, r, kept)
}

// handleRefreshStoreSource force re-downloads a single store and rebuilds the
// catalog (one reload per store, triggered from the source list).
//
// Unlike applySources this reports a failure as a failure: the user pressed ⟳ and
// is owed an answer about whether the catalog they are looking at came from the
// origin. A store that couldn't be fetched leaves its cached copy in place and
// answers 502.
func (s *Server) handleRefreshStoreSource(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store unavailable"})
		return
	}
	url, ok := decodeURL(w, r)
	if !ok {
		return
	}
	rc, cancel := detachedStoreCtx(r)
	defer cancel()
	if err := s.store.RefreshStore(rc, url); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, sourcesResponse{Sources: s.store.Sources()})
}

// decodeURL reads the {"url": …} body the source-list endpoints take, and
// canonicalises it — the single boundary where a hand-typed source enters.
// Without that, adding a store already on the list under a different spelling
// appends a duplicate that then downloads and parses a second time, and removing
// one by the spelling on screen would not match what was stored.
func decodeURL(w http.ResponseWriter, r *http.Request) (string, bool) {
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.URL) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url required"})
		return "", false
	}
	return appstore.CanonicalURL(body.URL), true
}

// applySources updates the store URLs, persists them, refreshes the catalog, and
// returns the new source list.
//
// The refresh error is reported as a warning rather than a failure: the source
// list was changed and persisted whatever the network did, and removing a source
// must not appear to fail because some *other* store is unreachable. It is still
// reported — a freshly added URL that turns out to be a 404 used to look like it
// had worked, leaving a permanently empty entry in the list with no explanation.
func (s *Server) applySources(w http.ResponseWriter, r *http.Request, urls []string) {
	s.store.SetURLs(urls)
	cur := s.settings.Get()
	cur.StoreSources = urls
	_ = s.settings.Set(cur)

	rc, cancel := detachedStoreCtx(r)
	defer cancel()

	resp := sourcesResponse{}
	if err := s.store.Refresh(rc); err != nil {
		resp.Warning = err.Error()
	}
	resp.Sources = s.store.Sources()
	writeJSON(w, http.StatusOK, resp)
}

// handleStoreApp returns one store app. The optional ?store= pins the lookup to
// that store — which need not be a configured source, so a deep link can address
// an app in a store the user has never added (the UI warns before installing
// one). Without it, the merged catalog answers: first store wins.
func (s *Server) handleStoreApp(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store unavailable"})
		return
	}
	app, _, err := s.store.GetFrom(r.Context(), storeRef(r))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, app)
}

// storeRef builds the store reference addressed by a request: the {id} path
// param, plus the optional ?store= locator and ?apps_path= folder shared by the
// store app, backups and install endpoints. No locator means "use the merged
// catalog"; no apps path means the default layout.
//
// The locator's scheme may be omitted — CanonicalURL supplies https — so the
// readable form the SPA puts in a deep link is the form the API accepts.
//
// The endpoints keep a single-segment {id} rather than a wildcard: two of the
// three have path segments after it (/backups, /install), where a path-shaped id
// could not be told apart from the segments that follow. The URL grammar that
// carries the apps folder inline is a presentation concern, and it is parsed in
// web/src/lib/route.ts.
func storeRef(r *http.Request) appstore.Ref {
	q := r.URL.Query()
	return appstore.NewRef(q.Get("store"), q.Get("apps_path"), chi.URLParam(r, "id"))
}

// storeBackupEngine is one engine's offer in the store's install-from-backup picker.
//
// Grouped by engine rather than merged into one list, for the reason the Backups page
// is tabbed by engine: a stamp held by two engines is two backups, and a merged row
// has to invent a vocabulary for "in both places" that every attempt has got wrong. A
// group needs none — every row in it belongs to that engine, and so does the install
// it starts.
type storeBackupEngine struct {
	Engine string `json:"engine"`
	// Name is the deployment's name for it, empty when nobody provisioned one — the
	// client then falls back to describing the engine. See engineInfo.Name.
	Name    string        `json:"name,omitempty"`
	Offsite bool          `json:"offsite"`
	Backups []apps.Backup `json:"backups"`
}

// handleStoreBackups lists a store app's backups, grouped by the engine holding each,
// so the store can offer "install from backup" next to a fresh install. The compose
// project name is resolved server-side (it can come from the compose file's own
// `name:`, which the client cannot see).
//
// It reads through the engine set like every other backup listing. Walking the data
// disk directly — which this used to do — meant a box whose default engine writes to a
// repository could not install from anything it had backed up: the picker showed only
// what predated the switch, and on a box that had always been remote, nothing at all.
//
// Sizes are left unmeasured here, unlike the per-app Backups tab: this runs on a click
// in the catalog, and measuring a folder archive is a tree walk per row.
func (s *Server) handleStoreBackups(w http.ResponseWriter, r *http.Request) {
	if s.installer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "install unavailable"})
		return
	}
	project, err := s.installer.ProjectFor(r.Context(), storeRef(r))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	engines := []storeBackupEngine{}
	if s.engines != nil {
		for _, id := range s.engines.IDs() {
			list := s.engines.ListIn(r.Context(), id, project)
			// An engine holding nothing for this app contributes no group. The picker is a
			// dropdown on a catalog row, and an empty heading per configured engine is
			// noise on every app that has never been installed.
			if len(list) == 0 {
				continue
			}
			name, offsite := s.engineDisplay(r.Context(), id)
			engines = append(engines, storeBackupEngine{
				Engine: id, Name: name, Offsite: offsite, Backups: list,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": project, "engines": engines})
}

// handleInstall starts a detached install and returns immediately. Progress is
// not streamed on this request: the install runs on a background context (so
// closing the store panel never cancels it) and its progress rides the live
// "apps" channel as Download/Start bars on the app's tile (see appsSnapshot).
//
// An optional {"from_backup": "<stamp>", "engine": "<engine>"} body reinstalls the app
// on top of one of its backups instead of on a clean slate. Engine says which copy,
// because the picker offers a row per engine and two of them can hold the same stamp;
// omitting it still works and means "whichever engine has it".
func (s *Server) handleInstall(w http.ResponseWriter, r *http.Request) {
	if s.installer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "install unavailable"})
		return
	}
	var body struct {
		FromBackup string `json:"from_backup"`
		Engine     string `json:"engine"`
	}
	// A body is optional here: a plain install posts nothing at all.
	_ = json.NewDecoder(r.Body).Decode(&body)

	from := installer.BackupRef{
		Name:   strings.TrimSpace(body.FromBackup),
		Engine: strings.TrimSpace(body.Engine),
	}
	project, err := s.installer.StartInstall(r.Context(), storeRef(r), from)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.broadcastApps()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started", "id": project})
}
