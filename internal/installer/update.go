package installer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/yundera/maison/internal/appstore"
	"github.com/yundera/maison/internal/composefile"
	"github.com/yundera/maison/internal/incident"
	"github.com/yundera/maison/internal/stackup"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// UpdateStatus reports whether a managed app has a pending store update. It is
// derived by diffing the store's current (transformed) docker-compose.yml against
// the copy on disk — the same strict base Maison brought the app up from.
type UpdateStatus struct {
	// HasRef is true when the app records where it was installed from (so an
	// update can be resolved at all). Apps installed before this feature, or
	// unmanaged stacks, have no reference.
	HasRef bool `json:"has_ref"`
	// Available is true when the store's compose differs from the installed one.
	Available bool `json:"available"`
	// Ref is the whole reference as one locator — `<store>/-/<folder>/<app id>` —
	// which is what the Update tab shows and takes back when the app is pointed at
	// a different store. The three fields below are the same reference taken apart,
	// kept because a caller that wants only the store should not have to parse.
	Ref string `json:"ref"`
	// Store is the reference store URL; StoreAppID the catalog id within it;
	// StoreAppsPath the folder inside the archive, when it is not the default.
	Store         string `json:"store"`
	StoreAppID    string `json:"store_app_id"`
	StoreAppsPath string `json:"store_apps_path,omitempty"`
	// Blocked is set, and Available still true, when reaching the store's version
	// crosses a manual data migration; DataPlan carries what the update would run.
	DataPlan
	// Error carries a non-fatal lookup failure (store unreachable, app pulled from
	// the catalog, …) so the UI can explain why a check couldn't complete.
	Error string `json:"error,omitempty"`
}

// CheckUpdate resolves the app's store reference and reports whether the store's
// current docker-compose.yml differs from the one on disk. A missing/unreachable
// store is surfaced via UpdateStatus.Error rather than as a hard error, so the
// Update tab can render a message instead of failing.
func (in *Installer) CheckUpdate(ctx context.Context, project string) (UpdateStatus, error) {
	dir := filepath.Join(in.cfg.AppsDir(), project)
	current, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	if err != nil {
		return UpdateStatus{}, err // not a managed app (no strict base on disk)
	}

	ref := in.readUpdateRef(project)
	if ref.ID == "" {
		return UpdateStatus{HasRef: false}, nil
	}
	st := statusOf(ref)

	newBase, err := in.store.AppComposeFrom(ctx, ref)
	if err != nil {
		st.Error = err.Error()
		return st, nil
	}
	st.Available = !bytes.Equal(current, newBase)
	if st.Available {
		// Available stays the byte comparison; Blocked or Error say why it would be refused.
		st.DataPlan, _, st.Error = describeData(dir, newBase)
	}
	return st, nil
}

// UpdateResult is what an update did.
type UpdateResult struct {
	// Applied is false when the app was already current.
	Applied bool `json:"applied"`
	// Backup names the backup taken before the update, empty when none was.
	Backup string `json:"backup,omitempty"`
	// RolledBack is true when the update failed and the app was put back.
	RolledBack bool `json:"rolled_back,omitempty"`
	// Warning explains an update applied without a backup (NoBackup, or no destination
	// enabled), a destination left out of the backup, or a rollback that itself failed.
	// It is not an error — the update still happened — but it is the thing the
	// operator most needs to see.
	Warning string `json:"warning,omitempty"`
}

// ErrNoRollback marks an update refused because its backup could not be taken — no
// enabled destination with room for it, a backup of the app already running, or the
// backup itself failing. Nothing was changed. The caller may retry that one app with
// UpdateOptions.NoBackup, which is an owner's explicit choice and never what "update
// all" does.
var ErrNoRollback = errors.New("no backup before the update")

// ErrBusy is what a ReserveUpdate failure is wrapped in: a backup or restore of the
// app is running, and the update would stop the app under it.
var ErrBusy = errors.New("a backup or restore of this app is running")

// ErrAlreadyUpdating is what a ReserveUpdate failure returns when the app is held by
// another update: the same update asked for twice (a double click, a second tab). It is
// refused quietly — no incident — because the update already running is the one that
// says how it went, and a "could not be backed up" warning over it would be both wrong
// and the last word if it then succeeds.
var ErrAlreadyUpdating = errors.New("this app is already being updated")

// UpdateOptions are the choices an owner makes for one update.
type UpdateOptions struct {
	// NoBackup applies the update without taking a backup, so a failed update cannot
	// be undone. The owner's choice, for one app at a time: the app's Update tab
	// offers it up front (a "Backup" box, ticked by default), and it is the way past
	// an update refused with ErrNoRollback.
	NoBackup bool

	// Budget bounds the update from the moment the old version is stopped: writing,
	// migrating and starting the new one. The backup before it is not counted — a
	// first backup of a large app to a repository takes as long as the upload does,
	// and the app keeps running through it (its stopped pass has its own limit). Zero
	// is no bound beyond the caller's context.
	Budget time.Duration
}

// ApplyUpdate pulls the store's current docker-compose.yml, and — if it differs
// from the installed copy — pulls its images, backs the app up to every enabled
// backup destination, stops the old version, overwrites the strict base and brings
// the stack back up (base + override) with `docker compose up -d`, in place, on the
// data already on disk. The user's override and .env are untouched.
//
// An update is the most common way an app breaks, and it is the one destructive
// change Maison makes on the user's behalf, so it takes a backup first and restores
// it over the app if bringing it up fails.
func (in *Installer) ApplyUpdate(ctx context.Context, project string, opts UpdateOptions) (UpdateResult, error) {
	var res UpdateResult
	dir := filepath.Join(in.cfg.AppsDir(), project)
	composePath := filepath.Join(dir, "docker-compose.yml")
	current, err := os.ReadFile(composePath)
	if err != nil {
		return res, err
	}

	ref := in.readUpdateRef(project)
	if ref.ID == "" {
		return res, fmt.Errorf("no update reference recorded for %q", project)
	}

	app, newBase, err := in.store.AppSyncedFrom(ctx, ref)
	if err != nil {
		return res, err
	}
	if bytes.Equal(current, newBase) {
		return res, nil // already up to date — nothing to do
	}

	// Held for the whole update, so the nightly run cannot stop and restart the app
	// under it. A backup already running refuses the update for free: nothing has been
	// pulled, backed up or stopped yet.
	if in.ReserveUpdate != nil {
		release, err := in.ReserveUpdate(project)
		if errors.Is(err, ErrAlreadyUpdating) {
			log.Printf("update %s: refused, already being updated", project)
			return res, err
		}
		if err != nil {
			return res, in.refuseNoRollback(project, fmt.Errorf("%w: %v", ErrBusy, err))
		}
		defer release()
	}

	// From here on the tile shows which step the update is on (UpdateState), and the
	// bar goes away however the update ends.
	defer in.endUpdate(project)
	step := func(phase, msg string, pct float64) {
		in.setUpdate(project, UpdateState{Phase: phase, Message: msg, Pct: pct})
	}

	// Which data the new version expects, against which data the app holds — read
	// while the old compose is still on disk, since it is the baseline for an install
	// with no marker. First of the refusals: each one here costs nothing.
	plan, err := in.planUpdateData(project, dir, newBase, opts)
	if err != nil {
		return res, err
	}

	// Refused before the image pull, so an app no destination can take a backup of
	// costs a size walk, not a download followed by a refusal. The walk is stat-only,
	// and only made when the local disk is one of the destinations.
	if !opts.NoBackup && in.BackupRoom != nil {
		step(UpdatePhaseCheck, "Checking where the backup can go", 0)
		var room *RoomError
		if err := in.BackupRoom(ctx, project); errors.As(err, &room) {
			return res, in.refuseNoRollback(project, err)
		}
	}

	// The new version's images, pulled while the old version is still serving: the stop
	// below would otherwise cost the whole download, not just the swap. Not fatal —
	// `compose up` pulls whatever is still missing, and fails loudly if it cannot.
	if f, err := composefile.Parse(newBase); err == nil {
		in.pullList(ctx, append(composeImages(f), migrationImages(plan)...), func(ev Event) { step(UpdatePhasePull, ev.Message, ev.Download) })
	}

	// The backup, before anything is written: to every enabled destination, the same
	// ones the nightly run writes to. A repository that holds the app's history takes
	// it as one more incremental snapshot.
	//
	// A backup that cannot be taken REFUSES the update: nothing has been stopped or
	// written yet, so refusing costs nothing, and going ahead would make the one
	// destructive change Maison makes irreversible without anyone having chosen that.
	// The owner can choose it — UpdateOptions.NoBackup, per app — which is what keeps
	// an app whose backup keeps failing from being pinned on an old version.
	switch {
	case opts.NoBackup:
		res.Warning = "updated without a backup, as requested: a failed update cannot be undone"
		log.Printf("update %s: %s", project, res.Warning)
	case in.BackupBeforeUpdate != nil:
		step(UpdatePhaseBackup, "Backing up", 0)
		name, warning, err := in.BackupBeforeUpdate(ctx, project, func(st UpdateState) {
			st.Phase = UpdatePhaseBackup
			in.setUpdate(project, st)
		})
		if err != nil {
			return res, in.refuseNoRollback(project, err)
		}
		res.Backup, res.Warning = name, warning
		if warning != "" {
			log.Printf("update %s: %s", project, warning)
		}
	}
	// An app that opts out of backups, or a box with no destination enabled, gets no
	// backup even when one is asked for. Nothing has been stopped or written yet, so a
	// conversion that could not be undone is still refused for free.
	if len(plan.Steps) > 0 && res.Backup == "" {
		return res, in.refuseData(project, &DataRefusedError{Reason: DataRefusedNoBackup,
			Title:  "it converts the app's data, which needs a backup",
			Detail: "This update converts the app's data, and no backup was taken to undo it — the app does not take backups, or no backup destination is enabled."})
	}

	// The budget starts here: the backup above is the app's own time, and the app was
	// running through all of it.
	if opts.Budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Budget)
		defer cancel()
	}

	// Stop the old version before anything of the new one runs. The new version's init
	// steps run in pre_up against the app's data, and the old containers still have that
	// data open — taking the backup restarts them on its way out — so a database
	// that takes an exclusive lock fails every step that opens it (FileBrowser's bolt
	// answers "timeout") and the update is rolled back for nothing. Nothing has been
	// written yet, so a stop that fails needs no undo.
	if in.StopBeforeUpdate != nil {
		step(UpdatePhaseStop, "Stopping the current version", 100)
		if err := in.StopBeforeUpdate(ctx, project); err != nil {
			log.Printf("update %s: stop before update: %v", project, err)
			return res, fmt.Errorf("stop %s before updating: %w", project, err)
		}
	}

	step(UpdatePhaseApply, "Writing the new version", 100)
	if err := os.WriteFile(composePath, newBase, 0o644); err != nil {
		return res, err
	}
	// The seed tree comes from the same sync as the compose above, so an update
	// that adds, changes or drops a seed file is reflected before the stack comes
	// back up. Files already written into the app folder are not touched — seeding
	// is create-if-absent — so an update cannot silently revert an app's config.
	// A failure here rolls back with everything else: the restore replaces the
	// whole folder, SeedDir included.
	if err := writeSeed(app, dir); err != nil {
		return in.rollBack(ctx, project, res, err)
	}
	// The icon comes from that same sync, so an update that changes the app's icon
	// changes the tile. Unlike the seed tree it cannot fail the update: a tile with
	// last version's icon is not a reason to roll an app back.
	writeIcon(ctx, app, dir, project)
	// The migration scripts, from the same sync again, so a script step runs the
	// script its compose was written against.
	if err := writeMigrations(app, dir); err != nil {
		return in.rollBack(ctx, project, res, err)
	}

	// stackup.Up creates any folder the updated compose newly introduces — declared
	// or bind-derived — before bringing the stack back up, so the new version can
	// write to it exactly as it would on a fresh install.
	files := []string{composePath}
	if override := filepath.Join(dir, "docker-compose.override.yml"); fileExists(override) {
		files = append(files, override)
	}
	// No measured track: converging and `compose up` report no progress, so the bar
	// sits full while the label says what is happening (as a backup's start does).
	step(UpdatePhaseStart, "Starting the new version", 100)
	up := stackup.UpOptions{}
	if len(plan.Steps) > 0 {
		up.Migrate = &plan
		up.Progress = func(i, n int, m xcomposeapp.Migration) {
			step(UpdatePhaseMigrate, fmt.Sprintf("Migrating data (%d/%d): %s", i, n, m.Label()), float64(i-1)/float64(n)*100)
		}
	}
	if err := stackup.UpWith(ctx, in.cfg, project, dir, files, up); err != nil {
		return in.rollBack(ctx, project, res, err)
	}
	res.Applied = true
	// A working update clears whatever the last one left behind.
	in.resolve("app.update:" + project)
	return res, nil
}

// rollBack puts the app back after a failed update, by restoring the backup taken
// before it over the app.
//
// The restore replaces the whole folder, so it takes the old compose with it — the
// app returns to exactly the state the backup captured, not to a new compose running
// against old data. Nothing of the failed state is kept.
//
// A failed rollback is reported alongside the failure that caused it rather than
// replacing it: the operator needs to know both that the update failed *and* that
// the app is now in neither state.
//
// Every way out of here leaves an incident, not only the ones where the rollback went
// wrong. The error also reaches the tile, but a tile is read only by whoever is looking
// at it when the update fails.
func (in *Installer) rollBack(ctx context.Context, project string, res UpdateResult, cause error) (UpdateResult, error) {
	log.Printf("update %s failed: %v", project, cause)
	if in.RollBack == nil || res.Backup == "" {
		in.report(incident.Report{
			ID: "app.update:" + project, Kind: incident.KindAppUpdate, Severity: incident.Critical,
			Title:  project + " is broken after a failed update",
			Detail: "The update failed and there was no backup to put the old version back from:\n" + cause.Error() + "\n\nThe new version is in place and did not come up. Its logs, from the tile's menu, will say why.",
			Args:   map[string]string{"app": project, "reason": "broken"},
		})
		return res, fmt.Errorf("update failed and could not be undone: %w", cause)
	}
	// Deliberately not the request's context: it may already be cancelled by the
	// failure, and abandoning a rollback half-done is the worst available outcome.
	in.setUpdate(project, UpdateState{Phase: UpdatePhaseRollback, Message: "Restoring the backup", Pct: 100})
	if err := in.RollBack(context.WithoutCancel(ctx), project, res.Backup); err != nil {
		res.Warning = "the update failed AND restoring the backup failed: " + err.Error()
		log.Printf("update %s: %s", project, res.Warning)
		// The worst state Maison can leave an app in: neither the old version nor the
		// new one. Nothing else in this codebase is more deserving of an alert.
		in.report(incident.Report{
			ID: "app.update:" + project, Kind: incident.KindAppUpdate, Severity: incident.Critical,
			Title:  project + " is broken after a failed update",
			Detail: "The update failed and restoring the backup taken before it failed too:\n" + err.Error() + "\n\nThe app is in neither state and needs attention. Its previous version is in the backup named " + res.Backup + ", which can be restored from the app's Backups tab.",
			Args:   map[string]string{"app": project, "reason": "rollback_failed"},
		})
		return res, fmt.Errorf("update failed and the rollback failed too (%v): %w", err, cause)
	}
	res.RolledBack = true

	// Put back is not running. The restore returns the old compose and the old data, but
	// whatever broke the update can be in that data, or in what the restore put back.
	if in.VerifyRunning != nil {
		if err := in.VerifyRunning(context.WithoutCancel(ctx), project); err != nil {
			res.Warning = "the update was rolled back, but the previous version is not running: " + err.Error()
			log.Printf("update %s: %s", project, res.Warning)
			in.report(incident.Report{
				ID: "app.update:" + project, Kind: incident.KindAppUpdate, Severity: incident.Critical,
				Title:  project + " is down after a failed update",
				Detail: "The update failed:\n" + cause.Error() + "\n\nThe previous version was restored from the backup named " + res.Backup + ", but it is not running: " + err.Error() + "\n\nIts logs, from the tile's menu, will say why.",
				Args:   map[string]string{"app": project, "reason": "not_running"},
			})
			return res, fmt.Errorf("update failed and was rolled back, but %s is not running (%v): %w", project, err, cause)
		}
	}
	// Rolled back and running is still a failed update: the app is on the old version,
	// and the store keeps offering the one that just failed.
	in.report(incident.Report{
		ID: "app.update:" + project, Kind: incident.KindAppUpdate, Severity: incident.Warning,
		Title:  project + " could not be updated",
		Detail: "The update failed and the previous version was restored from its backup; it is running.\n\n" + cause.Error() + "\n\nThe store will keep offering this update. If it fails the same way again, the store's version of the app needs a fix.",
		Args:   map[string]string{"app": project, "reason": "rolled_back"},
	})
	return res, fmt.Errorf("update failed and was rolled back: %w", cause)
}

// readUpdateRef reads the store reference recorded in the app's override
// x-compose-app block. A zero ID means the app has no reference.
//
// An app installed before store-apps-path existed records no folder, and reads
// back with the default — which is what it was installed from, since the default
// was the only thing there was.
func (in *Installer) readUpdateRef(project string) appstore.Ref {
	dir := filepath.Join(in.cfg.AppsDir(), project)
	f, err := composefile.Load(filepath.Join(dir, "docker-compose.override.yml"))
	if err != nil {
		return appstore.Ref{}
	}
	ca, err := f.ComposeApp()
	if err != nil {
		return appstore.Ref{}
	}
	if ca.StoreRef != "" {
		return appstore.ParseRef(ca.StoreRef)
	}
	// The superseded three-field spelling, still read so an app installed before
	// store-ref keeps updating from where it came from. Its next retarget — or its
	// next install from the store — writes the single locator instead.
	return appstore.Ref{URL: ca.Store, AppsPath: ca.StoreAppsPath, ID: ca.StoreAppID}
}

// statusOf is the part of an UpdateStatus that is just the reference, rendered
// both ways. One place, so the whole-locator form and the fields taken apart
// cannot drift.
func statusOf(ref appstore.Ref) UpdateStatus {
	return UpdateStatus{
		HasRef:        true,
		Ref:           ref.Path(),
		Store:         ref.URL,
		StoreAppID:    ref.ID,
		StoreAppsPath: ref.AppsPath,
	}
}

// writeUpdateRef merges the store reference into the app's override x-compose-app
// block, preserving any existing override content (user edits, webui-* fields).
// This is what lets the Update tab find a newer version later.
func writeUpdateRef(dir string, ref appstore.Ref) error {
	if ref.ID == "" {
		return nil // nothing to reference (e.g. a manual/unmanaged install)
	}
	overridePath := filepath.Join(dir, "docker-compose.override.yml")

	doc := map[string]any{}
	if raw, err := os.ReadFile(overridePath); err == nil {
		_ = yaml.Unmarshal(raw, &doc)
		if doc == nil {
			doc = map[string]any{}
		}
	}

	xca, _ := doc["x-compose-app"].(map[string]any)
	if xca == nil {
		xca = map[string]any{}
	}
	xca["store-ref"] = ref.Path()
	// The three-field spelling this replaces. Removed rather than left behind: two
	// records of where an app updates from, one of them stale after a retarget, is
	// the kind of thing that is only noticed once it has sent an app back to the
	// store it was moved off. readUpdateRef still reads them, for the apps that
	// have not been through here since.
	delete(xca, "store")
	delete(xca, "store-app-id")
	delete(xca, "store-apps-path")
	doc["x-compose-app"] = xca

	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(overridePath, out, 0o644)
}

// SetUpdateRef points an app at a different store, app or folder: it parses the
// reference, checks that it actually resolves, and records it in the app's
// override. The stack is NOT recreated — the reference says where the next update
// comes from and nothing Docker can see changes — so what comes back is the fresh
// update status against the new store, which is the next thing the operator wants
// to know.
//
// The reference is resolved before it is written, so a typo fails here, in the box
// it was typed into, rather than later as a "couldn't check" on a tile.
func (in *Installer) SetUpdateRef(ctx context.Context, project, refStr string) (UpdateStatus, error) {
	dir := filepath.Join(in.cfg.AppsDir(), project)
	if _, err := os.Stat(filepath.Join(dir, "docker-compose.yml")); err != nil {
		return UpdateStatus{}, err // not a managed app: nothing to update from a store
	}

	ref, err := appstore.ParseUserRef(refStr)
	if err != nil {
		return UpdateStatus{}, err
	}
	current, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	if err != nil {
		return UpdateStatus{}, err
	}
	_, newBase, err := in.store.AppSyncedFrom(ctx, ref)
	if err != nil {
		return UpdateStatus{}, fmt.Errorf("%s: %w", ref.Path(), err)
	}
	if err := writeUpdateRef(dir, ref); err != nil {
		return UpdateStatus{}, err
	}

	st := statusOf(ref)
	st.Available = !bytes.Equal(current, newBase)
	if st.Available {
		// Available stays the byte comparison; Blocked or Error say why it would be refused.
		st.DataPlan, _, st.Error = describeData(dir, newBase)
	}
	return st, nil
}

// Why an update was refused for want of a backup. Carried on the error, the run item
// and the incident's args, so the Updates page can say it in plain words instead of
// relaying an engine's error string.
const (
	// NoRollbackNoRoom: no enabled destination can take the backup — the local disk
	// is the only one, and the copy would not fit (Needed > Free).
	NoRollbackNoRoom = "no_room"
	// NoRollbackTimeout: the backup ran out of time — in practice the stopped pass of
	// an app with a very large number of files outrunning its downtime budget.
	NoRollbackTimeout = "backup_timeout"
	// NoRollbackBusy: a backup or restore of the app was already running. Retrying
	// once it is done is the whole answer.
	NoRollbackBusy = "backup_busy"
	// NoRollbackFailed: the backup failed for any other reason.
	NoRollbackFailed = "backup_failed"
)

// RoomError is what a BackupRoom check or the backup returns when no enabled
// destination can take it: the local disk's numbers are what let the owner see how
// far off it is.
type RoomError struct {
	Needed, Free int64
}

func (e *RoomError) Error() string {
	return fmt.Sprintf("the backup needs %s of free disk on this server, %s is free, and no other backup destination is enabled",
		humanBytes(e.Needed), humanBytes(e.Free))
}

// NoRollbackError is an update refused because its backup could not be taken.
// errors.Is(err, ErrNoRollback) holds for it.
type NoRollbackError struct {
	Reason       string // NoRollbackNoRoom | NoRollbackTimeout | NoRollbackBusy | NoRollbackFailed
	Needed, Free int64  // NoRollbackNoRoom only
	Cause        error
}

func (e *NoRollbackError) Error() string {
	var why string
	switch e.Reason {
	case NoRollbackNoRoom:
		why = "no backup destination can take the backup (" + e.Cause.Error() + ")"
	case NoRollbackTimeout:
		why = "the backup took too long"
	case NoRollbackBusy:
		why = "a backup or restore of this app is running"
	default:
		why = "the backup failed: " + e.Cause.Error()
	}
	return "not updated, nothing was changed: " + why
}

func (e *NoRollbackError) Is(target error) bool { return target == ErrNoRollback }
func (e *NoRollbackError) Unwrap() error        { return e.Cause }

// classifyNoRollback turns the error from ReserveUpdate, BackupRoom or
// BackupBeforeUpdate into a NoRollbackError with its reason.
func classifyNoRollback(cause error) *NoRollbackError {
	e := &NoRollbackError{Reason: NoRollbackFailed, Cause: cause}
	var room *RoomError
	switch {
	case errors.As(cause, &room):
		e.Reason, e.Needed, e.Free = NoRollbackNoRoom, room.Needed, room.Free
	case errors.Is(cause, ErrBusy):
		e.Reason = NoRollbackBusy
	case errors.Is(cause, context.DeadlineExceeded):
		e.Reason = NoRollbackTimeout
	}
	return e
}

// refuseNoRollback reports an update refused for want of a backup. The app is
// untouched, so it is a warning, and the incident says how to go ahead anyway — with
// advice that fits the cause: an error that leaves the owner no next step, or the
// wrong one, is a design bug.
func (in *Installer) refuseNoRollback(project string, cause error) error {
	err := classifyNoRollback(cause)
	log.Printf("update %s: refused (%s): %v", project, err.Reason, cause)
	var advice string
	switch err.Reason {
	case NoRollbackNoRoom:
		advice = "Free some disk, enable an offsite backup destination in Settings › Backups, or"
	case NoRollbackTimeout:
		advice = "The backup ran out of time — typically an app with a very large number of files. Retry, or"
	case NoRollbackBusy:
		advice = "Retry once the backup or restore is done, or"
	default:
		advice = "Retry, or"
	}
	args := map[string]string{"app": project, "reason": err.Reason}
	if err.Reason == NoRollbackNoRoom {
		args["needed"] = strconv.FormatInt(err.Needed, 10)
		args["free"] = strconv.FormatInt(err.Free, 10)
	}
	in.report(incident.Report{
		ID: "app.update:" + project, Kind: incident.KindAppUpdate, Severity: incident.Warning,
		Title: project + " was not updated: it could not be backed up first",
		Detail: cause.Error() + "\n\nNothing was changed; the app is still running its current version. " +
			advice + " update this app on its own with \"Update without backup\" — a failed update then cannot be undone.",
		Args: args,
	})
	return err
}

// humanBytes renders a byte count the way the dashboard does (binary units).
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Reasons an update is refused over its data plan, carried in the app.update
// incident's args like the no-rollback ones.
const (
	DataRefusedStoreError = "data_store_error"
	DataRefusedNewer      = "data_newer"
	DataRefusedBlocked    = "data_blocked"
	DataRefusedNoBackup   = "data_no_backup"
)

// DataRefusedError is an update refused over its data plan, before anything was
// pulled, stopped or written.
type DataRefusedError struct {
	Reason string // one of DataRefused*
	Title  string // what the owner is told happened, in a few words
	Detail string // why, in the maintainer's or Maison's words
}

func (e *DataRefusedError) Error() string { return e.Title + ": " + e.Detail }

// PlanData works out how an update would move the app's data: from where the data
// is (the marker, else the compose still on disk — so this must run before the
// update overwrites it) to the data_version the store's new compose expects, and
// the steps between. See docs/migrations.md, "The update sequence".
//
// A *DataRefusedError means the update cannot happen as things stand: the store's
// manifest is malformed, the data is newer than the store's version, or a step on
// the way is marked as manual. Any other error is a failure to read the app folder.
func PlanData(dir string, newBase []byte) (stackup.MigratePlan, error) {
	f, err := composefile.Parse(newBase)
	if err != nil {
		return stackup.MigratePlan{}, fmt.Errorf("parse the store's compose: %w", err)
	}
	data, err := f.Data()
	if err != nil {
		return stackup.MigratePlan{}, &DataRefusedError{Reason: DataRefusedStoreError,
			Title:  "its new version declares its data migrations wrongly",
			Detail: "The store's x-compose-app block is invalid (" + err.Error() + "). This is for the app's maintainer to fix."}
	}
	from, ok, err := stackup.InstalledDataVersion(dir)
	if err != nil {
		return stackup.MigratePlan{}, err
	}
	if !ok {
		from = 1
	}
	plan := stackup.MigratePlan{From: from, To: data.Version}
	if plan.Steps, err = data.Pending(from); err != nil {
		return plan, &DataRefusedError{Reason: DataRefusedNewer,
			Title: "its data is newer than the store's version", Detail: err.Error()}
	}
	for _, m := range plan.Steps {
		if m.Blocked != "" {
			return plan, &DataRefusedError{Reason: DataRefusedBlocked,
				Title: "this update cannot be applied automatically",
				Detail: fmt.Sprintf("Moving the data from version %d to %d crosses %q, which the app's maintainer marked as manual:\n\n%s",
					from, data.Version, m.Label(), strings.TrimSpace(m.Blocked))}
		}
	}
	return plan, nil
}

// planUpdateData is PlanData for ApplyUpdate: it adds the refusal only an apply can
// make — a conversion with no backup — and reports every refusal as the
// app's update incident.
func (in *Installer) planUpdateData(project, dir string, newBase []byte, opts UpdateOptions) (stackup.MigratePlan, error) {
	plan, err := PlanData(dir, newBase)
	if err == nil && len(plan.Steps) > 0 && (opts.NoBackup || in.BackupBeforeUpdate == nil) {
		// "Update without backup" exists so a large app is not pinned on an old
		// version. With a migration pending it would make an irreversible conversion
		// with no undo, so this is the one place it is not honoured.
		err = &DataRefusedError{Reason: DataRefusedNoBackup,
			Title: "it converts the app's data, which needs a backup",
			Detail: fmt.Sprintf("This update moves the data from version %d to %d. A data conversion is only run with a backup to undo it, and this update would have none.",
				plan.From, plan.To)}
	}
	var refused *DataRefusedError
	if errors.As(err, &refused) {
		return plan, in.refuseData(project, refused)
	}
	return plan, err
}

// refuseData reports a data refusal as the app's update incident and returns it.
func (in *Installer) refuseData(project string, refused *DataRefusedError) error {
	log.Printf("update %s: refused (%s): %s", project, refused.Reason, refused.Detail)
	in.report(incident.Report{
		ID: "app.update:" + project, Kind: incident.KindAppUpdate, Severity: incident.Warning,
		Title:  project + " was not updated: " + refused.Title,
		Detail: refused.Detail + "\n\nNothing was changed; the app is still running its current version.",
		Args:   map[string]string{"app": project, "reason": refused.Reason},
	})
	return refused
}

// migrationImages lists the images the plan's steps run, for the update's pull. An
// image spelled with a ${VAR} is left to the step itself, which renders and pulls it.
func migrationImages(plan stackup.MigratePlan) []string {
	var out []string
	for _, m := range plan.Steps {
		for _, img := range append([]string{m.Image}, m.Images...) {
			if img != "" && !strings.Contains(img, "${") {
				out = append(out, img)
			}
		}
	}
	return out
}
