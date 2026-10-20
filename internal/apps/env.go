package apps

import (
	"context"
	"os"
	"path/filepath"

	"github.com/yundera/maison/internal/appenv"
	"github.com/yundera/maison/internal/envinject"
)

// EnvEntry is one .env row as the editor sees it: the variable, plus who sets it
// (appenv.Source*; empty for the operator's own). A maison or deployment row is
// put back by the next up whatever the editor saves, so the editor locks it.
type EnvEntry struct {
	envinject.Var
	Source string `json:"source,omitempty"`
}

// GetEnv reads a managed app's .env as an ordered key/value list — the app's
// persistent variable record, prefilled at install from the base interpolation
// vars (PUID, DATA_ROOT, REF_*, …) and hand-editable since. A managed app whose
// .env was deleted reads as empty rather than failing.
func (r *Registry) GetEnv(id string) ([]EnvEntry, error) {
	dir := filepath.Join(r.cfg.AppsDir(), id)
	if _, err := os.Stat(filepath.Join(dir, "docker-compose.yml")); err != nil {
		return nil, err // only managed apps have an editable .env
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	sources := appenv.KeySources(r.cfg)
	out := []EnvEntry{}
	for _, v := range envinject.ParseEnvFile(raw) {
		out = append(out, EnvEntry{Var: v, Source: sources[v.Key]})
	}
	return out, nil
}

// SetEnv rewrites a managed app's .env to hold exactly vars and recreates the
// stack. The recreate is the point: `docker compose` reads .env when it brings a
// project up, so an edit that isn't followed by an up-d leaves the running
// containers on the old values and the UI lying about what took effect.
//
// The file is patched rather than regenerated (see envinject.PatchEnvFile), so
// comments and line order the operator added by hand survive a save.
func (r *Registry) SetEnv(ctx context.Context, id string, vars []envinject.Var) error {
	return r.SaveSettings(ctx, id, SettingsChange{Env: &vars})
}
