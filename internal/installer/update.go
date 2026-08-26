package installer

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/yundera/maison/internal/appstore"
	"github.com/yundera/maison/internal/composefile"
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
	st := UpdateStatus{HasRef: true, Store: ref.URL, StoreAppID: ref.ID, StoreAppsPath: ref.AppsPath}

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
	// Warning explains a rollback point that could not be taken, or a rollback that
	// itself failed. It is not an error — the update still happened — but it is the
	// thing the operator most needs to see.
	Warning string `json:"warning,omitempty"`
}

// ApplyUpdate pulls the store's current docker-compose.yml, and — if it differs
// from the installed copy — takes a rollback point, overwrites the strict base and
// brings the stack back up (base + override) with `docker compose up -d`. The user's
// override and .env are untouched.
//
// An update is the most common way an app breaks, and it is the one destructive
// change Maison makes on the user's behalf, so it takes a backup first and puts the
// app back if bringing it up fails.
func (in *Installer) ApplyUpdate(ctx context.Context, project string) (UpdateResult, error) {
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

	// The rollback point, before anything is written.
	//
	// It is deliberately *not* taken with whatever backup engine is configured: a
	// rollback has to be fast, and restoring from a repository is a download. The
	// server wires this to the local engine specifically, so putting the app back is
	// a rename.
	if in.BackupBeforeUpdate != nil {
		name, err := in.BackupBeforeUpdate(ctx, project)
		switch {
		case err != nil:
			// Not fatal. The commonest reason is that the app is too large to hold a
			// second copy of, and refusing to update on those grounds would leave the
			// app stuck on an old version — including for a security fix. Proceed, and
			// say plainly that there is no way back.
			res.Warning = "no rollback point could be taken, so this update cannot be undone: " + err.Error()
			log.Printf("update %s: %s", project, res.Warning)
		default:
			res.Backup = name
		}
	}

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
	if err := stackup.Up(ctx, in.cfg, project, dir, files); err != nil {
		return in.rollBack(ctx, project, res, err)
	}
	res.Applied = true
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
func (in *Installer) rollBack(ctx context.Context, project string, res UpdateResult, cause error) (UpdateResult, error) {
	if in.RollBack == nil || res.Backup == "" {
		return res, fmt.Errorf("update failed and could not be undone: %w", cause)
	}
	// Deliberately not the request's context: it may already be cancelled by the
	// failure, and abandoning a rollback half-done is the worst available outcome.
	if err := in.RollBack(context.WithoutCancel(ctx), project, res.Backup); err != nil {
		res.Warning = "the update failed AND rolling back failed: " + err.Error()
		log.Printf("update %s: %s", project, res.Warning)
		return res, fmt.Errorf("update failed and the rollback failed too (%v): %w", err, cause)
	}
	res.RolledBack = true
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
	return appstore.Ref{URL: ca.Store, AppsPath: ca.StoreAppsPath, ID: ca.StoreAppID}
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
	if ref.URL != "" {
		xca["store"] = ref.URL
	}
	xca["store-app-id"] = ref.ID
	// Only when it differs from the default, so the common app's override stays as
	// short as it was.
	if ref.AppsPath != "" && ref.AppsPath != appstore.DefaultAppsPath {
		xca["store-apps-path"] = ref.AppsPath
	}
	doc["x-compose-app"] = xca

	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(overridePath, out, 0o644)
}
