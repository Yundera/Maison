package apps

import (
	"context"
	"os"
	"path/filepath"

	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/stackup"
)

// GetEnv reads a managed app's .env as an ordered key/value list — the app's
// persistent variable record, prefilled at install from the base interpolation
// vars (PUID, DATA_ROOT, REF_*, …) and hand-editable since. A managed app whose
// .env was deleted reads as empty rather than failing.
func (r *Registry) GetEnv(id string) ([]envinject.Var, error) {
	dir := filepath.Join(r.cfg.AppsDir(), id)
	if _, err := os.Stat(filepath.Join(dir, "docker-compose.yml")); err != nil {
		return nil, err // only managed apps have an editable .env
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	vars := envinject.ParseEnvFile(raw)
	if vars == nil {
		vars = []envinject.Var{}
	}
	return vars, nil
}

// SetEnv rewrites a managed app's .env to hold exactly vars and recreates the
// stack. The recreate is the point: `docker compose` reads .env when it brings a
// project up, so an edit that isn't followed by an up-d leaves the running
// containers on the old values and the UI lying about what took effect.
//
// The file is patched rather than regenerated (see envinject.PatchEnvFile), so
// comments and line order the operator added by hand survive a save.
func (r *Registry) SetEnv(ctx context.Context, id string, vars []envinject.Var) error {
	dir := filepath.Join(r.cfg.AppsDir(), id)
	basePath := filepath.Join(dir, "docker-compose.yml")
	if _, err := os.Stat(basePath); err != nil {
		return err // only managed apps have an editable .env
	}

	envPath := filepath.Join(dir, ".env")
	old, err := os.ReadFile(envPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	next, err := envinject.PatchEnvFile(old, vars)
	if err != nil {
		return err // invalid key / duplicate / multi-line value — rejected before writing
	}
	if next == nil {
		// Every variable was removed. Keep an empty file rather than deleting it:
		// its absence is how the installer decides to re-seed the base vars, and
		// an operator who cleared it on purpose shouldn't get them back.
		next = []byte{}
	}
	if err := os.WriteFile(envPath, next, 0o644); err != nil {
		return err
	}
	return stackup.Up(ctx, r.cfg, id, dir, r.composeFiles(dir))
}
