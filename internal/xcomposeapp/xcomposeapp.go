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
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ExtensionKey is the compose extension key this package reads.
const ExtensionKey = "x-compose-app"

var (
	// ErrNoExtension is returned when a compose file has no x-compose-app block.
	ErrNoExtension = errors.New("x-compose-app extension not found")
	// ErrUnsupportedVersion is returned for a schema_version this build predates.
	// The caller treats the block as absent: see composefile.Resolve.
	ErrUnsupportedVersion = errors.New("x-compose-app schema_version not supported")
)

// SchemaVersion is the highest x-compose-app schema this build understands.
// v2 added `folders` and `hooks` (since replaced by `steps`); v1 files keep working unchanged.
const SchemaVersion = 2

// App is the Maison-native app metadata. Only the fields Maison consumes are
// modelled; unknown keys are ignored.
type App struct {
	Schema   int       `yaml:"schema_version,omitempty"`
	ID       string    `yaml:"id,omitempty"`
	Title    Localized `yaml:"title,omitempty"`
	Icon     string    `yaml:"icon,omitempty"`
	Category string    `yaml:"category,omitempty"`
	// Version is the app's release as people read it (`2.4.0`, `2024.10.1-ls45`).
	// Display only: nothing compares it. Which data an install holds is
	// data_version's job, read apart from App — see ParseData and
	// docs/migrations.md. Presentation, so it does not raise SchemaVersion.
	Version string `yaml:"version,omitempty"`
	// View is the dashboard grid the app's tile lands in — see the View*
	// constants. Presentation only, which is why it does NOT raise
	// SchemaVersion: a build that predates it renders the app in the ordinary
	// grid, which is exactly where the app used to be.
	View string `yaml:"view,omitempty"`
	// Parent names the app this one extends, by Compose project name. An app with
	// a resolvable parent is an *extension*: the dashboard gives it no top-level
	// tile and nests it under the parent's instead.
	//
	// Like View this is presentation, so it does NOT raise SchemaVersion — a build
	// that predates the key renders an ordinary tile, which is exactly where an
	// unresolvable parent lands anyway.
	//
	// The value is an unverified claim about *another maintainer's* app, so it is
	// resolved against what is actually installed rather than trusted: that rule
	// lives with the app listing, the only place that knows what else is on the
	// box — see apps.resolveParents.
	Parent        string    `yaml:"parent,omitempty"`
	Tagline       Localized `yaml:"tagline,omitempty"`
	Description   Localized `yaml:"description,omitempty"`
	Developer     string    `yaml:"developer,omitempty"`
	Screenshots   []string  `yaml:"screenshots,omitempty"`
	Thumbnail     string    `yaml:"thumbnail,omitempty"`
	Architectures []string  `yaml:"architectures,omitempty"`
	// MinMemory is the memory the app needs, in MB. Advisory, shown in the store.
	MinMemory int `yaml:"min_memory,omitempty"`

	// Legacy is set when this App was translated from an x-casaos block (see
	// composefile.Resolve) and carries the one thing that block says which the
	// native schema has no key for. Never read from or written to YAML.
	Legacy *Legacy `yaml:"-"`

	// Tips is the app's guidance note (Markdown, may reference ${VAR}). It is also
	// where Maison persists operator edits — into the override's x-compose-app
	// block, never into the store-provided base compose.
	Tips Localized `yaml:"tips,omitempty"`

	// The click URL, declared directly (see package doc).
	WebUIHost   string `yaml:"webui-host,omitempty"`
	WebUIPort   string `yaml:"webui-port,omitempty"`
	WebUIScheme string `yaml:"webui-scheme,omitempty"`
	WebUIPath   string `yaml:"webui-path,omitempty"`

	// StoreRef is where this app was installed from, so Maison can pull a fresher
	// docker-compose.yml from the same store and re-apply it. Written into the
	// override's x-compose-app block at install time (see installer), and editable
	// from the Update tab to point an app at a different store.
	//
	// It is one locator — `<store>/-/<apps folder>/<app id>`, the same grammar the
	// store's own deep links use (appstore.ParseRef, which is the authority) — so
	// what a person copies out of the store address bar is what they can paste
	// here. See docs/app-model.md.
	StoreRef string `yaml:"store-ref,omitempty"`

	// Store / StoreAppID / StoreAppsPath are the superseded three-field spelling of
	// StoreRef, still read so an app installed before the single-locator form keeps
	// updating. Nothing writes them any more: the first install or retarget after
	// the change replaces them with store-ref (see installer.writeUpdateRef).
	Store         string `yaml:"store,omitempty"`
	StoreAppID    string `yaml:"store-app-id,omitempty"`
	StoreAppsPath string `yaml:"store-apps-path,omitempty"`

	Links []Link `yaml:"links,omitempty"`

	// Routes are the app's web endpoints, declared in the router's absence: a
	// service, the name it answers on, and the port behind it. Maison publishes
	// each one on every domain the deployment carries (see internal/routes), so an
	// app never names a domain, a proxy, or a TLS setting.
	//
	// Deliberately NOT raising SchemaVersion, for the same reason View doesn't —
	// but the trade is worth stating. A build that predates this field ignores
	// `routes` and generates nothing, so a label-free app is unreachable on it;
	// raising the version instead makes that build drop the whole x-compose-app
	// block, costing the app its folders and steps as well (see stackup.Load).
	// Unreachable-but-correctly-built beats half-built. The real guard is the
	// rollout: a store trimmed of its own labels ships as a *new* store URL, which
	// only a build that understands routes ever subscribes to.
	Routes []Route `yaml:"routes,omitempty"`

	// Bring-up: directories ensured before every `compose up`. See
	// docs/x-compose-app.md.
	Folders []Folder `yaml:"folders,omitempty"`

	// Lifecycle is what the app allows the operator to do to it: whether Maison
	// may stop it, uninstall it, and update it. All three default to yes.
	//
	// It is the app's own declaration, like Backup.Skip — and like it, separate
	// from View, which only picks the grid. See LifecycleSpec.
	Lifecycle LifecycleSpec `yaml:"lifecycle,omitempty"`

	// Backup is what this app asks Maison to leave out of its backups — the
	// directories it can rebuild on its own, or the whole app. See docs/backup.md.
	//
	// Deliberately NOT raising SchemaVersion, and the argument is stronger here than
	// it is for View or Routes: a build that predates this field ignores it and backs
	// the app up whole, which is a superset and therefore never data loss. Raising
	// the version instead makes that build drop the entire x-compose-app block (see
	// stackup.Load) and with it the app's folders, steps, title and URL — a much
	// worse trade than storing a cache directory it did not have to.
	//
	// That argument survives `skip` intact, and it is also what fixes the field's
	// polarity: an older build ignoring `skip: true` backs the app up anyway, which
	// is again a superset. The inverse spelling — a default-on flag an old build
	// could ignore into NOT taking a backup — would not be safe, which is why this
	// one defaults to false and there is no way to say "back this up" explicitly.
	Backup BackupSpec `yaml:"backup,omitempty"`

	// Secrets are values generated ONCE and ensured in the app's .env — a key
	// already carrying a value is reused, never regenerated, because a rotated
	// signing key is indistinguishable from data loss to the app that signed with
	// the old one. Values are generator specs (see internal/secretgen), never
	// literals: that is what lets a plain map be read unambiguously. A spec may
	// be seeded — `${APP_DEFAULT_PASSWORD}|alnum:32` — to keep the value an
	// installed app already had (secretgen.ParseSpec, stackup.EnsureSecrets).
	Secrets StringMap `yaml:"secrets,omitempty"`

	// Files are individual files Maison writes into the app's tree. It is the
	// escape hatch beside the seed convention, not the main road: a file only
	// needs an entry here when the seed tree cannot express it — because it must
	// be re-rendered on every up (ensure: always), or because it needs an owner
	// or mode other than the default.
	Files []File `yaml:"files,omitempty"`

	// Steps are the script entries run around the app's stack on every up: the
	// imperative work no declaration expresses — seeding a database with the
	// app's own binary, configuring the running app, reaching the host.
	Steps []Step `yaml:"steps,omitempty"`

	// Variables are ordinary templates ensured in the app's .env on every up, so
	// a value derived from the deployment (a domain, a URL) follows the
	// deployment instead of freezing at install time. Values are always
	// templates, never generator specs — the counterpart split to Secrets.
	Variables StringMap `yaml:"variables,omitempty"`

	// Credentials are the app's initial logins, shown to the operator the first
	// time the app is opened and again from the tile menu. Username and password
	// are templates rendered against the app's .env (like Tips), so they normally
	// point at a generated secret — `password: ${ADMIN_PASSWORD}` — or, during the
	// migration away from it, at ${APP_DEFAULT_PASSWORD}.
	//
	// Display-only, so it does not raise SchemaVersion: an older build ignores the
	// block and the app still installs and runs; only the popup is missing.
	Credentials []Credential `yaml:"credentials,omitempty"`

	// Problems are the declarations Parse refused and dropped — today, user-facing
	// strings that are not a locale map with an en_us entry (see localized.go).
	// Never read from or written to YAML.
	Problems []string `yaml:"-"`
}

// Credential is one login an app declares. Without Change, Maison only displays
// it: it does not know about a password the operator later changes inside the
// app, which is why the UI calls these *initial* credentials. With Change, the
// operator can replace the password from Maison and the app is told how.
type Credential struct {
	Label    Localized         `yaml:"label,omitempty"`
	Username string            `yaml:"username,omitempty"`
	Password string            `yaml:"password,omitempty"`
	Note     Localized         `yaml:"note,omitempty"`
	Change   *CredentialChange `yaml:"change,omitempty"`
}

// How a changed credential reaches the app.
const (
	// ApplyEnv: the app reads the variable on every start, so writing .env and
	// recreating the stack is the whole change.
	ApplyEnv = "env"
	// ApplyScript: the app keeps its own copy (a user database), so a script
	// changes it inside the running app first; .env follows.
	ApplyScript = "script"
)

// CredentialChange says that a credential may be changed from Maison, and how.
// Absent, the credential is initial-only: writing .env would make the dialog
// show a password the app does not accept.
type CredentialChange struct {
	Apply  string           `yaml:"apply,omitempty"`
	Policy CredentialPolicy `yaml:"policy,omitempty"`
	// Script is the script entry ApplyScript runs — the same runner, environment
	// and command set as a step — with the secret's variable set to the NEW value
	// and OLD_<NAME> to the previous one.
	Script `yaml:",inline"`
}

// CredentialPolicy constrains a new value, typed or generated. Zero fields take
// the defaults secretgen.Policy applies; Maison's own forbidden characters apply
// whatever the app says.
type CredentialPolicy struct {
	MinLength int      `yaml:"min_length,omitempty" json:"min_length,omitempty"`
	MaxLength int      `yaml:"max_length,omitempty" json:"max_length,omitempty"`
	Charset   string   `yaml:"charset,omitempty" json:"charset,omitempty"`
	Require   []string `yaml:"require,omitempty" json:"require,omitempty"`
	Forbid    string   `yaml:"forbid,omitempty" json:"forbid,omitempty"`
}

// UnmarshalYAML decodes through tolerant scalars, so a numeric password or
// username (`password: 123456`) types as written. An entry that is not a mapping
// decodes as empty and is dropped at render time rather than failing the block —
// Parse's error costs the app its whole x-compose-app (see Route).
func (c *Credential) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		*c = Credential{}
		return nil
	}
	var raw struct {
		Label    Localized `yaml:"label"`
		Username text      `yaml:"username"`
		Password text      `yaml:"password"`
		Note     Localized `yaml:"note"`
		Change   yaml.Node `yaml:"change"`
	}
	if err := n.Decode(&raw); err != nil {
		return err
	}
	*c = Credential{
		Label:    raw.Label,
		Username: string(raw.Username),
		Password: string(raw.Password),
		Note:     raw.Note,
		Change:   decodeChange(&raw.Change),
	}
	return nil
}

// decodeChange reads a credential's `change` block, or nil. A malformed block
// makes the credential initial-only rather than failing the decode: the login is
// still worth showing, and a change Maison cannot read must not be offered.
func decodeChange(n *yaml.Node) *CredentialChange {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	var ch CredentialChange
	if err := n.Decode(&ch); err != nil {
		return nil
	}
	ch.Apply = strings.ToLower(strings.TrimSpace(ch.Apply))
	switch ch.Apply {
	case ApplyEnv:
	case ApplyScript:
		if ch.Script.Validate() != nil {
			return nil
		}
	default:
		return nil
	}
	return &ch
}

// Ensure modes for a File: written once if absent, or re-rendered on every up.
const (
	// EnsureOnce writes the file only when it is not there. The default, and what
	// a seed file does.
	EnsureOnce = "once"
	// EnsureAlways re-renders on every up, so a config that embeds a deployment
	// value (a domain, a URL) follows a change to it instead of freezing at
	// install time. The app or an operator editing such a file will lose the edit
	// on the next start, which is the trade the declaration is making.
	EnsureAlways = "always"
)

// File is one file Maison ensures in the app's tree.
//
// Content comes from exactly one of From (a path in the app's seed tree, rendered
// if it carries the .tmpl suffix) or Content (an inline body, always rendered).
type File struct {
	Path    string `yaml:"path,omitempty"`
	From    string `yaml:"from,omitempty"`
	Content string `yaml:"content,omitempty"`
	// Ensure is EnsureOnce (default) or EnsureAlways.
	Ensure string `yaml:"ensure,omitempty"`
	// User, Group and Mode carry the same meaning, defaults and quoting rule as
	// on Folder — mode: "0644" must be quoted.
	User  string `yaml:"user,omitempty"`
	Group string `yaml:"group,omitempty"`
	Mode  string `yaml:"mode,omitempty"`
}

// UnmarshalYAML decodes the scalar fields as text, so `mode: 0644` and
// `user: 1000` survive YAML's retyping the way Folder's do.
func (f *File) UnmarshalYAML(n *yaml.Node) error {
	var raw struct {
		Path    text `yaml:"path"`
		From    text `yaml:"from"`
		Content text `yaml:"content"`
		Ensure  text `yaml:"ensure"`
		User    text `yaml:"user"`
		Group   text `yaml:"group"`
		Mode    text `yaml:"mode"`
	}
	if err := n.Decode(&raw); err != nil {
		return err
	}
	*f = File{
		Path:    string(raw.Path),
		From:    string(raw.From),
		Content: string(raw.Content),
		Ensure:  string(raw.Ensure),
		User:    string(raw.User),
		Group:   string(raw.Group),
		Mode:    string(raw.Mode),
	}
	return nil
}

// StringMap is a YAML mapping whose values are taken verbatim as text.
//
// It exists so `variables: PORT: 8080` and `secrets: KEY: hex:32` are read the
// same way: plain map[string]string refuses the first (YAML types it as an int),
// and the extension block round-trips through map[string]any before it reaches
// here, so a retyped scalar cannot be recovered later.
type StringMap map[string]string

// UnmarshalYAML decodes each value through text, which accepts any scalar.
func (m *StringMap) UnmarshalYAML(n *yaml.Node) error {
	var raw map[string]text
	if err := n.Decode(&raw); err != nil {
		return err
	}
	out := make(StringMap, len(raw))
	for k, v := range raw {
		out[k] = string(v)
	}
	*m = out
	return nil
}

// Dashboard views an app's tile can land in (the `view` field).
const (
	// ViewApps is the ordinary app grid, and the default for anything that does
	// not say otherwise.
	ViewApps = "apps"
	// ViewSystem is the platform's own pieces — the dashboard, its gateway, the
	// host stack. They get their own grid, and that is all the value does: it is a
	// category, not a behaviour. Whether such an app can be stopped, uninstalled
	// or backed up is declared separately (Lifecycle, Backup.Skip), so the grid an
	// app sits in never decides what Maison will do to it.
	ViewSystem = "system"
	// ViewService is an ordinary app with no web UI — a file share, a database, a
	// peer port. It is the one view that is normally *derived* rather than
	// declared: an app that declares no view and no web UI lands here (see
	// apps.buildApp). Like every view, it buys no behaviour.
	ViewService = "service"
)

// NormalizeView maps a declared `view` onto one of the constants above,
// defaulting to ViewApps.
//
// An unrecognised value is deliberately not an error. Unknown keys are tolerated
// everywhere else in this extension, and refusing to render an app over a
// cosmetic hint would be a worse failure than putting it in the ordinary grid.
func NormalizeView(v string) string {
	if view, ok := DeclaredView(v); ok {
		return view
	}
	return ViewApps
}

// DeclaredView is NormalizeView without the default: it reports whether v names
// a view at all, so a caller can tell "the maintainer chose the app grid" from
// "the maintainer said nothing" and derive a view only for the latter. An
// unrecognised value counts as saying nothing.
func DeclaredView(v string) (string, bool) {
	switch view := strings.ToLower(strings.TrimSpace(v)); view {
	case ViewApps, ViewSystem, ViewService:
		return view, true
	default:
		return "", false
	}
}

// Legacy is what an x-casaos block says that x-compose-app has no key for.
type Legacy struct {
	// WebUIPort is x-casaos `webui_port`: the web UI's port *inside* the main
	// container. Only used to pick which published host port opens the app.
	WebUIPort string
}

// DeclaresWebUI reports whether the block names a web endpoint: a click-URL
// host, or a route Maison publishes. A webui-path or webui-port alone is not an
// address, so neither counts — except on a translated x-casaos block, where the
// port (`port_map`, or the container's `webui_port`) is how CasaOS names one.
func (a *App) DeclaresWebUI() bool {
	if a == nil {
		return false
	}
	if strings.TrimSpace(a.WebUIHost) != "" || len(a.Routes) > 0 {
		return true
	}
	return a.Legacy != nil && (strings.TrimSpace(a.WebUIPort) != "" || strings.TrimSpace(a.Legacy.WebUIPort) != "")
}

// WebUIContainerPort is the port the web UI listens on inside its container,
// when the metadata says: x-casaos `webui_port` on a translated block, else the
// upstream port of the app's first route. 0 when unknown.
func (a *App) WebUIContainerPort() int {
	if a == nil {
		return 0
	}
	if a.Legacy != nil {
		if n, err := strconv.Atoi(strings.TrimSpace(a.Legacy.WebUIPort)); err == nil {
			return n
		}
		return 0
	}
	for _, r := range a.Routes {
		if n, err := strconv.Atoi(strings.TrimSpace(r.UpstreamPort)); err == nil {
			return n
		}
	}
	return 0
}

// BackupSpec is the app's say in how it is backed up.
//
// Exclude names directories holding **derived** data — thumbnails, transcodes,
// search indexes, model downloads — which the app can rebuild and which would
// otherwise ship on every backup and be stored offsite forever. It is a declaration
// by the app's author, never a heuristic: Maison does not guess that a directory
// looks like a cache, because a wrong guess silently drops real data from a backup.
//
// Each entry is a directory, app-folder-relative (`cache/`, `data/transcodes/`) or
// `**/<name>/` for that name at any depth; an absolute path inside the app's own
// folder is accepted too, since that is the spelling `folders:` uses. Everything
// else is refused, one entry at a time — see internal/exclude, which owns the
// grammar and is what both backup engines read it through.
//
// What is excluded is not restored either: a restore replaces the app folder, so a
// declared directory comes back empty and the app refills it. That is the point of
// declaring it, and it is why `folders:` and this list belong side by side.
//
// Skip is the whole-app form of the same declaration: there is nothing in this
// folder worth keeping. It is not a bigger Exclude — it removes the app from the
// nightly run and refuses a backup of it outright, rather than snapshotting an empty
// tree. Restore is deliberately untouched: an app can be marked skipped while older
// backups still exist, and those must stay restorable.
//
// It is separate from `view: system` on purpose. That field used to decide three
// unrelated things at once (tile grouping, refusal to stop or uninstall, and
// skipped-by-backup), so an ordinary app with nothing worth backing up had no way to
// say so without claiming to be a platform piece, and a platform piece had no way to
// ask to be backed up. View is now only the grid; this says one thing.
//
// It is also not the per-app opt-out the *user* gets in the UI. This is the author
// saying there is nothing here; that would be the owner saying they do not want it.
type BackupSpec struct {
	Exclude []string `yaml:"exclude,omitempty"`
	Skip    bool     `yaml:"skip,omitempty"`
}

// UnmarshalYAML decodes the entries through text, so a pattern YAML would otherwise
// retype survives as written — the extension block round-trips through
// map[string]any in internal/composefile before it ever reaches this package.
// Skip is a plain bool rather than text: it is not a scalar YAML could retype into
// something lossy, and the zero value is the right default on its own.
func (b *BackupSpec) UnmarshalYAML(n *yaml.Node) error {
	var raw struct {
		Exclude []text `yaml:"exclude"`
		Skip    bool   `yaml:"skip"`
	}
	if err := n.Decode(&raw); err != nil {
		return err
	}
	b.Exclude = nil
	for _, e := range raw.Exclude {
		b.Exclude = append(b.Exclude, string(e))
	}
	b.Skip = raw.Skip
	return nil
}

// LifecycleSpec is the app's say in what the operator may do to it.
//
// Stoppable false is for an app whose absence takes the operator's way back in with
// it — the dashboard, the gateway in front of it, the login broker. Maison refuses
// Stop, updates the app without stopping it first, and leaves it out of "Update all"
// (it is updated only when named on its own), because each of those takes it down
// with nothing left running to bring it back. Restart and Start stay available: the
// stack comes back on its own, and that is how a wedged app is recovered without SSH.
//
// Uninstallable false is for an app the box cannot do without. Maison refuses
// Uninstall, and the cleanup sweep never counts its containers as orphans.
//
// Updatable false is for an app something else keeps up to date — a platform
// stack its installer pins and upgrades. Maison leaves it out of the update system
// altogether: no row on the Updates page (not even "not linked to a store"), no
// Update tab, and the update and update-source routes refuse it. Its version is
// its deployer's business, and offering to "link" it to a look-alike store app
// would replace it in place on the next update.
//
// All three default to true, which is the opposite polarity to Backup.Skip and safe for
// the opposite reason: a build that predates a field ignores a `false` and offers
// Stop, Uninstall or Update, which is the ordinary behaviour of every app. Platform stacks
// that must stay guarded on such a build also keep `view: system`, from which those
// builds derived the same guard.
//
// It is a foot-gun guard, not a security boundary: the app declares it about itself,
// and the operator can edit it out through Settings → override, which is the escape
// hatch for one who genuinely means to remove a platform piece. That a store app
// declares it at all is a store-review question, not something Maison polices.
//
// Pointers, because "unset" must read as true and a bool's zero value cannot say so.
// Read them through CanStop, CanUninstall and CanUpdate, never directly.
type LifecycleSpec struct {
	Stoppable     *bool `yaml:"stoppable,omitempty"`
	Uninstallable *bool `yaml:"uninstallable,omitempty"`
	Updatable     *bool `yaml:"updatable,omitempty"`
}

// CanStop reports whether the app allows Maison to stop it. True unless declared false.
func (l LifecycleSpec) CanStop() bool { return l.Stoppable == nil || *l.Stoppable }

// CanUninstall reports whether the app allows Maison to uninstall it. True unless
// declared false.
func (l LifecycleSpec) CanUninstall() bool { return l.Uninstallable == nil || *l.Uninstallable }

// CanUpdate reports whether the app allows Maison to update it. True unless
// declared false.
func (l LifecycleSpec) CanUpdate() bool { return l.Updatable == nil || *l.Updatable }

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

// Link is an extra button on the app detail view (absolute URL only).
type Link struct {
	Name Localized `yaml:"name,omitempty"`
	URL  string    `yaml:"url,omitempty"`
	Icon string    `yaml:"icon,omitempty"`
}

// Route is one web endpoint of an app, stated without reference to a reverse
// proxy: which service serves it, the name it answers on, and the port behind it.
//
// It carries no domain. The deployment's domains are applied to every route alike
// (internal/routes), which is what lets the same app compose run behind this
// deployment's gateway, behind someone else's, or behind nothing at all.
//
// The optional fields exist because three real store apps need them and every
// reverse proxy has the concept — an upstream that speaks TLS, an upstream whose
// certificate cannot be verified, a body larger than the proxy's default cap. That
// is the bar for adding another one: if only one proxy has the concept, it belongs
// in hand-written labels, not here.
type Route struct {
	// Service is the compose service the labels are written onto — which must be
	// the service that serves the port, since a generated upstream is resolved in
	// that container's network context. Empty means the app's only service, so
	// an app with several must name it.
	Service string `yaml:"service,omitempty"`

	// Name is the host's leading label — `outline` in `outline-${APP_DOMAIN}`.
	// Empty means the app's id.
	Name string `yaml:"name,omitempty"`

	// UpstreamPort is the port inside the container. It is *not* WebUIPort, which
	// is the port in the click URL and is empty for anything behind a gateway on
	// 443; the two differ for essentially every routed app, which is why this one
	// is not called `port`.
	//
	// A string, decoded through text, so `80` and `"80"` are both accepted: a
	// store author will write it bare, and a type error here does not cost the app
	// its routes — it costs it its whole x-compose-app block, and with it the
	// tile's name, icon and URL (every caller of Parse discards the error).
	UpstreamPort string `yaml:"upstream-port,omitempty"`

	// UpstreamScheme is how the proxy should reach the container: "http" (default)
	// or "https", for an app that terminates TLS itself.
	UpstreamScheme string `yaml:"upstream-scheme,omitempty"`

	// InsecureUpstream skips verification of the upstream's certificate — needed
	// by apps that serve HTTPS with a self-signed certificate of their own.
	InsecureUpstream bool `yaml:"insecure-upstream,omitempty"`

	// MaxBody raises the proxy's request-body cap for this route, as a size string
	// the proxy understands ("10G"). Empty leaves the proxy's default.
	MaxBody string `yaml:"max-body,omitempty"`
}

// UnmarshalYAML decodes a route through tolerant scalars, so a port or a size
// written bare (`upstream-port: 80`, `max-body: 10G`) types as written.
//
// Like Folder's, and for a sharper reason: Parse's error is discarded by every
// caller (apps.mergedMeta, apps.GetConfig), so one mistyped scalar anywhere in
// this block costs the app its title, icon, category and click URL — not just its
// routes. A route Maison cannot read is a route it should skip, never an app it
// refuses to render.
func (r *Route) UnmarshalYAML(n *yaml.Node) error {
	var raw struct {
		Service          text `yaml:"service"`
		Name             text `yaml:"name"`
		UpstreamPort     text `yaml:"upstream-port"`
		UpstreamScheme   text `yaml:"upstream-scheme"`
		InsecureUpstream bool `yaml:"insecure-upstream"`
		MaxBody          text `yaml:"max-body"`
	}
	if err := n.Decode(&raw); err != nil {
		return err
	}
	*r = Route{
		Service:          string(raw.Service),
		Name:             string(raw.Name),
		UpstreamPort:     string(raw.UpstreamPort),
		UpstreamScheme:   string(raw.UpstreamScheme),
		InsecureUpstream: raw.InsecureUpstream,
		MaxBody:          string(raw.MaxBody),
	}
	return nil
}

// Localized is a user-facing string: a locale map ({en_us: Jellyfin, fr_fr: …})
// that must carry en_us. Parse refuses any other spelling before it gets here (see
// localized.go); a bare string reaching this decoder anyway reads as absent.
type Localized map[string]string

// UnmarshalYAML accepts a locale map only. A scalar decodes as nil rather than an
// error, because an error here would cost the app its whole x-compose-app block.
func (l *Localized) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		*l = nil
		return nil
	}
	var m map[string]string
	if err := n.Decode(&m); err != nil {
		return err
	}
	*l = m
	return nil
}

// Value returns the en_us entry — the text for server-side readers (logs, mail,
// incidents), which have no viewer locale. The UI picks its own locale from the map.
func (l Localized) Value() string { return l.In("en_us") }

// In returns the entry for locale, falling back to en_us, else "".
func (l Localized) In(locale string) string {
	if v := l[locale]; v != "" {
		return v
	}
	return l["en_us"]
}

// Map applies f to every entry — rendering ${VAR} references, say — and drops the
// entries it empties.
func (l Localized) Map(f func(string) string) Localized {
	if l == nil {
		return nil
	}
	out := make(Localized, len(l))
	for k, v := range l {
		if v = f(v); v != "" {
			out[k] = v
		}
	}
	return out
}

// Parse decodes an x-compose-app extension map into an App. It returns
// ErrNoExtension when the block is absent and ErrUnsupportedVersion for a
// schema_version newer than this build. It reads x-compose-app only; the
// x-casaos fallback for a compose with no x-compose-app is composefile.Resolve's.
func Parse(ext map[string]any) (*App, error) {
	if ext == nil {
		return nil, ErrNoExtension
	}
	ext, problems := checkLocalized(ext)
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
	a.Problems = problems
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

// domainTokens are the webui-host spellings of the deployment domain. ${domain} is
// the store's webui-host convention; ${APP_DOMAIN} is the variable every other part
// of an app's compose (caddy labels, REDIRECT_HOST_SUFFIXES) uses, so a webui-host
// copied from its caddy_0 label must resolve too — it used to fall through as
// "unknown placeholder" and leave the tile unclickable (the template-root Terminal
// system app, 2026-09-29). All three are the same value: .env.app's APP_DOMAIN.
var domainTokens = []string{"${domain}", "${DOMAIN}", "${APP_DOMAIN}"}

// resolveHost substitutes deployment placeholders in a webui-host template.
// Any of domainTokens → domain. If the template references a domain that isn't
// configured, or any placeholder is left unresolved, it returns "" so the URL is
// reported as unreachable rather than built broken.
func resolveHost(host, domain string) string {
	h := strings.TrimSpace(host)
	if h == "" {
		return ""
	}
	for _, tok := range domainTokens {
		if !strings.Contains(h, tok) {
			continue
		}
		if domain == "" {
			return ""
		}
		h = strings.ReplaceAll(h, tok, domain)
	}
	if strings.Contains(h, "${") {
		return "" // an unresolved placeholder we don't understand
	}
	return h
}
