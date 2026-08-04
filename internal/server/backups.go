package server

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/shirou/gopsutil/v4/disk"

	"github.com/yundera/maison/internal/apps"
)

// Backups have two surfaces, because they outlive the app they belong to.
//
//   - /api/apps/{id}/… is the per-app view, reached from the app's Backups tab.
//     It only exists while the app does.
//   - /api/backups is the global view. It is the only way to reach the archives of
//     an app that has been uninstalled — which has no folder, so no tile, so no
//     tab — and the only place the disk cost of all backups is visible at once.
//
// Both read and write the same directory (config.BackupsDir); neither knows or
// cares whether an archive came from an uninstall or from an explicit backup.

// handleAppBackups lists one app's archives, with folder archives measured — the
// tab shows a size against every row, and a row reading "folder · —" tells the
// operator nothing about whether deleting it is worth doing.
//
// That measurement is a tree walk per archive, which is why the store's
// install-click path deliberately does not do it (see apps.ListBackups). Here it
// is affordable for the same reason the estimate below is: this tab is opened by
// hand, one app at a time.
func (s *Server) handleAppBackups(w http.ResponseWriter, r *http.Request) {
	dir := s.cfg.BackupsDir()
	list := apps.Measure(dir, apps.ListBackups(dir, chi.URLParam(r, "id")))
	if list == nil {
		list = []apps.Backup{}
	}
	writeJSON(w, http.StatusOK, list)
}

// handleBackupEstimate answers "can this app be backed up right now" for the
// confirmation dialog: measured size, free space, and whether the one fits in the
// other with headroom. The dialog calls it on open, not on a poll — it walks the
// app folder.
func (s *Server) handleBackupEstimate(w http.ResponseWriter, r *http.Request) {
	if !s.requireApps(w) {
		return
	}
	est, err := s.apps.EstimateBackup(chi.URLParam(r, "id"), r.URL.Query().Get("zip") == "true")
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, est)
}

// handleStartBackup starts a backup and returns immediately: the work runs
// detached and its progress is overlaid on the app's tile, so the dialog never
// blocks on a copy that takes minutes. Only the up-front refusals (unknown app,
// not enough free space) are reported here; a later failure lands on the tile.
func (s *Server) handleStartBackup(w http.ResponseWriter, r *http.Request) {
	if !s.requireApps(w) {
		return
	}
	// zip=true compresses the snapshot and keeps only the zip; otherwise the
	// snapshot itself is the backup (docs/lifecycle.md).
	zip := r.URL.Query().Get("zip") == "true"
	if err := s.apps.StartBackup(chi.URLParam(r, "id"), zip); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// handleStartRestore restores an archive over the live app, detached like a
// backup. The app's current state is archived first, so this is reversible.
func (s *Server) handleStartRestore(w http.ResponseWriter, r *http.Request) {
	if !s.requireApps(w) {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := s.apps.StartRestore(chi.URLParam(r, "id"), body.Name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// handleGlobalBackups is the Backups settings page: every app that has archives,
// with orphans marked and folder archives measured. Deliberately the expensive
// read — it is what answers "what is eating the disk", and it is opened by hand.
func (s *Server) handleGlobalBackups(w http.ResponseWriter, _ *http.Request) {
	list := apps.ListAll(s.cfg.BackupsDir(), s.cfg.AppsDir())
	if list == nil {
		list = []apps.AppBackups{}
	}
	out := map[string]any{"apps": list}
	// Free space is part of this page rather than a separate call: deciding whether
	// to delete a backup is deciding against the space it frees.
	if u, err := disk.Usage(s.cfg.DataRoot); err == nil {
		out["free"] = u.Free
		out["total"] = u.Total
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDeleteBackup removes one archive. This is the only endpoint in Maison
// that destroys user data, so it takes the app and the archive by name and
// validates both (apps.DeleteBackup): nothing outside the backups tree, and
// nothing that is not an archive, is reachable through it.
func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	app := chi.URLParam(r, "app")
	name := chi.URLParam(r, "name")
	if err := apps.DeleteBackup(s.cfg.BackupsDir(), app, name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleRestoreOrphan restores an archive for an app with no live folder, from
// the global page. It reuses the same detached path as a per-app restore, which
// handles the no-current-folder case by simply having nothing to archive first —
// and once the folder lands, the app has a tile again.
func (s *Server) handleRestoreOrphan(w http.ResponseWriter, r *http.Request) {
	if !s.requireApps(w) {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := s.apps.StartRestore(chi.URLParam(r, "app"), body.Name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}
