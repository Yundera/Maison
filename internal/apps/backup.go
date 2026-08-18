package apps

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
)

// Backup phases, in order. A folder backup skips `compress`.
const (
	PhaseCopy     = "copy"     // mirroring the app folder while the app is still up
	PhaseSync     = "sync"     // app stopped: copying the delta the first pass missed
	PhaseStart    = "start"    // bringing the app back up
	PhaseCompress = "compress" // zipping the snapshot (zip backups only)
	PhaseRestore  = "restore"  // putting an archive back as the live app folder
)

// Headroom multipliers applied to the app folder's measured size when deciding
// whether a backup fits. A folder backup needs one copy; a zip needs the snapshot
// *and* the zip on disk at the same time, and a zip of incompressible data is no
// smaller than its input — so the worst case is two.
//
// Filling the data disk does not just fail the backup: every other app on the box
// starts failing writes. Refusing early is the whole point of measuring.
const (
	folderHeadroom = 1.1
	zipHeadroom    = 2.0
)

// BackupEvent is a progress update emitted while an app is backed up. Like an
// uninstall it has independent tracks that the UI shows one at a time on a single
// bar: Copy (the live pass), Sync (the stopped pass), then Compress.
type BackupEvent struct {
	Phase    string  `json:"phase"`    // copy | sync | start | compress | done | error
	Message  string  `json:"message"`  // human-readable detail
	Copy     float64 `json:"copy"`     // first (live) mirror pass, 0-100
	Sync     float64 `json:"sync"`     // second (stopped) mirror pass, 0-100
	Compress float64 `json:"compress"` // zipping the snapshot, 0-100
}

// BackupState is a snapshot of one in-flight (or failed) backup or restore. The
// server overlays it onto the app's tile so the progress survives a reload and
// rides the live app list, exactly like UninstallState does.
type BackupState struct {
	ID       string  `json:"id"`       // compose project name (== app tile id)
	Phase    string  `json:"phase"`    // copy | sync | start | compress | restore | done | error
	Message  string  `json:"message"`  // human-readable detail
	Copy     float64 `json:"copy"`     // first (live) mirror pass, 0-100
	Sync     float64 `json:"sync"`     // second (stopped) mirror pass, 0-100
	Compress float64 `json:"compress"` // zipping the snapshot, 0-100
	Error    string  `json:"error"`    // set when Phase == error
}

// Estimate is the up-front answer to "can this app be backed up right now",
// shown in the confirmation dialog before anything is copied.
type Estimate struct {
	Size   int64 `json:"size"`   // measured size of the app folder
	Needed int64 `json:"needed"` // free space required, including headroom
	Free   int64 `json:"free"`   // free space on the data filesystem
	Enough bool  `json:"enough"` // Free >= Needed
	Zip    bool  `json:"zip"`    // which headroom Needed was computed with

	// Streamed is true when the engine sends the app straight to a repository
	// instead of staging a copy beside it, so the backup needs no free disk and the
	// guard below does not apply. The dialog uses this to explain why it is not
	// showing a space requirement, rather than showing "0 bytes needed".
	Streamed bool `json:"streamed"`
}

// EstimateBackup measures what a backup of `id` would cost. The size walk is
// stat-only, but it does visit every file — the dialog calls this once on open,
// not on a poll.
func (r *Registry) EstimateBackup(id string, zip bool) (Estimate, error) {
	if !projectRe.MatchString(id) {
		return Estimate{}, fmt.Errorf("invalid app name: %s", id)
	}
	appDir := filepath.Join(r.cfg.AppsDir(), id)
	if _, err := os.Stat(appDir); err != nil {
		return Estimate{}, fmt.Errorf("%s has no folder to back up", id)
	}
	size := dirSize(appDir)

	// An engine that streams to a repository needs no room beside the app, so the
	// guard is skipped entirely rather than computed and passed. This is what lets an
	// app occupying most of its own disk be backed up at all — the local engine
	// refuses it, because a full second copy genuinely does not fit.
	if !r.engine().Caps().NeedsLocalSpace {
		return Estimate{Size: size, Needed: 0, Free: freeSpace(r.cfg.DataRoot), Enough: true, Zip: zip, Streamed: true}, nil
	}

	headroom := folderHeadroom
	if zip {
		headroom = zipHeadroom
	}
	needed := int64(float64(size) * headroom)

	var free int64
	if u, err := disk.Usage(r.cfg.DataRoot); err == nil {
		free = int64(u.Free)
	} else {
		// Without a usable reading we cannot refuse honestly, so we let the backup
		// proceed and fail on ENOSPC rather than blocking it on a guess.
		log.Printf("backup: disk usage for %s: %v (skipping free-space guard)", r.cfg.DataRoot, err)
		return Estimate{Size: size, Needed: needed, Free: -1, Enough: true, Zip: zip}, nil
	}
	return Estimate{Size: size, Needed: needed, Free: free, Enough: free >= needed, Zip: zip}, nil
}

// StartBackup launches a detached backup of app `id` and tracks its progress so it
// rides the live app list — the confirmation dialog closes immediately and the
// tile carries the progress bar to the end.
//
// It returns only what is knowable up front: an unknown app, or not enough free
// space. A later failure lands in the tracked state (Phase == error) and stays on
// the tile until it is retried or dismissed.
//
// Idempotent: a second call while the same app is being backed up is a no-op.
func (r *Registry) StartBackup(id string, zip bool) error {
	est, err := r.EstimateBackup(id, zip)
	if err != nil {
		return err
	}
	if !est.Enough {
		return fmt.Errorf("not enough free space: %s needs %s, %s available",
			id, humanBytes(est.Needed), humanBytes(est.Free))
	}

	r.mu.Lock()
	if st := r.backups[id]; st != nil && st.Phase != PhaseError {
		r.mu.Unlock()
		return nil // already running — attach, don't restart
	}
	r.backups[id] = &BackupState{ID: id, Phase: PhaseCopy, Message: "Queued"}
	r.mu.Unlock()
	r.changed()

	go func() {
		// Deliberately not a request context: the backup must outlive the request
		// that asked for it.
		_, err := r.Backup(context.Background(), id, zip, r.trackBackup(id))
		r.finishBackup(id, err, "backup")
	}()
	return nil
}

// StartRestore launches a detached restore of `name` over app `id`.
//
// The live folder is not thrown away: it is archived first, which — being a
// rename within the same directory tree — costs nothing and takes no time. So a
// restore is reversible, and a folder archive being consumed by the restore is
// balanced by the archive the restore just made of what was there.
// ctx bounds the up-front lookup only, not the restore: for a remote engine that
// lookup is a subprocess against the repository, so a caller that goes away should not
// leave it running. Nothing has been touched at that point, so refusing is safe. The
// restore itself is deliberately detached below.
func (r *Registry) StartRestore(ctx context.Context, id, name string) error {
	// Resolve through the engines, not the data disk. Checking .backups/ here would
	// reject a backup that exists only in a repository — the restore path below
	// handles it perfectly well via locate() — so the check has to ask the same
	// question the restore will: does any engine have this backup.
	if _, _, err := r.locate(ctx, id, name); err != nil {
		return err
	}

	r.mu.Lock()
	if st := r.backups[id]; st != nil && st.Phase != PhaseError {
		r.mu.Unlock()
		return nil
	}
	r.backups[id] = &BackupState{ID: id, Phase: PhaseRestore, Message: "Queued"}
	r.mu.Unlock()
	r.changed()

	go func() {
		err := r.Restore(context.Background(), id, name, r.trackBackup(id))
		r.finishBackup(id, err, "restore")
	}()
	return nil
}

// trackBackup returns the emit function that copies progress into the tracked
// state and pokes the throttled progress hook.
func (r *Registry) trackBackup(id string) func(BackupEvent) {
	return func(ev BackupEvent) {
		r.mu.Lock()
		if st := r.backups[id]; st != nil {
			st.Phase, st.Message = ev.Phase, ev.Message
			st.Copy, st.Sync, st.Compress = ev.Copy, ev.Sync, ev.Compress
		}
		r.mu.Unlock()
		r.progressed()
	}
}

// finishBackup settles a tracked operation: a failure stays visible on the tile
// until the operator retries or dismisses it, a success drops the overlay.
func (r *Registry) finishBackup(id string, err error, what string) {
	r.mu.Lock()
	if err != nil {
		log.Printf("%s %s failed: %v", what, id, err)
		if st := r.backups[id]; st != nil {
			st.Phase, st.Error, st.Message = PhaseError, err.Error(), err.Error()
		}
	} else {
		delete(r.backups, id)
	}
	r.mu.Unlock()
	r.changed()
}

// Backups returns a snapshot of every tracked backup/restore (in-flight or errored).
func (r *Registry) Backups() []BackupState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]BackupState, 0, len(r.backups))
	for _, st := range r.backups {
		out = append(out, *st)
	}
	return out
}

// ClearBackup drops a tracked backup (used to dismiss a failed one).
func (r *Registry) ClearBackup(id string) {
	r.mu.Lock()
	_, existed := r.backups[id]
	delete(r.backups, id)
	r.mu.Unlock()
	if existed {
		r.changed()
	}
}

// Backup archives the app's folder without uninstalling it, and returns the new
// archive's base name.
//
// The app is stopped for the archive to be consistent — a database zipped while
// its process is writing restores as corruption — but it is stopped for as little
// time as possible, by copying twice:
//
//	copy      app up      full mirror into a staging folder     (no downtime)
//	stop      ────────────────────────────────────────────────  downtime starts
//	sync      app down    only what changed since the first pass
//	start     ────────────────────────────────────────────────  downtime ends
//	compress  app up      zip the snapshot (zip backups only)
//
// So downtime is proportional to what the app wrote *during* the first pass, not
// to how big it is. The snapshot itself is consistent either way: every byte in it
// was read either before the app touched it again, or while the app was down.
//
// The restart is deferred, so a failure anywhere after the stop still brings the
// app back up. Leaving an app down is a worse outcome than a missing backup.
func (r *Registry) Backup(ctx context.Context, id string, zip bool, emit func(BackupEvent)) (string, error) {
	return r.BackupWith(ctx, r.engine(), id, zip, emit)
}

// BackupWith runs a backup through a named engine rather than the configured one.
//
// It exists for the update path, which needs a rollback point it can restore by
// *rename*: an update is undone in the seconds after it broke something, and a
// download is not that. Everything else should call Backup and let the user's choice
// of engine stand.
func (r *Registry) BackupWith(ctx context.Context, p Provider, id string, zip bool, emit func(BackupEvent)) (string, error) {
	if emit == nil {
		emit = func(BackupEvent) {}
	}
	if !projectRe.MatchString(id) {
		return "", fmt.Errorf("invalid app name: %s", id)
	}
	appDir := filepath.Join(r.cfg.AppsDir(), id)
	if _, err := os.Stat(appDir); err != nil {
		return "", fmt.Errorf("%s has no folder to back up", id)
	}

	r.enter(id)
	defer r.leave(id)

	opts := SnapshotOpts{Zip: zip}
	stamp := time.Now().Format(StampLayout)

	// Nothing the engine stages is durable until Commit, so an interrupted backup
	// discards it rather than leaving something a later List might offer for restore.
	// Registered before the stop below, so it runs *after* the restart: bringing the
	// app back up always takes precedence over cleaning up.
	committed := false
	defer func() {
		if !committed {
			if err := p.Abort(context.WithoutCancel(ctx), id, stamp); err != nil {
				log.Printf("backup %s: discard incomplete backup: %v", id, err)
			}
		}
	}()

	// Pass 1 — the app is still serving. This is the long one.
	emit(BackupEvent{Phase: PhaseCopy, Message: "Copying " + id})
	opts.Pass = 1
	if err := p.Snapshot(ctx, id, stamp, opts, func(ev Event) {
		emit(BackupEvent{Phase: PhaseCopy, Message: ev.Message, Copy: ev.Pct})
	}); err != nil {
		return "", fmt.Errorf("copy app folder: %w", err)
	}
	emit(BackupEvent{Phase: PhaseCopy, Message: "Copied", Copy: 100})

	// Stop only if it is actually up, so a backup of an already-stopped app does
	// not start it afterwards.
	wasRunning := r.isRunning(ctx, id)
	if wasRunning {
		emit(BackupEvent{Phase: PhaseSync, Message: "Stopping " + id, Copy: 100})
		if err := r.dx.StopProject(ctx, id); err != nil {
			return "", fmt.Errorf("stop app: %w", err)
		}
		defer func() {
			emit(BackupEvent{Phase: PhaseStart, Message: "Starting " + id, Copy: 100, Sync: 100})
			if err := r.EnsureStarted(context.WithoutCancel(ctx), id); err != nil {
				log.Printf("backup %s: restart after backup: %v", id, err)
			}
		}()
	}

	// From here until the restart the app is down, so the window is bounded.
	//
	// The whole reason the direct shape gives up provider-independence is that a
	// hanging repository now extends an outage instead of merely failing a backup.
	// This turns that back into "the backup failed, the app is up": the engine's
	// container is killed and removed (internal/engine), the deferred restart runs,
	// and the failure lands on the tile.
	//
	// It has no effect on the local engine, whose copy is a local file walk that does
	// not watch the context — which is correct, because a local copy cannot hang on
	// anything but the disk itself.
	downCtx := ctx
	if wasRunning {
		var cancel context.CancelFunc
		downCtx, cancel = context.WithTimeout(ctx, r.stoppedPassTimeout())
		defer cancel()
	}

	// Pass 2 — the app is down, so whatever this copies is the last word.
	emit(BackupEvent{Phase: PhaseSync, Message: "Syncing changes", Copy: 100})
	opts.Pass = 2
	if err := p.Snapshot(downCtx, id, stamp, opts, func(ev Event) {
		emit(BackupEvent{Phase: PhaseSync, Message: ev.Message, Copy: 100, Sync: ev.Pct})
	}); err != nil {
		return "", fmt.Errorf("sync app folder: %w", err)
	}
	emit(BackupEvent{Phase: PhaseSync, Message: "Synced", Copy: 100, Sync: 100})

	// The commit runs after the deferred restart is queued but before it runs, so the
	// app is still down for it. That is deliberate for the engine whose commit does
	// real work — zipping the snapshot: the alternative, start then zip, races the app
	// writing into a folder we are not reading anyway, and buys nothing, because the
	// zip reads the staged copy rather than the app folder.
	b, err := p.Commit(downCtx, id, stamp, opts, func(ev Event) {
		emit(BackupEvent{
			Phase: PhaseCompress, Message: ev.Message,
			Copy: 100, Sync: 100, Compress: ev.Pct,
		})
	})
	if err != nil {
		return "", fmt.Errorf("finalise backup: %w", err)
	}
	committed = true
	emit(BackupEvent{Phase: PhaseDone, Message: "Backed up", Copy: 100, Sync: 100, Compress: 100})
	return b.Name, nil
}

// defaultStoppedPassTimeout bounds how long an app may be held down for a backup.
// Generous enough that a large delta over a slow uplink finishes, short enough that
// a hung repository is an inconvenience rather than an outage.
const defaultStoppedPassTimeout = 15 * time.Minute

func (r *Registry) stoppedPassTimeout() time.Duration {
	if r.StoppedPassTimeout > 0 {
		return r.StoppedPassTimeout
	}
	return defaultStoppedPassTimeout
}

// freeSpace reports free bytes on the filesystem holding dir, or -1 when it cannot
// be read — in which case callers proceed and let the write fail honestly rather
// than refusing on a guess.
func freeSpace(dir string) int64 {
	u, err := disk.Usage(dir)
	if err != nil {
		log.Printf("backup: disk usage for %s: %v (skipping free-space guard)", dir, err)
		return -1
	}
	return int64(u.Free)
}

// engine is the backup engine new backups are written to.
//
// No Engines means the built-in local one, which keeps every caller that predates
// the engine seam — and every test constructing a Registry directly — working
// unchanged. It is built per call rather than cached because it holds nothing but
// the config it was handed.
func (r *Registry) engine() Provider {
	if r.Engines != nil {
		if p := r.Engines.Writer(); p != nil {
			return p
		}
	}
	return NewLocalProvider(r.cfg)
}

// locate finds the engine that holds a backup and the backup itself.
//
// Dispatch is on where the backup *is*, never on which engine is selected — the
// rule that keeps a user's existing backups reachable after they switch engines.
func (r *Registry) locate(ctx context.Context, id, name string) (Provider, Backup, error) {
	if r.Engines != nil {
		return r.Engines.Locate(ctx, id, name)
	}
	b, _, err := resolveBackup(r.cfg.BackupsDir(), id, name)
	if err != nil {
		return nil, Backup{}, err
	}
	return NewLocalProvider(r.cfg), b, nil
}

// Restore puts a backup back as the app's live folder, without going through the
// store. The app is stopped for it and started again afterwards if it was running,
// on the same deferred-restart rule as Backup.
//
// Which of three paths it takes depends on where the backup is and whether there is
// room, never on which engine is currently selected:
//
//	on disk            rename it into place, after renaming the live folder aside.
//	                   Instantaneous, atomic, and the displaced state becomes an
//	                   archive of its own, so the restore is undoable.
//	remote, room       fetch it beside the app, then exactly the above.
//	remote, no room    write it over the live folder. Not atomic; the undo is a
//	                   snapshot taken first. See restoreInPlace.
func (r *Registry) Restore(ctx context.Context, id, name string, emit func(BackupEvent)) error {
	if emit == nil {
		emit = func(BackupEvent) {}
	}
	if !projectRe.MatchString(id) {
		return fmt.Errorf("invalid app name: %s", id)
	}
	backupsDir := r.cfg.BackupsDir()
	src, _, err := r.locate(ctx, id, name)
	if err != nil {
		return err
	}

	r.enter(id)
	defer r.leave(id)

	appDir := filepath.Join(r.cfg.AppsDir(), id)
	_, liveErr := os.Stat(appDir)
	live := liveErr == nil

	wasRunning := live && r.isRunning(ctx, id)
	if wasRunning {
		emit(BackupEvent{Phase: PhaseRestore, Message: "Stopping " + id})
		if err := r.dx.StopProject(ctx, id); err != nil {
			return fmt.Errorf("stop app: %w", err)
		}
		defer func() {
			emit(BackupEvent{Phase: PhaseStart, Message: "Starting " + id})
			if err := r.EnsureStarted(context.WithoutCancel(ctx), id); err != nil {
				log.Printf("restore %s: restart after restore: %v", id, err)
			}
		}()
	}

	downCtx := ctx
	if wasRunning {
		var cancel context.CancelFunc
		downCtx, cancel = context.WithTimeout(ctx, r.stoppedPassTimeout())
		defer cancel()
	}

	// On disk already? Then the restore is two renames and costs nothing, which is
	// what makes the local tier worth keeping for apps that fit it.
	if _, _, err := resolveBackup(backupsDir, id, name); err == nil {
		return r.restoreBySwap(downCtx, id, name, live, emit)
	}

	// Otherwise it has to come out of a repository. Downloading it beside the app
	// needs room for a second full copy; when there is room that is the better path,
	// because the swap that follows is atomic and leaves a local archive behind. When
	// there is not — the case this whole design exists for — the engine writes over
	// the live folder instead.
	est, err := r.EstimateRestore(id, name)
	if err != nil {
		return err
	}
	if est.Enough {
		emit(BackupEvent{Phase: PhaseRestore, Message: "Fetching " + name})
		if err := src.Materialize(downCtx, id, name, func(ev Event) {
			emit(BackupEvent{Phase: PhaseRestore, Message: ev.Message, Copy: ev.Pct})
		}); err != nil {
			return fmt.Errorf("fetch backup: %w", err)
		}
		return r.restoreBySwap(downCtx, id, name, live, emit)
	}
	if !src.Caps().InPlaceRestore {
		return fmt.Errorf("not enough free space to restore %s: %s needed, %s available, and %s cannot restore in place",
			id, humanBytes(est.Needed), humanBytes(est.Free), src.ID())
	}
	return r.restoreInPlace(downCtx, src, id, name, appDir, emit)
}

// restoreBySwap is the original restore: archive the live folder by renaming it —
// instantaneous, and what makes a mis-clicked restore undoable — then move the
// archive into its place.
func (r *Registry) restoreBySwap(ctx context.Context, id, name string, live bool, emit func(BackupEvent)) error {
	backupsDir := r.cfg.BackupsDir()
	if live {
		emit(BackupEvent{Phase: PhaseRestore, Message: "Archiving current state"})
		dir := AppBackupDir(backupsDir, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create backup dir: %w", err)
		}
		safety := filepath.Join(dir, time.Now().Format(StampLayout))
		if err := os.Rename(filepath.Join(r.cfg.AppsDir(), id), safety); err != nil {
			return fmt.Errorf("archive current state: %w", err)
		}
	}
	emit(BackupEvent{Phase: PhaseRestore, Message: "Restoring " + name})
	if err := RestoreBackup(backupsDir, r.cfg.AppsDir(), id, name); err != nil {
		return err
	}
	emit(BackupEvent{Phase: PhaseDone, Message: "Restored", Copy: 100, Sync: 100, Compress: 100})
	return nil
}

// restoreInPlace writes the backup over the live app folder, for an app too large
// to hold two copies of.
//
// This is the one destructive path in Maison's backup handling, and it is not
// atomic: an interruption leaves the folder neither the old state nor the new one.
// Three things make that survivable, and none of them is optional.
func (r *Registry) restoreInPlace(ctx context.Context, src Provider, id, name, appDir string, emit func(BackupEvent)) error {
	// 1. The undo. There is no rename to make a safety archive from — the live folder
	//    *is* the target — so the only way back is a snapshot of the current state.
	//    The app is already stopped, so it is consistent, and it is incremental, so it
	//    costs roughly the delta since the last backup.
	//
	//    If it fails the restore is refused. An unrecoverable overwrite is a worse
	//    outcome than a restore that did not happen.
	undo := time.Now().Format(StampLayout)
	emit(BackupEvent{Phase: PhaseRestore, Message: "Saving current state"})
	if err := src.Snapshot(ctx, id, undo, SnapshotOpts{Pass: 2}, nil); err != nil {
		return fmt.Errorf("could not save the current state, so the restore was refused: %w", err)
	}
	if _, err := src.Commit(ctx, id, undo, SnapshotOpts{}, nil); err != nil {
		return fmt.Errorf("could not save the current state, so the restore was refused: %w", err)
	}

	// 2. The marker. It lives outside the folder being written, because a restore
	//    that deletes files absent from the backup would otherwise delete it. Its
	//    name cannot parse as a stamp, so ListBackups ignores it for free.
	if err := r.markRestoring(id, name); err != nil {
		return err
	}

	emit(BackupEvent{Phase: PhaseRestore, Message: "Restoring " + name})
	if err := src.RestoreInPlace(ctx, id, name, appDir, func(ev Event) {
		emit(BackupEvent{Phase: PhaseRestore, Message: ev.Message, Copy: ev.Pct})
	}); err != nil {
		// 3. The marker stays. The app must not come back up on a half-restored
		//    folder: it would initialise over the gap — fresh database, default
		//    config — and that state would become the next night's backup.
		return fmt.Errorf("restore %s: %w (its data is incomplete; restore it again or roll back to %s)", id, err, undo)
	}
	if err := r.clearRestoring(id); err != nil {
		return err
	}
	emit(BackupEvent{Phase: PhaseDone, Message: "Restored", Copy: 100, Sync: 100, Compress: 100})
	return nil
}

// restoringMarker is written while an app's folder is being overwritten in place.
// The name deliberately fails stampRe so no lister mistakes it for an archive.
func (r *Registry) restoringMarker(id string) string {
	return filepath.Join(AppBackupDir(r.cfg.BackupsDir(), id), ".restoring")
}

func (r *Registry) markRestoring(id, name string) error {
	if err := os.MkdirAll(AppBackupDir(r.cfg.BackupsDir(), id), 0o755); err != nil {
		return err
	}
	return os.WriteFile(r.restoringMarker(id), []byte(name), 0o644)
}

func (r *Registry) clearRestoring(id string) error {
	err := os.Remove(r.restoringMarker(id))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// RestoreInterrupted reports whether an in-place restore of this app was cut short,
// leaving its folder holding neither the old state nor the new one.
func (r *Registry) RestoreInterrupted(id string) bool {
	_, err := os.Stat(r.restoringMarker(id))
	return err == nil
}

// EstimateRestore answers "is there room to fetch this backup and swap it in".
//
// It is the mirror of EstimateBackup and exists for the same reason: downloading a
// backup beside the app needs space the app itself may already be occupying. It is
// what chooses between the two restore paths.
func (r *Registry) EstimateRestore(id, name string) (Estimate, error) {
	if !projectRe.MatchString(id) {
		return Estimate{}, fmt.Errorf("invalid app name: %s", id)
	}
	_, backup, err := r.locate(context.Background(), id, name)
	if err != nil {
		return Estimate{}, err
	}
	size := backup.Size
	if size == 0 {
		// A folder archive is listed unmeasured, and a repository that reports no size
		// tells us nothing — fall back to the live app, which is the best available
		// proxy for how big its backup is.
		size = dirSize(filepath.Join(r.cfg.AppsDir(), id))
	}
	needed := int64(float64(size) * folderHeadroom)
	free := freeSpace(r.cfg.DataRoot)
	if free < 0 {
		return Estimate{Size: size, Needed: needed, Free: -1, Enough: true}, nil
	}
	return Estimate{Size: size, Needed: needed, Free: free, Enough: free >= needed}, nil
}

// isRunning reports whether any of the project's containers are up. A Docker
// hiccup answers "no", which is the safe direction: the worst case is that a
// stopped-looking app is not restarted, and the operator can start it from the
// tile.
func (r *Registry) isRunning(ctx context.Context, id string) bool {
	if r.dx == nil {
		return false
	}
	conts, err := r.dx.ListProjectContainers(ctx)
	if err != nil {
		return false
	}
	for _, c := range conts {
		if c.Project == id && c.State == "running" {
			return true
		}
	}
	return false
}

func pct(done, total int64) float64 {
	if total <= 0 {
		return 100
	}
	return float64(done) / float64(total) * 100
}

// mirror makes dst an exact copy of src, and reports progress over the bytes it
// actually transfers.
//
// It is incremental on purpose — a file already in dst with the same size and
// modification time is left alone. That is what makes the second pass cheap: it
// copies only what the app wrote during the first one, which is the entire reason
// the app's downtime is short. Files in dst that are gone from src are removed, so
// a second pass cannot leave deleted data in the snapshot.
//
// Rather than shelling out to rsync (which the runtime image does not carry), this
// is a plain two-walk implementation: it needs no external binary and reports
// bytes through the same progressReader the zip path uses.
func mirror(src, dst string, onProgress func(copied, total int64)) error {
	if onProgress == nil {
		onProgress = func(int64, int64) {}
	}

	// Measure the work first so the bar means something: only the files that will
	// actually be transferred count, so an unchanged second pass reads as instant
	// rather than as a full copy that mysteriously flies by.
	var total int64
	stale := map[string]bool{}
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if upToDate(filepath.Join(dst, rel), fi) {
			return nil
		}
		stale[rel] = true
		total += fi.Size()
		return nil
	})
	if err != nil {
		return err
	}

	var copied int64
	onProgress(0, total)

	// Pass A — create directories and copy the stale files.
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		// Sockets, fifos and devices are runtime artefacts, not data: an app that
		// leaves a socket in its folder must not make its backup unrunnable.
		if !d.Type().IsRegular() {
			return nil
		}
		if !stale[rel] {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if err := copyFile(path, target, fi, func(n int64) {
			copied += n
			onProgress(copied, total)
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	onProgress(total, total)

	// Pass B — drop anything in dst that src no longer has.
	return prune(src, dst)
}

// upToDate reports whether dst already holds this exact file. Size plus
// modification time is the same test rsync makes by default: cheap, and wrong only
// for a write that preserves both, which no app does by accident.
func upToDate(dst string, src fs.FileInfo) bool {
	fi, err := os.Stat(dst)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Size() == src.Size() && fi.ModTime().Equal(src.ModTime())
}

// copyFile writes src to dst through a temporary, then renames — so an
// interrupted copy leaves the previous version (or nothing), never a truncated
// file that upToDate would go on to accept as current. The source's mode and
// modification time are carried over, the latter because the next pass's
// up-to-date test depends on it.
func copyFile(src, dst string, fi fs.FileInfo, onRead func(n int64)) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".partial"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, &progressReader{r: in, onRead: onRead}); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chtimes(tmp, fi.ModTime(), fi.ModTime()); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// prune removes everything under dst that no longer exists under src. Directories
// are handled after their contents, so removing a whole deleted subtree takes one
// RemoveAll at its root rather than a walk.
func prune(src, dst string) error {
	entries, err := os.ReadDir(dst)
	if err != nil {
		return err
	}
	for _, e := range entries {
		target := filepath.Join(dst, e.Name())
		// A .partial left by a failed copy belongs to no source file; drop it.
		origin := filepath.Join(src, strings.TrimSuffix(e.Name(), ".partial"))
		if _, err := os.Stat(origin); err != nil {
			if err := os.RemoveAll(target); err != nil {
				return err
			}
			continue
		}
		if e.IsDir() {
			if err := prune(filepath.Join(src, e.Name()), target); err != nil {
				return err
			}
		}
	}
	return nil
}

// humanBytes formats a byte count for an error message the operator reads.
func humanBytes(n int64) string {
	if n < 0 {
		return "unknown"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}
