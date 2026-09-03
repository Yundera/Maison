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

// ErrProtected is returned when a stop or an uninstall targets a system app —
// one whose compose declares `view: system` (see Registry.Protected).
var ErrProtected = errors.New("this is a system app and cannot be stopped or uninstalled")

// Uninstall phases, in order.
//
// The order is the point of the sequence: capture the data, make it durable, then tear
// down. Nothing is destroyed before the backup this uninstall produces is committed.
const (
	PhaseBackup  = "backup"  // stopping the app and backing it up through the engine
	PhaseArchive = "archive" // finalising the archive (a zip's compression)
	PhaseRemove  = "remove"  // removing the containers, then the app folder
	PhaseDone    = "done"
	PhaseError   = "error"
)

// UninstallEvent is a progress update emitted while an app is uninstalled. Like an
// install it has independent tracks, which the UI shows one at a time on a single
// (red) bar, in the order the phases above run: Backup, Archive, then Remove.
//
// Backup is the track that can run for a long time — on a remote engine it is an
// upload. It did not exist before because an uninstall did not produce a backup: it
// renamed the app folder aside and called that an archive, which is a recycle bin
// rather than a backup, and left nothing offsite on a box configured for offsite.
type UninstallEvent struct {
	Phase   string  `json:"phase"`   // backup | archive | remove | done | error
	Message string  `json:"message"` // human-readable detail
	Backup  float64 `json:"backup"`  // snapshot through the default engine, 0-100
	Archive float64 `json:"archive"` // finalising the archive, 0-100
	Remove  float64 `json:"remove"`  // containers and the app folder, 0-100
}

// UninstallState is a snapshot of one in-flight (or failed) uninstall. The
// server overlays it onto the app's tile so the progress survives a reload and
// rides the live app list, exactly like installer.InstallState does.
type UninstallState struct {
	ID      string  `json:"id"`      // compose project name (== app tile id)
	Phase   string  `json:"phase"`   // backup | archive | remove | done | error
	Message string  `json:"message"` // human-readable detail
	Backup  float64 `json:"backup"`  // snapshot through the default engine, 0-100
	Archive float64 `json:"archive"` // finalising the archive, 0-100
	Remove  float64 `json:"remove"`  // containers and the app folder, 0-100
	Error   string  `json:"error"`   // set when Phase == error
}

// StartUninstall launches a detached uninstall of app `id` and tracks its
// progress so it rides the live app list — the confirmation dialog can close
// immediately and the tile carries the (red) progress bars to the end.
//
// It returns only the errors that are knowable up front: a system app. The
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
	r.uninstalls[id] = &UninstallState{ID: id, Phase: PhaseBackup, Message: "Queued"}
	r.mu.Unlock()
	r.changed()

	go func() {
		// Deliberately not a request context: the uninstall must outlive the
		// request that asked for it.
		_, err := r.Uninstall(context.Background(), id, zip, func(ev UninstallEvent) {
			r.mu.Lock()
			if st := r.uninstalls[id]; st != nil {
				st.Phase, st.Message = ev.Phase, ev.Message
				st.Backup, st.Archive, st.Remove = ev.Backup, ev.Archive, ev.Remove
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

// Uninstall backs the app up through the default engine and then removes it,
// emitting progress events (safe to pass a nil emit). It returns the new backup's
// name (empty when there was nothing on disk to back up, e.g. an unmanaged stack).
//
// Maison never deletes user data, and this is where that promise is kept — so it is a
// real backup, written wherever the user's chosen engine writes:
//
//	stop      app down — one stop, and the app never comes back up on success
//	backup    Snapshot through the default engine, with Consume set
//	archive   Commit — the commit point; on a zip, the compression
//	remove    the containers, then the app folder if the engine did not take it
//
// **Nothing is destroyed before Commit returns**, and that ordering is the feature.
// Until then a failure anywhere restarts the app and leaves it installed with its data
// untouched, which is why the containers are stopped here rather than removed: removing
// them first would make "put it back" mean re-creating the stack, and it is what the
// previous version did before archiving unconditionally.
//
// What it replaces renamed the app folder into .backups and called that the backup. On
// the local engine that is still exactly what happens — SnapshotOpts.Consume makes the
// commit a single rename, so an uninstall stays instant and free at any size. On a
// remote engine it now uploads first and drops the folder after, which is the whole
// point: a box whose backups are offsite used to get nothing offsite from an uninstall,
// while the settings page promised that it would.
//
// A failure to reach the engine fails the uninstall rather than falling back to a local
// archive. The fallback is the more tempting behaviour and the wrong one: it puts the
// data somewhere other than where the user was told it goes, at the exact moment the
// tile disappears and stops being able to say so. The error stays on the tile, and the
// escape hatch is to point the default engine at this server's disk and retry.
func (r *Registry) Uninstall(ctx context.Context, id string, zip bool, emit func(UninstallEvent)) (string, error) {
	if emit == nil {
		emit = func(UninstallEvent) {}
	}
	if r.Protected(id) {
		return "", ErrProtected
	}
	if !projectRe.MatchString(id) {
		return "", fmt.Errorf("invalid app name: %s", id)
	}
	r.enter(id)
	defer r.leave(id)

	appDir := filepath.Join(r.cfg.AppsDir(), id)
	if _, err := os.Stat(appDir); err != nil {
		// Nothing on disk (an unmanaged stack): there is no data to back up, so this is
		// only a container removal. Done first, before any engine is resolved, so an
		// unmanaged stack stays removable on a box whose repository is unreachable.
		emit(UninstallEvent{Phase: PhaseRemove, Message: "Removing containers"})
		r.removeContainers(ctx, id, emit, 0)
		emit(UninstallEvent{Phase: PhaseDone, Message: "Uninstalled", Backup: 100, Archive: 100, Remove: 100})
		return "", nil
	}

	// The default engine, never a named one. An uninstall is not a "back this up over
	// there" request — there is no dialog to choose in and nobody to choose — so it
	// writes where the schedule writes, which is what the settings page says it does.
	p, err := r.engineFor("")
	if err != nil {
		return "", err
	}

	// Consume: this app's folder is going away, so an engine that can take it wholesale
	// should, rather than copying it first. Pass 2 because a stopped app gets one pass
	// and that pass is the *consistent* one — committing a pass-1 snapshot would
	// discard it as the torn throwaway it normally is.
	// The app's own exclusions apply here as they do to any other backup — no special
	// case. What that means differs by engine, and deliberately: kopia's ignore rules
	// live on the source path, so its uninstall snapshot honours them; the local
	// engine's uninstall is a *rename* of the folder, so its archive keeps everything.
	// A superset that costs nothing to produce is not worth an exception to remove.
	opts := SnapshotOpts{Pass: 2, Zip: zip, Consume: true, Exclude: r.excludeSet(id)}
	stamp := time.Now().Format(StampLayout)

	// Nothing staged is durable until Commit, so an interrupted uninstall discards it
	// rather than leaving something a later List would offer for restore. Registered
	// before the stop below so it runs after the restart: bringing the app back up
	// always takes precedence over cleaning up.
	committed := false
	defer func() {
		if !committed {
			if err := p.Abort(context.WithoutCancel(ctx), id, stamp); err != nil {
				log.Printf("uninstall %s: discard incomplete backup: %v", id, err)
			}
		}
	}()

	// Stopped, not removed. On the success path the containers are removed a few lines
	// down and the distinction never shows; on every failure path it is what lets the
	// app simply start again.
	wasRunning := r.isRunning(ctx, id)
	if wasRunning {
		emit(UninstallEvent{Phase: PhaseBackup, Message: "Stopping " + id})
		if err := r.dx.StopProject(ctx, id); err != nil {
			return "", fmt.Errorf("stop app: %w", err)
		}
		defer func() {
			// Only if we did not finish. A successful uninstall has no app to restart, and
			// starting one whose folder has just been archived would recreate it empty.
			if committed {
				return
			}
			if err := r.EnsureStarted(context.WithoutCancel(ctx), id); err != nil {
				log.Printf("uninstall %s: restart after failed uninstall: %v", id, err)
			}
		}()
	}

	// From here until the app is gone it is down, so the window is bounded for the same
	// reason the backup path bounds its stopped pass: a hung repository must turn into
	// "the uninstall failed, the app is up", not an outage.
	downCtx := ctx
	if wasRunning {
		var cancel context.CancelFunc
		downCtx, cancel = context.WithTimeout(ctx, r.stoppedPassTimeout())
		defer cancel()
	}

	emit(UninstallEvent{Phase: PhaseBackup, Message: "Backing up " + id})
	if err := p.Snapshot(downCtx, id, stamp, opts, func(ev Event) {
		emit(UninstallEvent{Phase: PhaseBackup, Message: ev.Message, Backup: max(ev.Pct, 0)})
	}); err != nil {
		return "", fmt.Errorf("back up app: %w", err)
	}
	emit(UninstallEvent{Phase: PhaseBackup, Message: "Backed up", Backup: 100})

	b, err := p.Commit(downCtx, id, stamp, opts, func(ev Event) {
		emit(UninstallEvent{Phase: PhaseArchive, Message: ev.Message, Backup: 100, Archive: max(ev.Pct, 0)})
	})
	if err != nil {
		return "", fmt.Errorf("finalise backup: %w", err)
	}
	committed = true
	emit(UninstallEvent{Phase: PhaseArchive, Message: "Archived", Backup: 100, Archive: 100})

	// Past the commit point: the data is safe, so what follows is cleanup and its
	// failures must not fail the uninstall. A Docker error here is swallowed exactly as
	// it always was — the app folder is already gone, so refusing now would leave a tile
	// the operator cannot get rid of.
	emit(UninstallEvent{Phase: PhaseRemove, Message: "Removing containers", Backup: 100, Archive: 100})
	r.removeContainers(ctx, id, emit, 100)

	// The engine took the folder, or it did not and this removes it. The local engine's
	// commit is a rename, so there is nothing here; an engine that streamed a copy to a
	// repository leaves the original behind for exactly this step.
	if _, err := os.Stat(appDir); err == nil {
		emit(UninstallEvent{Phase: PhaseRemove, Message: "Removing app folder", Backup: 100, Archive: 100, Remove: 100})
		if err := os.RemoveAll(appDir); err != nil {
			// Reported, not returned: the backup is committed and the containers are gone,
			// so the uninstall did happen. A folder that could not be removed is a disk
			// problem for the operator, not a reason to claim the app is still installed.
			log.Printf("uninstall %s: remove app folder: %v", id, err)
		}
	}

	emit(UninstallEvent{Phase: PhaseDone, Message: "Uninstalled", Backup: 100, Archive: 100, Remove: 100})
	return b.Name, nil
}

// removeContainers stops and removes the project's containers, ticking the Remove
// track once per container. done is what the earlier tracks are already at, so the bar
// does not appear to restart the operation.
//
// Its Docker error is swallowed by every caller, which is why it returns none: by the
// time it runs the data is already safe, and an app whose containers will not go away
// must still lose its tile or the operator has no way to be rid of it.
func (r *Registry) removeContainers(ctx context.Context, id string, emit func(UninstallEvent), earlier float64) {
	if r.dx == nil {
		return
	}
	_ = r.dx.RemoveProject(ctx, id, func(done, total int) {
		p := 100.0
		if total > 0 {
			p = float64(done) / float64(total) * 100
		}
		emit(UninstallEvent{
			Phase: PhaseRemove, Message: "Removing containers",
			Backup: earlier, Archive: earlier, Remove: p,
		})
	})
}
