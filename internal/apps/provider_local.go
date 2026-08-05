package apps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yundera/maison/internal/config"
)

// LocalProvider is the built-in backup engine: archives on the data disk under
// .backups/<app>/<stamp>, exactly as Maison has always written them.
//
// It lives in this package rather than under internal/backup because it is a
// wrapper around this package's own unexported machinery — mirror, archiveDir, the
// staging discipline — not a new implementation of it. Moving it out would mean
// exporting that machinery to one caller, which is a worse trade than one file
// here.
//
// It needs no configuration and no external binary, which is what makes it the
// default engine and the one that is always available. Its distinguishing
// limitation is the one the README states: the archive is on the same disk as the
// app, so it is a rollback mechanism, not disaster recovery.
type LocalProvider struct {
	cfg config.Config
}

// NewLocalProvider builds the built-in engine. It cannot fail: there is nothing to
// connect to.
func NewLocalProvider(cfg config.Config) *LocalProvider { return &LocalProvider{cfg: cfg} }

func (p *LocalProvider) ID() string { return EngineLocal }

func (p *LocalProvider) Caps() Caps {
	return Caps{
		// Not offsite: same disk as the apps. This is the whole reason another engine
		// exists.
		Offsite: false,
		// A folder archive is renamed into place, so restoring it costs nothing and
		// takes no time. That is worth keeping for apps that fit it.
		InstantRestore: true,
		// A backup is a full second copy of the app folder, so it needs room for one.
		// This is what makes an app larger than the free disk un-backup-able by this
		// engine — see EstimateBackup's headroom.
		NeedsLocalSpace: true,
		// Restoring is a rename of a whole folder, which cannot be done over a live
		// folder without first having staged the replacement — so there is no 1x path
		// here.
		InPlaceRestore: false,
		// Maison prunes local archives itself; there is no policy engine to delegate to.
		Retention: false,
	}
}

// stagingDir is where a backup accumulates before it is committed. The leading dot
// is load-bearing: stampRe rejects the name, so a snapshot interrupted by a crash
// is inert — it can never be mistaken for a finished archive by ListBackups or
// offered for restore.
func (p *LocalProvider) stagingDir(app, stamp string) string {
	return filepath.Join(AppBackupDir(p.cfg.BackupsDir(), app), ".staging-"+stamp)
}

// Snapshot mirrors the app folder into the staging directory.
//
// Both passes mirror into the *same* staging directory, and that is what makes the
// second one cheap: mirror skips any file already present with the same size and
// modification time, so pass 2 copies only what the app wrote during pass 1. The
// app's downtime is therefore proportional to that delta rather than to its size.
func (p *LocalProvider) Snapshot(ctx context.Context, app, stamp string, opts SnapshotOpts, emit func(Event)) error {
	if !projectRe.MatchString(app) {
		return fmt.Errorf("invalid app name: %s", app)
	}
	appDir := filepath.Join(p.cfg.AppsDir(), app)
	if _, err := os.Stat(appDir); err != nil {
		return fmt.Errorf("%s has no folder to back up", app)
	}
	staging := p.stagingDir(app, stamp)
	if err := os.MkdirAll(filepath.Dir(staging), 0o755); err != nil {
		return fmt.Errorf("create backup dir: %w", err)
	}
	// Only the first pass starts from nothing. Clearing on pass 2 would throw away
	// everything pass 1 copied and turn the stopped pass into a full copy — the exact
	// cost the two-pass shape exists to avoid.
	if opts.Pass <= 1 {
		if err := os.RemoveAll(staging); err != nil {
			return fmt.Errorf("clear staging: %w", err)
		}
	}
	return mirror(appDir, staging, func(copied, total int64) {
		emitPct(emit, pct(copied, total), "Copying "+app)
	})
}

// Commit turns the staged copy into a real archive.
//
// A folder archive is a rename, which consumes the staging directory and is
// instantaneous — .backups lives inside AppData, so the rename never crosses a
// filesystem. A zip is written to a dotted temporary and renamed only once whole,
// so an interrupted compress can never be restored as though it had finished; the
// staging directory is then scratch and is removed.
func (p *LocalProvider) Commit(ctx context.Context, app, stamp string, opts SnapshotOpts, emit func(Event)) (Backup, error) {
	dir := AppBackupDir(p.cfg.BackupsDir(), app)
	staging := p.stagingDir(app, stamp)

	if !opts.Zip {
		if err := os.Rename(staging, filepath.Join(dir, stamp)); err != nil {
			return Backup{}, fmt.Errorf("finalise backup: %w", err)
		}
		return p.backupFor(app, stamp)
	}

	name := stamp + ".zip"
	tmp := filepath.Join(dir, "."+name+".partial")
	emitPct(emit, 0, "Compressing "+name)
	if err := archiveDir(staging, tmp, func(copied, total int64) {
		emitPct(emit, pct(copied, total), "Compressing "+name)
	}); err != nil {
		_ = os.Remove(tmp)
		return Backup{}, fmt.Errorf("compress backup: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmp)
		return Backup{}, fmt.Errorf("finalise backup: %w", err)
	}
	// The zip is the artefact; the staged copy was only its input.
	_ = os.RemoveAll(staging)
	return p.backupFor(app, name)
}

// Abort drops an uncommitted staged copy.
func (p *LocalProvider) Abort(ctx context.Context, app, stamp string) error {
	return os.RemoveAll(p.stagingDir(app, stamp))
}

// List returns the app's on-disk archives. Sizes are left unmeasured for folder
// archives — walking one means walking a tree that can be terabytes — so callers
// that display sizes wrap this in Measure.
func (p *LocalProvider) List(ctx context.Context, app string) ([]Backup, error) {
	return ListBackups(p.cfg.BackupsDir(), app), nil
}

// Materialize is a no-op: this engine's backups are already on local disk, which is
// the whole point of it. It still resolves the name, so that asking for something
// that is not there fails here rather than further down.
func (p *LocalProvider) Materialize(ctx context.Context, app, stamp string, emit func(Event)) error {
	_, _, err := resolveBackup(p.cfg.BackupsDir(), app, stamp)
	return err
}

// RestoreInPlace is not available here. A folder archive is restored by renaming it
// over the app, which needs the replacement to exist beside the app first — so this
// engine has no path that avoids a second copy. Caps().InPlaceRestore says so, and
// callers fall back to refusing with an explanation.
func (p *LocalProvider) RestoreInPlace(ctx context.Context, app, stamp, dst string, emit func(Event)) error {
	return ErrNotSupported
}

func (p *LocalProvider) Delete(ctx context.Context, app, stamp string) error {
	return DeleteBackup(p.cfg.BackupsDir(), app, stamp)
}

// backupFor re-reads a just-written archive so the returned Backup carries the same
// fields a later List would report — rather than being assembled by hand here and
// drifting from it.
func (p *LocalProvider) backupFor(app, name string) (Backup, error) {
	b, _, err := resolveBackup(p.cfg.BackupsDir(), app, name)
	if err != nil {
		return Backup{}, err
	}
	return b, nil
}

// emitPct is the small adapter between the byte-counting callbacks this package's
// copy helpers use and the Event a provider reports.
func emitPct(emit func(Event), p float64, msg string) {
	if emit == nil {
		return
	}
	emit(Event{Pct: p, Message: msg})
}
