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

	"gopkg.in/yaml.v3"

	"github.com/yundera/maison/internal/appstore"
	"github.com/yundera/maison/internal/composefile"
	"github.com/yundera/maison/internal/incident"
	"github.com/yundera/maison/internal/stackup"
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
	return st, nil
}

// UpdateResult is what an update did.
type UpdateResult struct {
	// Applied is false when the app was already current.
	Applied bool `json:"applied"`
	// Backup names the rollback point taken before the update, empty when none was.
	Backup string `json:"backup,omitempty"`
	// RolledBack is true when the update failed and the app was put back.
	RolledBack bool `json:"rolled_back,omitempty"`
	// Warning explains an update applied without a rollback point (NoBackup), or a
	// rollback that itself failed. It is not an error — the update still happened —
	// but it is the thing the operator most needs to see.
	Warning string `json:"warning,omitempty"`
}

// ErrNoRollback marks an update refused because its rollback point could not be
// taken — not enough free disk for a second copy of the app, or the copy itself
// failing. Nothing was changed. The caller may retry that one app with
// UpdateOptions.NoBackup, which is an owner's explicit choice and never what
// "update all" does.
var ErrNoRollback = errors.New("no rollback point")

// UpdateOptions are the choices an owner makes for one update.
type UpdateOptions struct {
	// NoBackup applies the update without taking a rollback point, so a failed
	// update cannot be undone. The owner's choice, for one app at a time: the
	// app's Update tab offers it up front (a "Backup" box, ticked by default), and
	// it is the way past an update refused with ErrNoRollback.
	NoBackup bool
}

// ApplyUpdate pulls the store's current docker-compose.yml, and — if it differs
// from the installed copy — pulls its images, takes a rollback point, stops the old
// version, overwrites the strict base and brings the stack back up (base + override)
// with `docker compose up -d`. The user's override and .env are untouched.
//
// An update is the most common way an app breaks, and it is the one destructive
// change Maison makes on the user's behalf, so it takes a backup first and puts the
// app back if bringing it up fails.
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

	// From here on the tile shows which step the update is on (UpdateState), and the
	// bar goes away however the update ends.
	defer in.endUpdate(project)
	step := func(phase, msg string, pct float64) {
		in.setUpdate(project, UpdateState{Phase: phase, Message: msg, Pct: pct})
	}

	// Refused before the image pull, so an app whose rollback point cannot fit costs
	// a size walk, not a download followed by a refusal. The walk is stat-only.
	if !opts.NoBackup && in.RollbackRoom != nil {
		step(UpdatePhaseCheck, "Checking free space for the rollback point", 0)
		if err := in.RollbackRoom(ctx, project); err != nil {
			return res, in.refuseNoRollback(project, err)
		}
	}

	// The new version's images, pulled while the old version is still serving: the stop
	// below would otherwise cost the whole download, not just the swap. Not fatal —
	// `compose up` pulls whatever is still missing, and fails loudly if it cannot.
	if f, err := composefile.Parse(newBase); err == nil {
		in.pullImages(ctx, f, func(ev Event) { step(UpdatePhasePull, ev.Message, ev.Download) })
	}

	// The rollback point, before anything is written.
	//
	// It is deliberately *not* taken with whatever backup engine is configured: a
	// rollback has to be fast, and restoring from a repository is a download. The
	// server wires this to the local engine specifically, so putting the app back is
	// a rename.
	//
	// A rollback point that cannot be taken REFUSES the update: nothing has been
	// stopped or written yet, so refusing costs nothing, and going ahead would make
	// the one destructive change Maison makes irreversible without anyone having
	// chosen that. The owner can choose it — UpdateOptions.NoBackup, per app — which
	// is what keeps a too-large app from being pinned on an old version.
	switch {
	case opts.NoBackup:
		res.Warning = "updated without a rollback point, as requested: a failed update cannot be undone"
		log.Printf("update %s: %s", project, res.Warning)
	case in.BackupBeforeUpdate != nil:
		step(UpdatePhaseBackup, "Taking the rollback point", 0)
		name, err := in.BackupBeforeUpdate(ctx, project, func(st UpdateState) {
			st.Phase = UpdatePhaseBackup
			in.setUpdate(project, st)
		})
		if err != nil {
			return res, in.refuseNoRollback(project, err)
		}
		res.Backup = name
	}

	// Stop the old version before anything of the new one runs. The new version's init
	// steps run in pre_up against the app's data, and the old containers still have that
	// data open — taking the rollback point restarts them on its way out — so a database
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
	if err := stackup.Up(ctx, in.cfg, project, dir, files); err != nil {
		return in.rollBack(ctx, project, res, err)
	}
	res.Applied = true
	// A working update clears whatever the last one left behind.
	in.resolve("app.update:" + project)
	return res, nil
}

// rollBack puts the app back after a failed update.
//
// The restore replaces the whole folder, so it takes the old compose with it — the
// app returns to exactly the state the rollback point captured, not to a new compose
// running against old data.
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
			Detail: "The update failed and there was no rollback point to put the old version back from:\n" + cause.Error() + "\n\nThe new version is in place and did not come up. Its logs, from the tile's menu, will say why.",
			Args:   map[string]string{"app": project, "reason": "broken"},
		})
		return res, fmt.Errorf("update failed and could not be undone: %w", cause)
	}
	// Deliberately not the request's context: it may already be cancelled by the
	// failure, and abandoning a rollback half-done is the worst available outcome.
	in.setUpdate(project, UpdateState{Phase: UpdatePhaseRollback, Message: "Putting the previous version back", Pct: 100})
	if err := in.RollBack(context.WithoutCancel(ctx), project, res.Backup); err != nil {
		res.Warning = "the update failed AND rolling back failed: " + err.Error()
		log.Printf("update %s: %s", project, res.Warning)
		// The worst state Maison can leave an app in: neither the old version nor the
		// new one. Nothing else in this codebase is more deserving of an alert.
		in.report(incident.Report{
			ID: "app.update:" + project, Kind: incident.KindAppUpdate, Severity: incident.Critical,
			Title:  project + " is broken after a failed update",
			Detail: "The update failed and putting the old version back failed too:\n" + err.Error() + "\n\nThe app is in neither state and needs attention. Its previous version is in the backup named " + res.Backup + ".",
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
				Detail: "The update failed:\n" + cause.Error() + "\n\nThe previous version was put back from the backup named " + res.Backup + ", but it is not running: " + err.Error() + "\n\nIts logs, from the tile's menu, will say why. What the failed update left behind was archived with the app's other backups.",
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
		Detail: "The update failed and the previous version was put back; it is running.\n\n" + cause.Error() + "\n\nThe store will keep offering this update. If it fails the same way again, the store's version of the app needs a fix.",
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
	return st, nil
}

// Why an update was refused for want of a rollback point. Carried on the error, the
// run item and the incident's args, so the Updates page can say it in plain words
// instead of relaying an engine's error string.
const (
	// NoRollbackNoRoom: the rollback copy would not fit on the disk (Needed > Free).
	NoRollbackNoRoom = "no_room"
	// NoRollbackTimeout: the copy ran out of time — in practice the stopped pass of
	// an app with a very large number of files outrunning its downtime budget.
	NoRollbackTimeout = "backup_timeout"
	// NoRollbackFailed: the copy failed for any other reason.
	NoRollbackFailed = "backup_failed"
)

// RoomError is what a RollbackRoom check returns when the copy would not fit: the
// numbers are what let the owner see how far off it is.
type RoomError struct {
	Needed, Free int64
}

func (e *RoomError) Error() string {
	return fmt.Sprintf("the rollback copy needs %s of free disk, %s is free", humanBytes(e.Needed), humanBytes(e.Free))
}

// NoRollbackError is an update refused because its rollback point could not be
// taken. errors.Is(err, ErrNoRollback) holds for it.
type NoRollbackError struct {
	Reason       string // NoRollbackNoRoom | NoRollbackTimeout | NoRollbackFailed
	Needed, Free int64  // NoRollbackNoRoom only
	Cause        error
}

func (e *NoRollbackError) Error() string {
	var why string
	switch e.Reason {
	case NoRollbackNoRoom:
		why = "not enough free disk for the safety backup (" + e.Cause.Error() + ")"
	case NoRollbackTimeout:
		why = "the safety backup took too long"
	default:
		why = "the safety backup failed: " + e.Cause.Error()
	}
	return "not updated, nothing was changed: " + why
}

func (e *NoRollbackError) Is(target error) bool { return target == ErrNoRollback }
func (e *NoRollbackError) Unwrap() error        { return e.Cause }

// classifyNoRollback turns the error from RollbackRoom or BackupBeforeUpdate into
// a NoRollbackError with its reason.
func classifyNoRollback(cause error) *NoRollbackError {
	e := &NoRollbackError{Reason: NoRollbackFailed, Cause: cause}
	var room *RoomError
	switch {
	case errors.As(cause, &room):
		e.Reason, e.Needed, e.Free = NoRollbackNoRoom, room.Needed, room.Free
	case errors.Is(cause, context.DeadlineExceeded):
		e.Reason = NoRollbackTimeout
	}
	return e
}

// refuseNoRollback reports an update refused for want of a rollback point. The app is
// untouched, so it is a warning, and the incident says how to go ahead anyway — with
// advice that fits the cause: an error that leaves the owner no next step, or the
// wrong one, is a design bug.
func (in *Installer) refuseNoRollback(project string, cause error) error {
	err := classifyNoRollback(cause)
	log.Printf("update %s: refused (%s): %v", project, err.Reason, cause)
	var advice string
	switch err.Reason {
	case NoRollbackNoRoom:
		advice = "Free some disk and retry"
	case NoRollbackTimeout:
		advice = "The backup ran out of time — typically an app with a very large number of files. Retry, or"
	default:
		advice = "Retry, or"
	}
	if err.Reason == NoRollbackNoRoom {
		advice += ", or"
	}
	args := map[string]string{"app": project, "reason": err.Reason}
	if err.Reason == NoRollbackNoRoom {
		args["needed"] = strconv.FormatInt(err.Needed, 10)
		args["free"] = strconv.FormatInt(err.Free, 10)
	}
	in.report(incident.Report{
		ID: "app.update:" + project, Kind: incident.KindAppUpdate, Severity: incident.Warning,
		Title: project + " was not updated: no rollback point could be taken",
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
