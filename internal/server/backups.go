package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/shirou/gopsutil/v4/disk"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/backup"
)

// Backups have two surfaces, because they outlive the app they belong to.
//
//   - /api/apps/{id}/… is the per-app view, reached from the app's Backups tab.
//     It only exists while the app does.
//   - /api/backups is the global view. It is the only way to reach the archives of
//     an app that has been uninstalled — which has no folder, so no tile, so no
//     tab — and the only place the disk cost of all backups is visible at once.
//
// Both go through the engine set, so both show every backup this box can restore
// whatever wrote it, and neither knows or cares whether an archive came from an
// uninstall, an explicit backup or the nightly run. The engine is a setting; it
// governs which engine new backups are *written* to and nothing else.
//
// Reading through the set rather than off the disk is what makes a remote backup a
// first-class one. Listing the data disk directly — which these handlers used to do —
// meant a box configured for kopia showed an empty Backups page while its repository
// held everything, and that is the exact situation the page has to be right about.

// handleAppBackups lists one app's backups across every engine, with local folder
// archives measured — the tab shows a size against every row, and a row reading
// "folder · —" tells the operator nothing about whether deleting it is worth doing.
//
// That measurement is a tree walk per archive, which is why the store's
// install-click path deliberately does not do it (see apps.ListBackups). Here it
// is affordable for the same reason the estimate below is: this tab is opened by
// hand, one app at a time.
func (s *Server) handleAppBackups(w http.ResponseWriter, r *http.Request) {
	app := chi.URLParam(r, "id")
	list := apps.Measure(s.cfg.BackupsDir(), s.engines.List(r.Context(), app))
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
	// Engine names which copy to restore. Two engines can hold the same stamp — a
	// local archive and an offsite snapshot are different backups — so the row the
	// user clicked carries it. Empty still works and means "whichever engine has it",
	// which is what the store's install path relies on.
	var body struct {
		Name   string `json:"name"`
		Engine string `json:"engine"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := s.apps.StartRestore(r.Context(), chi.URLParam(r, "id"), body.Engine, body.Name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// handleGlobalBackups is the Backups settings page: every app with backups in any
// engine, with orphans marked and local folder archives measured. Deliberately the
// expensive read — it is what answers "what is eating the disk", and it is opened by
// hand.
func (s *Server) handleGlobalBackups(w http.ResponseWriter, r *http.Request) {
	list := s.engines.ListAll(r.Context(), s.cfg.BackupsDir(), s.cfg.AppsDir())
	if list == nil {
		list = []apps.AppBackups{}
	}
	out := map[string]any{"apps": list, "user_data": s.userDataView(r.Context())}

	// What the app backups cost *on this disk*, which is not the sum of their sizes:
	// a backup that exists only in a repository takes no space here, and adding its
	// size to a figure printed next to "free" would describe a disk that does not
	// exist. Only local and both-tier rows count.
	var localUsed int64
	for _, g := range list {
		for _, b := range g.Backups {
			if b.Engine == apps.EngineLocal {
				localUsed += b.Size
			}
		}
	}
	out["local_used"] = localUsed

	// Free space is part of this page rather than a separate call: deciding whether
	// to delete a backup is deciding against the space it frees.
	if u, err := disk.Usage(s.cfg.DataRoot); err == nil {
		out["free"] = u.Free
		out["total"] = u.Total
	}
	writeJSON(w, http.StatusOK, out)
}

// userDataView is the "your files" card: the set's snapshots, whether the box can do
// this at all, and any restore in flight.
type userDataView struct {
	// Available is false on a box that cannot back this set up — most often a default
	// install, where the local engine is selected and cannot write the set anywhere
	// useful. Reason says which case it is, because an empty list otherwise reads as
	// "nothing to worry about".
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`

	Backups []apps.Backup `json:"backups"`
	// Size is the newest snapshot's size — what the set currently *is* — and
	// deliberately not the sum across snapshots. Snapshots are incremental, so summing
	// thirty nightly copies of a media library reports tens of terabytes that were
	// never stored and are not on any disk.
	Size int64 `json:"size"`

	// Source and Excluded describe what the set actually covers, so a restore that does
	// not bring something back is diagnosable rather than mysterious.
	Source   string   `json:"source"`
	Excluded []string `json:"excluded"`

	Restore backup.RestoreState `json:"restore"`
}

func (s *Server) userDataView(ctx context.Context) userDataView {
	out := userDataView{
		Backups:  []apps.Backup{},
		Source:   s.cfg.DataRoot,
		Excluded: backup.UserDataExclusions,
	}
	if s.userData == nil {
		out.Reason = "Backups are not available on this box."
		return out
	}
	out.Available, out.Reason = s.userData.Available()
	out.Restore = s.userData.State()
	if list := s.userData.List(ctx); len(list) > 0 {
		out.Backups = list
		out.Size = list[0].Size // newest first
	}
	return out
}

// handleRestoreUserData restores the user-data set, detached like an app restore.
//
// The destructive mode is the default spelling — an empty destination means in place —
// so the client has to say nothing special to do the dangerous thing. That is
// deliberate: the danger is in the operation, not in the spelling, and the guards that
// make it survivable live in backup.UserData.Restore where no caller can skip them.
func (s *Server) handleRestoreUserData(w http.ResponseWriter, r *http.Request) {
	if s.userData == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "backups are not available"})
		return
	}
	var body struct {
		Name    string   `json:"name"`
		Dest    string   `json:"dest"`
		Entries []string `json:"entries"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	// Not r.Context(): the restore outlives the request that asked for it, exactly like
	// an app backup.
	err := s.userData.Restore(context.Background(), body.Name,
		apps.UserDataRestoreOpts{Dest: body.Dest, Entries: body.Entries}, nil)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// handleDeleteBackup removes one backup from ONE engine. This is the only endpoint in
// Maison that destroys user data, so it takes the app and the backup by name and
// validates both: the local engine refuses anything outside the backups tree or that
// is not an archive name (apps.DeleteBackup), and a remote one re-validates the stamp
// on the way in.
//
// The engine is required, and deliberately has no "delete everywhere" default. A user
// clearing space on the data disk must not have the offsite copy taken with it — that
// copy is the entire disaster-recovery story, and a delete that silently spanned
// engines would destroy it while appearing to tidy a local folder.
func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	app := chi.URLParam(r, "app")
	name := chi.URLParam(r, "name")
	engine := r.URL.Query().Get("engine")
	if engine == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "engine is required"})
		return
	}
	if err := s.engines.Delete(r.Context(), engine, app, name); err != nil {
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
		Name   string `json:"name"`
		Engine string `json:"engine"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := s.apps.StartRestore(r.Context(), chi.URLParam(r, "app"), body.Engine, body.Name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}
