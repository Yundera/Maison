package apps

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/yundera/maison/internal/composefile"
	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// credentialsDismissedFile marks, inside the app's own folder, that the operator
// ticked "Don't show again". It lives there rather than under Maison's state dir
// on purpose: uninstall removes the folder, so a reinstall — which may come with
// a freshly generated password — shows the credentials again.
const credentialsDismissedFile = ".credentials-dismissed"

// RenderedCredential is one x-compose-app credential with its templates resolved
// against the app's .env, ready to display.
type RenderedCredential struct {
	Label    string `json:"label,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Note     string `json:"note,omitempty"`
}

// declaresCredentials reports whether ca carries at least one credential with
// something to show.
func declaresCredentials(ca *xcomposeapp.App) bool {
	if ca == nil {
		return false
	}
	for _, c := range ca.Credentials {
		if strings.TrimSpace(c.Username) != "" || strings.TrimSpace(c.Password) != "" {
			return true
		}
	}
	return false
}

// RenderedCredentials returns a managed app's declared credentials, rendered the
// way tips are (envinject.Render against the app's .env, which already carries
// the deployment's .env.app keys such as APP_DEFAULT_PASSWORD). An unresolved
// ${VAR} stays visible as written; an entry with neither username nor password
// is dropped. The values are secrets: callers must never log them.
func (r *Registry) RenderedCredentials(id string) ([]RenderedCredential, error) {
	dir := filepath.Join(r.cfg.AppsDir(), id)
	base, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	if err != nil {
		return nil, err
	}
	baseF, err := composefile.Parse(base)
	if err != nil {
		return nil, err
	}
	override, _ := os.ReadFile(filepath.Join(dir, "docker-compose.override.yml"))
	overF, _ := composefile.Parse(override)
	_, ca := mergedMeta(baseF, overF)
	if ca == nil {
		return nil, nil
	}
	env, _ := os.ReadFile(filepath.Join(dir, ".env"))
	render := func(s string) string { return strings.TrimSpace(envinject.Render(s, r.cfg, id, env)) }
	var out []RenderedCredential
	for _, c := range ca.Credentials {
		rc := RenderedCredential{
			Label:    render(c.Label.Value()),
			Username: render(c.Username),
			Password: render(c.Password),
			Note:     render(c.Note.Value()),
		}
		if rc.Username == "" && rc.Password == "" {
			continue
		}
		out = append(out, rc)
	}
	return out, nil
}

// CredentialsDismissed reports whether the operator asked not to be shown app
// id's credentials on open. False on any error, which errs towards showing them.
func (r *Registry) CredentialsDismissed(id string) bool {
	if id == "" || strings.ContainsAny(id, `/\`) {
		return false
	}
	_, err := os.Stat(filepath.Join(r.cfg.AppsDir(), id, credentialsDismissedFile))
	return err == nil
}

// SetCredentialsDismissed records (or clears) the operator's "Don't show again"
// for app id. Only a managed app — one with a compose in its folder — has one.
func (r *Registry) SetCredentialsDismissed(id string, dismissed bool) error {
	if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return os.ErrNotExist
	}
	dir := filepath.Join(r.cfg.AppsDir(), id)
	if _, err := os.Stat(filepath.Join(dir, "docker-compose.yml")); err != nil {
		return err
	}
	marker := filepath.Join(dir, credentialsDismissedFile)
	if !dismissed {
		if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return os.WriteFile(marker, nil, 0o644)
}
