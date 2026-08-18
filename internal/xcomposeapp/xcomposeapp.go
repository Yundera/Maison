// Package xcomposeapp models Maison's own `x-compose-app` compose extension and
// resolves an app's web-UI click URL from it.
//
// Unlike x-casaos (which declares a container port and derives a hostname at
// install time), x-compose-app declares the final web-UI URL directly — the
// `webui-host` value is the app's reverse-proxy route host, e.g. `app-${domain}`.
// The URL is built by string construction on every render, so it tracks domain
// changes and works for apps Maison merely discovered.
package xcomposeapp

import (
	"errors"
	"strings"

	"gopkg.in/yaml.v3"
)

// ExtensionKey is the compose extension key this package reads.
const ExtensionKey = "x-compose-app"

var (
	// ErrNoExtension is returned when a compose file has no x-compose-app block.
	ErrNoExtension = errors.New("x-compose-app extension not found")
	// ErrUnsupportedVersion is returned for a schema_version this build predates,
	// so the caller falls back to x-casaos.
	ErrUnsupportedVersion = errors.New("x-compose-app schema_version not supported")
)

// SchemaVersion is the highest x-compose-app schema this build understands.
// v2 added `folders` and `hooks`; v1 files keep working unchanged.
const SchemaVersion = 2

// App is the Maison-native app metadata. Only the fields Maison consumes are
// modelled; unknown keys are ignored.
type App struct {
	Schema   int       `yaml:"schema_version,omitempty"`
	ID       string    `yaml:"id,omitempty"`
	Title    Localized `yaml:"title,omitempty"`
	Icon     string    `yaml:"icon,omitempty"`
	Category string    `yaml:"category,omitempty"`
	// View is the dashboard grid the app's tile lands in — see the View*
	// constants. Presentation only, which is why it does NOT raise
	// SchemaVersion: a build that predates it renders the app in the ordinary
	// grid, which is exactly where the app used to be.
	View          string    `yaml:"view,omitempty"`
	Tagline       Localized `yaml:"tagline,omitempty"`
	Description   Localized `yaml:"description,omitempty"`
	Developer     string    `yaml:"developer,omitempty"`
	Screenshots   []string  `yaml:"screenshots,omitempty"`
	Thumbnail     string    `yaml:"thumbnail,omitempty"`
	Architectures []string  `yaml:"architectures,omitempty"`

	// Tips is the app's guidance note (Markdown, may reference ${VAR}). It is also
	// where Maison persists operator edits — into the override's x-compose-app
	// block, never into the store-provided base compose.
	Tips Localized `yaml:"tips,omitempty"`

	// The click URL, declared directly (see package doc).
	WebUIHost   string `yaml:"webui-host,omitempty"`
	WebUIPort   string `yaml:"webui-port,omitempty"`
	WebUIScheme string `yaml:"webui-scheme,omitempty"`
	WebUIPath   string `yaml:"webui-path,omitempty"`

	// Update reference: where this app was installed from, so Maison can pull a
	// fresher docker-compose.yml from the same store and re-apply it. Written into
	// the override's x-compose-app block at install time (see installer). Store is
	// the reference store URL; StoreAppID is the catalog id within that store.
	Store      string `yaml:"store,omitempty"`
	StoreAppID string `yaml:"store-app-id,omitempty"`

	Links []Link `yaml:"links,omitempty"`

	// Lifecycle: directories ensured before every `compose up`, and the shell
	// hooks that bracket install and up. See docs/x-compose-app.md.
	Folders []Folder `yaml:"folders,omitempty"`
	Hooks   Hooks    `yaml:"hooks,omitempty"`
}

// Dashboard views an app's tile can land in (the `view` field).
const (
	// ViewApps is the ordinary app grid, and the default for anything that does
	// not say otherwise.
	ViewApps = "apps"
	// ViewSystem is the platform's own pieces — the dashboard, its gateway, the
	// host stack. They get their own grid, and declaring it is also what makes an
	// app *protected*: Maison refuses to stop or uninstall it and the backup
	// scheduler leaves it alone. There is no operator-side list; an app is a
	// system app because its own compose says so.
	ViewSystem = "system"
	// ViewHidden keeps an app off the dashboard altogether — infrastructure with
	// nothing worth clicking.
	ViewHidden = "hidden"
)

// NormalizeView maps a declared `view` onto one of the constants above,
// defaulting to ViewApps.
//
// An unrecognised value is deliberately not an error. Unknown keys are tolerated
// everywhere else in this extension, and refusing to render an app over a
// cosmetic hint would be a worse failure than putting it in the ordinary grid.
func NormalizeView(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case ViewSystem:
		return ViewSystem
	case ViewHidden:
		return ViewHidden
	default:
		return ViewApps
	}
}

// Folder is a directory Maison creates (and takes ownership of) before it
// brings the stack up, so an app that drops privileges can write to its bind
// mounts on first boot. Paths live under the data root and may use the app's
// interpolation variables (${DATA_ROOT}, ${AppID}, ${PUID}, …).
type Folder struct {
	Path string `yaml:"path,omitempty"`
	// User and Group are a uid/gid or a name; both default to the deployment's
	// PUID/PGID.
	User  string `yaml:"user,omitempty"`
	Group string `yaml:"group,omitempty"`
	// Mode is an octal permission string, applied to Path itself. It must be
	// QUOTED in YAML (mode: "0755") — a bare 0755 is an octal int to YAML, and the
	// extension block is round-tripped through map[string]any before it reaches
	// here, which would drop the leading zero and leave a meaningless 493.
	Mode string `yaml:"mode,omitempty"`
	// Recursive applies User/Group to everything already inside Path, not just
	// Path itself — for apps that need to reclaim a tree restored from a backup.
	Recursive bool `yaml:"recursive,omitempty"`
}

// UnmarshalYAML accepts either a bare path ("- /DATA/AppData/app/config") or the
// full mapping form.
func (f *Folder) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		f.Path = n.Value
		return nil
	}
	// Scalars are decoded as text so `mode: 0755` and `user: 1000` (which YAML
	// would otherwise type as ints) survive as written.
	var raw struct {
		Path      text `yaml:"path"`
		User      text `yaml:"user"`
		Group     text `yaml:"group"`
		Mode      text `yaml:"mode"`
		Recursive bool `yaml:"recursive"`
	}
	if err := n.Decode(&raw); err != nil {
		return err
	}
	*f = Folder{
		Path:      string(raw.Path),
		User:      string(raw.User),
		Group:     string(raw.Group),
		Mode:      string(raw.Mode),
		Recursive: raw.Recursive,
	}
	return nil
}

// text is a string that accepts any YAML scalar verbatim, keeping `0755` octal
// and `1000` numeric values from being retyped.
type text string

func (t *text) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return errors.New("expected a scalar")
	}
	*t = text(n.Value)
	return nil
}

// Hooks are host shell snippets run around an app's lifecycle. The install hooks
// run once, when Maison first installs the app; the up hooks run on every
// `docker compose up` (install, start, update, config save).
type Hooks struct {
	PreInstall  string `yaml:"pre_install,omitempty"`
	PostInstall string `yaml:"post_install,omitempty"`
	PreUp       string `yaml:"pre_up,omitempty"`
	PostUp      string `yaml:"post_up,omitempty"`
}

// Link is an extra button on the app detail view (absolute URL only).
type Link struct {
	Name string `yaml:"name,omitempty"`
	URL  string `yaml:"url,omitempty"`
	Icon string `yaml:"icon,omitempty"`
}

// Localized is a value that may be written as a bare string or a locale map.
type Localized map[string]string

// UnmarshalYAML accepts either a scalar ("Jellyfin") or a map ({en_us: Jellyfin}).
func (l *Localized) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*l = Localized{"en_us": n.Value}
		return nil
	}
	var m map[string]string
	if err := n.Decode(&m); err != nil {
		return err
	}
	*l = m
	return nil
}

// Value returns the en_us entry if present, else any entry, else "".
func (l Localized) Value() string {
	if l == nil {
		return ""
	}
	if v, ok := l["en_us"]; ok {
		return v
	}
	for _, v := range l {
		return v
	}
	return ""
}

// Parse decodes an x-compose-app extension map into an App. It returns
// ErrNoExtension when the block is absent and ErrUnsupportedVersion for a
// schema_version newer than this build — both signal the caller to fall back to
// x-casaos.
func Parse(ext map[string]any) (*App, error) {
	if ext == nil {
		return nil, ErrNoExtension
	}
	b, err := yaml.Marshal(ext)
	if err != nil {
		return nil, err
	}
	var a App
	if err := yaml.Unmarshal(b, &a); err != nil {
		return nil, err
	}
	if a.Schema != 0 && a.Schema > SchemaVersion {
		return nil, ErrUnsupportedVersion
	}
	return &a, nil
}

// WebURL builds the click URL from the webui-* fields, resolving host
// placeholders against domain (the deployment's REF_DOMAIN). It returns "" when
// there is no host or the host cannot be resolved — the tile then shows the
// "no reachable address" hint instead of a broken link.
func (a *App) WebURL(domain string) string {
	host := resolveHost(a.WebUIHost, domain)
	if host == "" {
		return ""
	}
	scheme := strings.TrimSpace(a.WebUIScheme)
	if scheme == "" {
		scheme = "https"
	}
	port := ""
	if p := strings.TrimSpace(a.WebUIPort); p != "" {
		port = ":" + p
	}
	path := a.WebUIPath
	if path == "" {
		path = "/"
	} else if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return scheme + "://" + host + port + path
}

// resolveHost substitutes deployment placeholders in a webui-host template.
// ${domain}/${DOMAIN} → domain. If the template references a domain that isn't
// configured, or any placeholder is left unresolved, it returns "" so the URL is
// reported as unreachable rather than built broken.
func resolveHost(host, domain string) string {
	h := strings.TrimSpace(host)
	if h == "" {
		return ""
	}
	if strings.Contains(h, "${domain}") || strings.Contains(h, "${DOMAIN}") {
		if domain == "" {
			return ""
		}
		h = strings.ReplaceAll(h, "${domain}", domain)
		h = strings.ReplaceAll(h, "${DOMAIN}", domain)
	}
	if strings.Contains(h, "${") {
		return "" // an unresolved placeholder we don't understand
	}
	return h
}
