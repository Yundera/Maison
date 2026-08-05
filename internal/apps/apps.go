// Package apps builds the dashboard's view of installed applications by
// reconciling Maison-managed compose projects (on disk) with what is actually
// running in Docker, and surfacing externally-created x-casaos stacks as
// "unmanaged" apps.
package apps

import (
	"context"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

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
	// Protected marks an app the operator pinned via PROTECTED_APPS (matched on
	// store id, e.g. "maison"): it shows as a normal tile but its Uninstall
	// entry is hidden and the DELETE endpoint refuses it.
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
	// bar, in red — Remove (containers) then Archive (folder) — while
	// Uninstalling is true. Also never set by List() itself.
	Uninstalling   bool    `json:"uninstalling,omitempty"`
	Remove         float64 `json:"remove,omitempty"`
	Archive        float64 `json:"archive,omitempty"`
	UninstallError string  `json:"uninstall_error,omitempty"`
	// Backup/restore progress, overlaid the same way again (see BackupState). The
	// tile renders one bar in its own colour, stepping through Copy (the live
	// pass), Sync (the stopped pass) and Compress, while BackingUp is true. Also
	// never set by List() itself.
	BackingUp   bool    `json:"backing_up,omitempty"`
	Copy        float64 `json:"copy,omitempty"`
	Sync        float64 `json:"sync,omitempty"`
	Compress    float64 `json:"compress,omitempty"`
	BackupError string  `json:"backup_error,omitempty"`
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

	// Engine is the backup engine new backups are written to. Nil means the built-in
	// local one — archives on the data disk — which needs no configuration and is
	// always available, so a Registry built without one behaves exactly as Maison
	// always has.
	//
	// It is an exported field set after construction, like OnChange, rather than a
	// New parameter: an engine is an optional collaborator wired by the server, and
	// making it a parameter would force every caller that has no opinion about
	// backups to have one.
	//
	// Only *writes* follow this. Reading a backup dispatches on where that backup
	// actually is, never on which engine is selected now — otherwise switching engine
	// would strand everything the previous one wrote. See internal/backup.
	Engine Provider

	mu         sync.Mutex
	busy       map[string]int             // app id -> in-flight operation count
	uninstalls map[string]*UninstallState // app id -> live uninstall progress
	backups    map[string]*BackupState    // app id -> live backup/restore progress
}

// New creates a Registry.
func New(cfg config.Config, dx *dockerx.Client) *Registry {
	return &Registry{
		cfg:        cfg,
		dx:         dx,
		busy:       map[string]int{},
		uninstalls: map[string]*UninstallState{},
		backups:    map[string]*BackupState{},
	}
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
// available local read — and Docker state is layered on top as best-effort. A
// failed or slow Docker query therefore yields greyed (stopped) tiles rather than
// an empty grid, and never returns an error to the caller. Externally-created
// x-casaos stacks are only surfaced when Docker actually answers.
func (r *Registry) List(ctx context.Context) ([]App, error) {
	conts, err := r.dx.ListProjectContainers(ctx)
	if err != nil {
		// Best-effort: fall through with no container state so installed apps still
		// render (greyed) instead of the grid blanking on a Docker hiccup.
		log.Printf("apps: docker list failed, showing installed apps as stopped: %v", err)
	}

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
		app.Busy = r.isBusy(name)
		app.Protected = r.cfg.IsProtected(app.Store, name)
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
		app.Protected = r.cfg.IsProtected(app.Store, name)
		out = append(out, app)
		seen[name] = true
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
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
	app := App{ID: name, Name: name, Managed: managed, Status: status}
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
	}
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

func (r *Registry) Stop(ctx context.Context, id string) error {
	r.enter(id)
	defer r.leave(id)
	return r.dx.StopProject(ctx, id)
}

func (r *Registry) Restart(ctx context.Context, id string) error {
	r.enter(id)
	defer r.leave(id)
	return r.dx.RestartProject(ctx, id)
}

// Protected reports whether the app is exempt from uninstall, resolving its store
// id from its compose metadata (falling back to the project name).
func (r *Registry) Protected(id string) bool {
	storeID := ""
	si, ca := r.metaFor(id, "")
	if si != nil {
		storeID = si.StoreAppID
	}
	if ca != nil && ca.ID != "" {
		storeID = ca.ID
	}
	return r.cfg.IsProtected(storeID, id)
}
