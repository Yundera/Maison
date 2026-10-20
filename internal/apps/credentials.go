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
// against the app's .env, ready to display. Index is its position in the declared
// list — what a change names, since empty entries are dropped from this one.
type RenderedCredential struct {
	Index int `json:"index"`
	// Label and Note are locale maps, rendered entry by entry; the UI picks its locale.
	Label    xcomposeapp.Localized `json:"label,omitempty"`
	Username string                `json:"username,omitempty"`
	Password string                `json:"password,omitempty"`
	Note     xcomposeapp.Localized `json:"note,omitempty"`
	Change   *CredentialChangeInfo `json:"change,omitempty"`
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
	dir, ca, err := r.mergedApp(id)
	if err != nil {
		return nil, err
	}
	if ca == nil {
		return nil, nil
	}
	env, _ := os.ReadFile(filepath.Join(dir, ".env"))
	render := func(s string) string { return strings.TrimSpace(envinject.Render(s, r.cfg, id, env)) }
	var out []RenderedCredential
	for i, c := range ca.Credentials {
		rc := RenderedCredential{
			Index:    i,
			Label:    c.Label.Map(render),
			Username: render(c.Username),
			Password: render(c.Password),
			Note:     c.Note.Map(render),
			Change:   changeInfo(ca, c),
		}
		if rc.Username == "" && rc.Password == "" {
			continue
		}
		out = append(out, rc)
	}
	return out, nil
}

// mergedApp reads a managed app's x-compose-app as it runs: base merged with the
// override. ca is nil when the app declares none.
func (r *Registry) mergedApp(id string) (dir string, ca *xcomposeapp.App, err error) {
	dir = filepath.Join(r.cfg.AppsDir(), id)
	base, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	if err != nil {
		return dir, nil, err
	}
	baseF, err := composefile.Parse(base)
	if err != nil {
		return dir, nil, err
	}
	override, _ := os.ReadFile(filepath.Join(dir, "docker-compose.override.yml"))
	overF, _ := composefile.Parse(override)
	ca = mergedMeta(baseF, overF)
	return dir, ca, nil
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
