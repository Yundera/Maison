package apps

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// ErrProtected is returned when an uninstall targets an app the operator listed
// in PROTECTED_APPS (see config.Config.IsProtected).
var ErrProtected = errors.New("this app is protected and cannot be uninstalled")

// Uninstall phases, in order.
const (
	PhaseRemove  = "remove"  // stopping and removing the project's containers
	PhaseArchive = "archive" // renaming or zipping the app folder
	PhaseDone    = "done"
	PhaseError   = "error"
)

// UninstallEvent is a progress update emitted while an app is uninstalled. Like
// an install it has two independent tracks, which the UI shows one at a time on
// a single (red) bar: Remove (containers), then Archive (the app folder).
type UninstallEvent struct {
	Phase   string  `json:"phase"`   // remove | archive | done | error
	Message string  `json:"message"` // human-readable detail
	Remove  float64 `json:"remove"`  // container removal, 0-100
	Archive float64 `json:"archive"` // folder archiving, 0-100
}

// UninstallState is a snapshot of one in-flight (or failed) uninstall. The
// server overlays it onto the app's tile so the progress survives a reload and
// rides the live app list, exactly like installer.InstallState does.
type UninstallState struct {
	ID      string  `json:"id"`      // compose project name (== app tile id)
	Phase   string  `json:"phase"`   // remove | archive | done | error
	Message string  `json:"message"` // human-readable detail
	Remove  float64 `json:"remove"`  // container removal, 0-100
	Archive float64 `json:"archive"` // folder archiving, 0-100
	Error   string  `json:"error"`   // set when Phase == error
}

// StartUninstall launches a detached uninstall of app `id` and tracks its
// progress so it rides the live app list — the confirmation dialog can close
// immediately and the tile carries the (red) progress bars to the end.
//
// It returns only the errors that are knowable up front: a protected app. The
// uninstall itself runs on a background context, so it is NOT cancelled when the
// caller goes away; its failure lands in the tracked state (Phase == error) and
// stays on the tile until it is retried or dismissed.
//
// Idempotent: a second call while the same app is being uninstalled is a no-op.
func (r *Registry) StartUninstall(id string, zip bool) error {
	if r.Protected(id) {
		return ErrProtected
	}

	r.mu.Lock()
	if st := r.uninstalls[id]; st != nil && st.Phase != PhaseError {
		r.mu.Unlock()
		return nil // already uninstalling — attach, don't restart
	}
	r.uninstalls[id] = &UninstallState{ID: id, Phase: PhaseRemove, Message: "Queued"}
	r.mu.Unlock()
	r.changed()

	go func() {
		// Deliberately not a request context: the uninstall must outlive the
		// request that asked for it.
		_, err := r.Uninstall(context.Background(), id, zip, func(ev UninstallEvent) {
			r.mu.Lock()
			if st := r.uninstalls[id]; st != nil {
				st.Phase, st.Message, st.Remove, st.Archive = ev.Phase, ev.Message, ev.Remove, ev.Archive
			}
			r.mu.Unlock()
			r.progressed()
		})
		r.mu.Lock()
		if err != nil {
			log.Printf("uninstall %s failed: %v", id, err)
			// Keep the entry so the failure stays visible on the tile until the
			// user retries or dismisses it.
			if st := r.uninstalls[id]; st != nil {
				st.Phase, st.Error, st.Message = PhaseError, err.Error(), err.Error()
			}
		} else {
			// Success: drop the overlay. The app's folder is gone, so the tile
			// goes with it on the next snapshot.
			delete(r.uninstalls, id)
		}
		r.mu.Unlock()
		r.changed()
	}()
	return nil
}

// Uninstalls returns a snapshot of every tracked uninstall (in-flight or errored).
func (r *Registry) Uninstalls() []UninstallState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]UninstallState, 0, len(r.uninstalls))
	for _, st := range r.uninstalls {
		out = append(out, *st)
	}
	return out
}

// ClearUninstall drops a tracked uninstall (used to dismiss a failed one).
func (r *Registry) ClearUninstall(id string) {
	r.mu.Lock()
	_, existed := r.uninstalls[id]
	delete(r.uninstalls, id)
	r.mu.Unlock()
	if existed {
		r.changed()
	}
}

func (r *Registry) progressed() {
	if r.OnProgress != nil {
		r.OnProgress()
	}
}

// Uninstall stops+removes the project's containers and archives its app
// directory, emitting progress events (safe to pass a nil emit). CasaDash never
// deletes user data: the whole ${DATA_ROOT}/AppData/<id> folder (compose +
// override + .env + data) is renamed to <id>.<date>.archive, or, when zip is
// set, compressed to <id>.<date>.archive.zip and the folder removed. Either way
// the dotted name hides it from the dashboard. Returns the archive's base name
// (empty when there was nothing on disk to archive, e.g. an unmanaged stack).
func (r *Registry) Uninstall(ctx context.Context, id string, zip bool, emit func(UninstallEvent)) (string, error) {
	if emit == nil {
		emit = func(UninstallEvent) {}
	}
	if r.Protected(id) {
		return "", ErrProtected
	}
	r.enter(id)
	defer r.leave(id)

	// Track 1 — Remove: stop and remove the containers, one bar tick each. A
	// Docker failure here is deliberately swallowed (as it always was): the app
	// folder must still be archived, or the operator is left with a tile they
	// cannot get rid of.
	emit(UninstallEvent{Phase: PhaseRemove, Message: "Stopping containers"})
	if r.dx != nil {
		_ = r.dx.RemoveProject(ctx, id, func(done, total int) {
			pct := 100.0
			if total > 0 {
				pct = float64(done) / float64(total) * 100
			}
			emit(UninstallEvent{Phase: PhaseRemove, Message: "Removing containers", Remove: pct})
		})
	}
	emit(UninstallEvent{Phase: PhaseRemove, Message: "Containers removed", Remove: 100})

	appDir := filepath.Join(r.cfg.AppsDir(), id)
	if _, err := os.Stat(appDir); err != nil {
		// Nothing on disk (unmanaged) — containers already removed.
		emit(UninstallEvent{Phase: PhaseDone, Message: "Uninstalled", Remove: 100, Archive: 100})
		return "", nil
	}

	stamp := time.Now().Format("2006-01-02")
	base := uniqueName(r.cfg.AppsDir(), id+"."+stamp+".archive")

	// Track 2 — Archive. A rename is instantaneous, so its bar simply completes;
	// a zip is metered by bytes, since it is the one step that can run for
	// minutes on a large app.
	if zip {
		zipName := base + ".zip"
		zipPath := filepath.Join(r.cfg.AppsDir(), zipName)
		emit(UninstallEvent{Phase: PhaseArchive, Message: "Compressing " + zipName, Remove: 100})
		err := archiveDir(appDir, zipPath, func(copied, total int64) {
			if total <= 0 {
				return
			}
			emit(UninstallEvent{
				Phase:   PhaseArchive,
				Message: "Compressing " + zipName,
				Remove:  100,
				Archive: float64(copied) / float64(total) * 100,
			})
		})
		if err != nil {
			return "", fmt.Errorf("archive app: %w", err)
		}
		if err := os.RemoveAll(appDir); err != nil {
			return zipName, fmt.Errorf("remove app dir: %w", err)
		}
		emit(UninstallEvent{Phase: PhaseDone, Message: "Uninstalled", Remove: 100, Archive: 100})
		return zipName, nil
	}

	emit(UninstallEvent{Phase: PhaseArchive, Message: "Archiving " + base, Remove: 100})
	if err := os.Rename(appDir, filepath.Join(r.cfg.AppsDir(), base)); err != nil {
		return "", fmt.Errorf("archive app: %w", err)
	}
	emit(UninstallEvent{Phase: PhaseDone, Message: "Uninstalled", Remove: 100, Archive: 100})
	return base, nil
}

// uniqueName returns name, or name with a "-HHMMSS" suffix inserted before any
// extension, if a same-day archive already exists in dir — so uninstalling the
// same app twice in one day never clobbers the earlier archive.
func uniqueName(dir, name string) string {
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		return name
	}
	return name + "-" + time.Now().Format("150405")
}
