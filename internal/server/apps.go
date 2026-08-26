package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/installer"
)

func (s *Server) requireApps(w http.ResponseWriter) bool {
	if s.apps == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]string{"error": "docker unavailable"})
		return false
	}
	return true
}

func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	if !s.requireApps(w) {
		return
	}
	list := s.listApps(r.Context())
	if list == nil {
		list = []apps.App{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleAppAction(w http.ResponseWriter, r *http.Request) {
	if !s.requireApps(w) {
		return
	}
	id := chi.URLParam(r, "id")
	action := chi.URLParam(r, "action")

	var err error
	switch action {
	case "start":
		err = s.apps.Start(r.Context(), id)
	case "stop":
		err = s.apps.Stop(r.Context(), id)
	case "restart":
		err = s.apps.Restart(r.Context(), id)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown action"})
		return
	}
	// A stop aimed at a system app is refused, not failed: the UI withholds the
	// menu entry, and the API says why for anything that asks anyway.
	if errors.Is(err, apps.ErrProtected) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleCheckUpdate reports whether the app's reference store carries a newer
// docker-compose.yml than the installed copy (see installer.CheckUpdate).
func (s *Server) handleCheckUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.requireApps(w) {
		return
	}
	if s.installer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	st, err := s.installer.CheckUpdate(ctx, chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleApplyUpdate copies the store's current compose over the app's strict base
// (when it differs) and brings the stack back up. The tile shows a "…" overlay
// while it runs.
func (s *Server) handleApplyUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.requireApps(w) {
		return
	}
	if s.installer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store unavailable"})
		return
	}
	id := chi.URLParam(r, "id")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	var res installer.UpdateResult
	err := s.apps.WithBusy(id, func() error {
		var e error
		res, e = s.installer.ApplyUpdate(ctx, id)
		return e
	})
	updated := res.Applied
	if err != nil {
		// A failed update that was rolled back leaves the app exactly as it was, so
		// the tile has to be rebroadcast either way — the restore changed it back.
		s.broadcastApps()
		out := map[string]any{"error": err.Error(), "rolled_back": res.RolledBack}
		if res.Warning != "" {
			out["warning"] = res.Warning
		}
		writeJSON(w, http.StatusInternalServerError, out)
		return
	}
	s.broadcastApps()
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "updated": updated})
}

// handleUninstallApp starts an uninstall and returns immediately: the work runs
// detached and its progress is overlaid on the app's tile (one red bar — remove,
// then archive), so the confirmation dialog never has to block on a zip that
// takes minutes. Only the up-front refusal (a protected app) is reported here;
// a later failure lands on the tile.
func (s *Server) handleUninstallApp(w http.ResponseWriter, r *http.Request) {
	if !s.requireApps(w) {
		return
	}
	id := chi.URLParam(r, "id")
	// zip=true compresses the archived folder; otherwise it is a plain rename.
	// Either way the app's data is preserved (docs/app-model.md).
	zip := r.URL.Query().Get("zip") == "true"
	// Drop any lingering install-progress overlay (e.g. a failed install) so the
	// tile disappears with the archived app instead of ghosting as an error.
	if s.installer != nil {
		s.installer.ClearInstall(id)
	}
	err := s.apps.StartUninstall(id, zip)
	if errors.Is(err, apps.ErrProtected) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// handleDismissApp clears a failed install/uninstall/backup overlay for an app, so
// the operator can get rid of the red "!" without retrying the operation.
func (s *Server) handleDismissApp(w http.ResponseWriter, r *http.Request) {
	if !s.requireApps(w) {
		return
	}
	id := chi.URLParam(r, "id")
	if s.installer != nil {
		s.installer.ClearInstall(id)
	}
	s.apps.ClearUninstall(id)
	s.apps.ClearBackup(id)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleAppIcon serves an installed app's icon from its folder — the copy taken
// at install (see internal/appicon), which is what the app list points every tile
// at. 404 when the app has no copy: the client asked for a URL the list did not
// hand it, and there is nothing to fall back to here.
//
// no-cache, not no-store: the icon is unchanged between updates and worth
// revalidating rather than refetching, but a browser holding last version's icon
// for a heuristic hour after an update is exactly what this must not do.
func (s *Server) handleAppIcon(w http.ResponseWriter, r *http.Request) {
	if s.apps == nil {
		http.NotFound(w, r)
		return
	}
	path := s.apps.IconPath(chi.URLParam(r, "id"))
	if path == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, path)
}
