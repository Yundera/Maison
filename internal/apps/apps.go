// Package apps builds the dashboard's view of installed applications by
// reconciling Maison-managed compose projects (on disk) with what is actually
// running in Docker, and surfacing externally-created x-casaos stacks as
// "unmanaged" apps.
package apps

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yundera/maison/internal/asset"
	"github.com/yundera/maison/internal/composefile"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/dockerx"
	"github.com/yundera/maison/internal/stackup"
	"github.com/yundera/maison/internal/xcasaos"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// Status values for an app tile.
const (
	StatusRunning = "running"
	StatusStopped = "stopped"
	StatusPartial = "partial"
)

// App is one dashboard tile.
type App struct {
	ID       string `json:"id"`      // compose project name
	Name     string `json:"name"`    // display title
	Icon     string `json:"icon"`    // icon URL
	Status   string `json:"status"`  // running|stopped|partial
	Managed  bool   `json:"managed"` // installed by Maison
	Store    string `json:"store,omitempty"`
	Scheme   string `json:"scheme,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	Port     string `json:"port,omitempty"`
	Index    string `json:"index,omitempty"`
	Category string `json:"category,omitempty"`
	// URL is a fully-resolved click URL, set when x-compose-app declares one
	// (webui-*). When empty, the frontend derives the URL from the legacy
	// scheme/hostname/port/index fields.
	URL string `json:"url,omitempty"`
	// Health is the aggregated Docker health-check verdict: "healthy",
	// "unhealthy", "starting", or "" when no container declares a health check.
	// Drives the tile's top-left status dot (green/orange).
	Health string `json:"health,omitempty"`
	// View is the dashboard grid this tile belongs in — "apps" (the default),
	// "system", or "hidden" (no tile at all). Declared by the app's own
	// x-compose-app `view`; see xcomposeapp.NormalizeView.
	View string `json:"view,omitempty"`
	// Protected marks a system app: it renders as an ordinary tile in the System
	// grid, but Maison refuses to stop or uninstall it (the menu withholds those
	// entries and the API answers 403), and the backup scheduler skips it.
	//
	// It is derived from View in one place — buildApp — rather than resolved
	// independently by each guard, so the tile, the API and the scheduler cannot
	// disagree about what is protected. That single derivation is also where an
	// explicit `protected:` key would slot in, should a system-looking app ever
	// need to stay uninstallable.
	Protected bool `json:"protected,omitempty"`
	// Busy is set while a lifecycle operation (start/stop/restart/uninstall) is
	// in flight for this app. The tile then shows a "…" overlay and hides its
	// burger menu until the operation settles.
	Busy bool `json:"busy,omitempty"`
	// Install progress, overlaid by the server from the installer's tracker while
	// a store install is in flight (see installer.InstallState). The tile renders
	// tracks in turn on one bar — Download (image pull) then Start (Docker
	// bring-up) — while Installing is true. Never set by List() itself.
	Installing   bool    `json:"installing,omitempty"`
	Download     float64 `json:"download,omitempty"`
	Start        float64 `json:"start,omitempty"`
	Phase        string  `json:"phase,omitempty"`
	InstallError string  `json:"install_error,omitempty"`
	// Uninstall progress, the mirror image of the install fields above and
	// overlaid the same way (see UninstallState). The tile renders the same one
	// bar, in red — UninstallBackup (backing the app up through the default engine),
	// then Archive (finalising it), then Remove (containers and folder) — while
	// Uninstalling is true. Also never set by List() itself.
	//
	// UninstallBackup is spelled out rather than reusing Copy below: an uninstall and a
	// backup are separate overlays with separate booleans, and a tile carrying both
	// fields under one name could not say which operation a number belonged to.
	Uninstalling    bool    `json:"uninstalling,omitempty"`
	UninstallBackup float64 `json:"uninstall_backup,omitempty"`
	Archive         float64 `json:"archive,omitempty"`
	Remove          float64 `json:"remove,omitempty"`
	UninstallError  string  `json:"uninstall_error,omitempty"`
	// Backup/restore progress, overlaid the same way again (see BackupState). The
	// tile renders one bar in its own colour, stepping through Copy (the live
	// pass), Sync (the stopped pass) and Compress, while BackingUp is true. Also
	// never set by List() itself.
	BackingUp   bool    `json:"backing_up,omitempty"`
	Copy        float64 `json:"copy,omitempty"`
	Sync        float64 `json:"sync,omitempty"`
	Compress    float64 `json:"compress,omitempty"`
	BackupError string  `json:"backup_error,omitempty"`
	// What the bar is made of, beyond how full it is: the byte counts behind the
	// current phase, the transfer rate, and how long the phase has left. All four are
	// omitted when unknown — which is the honest answer for an engine that reports no
	// byte counts, and for the opening seconds of one that does — so the tile shows a
	// plain bar rather than "0 B/s, 0s left".
	BackupDone  int64   `json:"backup_done,omitempty"`
	BackupTotal int64   `json:"backup_total,omitempty"`
	BackupRate  float64 `json:"backup_rate,omitempty"`
	BackupETA   int     `json:"backup_eta,omitempty"`
}

// Health verdicts, aggregated across a project's containers.
const (
	HealthHealthy   = "healthy"
	HealthUnhealthy = "unhealthy"
	HealthStarting  = "starting"
)

// Registry reconciles on-disk projects with Docker state.
type Registry struct {
	cfg config.Config
	dx  *dockerx.Client

	// OnChange, if set, is invoked whenever the busy set changes so the server
	// can rebroadcast the app list (making the "…" overlay appear/disappear
	// live). Optional.
	OnChange func()

	// OnProgress, if set, is invoked as a tracked uninstall's or backup's progress
	// advances. It is the fine-grained twin of OnChange — events are frequent (per
	// copied chunk), so the server is expected to throttle it. Optional.
	OnProgress func()

	// Engines is the set of backup engines. Nil means the built-in local one alone —
	// archives on the data disk — which needs no configuration and is always
	// available, so a Registry built without it behaves exactly as Maison always has.
	//
	// It is an exported field set after construction, like OnChange, rather than a
	// New parameter: engines are an optional collaborator wired by the server, and
	// making them a parameter would force every caller with no opinion about backups
	// to have one.
	Engines Engines

	// StoppedPassTimeout bounds how long an app may be held down while it is backed
	// up or restored. Zero uses defaultStoppedPassTimeout. It exists because the
	// engine reads the app folder directly rather than a frozen copy, so a hung
	// repository would otherwise extend an outage instead of failing a backup.
	StoppedPassTimeout time.Duration

	// containers is the host's container listing, cached. Every read of Docker
	// state that only *describes* the box goes through it; the reads that decide
	// whether to stop or restart an app (isRunning) deliberately do not.
	containers *containerCache

	mu         sync.Mutex
	views      map[string]string          // app id -> view from the last listing
	busy       map[string]int             // app id -> in-flight operation count
	uninstalls map[string]*UninstallState // app id -> live uninstall progress
	backups    map[string]*BackupState    // app id -> live backup/restore progress
}

// workingDirTimeout bounds the container lookup that locates an unmanaged stack's
// compose. It is only reached when the app list hasn't been built yet, so a
// slow or wedged daemon costs one guard check, not the dashboard. Since the lookup
// reads the cache, it waits at all only before the first listing has landed.
const workingDirTimeout = 5 * time.Second

// New creates a Registry.
func New(cfg config.Config, dx *dockerx.Client) *Registry {
	r := &Registry{
		cfg:        cfg,
		dx:         dx,
		busy:       map[string]int{},
		uninstalls: map[string]*UninstallState{},
		backups:    map[string]*BackupState{},
	}
	r.containers = &containerCache{
		list: func(ctx context.Context) ([]dockerx.Container, error) {
			if dx == nil {
				return nil, errNoDocker
			}
			return dx.ListProjectContainers(ctx)
		},
		// A refresh that landed after its reader had already been served has to
		// announce itself, or the grid keeps the answer it was given. changed() is
		// the same signal a busy tile uses, so this needs no new plumbing — and it
		// reads OnChange at call time, which the server sets after construction.
		onRefresh: r.changed,
	}
	return r
}

// InvalidateContainers marks the cached container listing out of date. The server
// calls it when Docker reports a container event, before rebroadcasting the app
// list.
func (r *Registry) InvalidateContainers() {
	r.containers.invalidate()
}

// enter/leave bracket an in-flight lifecycle operation on id. A counter (not a
// bool) tolerates nesting — e.g. Start delegating to EnsureStarted. OnChange
// fires on both edges so the tile's busy overlay tracks the operation live.
func (r *Registry) enter(id string) {
	r.mu.Lock()
	r.busy[id]++
	r.mu.Unlock()
	r.changed()
}

func (r *Registry) leave(id string) {
	r.mu.Lock()
	if r.busy[id] > 0 {
		r.busy[id]--
		if r.busy[id] == 0 {
			delete(r.busy, id)
		}
	}
	r.mu.Unlock()
	r.changed()
}

// WithBusy runs fn while marking id busy, so the tile shows its "…" overlay and
// hides the burger menu for the duration (e.g. while a store update is applied
// out-of-band by the installer). See docs/app-model.md.
func (r *Registry) WithBusy(id string, fn func() error) error {
	r.enter(id)
	defer r.leave(id)
	return fn()
}

func (r *Registry) isBusy(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.busy[id] > 0
}

func (r *Registry) changed() {
	if r.OnChange != nil {
		r.OnChange()
	}
}

type projectState struct {
	workingDir string
	running    int
	total      int
	healthy    int
	unhealthy  int
	starting   int
	svcPorts   map[string][]dockerx.Port // service name -> published ports
}

// health aggregates the project's per-container verdicts into one dot state:
// any unhealthy container wins, then any still-starting, then healthy; "" when
// no container declares a health check.
func (ps *projectState) health() string {
	switch {
	case ps.unhealthy > 0:
		return HealthUnhealthy
	case ps.starting > 0:
		return HealthStarting
	case ps.healthy > 0:
		return HealthHealthy
	default:
		return ""
	}
}

// List returns all app tiles, sorted by display name.
//
// Existence is driven by the filesystem, appearance by Docker (docs/app-model.md):
// managed apps come from the on-disk `AppData/<app>/` folders — a cheap, always-
// available local read — and Docker state is layered on top as best-effort, so a
// Docker query that cannot be answered yields greyed (stopped) tiles rather than an
// empty grid, and never returns an error to the caller. Externally-created x-casaos
// stacks are only surfaced when Docker actually answers.
//
// A *slow* query is not one that cannot be answered, and no longer greys anything:
// the listing is read through containerCache, which serves the last good answer
// while a refresh runs behind it. That distinction is the whole point of the cache —
// see the note there on what the call actually costs.
func (r *Registry) List(ctx context.Context) ([]App, error) {
	// Best-effort: no container state means installed apps still render (greyed)
	// instead of the grid blanking on a Docker hiccup. The cache logs its own
	// failures and serves the last good listing rather than none for as long as one
	// is worth serving, so reaching here empty means Docker has been silent for a
	// while — by which point greyed tiles are the truth.
	conts, _ := r.containers.get(ctx)

	projects := map[string]*projectState{}
	for _, c := range conts {
		// A dot in the project name is reserved (archives, internal dirs) and never
		// surfaces as a tile — see docs/app-model.md.
		if strings.Contains(c.Project, ".") {
			continue
		}
		ps := projects[c.Project]
		if ps == nil {
			ps = &projectState{workingDir: c.WorkingDir, svcPorts: map[string][]dockerx.Port{}}
			projects[c.Project] = ps
		}
		ps.total++
		if c.State == "running" {
			ps.running++
		}
		switch c.Health {
		case HealthHealthy:
			ps.healthy++
		case HealthUnhealthy:
			ps.unhealthy++
		case HealthStarting:
			ps.starting++
		}
		if len(c.Ports) > 0 {
			ps.svcPorts[c.Service] = c.Ports
		}
	}

	seen := map[string]bool{}
	var out []App

	// Managed apps first — existence comes from the folder, so these always produce
	// a tile even when Docker is unreachable. Docker state decorates them when known.
	for _, name := range r.managedDirs() {
		ps := projects[name]
		var app App
		if ps != nil {
			si, ca := r.metaFor(name, ps.workingDir)
			app = buildApp(name, si, ca, r.cfg.AppDomain(), true, statusOf(ps.running, ps.total), ps.svcPorts)
			app.Health = ps.health()
		} else {
			// Installed but down (or Docker didn't answer): greyed, stopped tile.
			si, ca := r.metaFor(name, "")
			app = buildApp(name, si, ca, r.cfg.AppDomain(), true, StatusStopped, nil)
		}
		// An installed app is rendered from the icon in its own folder, so the grid
		// does not depend on the store's CDN (see internal/appicon). The compose's
		// icon URL stays the fallback for an app that has no copy.
		if u := r.localIcon(name); u != "" {
			app.Icon = u
		}
		app.Busy = r.isBusy(name)
		out = append(out, app)
		seen[name] = true
	}

	// Unmanaged stacks discovered via Docker (externally-created x-casaos apps):
	// only knowable when Docker answered, and only if they carry recognised metadata.
	for name, ps := range projects {
		if seen[name] {
			continue
		}
		si, ca := r.metaFor(name, ps.workingDir)
		if si == nil && ca == nil {
			continue // a non-Maison stack without any recognised metadata: not ours.
		}
		app := buildApp(name, si, ca, r.cfg.AppDomain(), false, statusOf(ps.running, ps.total), ps.svcPorts)
		app.Health = ps.health()
		app.Busy = r.isBusy(name)
		out = append(out, app)
		seen[name] = true
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	r.rememberViews(out)
	return out, nil
}

// rememberViews caches the view resolved for each app on the last listing.
//
// The list is rebuilt constantly (every WebSocket broadcast), so this is the
// cheap path for Protected(): it answers from metadata already parsed, and keeps
// answering for a *stopped* unmanaged stack — whose compose location is only
// knowable from a running container. Without the cache, stopping Docker or
// stopping the stack would quietly unprotect it.
func (r *Registry) rememberViews(list []App) {
	views := make(map[string]string, len(list))
	for _, a := range list {
		views[a.ID] = a.View
	}
	r.mu.Lock()
	r.views = views
	r.mu.Unlock()
}

// cachedView returns the view remembered for an app by the last List, and
// whether it was known at all.
func (r *Registry) cachedView(id string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.views[id]
	return v, ok
}

func (r *Registry) isManaged(project string) bool {
	_, err := os.Stat(filepath.Join(r.cfg.AppsDir(), project, "docker-compose.yml"))
	return err == nil
}

func (r *Registry) managedDirs() []string {
	entries, err := os.ReadDir(r.cfg.AppsDir())
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		// A dot in the directory name hides it from the dashboard: this covers
		// uninstall archives (<app>.<date>.archive) and Maison's own
		// dot-prefixed state dir. See docs/app-model.md.
		if e.IsDir() && !strings.Contains(e.Name(), ".") && r.isManaged(e.Name()) {
			names = append(names, e.Name())
		}
	}
	return names
}

// metaFor loads an app's metadata (x-casaos and/or x-compose-app), preferring the
// Maison-managed copy, then the working dir Docker reports. For a managed app the
// user override is merged on top of the strict base, so override-only metadata (a
// webui-host pinned via the Web UI editor, say) wins. Both may be nil when neither
// file carries a recognised block.
func (r *Registry) metaFor(project, workingDir string) (*xcasaos.StoreInfo, *xcomposeapp.App) {
	dir := filepath.Join(r.cfg.AppsDir(), project)
	if base, err := composefile.Load(filepath.Join(dir, "docker-compose.yml")); err == nil {
		si, ca := mergedMeta(base, loadOptional(filepath.Join(dir, "docker-compose.override.yml")))
		if si != nil || ca != nil {
			return si, ca
		}
	}
	if workingDir != "" {
		for _, path := range []string{
			filepath.Join(workingDir, "docker-compose.yml"),
			filepath.Join(workingDir, "docker-compose.yaml"),
		} {
			f, err := composefile.Load(path)
			if err != nil {
				continue
			}
			si, _ := f.StoreInfo()
			ca, _ := f.ComposeApp()
			if si != nil || ca != nil {
				return si, ca
			}
		}
	}
	return nil, nil
}

// loadOptional loads a compose file, returning nil if it is absent/unreadable.
func loadOptional(path string) *composefile.File {
	f, err := composefile.Load(path)
	if err != nil {
		return nil
	}
	return f
}

// mergedMeta parses x-casaos / x-compose-app from base with over's blocks
// shallow-merged on top (override keys win). over may be nil.
func mergedMeta(base, over *composefile.File) (*xcasaos.StoreInfo, *xcomposeapp.App) {
	xc, xa := base.XCasaOS, base.XComposeApp
	if over != nil {
		xc = shallowMerge(xc, over.XCasaOS)
		xa = shallowMerge(xa, over.XComposeApp)
	}
	si, _ := xcasaos.Parse(xc)
	ca, _ := xcomposeapp.Parse(xa)
	return si, ca
}

// shallowMerge returns base with over's keys layered on top (over wins). Either
// map may be nil.
func shallowMerge(base, over map[string]any) map[string]any {
	if over == nil {
		return base
	}
	if base == nil {
		return over
	}
	out := make(map[string]any, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

func statusOf(running, total int) string {
	switch {
	case total == 0 || running == 0:
		return StatusStopped
	case running == total:
		return StatusRunning
	default:
		return StatusPartial
	}
}

func buildApp(name string, si *xcasaos.StoreInfo, ca *xcomposeapp.App, domain string, managed bool, status string, svcPorts map[string][]dockerx.Port) App {
	app := App{ID: name, Name: name, Managed: managed, Status: status, View: xcomposeapp.ViewApps}
	if si != nil {
		if t := xcasaos.Localized(si.Title); t != "" {
			app.Name = t
		}
		app.Icon = si.Icon
		app.Scheme = si.Scheme
		app.Hostname = si.Hostname
		app.Port = si.PortMap
		app.Index = si.Index
		app.Category = si.Category
		app.Store = si.StoreAppID
	}
	// x-compose-app wins over x-casaos, field by field. Its webui-* fields yield a
	// fully-resolved click URL, so the frontend opens app.URL directly and skips
	// the legacy scheme/hostname/port derivation below.
	if ca != nil {
		if t := ca.Title.Value(); t != "" {
			app.Name = t
		}
		if ca.Icon != "" {
			app.Icon = ca.Icon
		}
		if ca.Category != "" {
			app.Category = ca.Category
		}
		if ca.ID != "" {
			app.Store = ca.ID
		}
		app.URL = ca.WebURL(domain)
		app.View = xcomposeapp.NormalizeView(ca.View)
	}
	// A system app is a protected app: no stop, no uninstall, no scheduled
	// backup. One derivation for all three (see the Protected field).
	app.Protected = app.View == xcomposeapp.ViewSystem
	// Prefer the container's ACTUAL published host port so "Open" works without a
	// gateway. Only when x-compose-app gave no URL and no hostname (gateway route)
	// is configured.
	if app.URL == "" && app.Hostname == "" && svcPorts != nil {
		main := ""
		if si != nil {
			main = si.Main
		}
		webui := 0
		if si != nil {
			webui, _ = strconv.Atoi(si.WebUIPort)
		}
		if hp := reachableHostPort(svcPorts, main, webui); hp > 0 {
			app.Port = strconv.Itoa(hp)
		}
	}
	// A compose-relative icon names a file beside the compose, which is not something
	// a browser can fetch: an installed app's tile gets it from the copy in the app's
	// own folder (localIcon, filled by the installer or EnsureIcons), and until that
	// copy exists there is no icon to show. Passing the raw value on would render as
	// a request to the dashboard for a path that means nothing there.
	if app.Icon != "" && !asset.IsURL(app.Icon) {
		app.Icon = ""
	}
	return app
}

// reachableHostPort picks a published host port to open: the one bound to the
// web-UI port on the main service if present, otherwise the first published port.
func reachableHostPort(svcPorts map[string][]dockerx.Port, main string, webui int) int {
	try := func(ports []dockerx.Port) int {
		if webui > 0 {
			for _, p := range ports {
				if int(p.Private) == webui && p.Public > 0 {
					return int(p.Public)
				}
			}
		}
		for _, p := range ports {
			if p.Public > 0 {
				return int(p.Public)
			}
		}
		return 0
	}
	if main != "" {
		if hp := try(svcPorts[main]); hp > 0 {
			return hp
		}
	}
	for _, ports := range svcPorts {
		if hp := try(ports); hp > 0 {
			return hp
		}
	}
	return 0
}

// FindByHost resolves the app whose click URL is served at host (an app gateway
// host such as `<app>-<domain>`), so the launch gate can identify which app it is
// standing in for. It matches the app's resolved web URL host, its x-casaos
// hostname, or the `<id>-<refDomain>` convention. Returns false when no app maps
// to host.
func (r *Registry) FindByHost(ctx context.Context, host, refDomain string) (App, bool) {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" {
		return App{}, false
	}
	list, err := r.List(ctx)
	if err != nil {
		return App{}, false
	}
	for _, a := range list {
		if h := urlHost(a.URL); h != "" && strings.EqualFold(h, host) {
			return a, true
		}
		if a.Hostname != "" && strings.EqualFold(a.Hostname, host) {
			return a, true
		}
		if refDomain != "" && strings.EqualFold(a.ID+"-"+refDomain, host) {
			return a, true
		}
	}
	return App{}, false
}

// urlHost extracts the host (no port) from a full URL, or "" if it cannot parse.
func urlHost(u string) string {
	if u == "" {
		return ""
	}
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	return p.Hostname()
}

// composeFiles returns the compose file list for a managed app: the strict base
// plus its user override when present (Compose override semantics — the running
// stack is base + override). Because we pass explicit -f flags, the override is
// not auto-discovered by `docker compose`; we add it here.
func (r *Registry) composeFiles(dir string) []string {
	files := []string{filepath.Join(dir, "docker-compose.yml")}
	override := filepath.Join(dir, "docker-compose.override.yml")
	if _, err := os.Stat(override); err == nil {
		files = append(files, override)
	}
	return files
}

// EnsureStarted brings an app up. For a Maison-managed project it goes through
// stackup (ensure folders → pre_up hook → `docker compose up -d` from base +
// override → post_up hook), which is idempotent: it starts a stopped stack or
// recreates a removed one. For a discovered/unmanaged stack — one Maison has no
// compose files for — it just starts the existing containers. The tile shows a "…"
// busy overlay while it runs.
func (r *Registry) EnsureStarted(ctx context.Context, id string) error {
	r.enter(id)
	defer r.leave(id)
	// An app whose in-place restore was cut short holds neither the state it had nor
	// the one it was being given. Starting it there is worse than leaving it down: it
	// would initialise over the gap — fresh database, default config — and that
	// invented state would become the next night's backup. The refusal stands until
	// the restore is retried or rolled back.
	if r.RestoreInterrupted(id) {
		return fmt.Errorf("%s was not started: an interrupted restore left its data incomplete", id)
	}
	if r.isManaged(id) {
		dir := filepath.Join(r.cfg.AppsDir(), id)
		return stackup.Up(ctx, r.cfg, id, dir, r.composeFiles(dir))
	}
	return r.dx.StartProject(ctx, id)
}

// Start brings an app up. For a managed app this is `compose up -d` (so a fully
// down stack whose containers were removed is recreated); for an unmanaged stack
// it starts the existing containers.
func (r *Registry) Start(ctx context.Context, id string) error {
	return r.EnsureStarted(ctx, id)
}

// Republish brings every running managed app up again, so that a change to the
// deployment's domains reaches its containers: a Caddy label is read off the
// container, so it only takes effect on a recreate.
//
// Stopped apps are deliberately left alone — republishing must not resurrect an
// app the operator turned off, and it doesn't need to: their routes are
// regenerated by the up itself, whenever they are next started.
//
// One app's failure doesn't stop the rest: a stack that was already broken must
// not block every other app from being republished. The errors are logged and the
// tiles will show it.
func (r *Registry) Republish(ctx context.Context) {
	list, _ := r.List(ctx)
	for _, app := range list {
		if !app.Managed || app.Status == StatusStopped {
			continue
		}
		if err := r.EnsureStarted(ctx, app.ID); err != nil {
			log.Printf("apps: republish %s: %v", app.ID, err)
		}
	}
	r.changed()
}

// Stop brings a project down. A system app is refused: stopping the dashboard —
// or the gateway in front of it — takes the UI down with the request that asked
// for it, and nothing is left to start it again. Restart stays available, since
// the stack comes back on its own.
func (r *Registry) Stop(ctx context.Context, id string) error {
	if r.Protected(id) {
		return ErrProtected
	}
	r.enter(id)
	defer r.leave(id)
	return r.dx.StopProject(ctx, id)
}

func (r *Registry) Restart(ctx context.Context, id string) error {
	r.enter(id)
	defer r.leave(id)
	return r.dx.RestartProject(ctx, id)
}

// Protected reports whether the app is a system app — exempt from stop and
// uninstall, and skipped by the backup scheduler.
func (r *Registry) Protected(id string) bool {
	return r.viewOf(id) == xcomposeapp.ViewSystem
}

// viewOf resolves an app's declared view: from the last listing when it is
// known, else straight from the app's compose.
//
// The working-dir lookup in the fallback is not optional. An unmanaged stack has
// no folder under AppsDir, so metaFor can only reach its compose through the
// directory Docker reports for the project — and the platform's own discovered
// stacks are exactly the apps this guard exists for. Resolving them with an empty
// working dir would find no metadata, report "not a system app", and silently
// drop the guard from the stacks whose removal is fatal.
func (r *Registry) viewOf(id string) string {
	if v, ok := r.cachedView(id); ok {
		return v
	}
	workingDir := ""
	if !r.isManaged(id) {
		workingDir = r.workingDirOf(id)
	}
	_, ca := r.metaFor(id, workingDir)
	if ca == nil {
		return xcomposeapp.ViewApps
	}
	return xcomposeapp.NormalizeView(ca.View)
}

// workingDirOf returns the directory Docker reports for a project's containers,
// or "" when Docker doesn't answer or nothing of the project is up.
func (r *Registry) workingDirOf(project string) string {
	if r.dx == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), workingDirTimeout)
	defer cancel()
	// Through the cache: this is a lookup, not a probe, and the answer it wants —
	// where a project's compose lives — cannot change without recreating the
	// containers, which invalidates the cache anyway.
	conts, err := r.containers.get(ctx)
	if err != nil {
		return ""
	}
	for _, c := range conts {
		if c.Project == project && c.WorkingDir != "" {
			return c.WorkingDir
		}
	}
	return ""
}
