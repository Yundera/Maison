package apps

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// The backup an app update takes, and the restore that undoes a failed one.
//
// An update backs the app up to the same destinations as everything else — every
// enabled engine (Engines.Writers) — then updates the app in place, on the data
// already on disk, and if the new version does not come up, restores that backup over
// the app. There is no separate "rollback copy": a repository that already holds the
// app's nightly history takes the update's backup as one more incremental snapshot,
// and a box backing up offsite does not need a second copy of every app on its own
// disk to update it.
//
// The local engine is one destination among the others. It is the fastest to restore
// from (a rename), so it is preferred when it holds the backup; when the app does not
// fit beside itself it is left out of that one backup rather than refusing the update,
// so long as another destination takes it.

// ErrUpdating reports that the app is being updated, so a backup or restore of it has
// to wait: either would stop and restart the app under the update.
var ErrUpdating = errors.New("this app is being updated")

// NoRoomError is an update backup refused because no enabled destination can take it:
// the local engine needs Needed bytes beside the app and Free are free, and nothing
// that streams elsewhere is enabled.
type NoRoomError struct {
	Needed, Free int64
}

func (e *NoRoomError) Error() string {
	return fmt.Sprintf("a backup needs %s of free disk on this server, %s is free, and no other backup destination is enabled",
		humanBytes(e.Needed), humanBytes(e.Free))
}

// ReserveUpdate marks id as being updated until release is called. While it is held,
// the nightly run skips the app and a backup or restore of it is refused — each would
// stop and restart the app in the middle of the update. It fails with
// ErrBackupInFlight when a backup or restore of the app is already running, so the
// update is refused rather than racing it.
func (r *Registry) ReserveUpdate(id string) (release func(), err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.backups[id]; st != nil && st.Phase != PhaseError {
		return nil, fmt.Errorf("%s: %w", id, ErrBackupInFlight)
	}
	if r.updating[id] {
		return nil, fmt.Errorf("%s: %w", id, ErrUpdating)
	}
	r.updating[id] = true
	return func() {
		r.mu.Lock()
		delete(r.updating, id)
		r.mu.Unlock()
	}, nil
}

// UpdateTargets is where an update's backup of id goes: every enabled engine, less a
// local one the app does not fit beside. Dropped describes what was left out, for the
// update's warning. The errors:
//
//	ErrBackupSkipped   the app declares backup.skip — no backup, by design
//	ErrNoBackupEngine  no destination is enabled — the owner switched backups off
//	*NoRoomError       destinations are enabled, and none can take this backup
//
// It walks the app folder only when a target needs room beside it, and is cheap to
// call twice: once before the image pull so a refusal costs nothing, once again for
// the backup itself.
func (r *Registry) UpdateTargets(id string) (targets []Provider, dropped string, err error) {
	if r.BackupSkipped(id) {
		return nil, "", ErrBackupSkipped
	}
	all, err := r.enginesFor("")
	if err != nil {
		return nil, "", err
	}
	var room *NoRoomError
	for _, p := range all {
		if !p.Caps().NeedsLocalSpace {
			targets = append(targets, p)
			continue
		}
		est, err := r.EstimateBackup(id, p.ID(), false)
		if err != nil || est.Enough {
			// Unmeasurable: let the copy try and fail honestly on ENOSPC rather than
			// dropping a destination on a guess.
			targets = append(targets, p)
			continue
		}
		room = &NoRoomError{Needed: est.Needed, Free: est.Free}
	}
	if len(targets) == 0 && room != nil {
		return nil, "", room
	}
	if room != nil {
		dropped = fmt.Sprintf("not backed up to this server's disk before updating: it needs %s, %s is free",
			humanBytes(room.Needed), humanBytes(room.Free))
	}
	return targets, dropped, nil
}

// RestoreOver puts back the backup an update took, over the app, after the update
// failed. Nothing of the failed state is kept: the update's backup is the state to
// return to, and an archive of the broken one is a second copy of the app on the disk
// for nobody (a 110 GB one filled watch.nsl.sh's disk a week after a failed update).
//
// The app is already stopped — the failed converge left it so — and is not started
// here; the caller starts it and watches it.
//
// It tries every enabled engine holding the backup, the one that restores by rename
// first, and moves to the next when one fails: a backup written to several
// destinations is several chances to put the app back.
//
//	local      rename the live folder into the trash, rename the backup into place.
//	           Instant, and needs no free disk.
//	in place   the engine writes the backup over the live folder and deletes what the
//	           backup does not hold. No second copy on disk, so no room needed —
//	           which is the point: the app that did not fit beside itself is exactly
//	           the one being restored. No time limit either: the app is down already,
//	           and abandoning a restore half-way is the worst available outcome.
//	fetch      an engine that cannot restore in place downloads beside the app first,
//	           when there is room for it.
func (r *Registry) RestoreOver(ctx context.Context, id, name string) error {
	if !projectRe.MatchString(id) {
		return fmt.Errorf("invalid app name: %s", id)
	}
	if _, ok := r.ownsBackupsDir(id); ok {
		return ErrHoldsBackups
	}
	r.enter(id)
	defer r.leave(id)

	sources := r.restoreSources(ctx, id, name)
	if len(sources) == 0 {
		return fmt.Errorf("backup not found: %s", name)
	}
	var errs []error
	for _, src := range sources {
		err := r.restoreOverFrom(ctx, src, id, name)
		if err == nil {
			return nil
		}
		log.Printf("update %s: restoring %s from %s failed: %v", id, name, src.ID(), err)
		errs = append(errs, fmt.Errorf("%s: %w", src.ID(), err))
	}
	return errors.Join(errs...)
}

// restoreSources is every engine holding id's backup name, the ones that restore by
// rename first, then in registration order.
func (r *Registry) restoreSources(ctx context.Context, id, name string) []Provider {
	if r.Engines == nil {
		local := NewLocalProvider(r.cfg)
		if _, _, err := resolveBackup(r.cfg.BackupsDir(), id, name); err == nil {
			return []Provider{local}
		}
		return nil
	}
	var out []Provider
	for _, p := range r.Engines.Writers() {
		if src, _, err := r.Engines.LocateIn(ctx, p.ID(), id, name); err == nil {
			out = append(out, src)
		}
	}
	// A destination disabled since the backup was taken still holds it.
	if len(out) == 0 {
		if src, _, err := r.Engines.Locate(ctx, id, name); err == nil {
			out = append(out, src)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Caps().InstantRestore && !out[j].Caps().InstantRestore
	})
	return out
}

func (r *Registry) restoreOverFrom(ctx context.Context, src Provider, id, name string) error {
	appDir := filepath.Join(r.cfg.AppsDir(), id)
	if src.Caps().InstantRestore {
		return r.swapOver(id, name, appDir)
	}
	if src.Caps().InPlaceRestore {
		if err := r.markRestoring(id, name); err != nil {
			return err
		}
		if err := src.RestoreInPlace(ctx, id, name, appDir, func(Event) {}); err != nil {
			// The marker stays: the app must not come back up on a half-restored
			// folder. See restoreInPlace.
			return fmt.Errorf("%w (its data is incomplete; restore %s again from Backups)", err, name)
		}
		return r.clearRestoring(id)
	}
	est, err := r.EstimateRestore(id, src.ID(), name)
	if err != nil {
		return err
	}
	if !est.Enough {
		return fmt.Errorf("not enough free space to fetch the backup: %s needed, %s available, and %s cannot restore in place",
			humanBytes(est.Needed), humanBytes(est.Free), src.ID())
	}
	if err := src.Materialize(ctx, id, name, func(Event) {}); err != nil {
		return fmt.Errorf("fetch backup: %w", err)
	}
	return r.swapOver(id, name, appDir)
}

// swapOver renames the live folder into the trash and the archive into its place.
// The trashed folder is removed in the background once the archive is in place, like
// a deleted archive, and a restart mid-removal is finished by SweepBackupTrash. A
// restore that fails puts the live folder back, so the next source can try.
func (r *Registry) swapOver(id, name, appDir string) error {
	var trash string
	if _, err := os.Stat(appDir); err == nil {
		dir := AppBackupDir(r.cfg.BackupsDir(), id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create backup dir: %w", err)
		}
		trash = filepath.Join(dir, fmt.Sprintf("%sfailed-update-%d", trashPrefix, time.Now().UnixNano()))
		if err := os.Rename(appDir, trash); err != nil {
			return fmt.Errorf("move the failed state aside: %w", err)
		}
	}
	if err := RestoreBackup(r.cfg.BackupsDir(), r.cfg.AppsDir(), id, name); err != nil {
		if trash != "" {
			if _, statErr := os.Stat(appDir); os.IsNotExist(statErr) {
				if backErr := os.Rename(trash, appDir); backErr != nil {
					log.Printf("update %s: putting the failed state back after a failed restore: %v", id, backErr)
				}
			}
		}
		return err
	}
	if trash != "" {
		removeTrash(trash)
	}
	return nil
}
