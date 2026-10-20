package apps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yundera/maison/internal/composecmd"
	"github.com/yundera/maison/internal/composefile"
	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/overrideform"
)

// overrideName is the file the settings window edits, and the name a validation
// error must blame — never the scratch copy the check actually ran on.
const overrideName = "docker-compose.override.yml"

// GetOverrideForm reads a managed app's compose files into the friendly,
// field-by-field view of its override (see internal/overrideform).
func (r *Registry) GetOverrideForm(id string) (*overrideform.Form, error) {
	base, override, err := r.readCompose(id)
	if err != nil {
		return nil, err
	}
	return r.buildForm(base, override)
}

// ParseOverrideForm builds the form from a candidate override instead of the one
// on disk — how the settings window carries unsaved YAML edits into the Form view.
func (r *Registry) ParseOverrideForm(id, override string) (*overrideform.Form, error) {
	base, _, err := r.readCompose(id)
	if err != nil {
		return nil, err
	}
	return r.buildForm(base, []byte(override))
}

// RenderOverrideForm is the override a form would save, without saving it — how
// the settings window carries unsaved Form edits into the YAML view. draft is the
// override the form was built from when it is not the one on disk (the YAML view
// has unsaved edits of its own); nil means the file.
func (r *Registry) RenderOverrideForm(id string, form *overrideform.Form, draft *string) (string, error) {
	base, override, err := r.readCompose(id)
	if err != nil {
		return "", err
	}
	if draft != nil {
		override = []byte(*draft)
	}
	out, err := overrideform.Apply(base, override, form)
	return string(out), err
}

// buildForm is overrideform.Build plus what only this package knows: the store's
// opening URL as Maison reads it — translated from x-casaos for an app that has no
// x-compose-app — and the click URL it resolves to on this deployment.
func (r *Registry) buildForm(base, override []byte) (*overrideform.Form, error) {
	form, err := overrideform.Build(base, override)
	if err != nil {
		return nil, err
	}
	baseF, err := composefile.Parse(base)
	if err != nil {
		return form, nil // the form itself parsed; the preview is a nicety
	}
	if ca := mergedMeta(baseF, nil); ca != nil {
		w := form.WebUI
		for _, f := range []struct {
			s    *overrideform.Scalar
			base string
		}{
			{&w.Scheme, ca.WebUIScheme},
			{&w.Host, ca.WebUIHost},
			{&w.Port, ca.WebUIPort},
			{&w.Path, ca.WebUIPath},
		} {
			if f.s.Complex || f.s.Base != "" {
				continue
			}
			// Base only: Apply compares against the base file's own x-compose-app,
			// so a Value equal to a translated x-casaos one would be written out.
			f.s.Base = f.base
		}
	}
	overF, _ := composefile.Parse(override)
	if ca := mergedMeta(baseF, overF); ca != nil {
		form.WebURL = ca.WebURL(r.cfg.AppDomain())
	}
	return form, nil
}

// SetOverrideForm writes the settings form back into the override and recreates
// the app. The override is patched, not regenerated: keys the form doesn't model
// (x-compose-app, healthchecks, …) and the comments around them survive.
//
// The result is validated before anything is written — an override that Compose
// won't parse never reaches the disk, so a bad save can't leave an app that no
// longer comes up.
func (r *Registry) SetOverrideForm(ctx context.Context, id string, form *overrideform.Form) error {
	return r.SaveSettings(ctx, id, SettingsChange{Form: form})
}

// ValidateOverride checks a candidate override against the app's base compose
// without applying it — what the settings window's Validate button calls, and
// what SetConfig runs before it writes anything.
func (r *Registry) ValidateOverride(ctx context.Context, id, override string) error {
	dir := filepath.Join(r.cfg.AppsDir(), id)
	basePath := filepath.Join(dir, "docker-compose.yml")
	if _, err := os.Stat(basePath); err != nil {
		return err
	}

	files := []string{basePath}
	tmp := ""
	if strings.TrimSpace(override) != "" {
		// Compose resolves relative paths and the .env against the project directory,
		// so the candidate has to be validated from inside it. The dot keeps it out of
		// the dashboard (docs/app-model.md) in the window where it exists.
		f, err := os.CreateTemp(dir, ".override-*.yml")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		if _, err := f.WriteString(override); err != nil {
			f.Close()
			return err
		}
		f.Close()
		tmp = f.Name()
		files = append(files, tmp)
	}

	err := composecmd.Validate(ctx, dir, id, files, envinject.Env(r.cfg, id))
	if err != nil && tmp != "" {
		// Compose names the file it choked on, which here is a scratch file nobody has
		// ever seen. Say the name the operator is actually editing.
		return errors.New(strings.ReplaceAll(err.Error(), tmp, overrideName))
	}
	return err
}

// EffectiveConfig returns the project as Compose actually resolves it — base plus
// override, merged, with every variable interpolated. It is the answer to "what
// did my override actually do", which neither file shows on its own.
//
// resolve chooses between the merge with every ${VAR} still written as a variable
// (false — what the settings window shows by default) and the merge with them
// filled in from the .env (true — "what value did this actually get").
func (r *Registry) EffectiveConfig(ctx context.Context, id string, resolve bool) (string, error) {
	dir := filepath.Join(r.cfg.AppsDir(), id)
	basePath := filepath.Join(dir, "docker-compose.yml")
	if _, err := os.Stat(basePath); err != nil {
		return "", err
	}
	return composecmd.Config(ctx, dir, id, r.composeFiles(dir), envinject.Env(r.cfg, id), resolve)
}

// readCompose reads an app's base compose (required — only managed apps have one)
// and its override (optional).
func (r *Registry) readCompose(id string) (base, override []byte, err error) {
	dir := filepath.Join(r.cfg.AppsDir(), id)
	base, err = os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	if err != nil {
		return nil, nil, fmt.Errorf("%s has no store compose: %w", id, err)
	}
	override, _ = os.ReadFile(filepath.Join(dir, "docker-compose.override.yml"))
	return base, override, nil
}
