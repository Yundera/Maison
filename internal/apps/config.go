package apps

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/yundera/maison/internal/composefile"
	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/overrideform"
	"github.com/yundera/maison/internal/stackup"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// Config carries a managed app's base compose, its user override, and the
// effective web-UI (opening URL) configuration derived from both.
type Config struct {
	Base     string `json:"base"`
	Override string `json:"override"`
	WebUI    WebUI  `json:"webui"`
	// Tips is the app's guidance, seeded from the store (x-compose-app tips; on an
	// x-casaos app, its before_install + custom) but editable: edits are persisted
	// into the override's x-compose-app.tips key (see SetTips). It doubles as the
	// operator's per-app note. A locale map: the UI shows the operator's locale,
	// falling back to en_us.
	Tips xcomposeapp.Localized `json:"tips"`
}

// WebUI is the effective x-compose-app webui-* configuration (base + override
// merged), plus the resolved click URL for preview.
type WebUI struct {
	Scheme string `json:"scheme"`
	Host   string `json:"host"`
	Port   string `json:"port"`
	Path   string `json:"path"`
	URL    string `json:"url"`
}

// GetConfig reads a managed app's compose + override files and resolves its
// effective web-UI configuration.
func (r *Registry) GetConfig(id string) (*Config, error) {
	dir := filepath.Join(r.cfg.AppsDir(), id)
	base, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	if err != nil {
		return nil, err
	}
	override, _ := os.ReadFile(filepath.Join(dir, "docker-compose.override.yml"))
	cfg := &Config{Base: string(base), Override: string(override)}

	if baseF, err := composefile.Parse(base); err == nil {
		overF, _ := composefile.Parse(override)
		ca := mergedMeta(baseF, overF)
		if ca != nil {
			cfg.WebUI = WebUI{
				Scheme: ca.WebUIScheme,
				Host:   ca.WebUIHost,
				Port:   ca.WebUIPort,
				Path:   ca.WebUIPath,
				URL:    ca.WebURL(r.cfg.AppDomain()),
			}
			cfg.Tips = ca.Tips.Map(strings.TrimSpace)
		}
	}
	return cfg, nil
}

// RenderedTips returns the app's tips with ${VAR} references resolved from its
// base interpolation vars and .env — the operator-facing preview shown from the
// tile menu (the settings editor shows the raw, still-templated text).
func (r *Registry) RenderedTips(id string) (xcomposeapp.Localized, error) {
	cfg, err := r.GetConfig(id)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(r.cfg.AppsDir(), id)
	env, _ := os.ReadFile(filepath.Join(dir, ".env"))
	return cfg.Tips.Map(func(s string) string { return envinject.Render(s, r.cfg, id, env) }), nil
}

// SetTips persists (or clears) the app's editable tips into the override's
// x-compose-app.tips key. Because tips never affect the running container, saving
// touches only the override file — no Docker recreate. The base compose (the
// store's shipped tips) is never mutated; the override's tips take precedence over
// it (see mergedTips), and clearing them falls back to the store's.
//
// The edit is stored as the en_us entry of a fresh locale map, replacing the
// store's whole map: an operator's note is one text for every reader of this box,
// and en_us is what every locale falls back to.
func (r *Registry) SetTips(id, tips string) error {
	dir := filepath.Join(r.cfg.AppsDir(), id)
	basePath := filepath.Join(dir, "docker-compose.yml")
	if _, err := os.Stat(basePath); err != nil {
		return err // only managed apps have an editable config
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
	if strings.TrimSpace(tips) == "" {
		delete(xca, "tips")
	} else {
		xca["tips"] = map[string]any{"en_us": tips}
	}
	if len(xca) == 0 {
		delete(doc, "x-compose-app")
	} else {
		doc["x-compose-app"] = xca
	}

	// Tips written by older Maison builds lived in the override's x-casaos block;
	// drop them so they can't resurface once the x-compose-app tips are cleared.
	if xc, ok := doc["x-casaos"].(map[string]any); ok {
		delete(xc, "tips")
		if len(xc) == 0 {
			delete(doc, "x-casaos")
		}
	}

	if len(doc) == 0 {
		if err := os.Remove(overridePath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(overridePath, out, 0o644)
}

// SetConfig writes the override file (or removes it when empty) and recreates
// the app as base + override — Maison never mutates the store-provided base.
func (r *Registry) SetConfig(ctx context.Context, id, override string) error {
	return r.SaveSettings(ctx, id, SettingsChange{Override: &override})
}

// SetWebUI writes the opening-URL fields into the override's x-compose-app block
// and recreates the app — the form's opening-URL group on its own. Empty fields
// are removed so they fall back to the store's.
func (r *Registry) SetWebUI(ctx context.Context, id string, w WebUI) error {
	form, err := r.GetOverrideForm(id)
	if err != nil {
		return err
	}
	form.WebUI.Scheme.Value = w.Scheme
	form.WebUI.Host.Value = w.Host
	form.WebUI.Port.Value = w.Port
	form.WebUI.Path.Value = w.Path
	return r.SaveSettings(ctx, id, SettingsChange{Form: form})
}

// SettingsChange is one save of the settings window's Config tab. Override is
// the YAML view's draft of the override and Form the Form view's edits, applied on
// top of it — on top of the file on disk when Override is nil. Both nil leave the
// override as it is. A nil Env leaves the .env as it is (an empty, non-nil one
// empties it).
type SettingsChange struct {
	Override *string
	Form     *overrideform.Form
	Env      *[]envinject.Var
}

// SaveSettings applies a Config-tab save — override and .env together — and
// recreates the app once. Every settings write goes through it.
//
// Nothing reaches the stack unchecked: the resulting project is validated by
// Compose before the override is written, against the new .env, so a typo or a
// variable the override now requires but nobody provides is caught here rather
// than by watching the app fail to come back up. A save that fails validation
// leaves both files as they were.
func (r *Registry) SaveSettings(ctx context.Context, id string, c SettingsChange) error {
	dir := filepath.Join(r.cfg.AppsDir(), id)
	base, current, err := r.readCompose(id) // only managed apps have an editable config
	if err != nil {
		return err
	}

	next := string(current)
	if c.Override != nil {
		next = *c.Override
	}
	if c.Form != nil {
		out, err := overrideform.Apply(base, []byte(next), c.Form)
		if err != nil {
			return err
		}
		next = string(out)
	}

	// The .env goes first, because the validation below must see it — and comes
	// back out if that validation fails.
	envPath := filepath.Join(dir, ".env")
	restoreEnv := func() {}
	if c.Env != nil {
		old, err := os.ReadFile(envPath)
		existed := err == nil
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		vars := r.protectEnv(id, old, *c.Env) // locked and masked rows keep what is on disk
		if err := envinject.ValidateVars(vars); err != nil {
			return err
		}
		patched, err := envinject.PatchEnvFile(old, vars)
		if err != nil {
			return err
		}
		if patched == nil {
			// Every variable was removed. Keep an empty file rather than deleting it;
			// the up below re-ensures what .env.app and Maison provide either way.
			patched = []byte{}
		}
		if err := envinject.WriteEnvFile(envPath, patched); err != nil {
			return err
		}
		restoreEnv = func() {
			if existed {
				_ = envinject.WriteEnvFile(envPath, old)
			} else {
				_ = os.Remove(envPath)
			}
		}
	}

	if err := r.ValidateOverride(ctx, id, next); err != nil {
		restoreEnv()
		return err
	}

	overridePath := filepath.Join(dir, overrideName)
	if strings.TrimSpace(next) != "" {
		if err := os.WriteFile(overridePath, []byte(next), 0o644); err != nil {
			restoreEnv()
			return err
		}
	} else if err := os.Remove(overridePath); err != nil && !os.IsNotExist(err) {
		restoreEnv()
		return err
	}
	return stackup.Up(ctx, r.cfg, id, dir, r.composeFiles(dir))
}
