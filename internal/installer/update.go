package installer

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/yundera/maison/internal/composefile"
	"github.com/yundera/maison/internal/envinject"
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
	// Store is the reference store URL; StoreAppID the catalog id within it.
	Store      string `json:"store"`
	StoreAppID string `json:"store_app_id"`
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

	storeURL, storeAppID := in.readUpdateRef(project)
	if storeAppID == "" {
		return UpdateStatus{HasRef: false}, nil
	}
	st := UpdateStatus{HasRef: true, Store: storeURL, StoreAppID: storeAppID}

	newBase, err := in.storeCompose(ctx, storeURL, storeAppID)
	if err != nil {
		st.Error = err.Error()
		return st, nil
	}
	st.Available = !bytes.Equal(current, newBase)
	return st, nil
}

// ApplyUpdate pulls the store's current docker-compose.yml, and — if it differs
// from the installed copy — overwrites the strict base and brings the stack back
// up (base + override) with `docker compose up -d`. The user's override and .env
// are untouched. Returns true when an update was actually applied, false when the
// app was already current.
func (in *Installer) ApplyUpdate(ctx context.Context, project string) (bool, error) {
	dir := filepath.Join(in.cfg.AppsDir(), project)
	composePath := filepath.Join(dir, "docker-compose.yml")
	current, err := os.ReadFile(composePath)
	if err != nil {
		return false, err
	}

	storeURL, storeAppID := in.readUpdateRef(project)
	if storeAppID == "" {
		return false, fmt.Errorf("no update reference recorded for %q", project)
	}

	newBase, err := in.storeCompose(ctx, storeURL, storeAppID)
	if err != nil {
		return false, err
	}
	if bytes.Equal(current, newBase) {
		return false, nil // already up to date — nothing to do
	}

	if err := os.WriteFile(composePath, newBase, 0o644); err != nil {
		return false, err
	}

	// stackup.Up creates any folder the updated compose newly introduces — declared
	// or bind-derived — before bringing the stack back up, so the new version can
	// write to it exactly as it would on a fresh install.
	files := []string{composePath}
	if override := filepath.Join(dir, "docker-compose.override.yml"); fileExists(override) {
		files = append(files, override)
	}
	if err := stackup.Up(ctx, in.cfg, project, dir, files); err != nil {
		return false, err
	}
	return true, nil
}

// storeCompose fetches app storeAppID from storeURL and applies the same PCS
// transform used at install time, yielding the exact bytes that would be written
// as the app's docker-compose.yml — so it is byte-comparable with the on-disk base.
func (in *Installer) storeCompose(ctx context.Context, storeURL, storeAppID string) ([]byte, error) {
	raw, err := in.store.AppComposeFrom(ctx, storeURL, storeAppID)
	if err != nil {
		return nil, err
	}
	f, err := composefile.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse store compose: %w", err)
	}
	main := ""
	if si, _ := f.StoreInfo(); si != nil {
		main = si.Main
	}
	return envinject.Transform(raw, in.cfg, main)
}

// readUpdateRef reads the store reference recorded in the app's override
// x-compose-app block. Returns empty strings when the app has no reference.
func (in *Installer) readUpdateRef(project string) (storeURL, storeAppID string) {
	dir := filepath.Join(in.cfg.AppsDir(), project)
	f, err := composefile.Load(filepath.Join(dir, "docker-compose.override.yml"))
	if err != nil {
		return "", ""
	}
	ca, err := f.ComposeApp()
	if err != nil {
		return "", ""
	}
	return ca.Store, ca.StoreAppID
}

// writeUpdateRef merges the store reference into the app's override x-compose-app
// block, preserving any existing override content (user edits, webui-* fields).
// This is what lets the Update tab find a newer version later.
func writeUpdateRef(dir, storeURL, storeAppID string) error {
	if storeAppID == "" {
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
	if storeURL != "" {
		xca["store"] = storeURL
	}
	xca["store-app-id"] = storeAppID
	doc["x-compose-app"] = xca

	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(overridePath, out, 0o644)
}
