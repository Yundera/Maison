package apps

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/yundera/maison/internal/composefile"
	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/stackup"
	"github.com/yundera/maison/internal/xcasaos"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// Config carries a managed app's base compose, its user override, and the
// effective web-UI (opening URL) configuration derived from both.
type Config struct {
	Base     string `json:"base"`
	Override string `json:"override"`
	WebUI    WebUI  `json:"webui"`
	// Tips is the app's guidance, seeded from the store (x-compose-app tips, else
	// x-casaos tips: before_install + custom) but editable: edits are persisted
	// into the override's x-compose-app.tips key (see SetTips). It doubles as the
	// operator's per-app note.
	Tips string `json:"tips"`
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
		si, ca := mergedMeta(baseF, overF)
		if ca != nil {
			cfg.WebUI = WebUI{
				Scheme: ca.WebUIScheme,
				Host:   ca.WebUIHost,
				Port:   ca.WebUIPort,
				Path:   ca.WebUIPath,
				URL:    ca.WebURL(r.cfg.AppDomain()),
			}
		}
		cfg.Tips = mergedTips(si, ca)
	}
	return cfg, nil
}

// mergedTips picks the app's guidance: x-compose-app's tips when it declares
// them (that is where operator edits land), else the store's x-casaos tips.
func mergedTips(si *xcasaos.StoreInfo, ca *xcomposeapp.App) string {
	if ca != nil {
		if t := strings.TrimSpace(ca.Tips.Value()); t != "" {
			return t
		}
	}
	if si != nil {
		return storeTips(si)
	}
	return ""
}

// storeTips flattens the x-casaos tips (localized before_install guidance plus
// the free-form custom note) into a single block for display.
func storeTips(si *xcasaos.StoreInfo) string {
	var parts []string
	if b := strings.TrimSpace(xcasaos.Localized(si.Tips.BeforeInstall)); b != "" {
		parts = append(parts, b)
	}
	if c := strings.TrimSpace(si.Tips.Custom); c != "" {
		parts = append(parts, c)
	}
	return strings.Join(parts, "\n\n")
}

// RenderedTips returns the app's tips with ${VAR} references resolved from its
// base interpolation vars and .env — the operator-facing preview shown from the
// tile menu (the settings editor shows the raw, still-templated text).
func (r *Registry) RenderedTips(id string) (string, error) {
	cfg, err := r.GetConfig(id)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(r.cfg.AppsDir(), id)
	env, _ := os.ReadFile(filepath.Join(dir, ".env"))
	return envinject.Render(cfg.Tips, r.cfg, id, env), nil
}

// SetTips persists (or clears) the app's editable tips into the override's
// x-compose-app.tips key. Because tips never affect the running container, saving
// touches only the override file — no Docker recreate. The base compose (the
// store's shipped tips) is never mutated; the override's tips take precedence over
// it (see mergedTips), and clearing them falls back to the store's.
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
	setOrDelete(xca, "tips", tips)
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
//
// The candidate is validated first: an override Compose won't parse is rejected
// before it is written, so a typo can't leave an app whose stack no longer comes
// up and whose config window is the only way to fix it.
func (r *Registry) SetConfig(ctx context.Context, id, override string) error {
	dir := filepath.Join(r.cfg.AppsDir(), id)
	basePath := filepath.Join(dir, "docker-compose.yml")
	if _, err := os.Stat(basePath); err != nil {
		return err // only managed apps have an editable config
	}
	if err := r.ValidateOverride(ctx, id, override); err != nil {
		return err
	}
	overridePath := filepath.Join(dir, "docker-compose.override.yml")

	files := []string{basePath}
	if strings.TrimSpace(override) != "" {
		if err := os.WriteFile(overridePath, []byte(override), 0o644); err != nil {
			return err
		}
		files = append(files, overridePath)
	} else {
		_ = os.Remove(overridePath)
	}
	return stackup.Up(ctx, r.cfg, id, dir, files)
}

// SetWebUI writes the opening-URL fields into the override's x-compose-app block
// and recreates the app. It is a friendly shortcut for editing those webui-*
// keys by hand: the rest of the override (service tweaks, etc.) is preserved, and
// empty fields are removed so they fall back to the base compose / x-casaos.
func (r *Registry) SetWebUI(ctx context.Context, id string, w WebUI) error {
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
	setOrDelete(xca, "webui-host", w.Host)
	setOrDelete(xca, "webui-port", w.Port)
	setOrDelete(xca, "webui-scheme", w.Scheme)
	setOrDelete(xca, "webui-path", w.Path)
	if len(xca) == 0 {
		delete(doc, "x-compose-app")
	} else {
		doc["x-compose-app"] = xca
	}

	files := []string{basePath}
	if len(doc) == 0 {
		_ = os.Remove(overridePath) // nothing left to override
	} else {
		out, err := yaml.Marshal(doc)
		if err != nil {
			return err
		}
		if err := os.WriteFile(overridePath, out, 0o644); err != nil {
			return err
		}
		files = append(files, overridePath)
	}
	return stackup.Up(ctx, r.cfg, id, dir, files)
}

// setOrDelete sets m[k]=v, or removes the key when v is blank.
func setOrDelete(m map[string]any, k, v string) {
	if strings.TrimSpace(v) == "" {
		delete(m, k)
		return
	}
	m[k] = v
}
