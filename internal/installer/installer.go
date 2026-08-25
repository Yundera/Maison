// Package installer installs a store app: it writes the store's docker-compose.yml
// under the data root unchanged, prefills the app's .env from the deployment's
// .env.app, and brings the stack up through internal/stackup (folders → pre_up →
// `docker compose up -d` → post_up). The install-only hooks (pre_install / post_install, x-compose-app or the x-casaos
// commands they generalise) are run here, around that — they fire once, when the
// app is first installed.
package installer

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/yundera/maison/internal/appenv"
	"github.com/yundera/maison/internal/appstore"
	"github.com/yundera/maison/internal/composefile"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/dockerx"
	"github.com/yundera/maison/internal/stackup"
)

// Event is a progress update emitted during installation. Progress is split into
// two independent tracks the UI shows one at a time on a single bar: Download
// (image pull, blue) and Start (bringing the stack up in Docker, green).
type Event struct {
	Phase    string  `json:"phase"`    // pull | prepare | start | done | error
	Message  string  `json:"message"`  // human-readable detail
	Download float64 `json:"download"` // image-pull progress, 0-100
	Start    float64 `json:"start"`    // stack-start progress, 0-100
}

// BackupRef names the backup an install should land on top of: a stamp, plus the
// engine holding it.
//
// The engine is part of the identity, not a hint. Two engines can hold the same stamp
// — a local archive and an offsite snapshot of the same app on the same day — and they
// are separate backups written at separate times; installing on the other one is not
// what the user clicked. A zero BackupRef means a fresh install.
type BackupRef struct {
	Name   string
	Engine string
}

var invalidProjectChars = regexp.MustCompile(`[^a-z0-9_-]+`)

// projectName derives a Docker-compatible (lowercase) compose project name,
// preferring the compose file's own `name:` and falling back to a sanitized id.
func projectName(fileName, id string) string {
	name := fileName
	if name == "" {
		name = id
	}
	name = invalidProjectChars.ReplaceAllString(strings.ToLower(name), "-")
	name = strings.Trim(name, "-_")
	if name == "" {
		name = "app"
	}
	return name
}

// Installer installs store apps.
type Installer struct {
	cfg   config.Config
	store *appstore.Manager
	dx    *dockerx.Client // optional; used for image-pull progress

	// OnUpdate, if set, is called whenever a tracked install's progress changes
	// so the server can rebroadcast the app list (making the tile's progress bars
	// advance live). The server is expected to throttle it. Optional.
	OnUpdate func()

	// BackupBeforeUpdate takes a rollback point before an update is applied and
	// returns the backup's name. RollBack restores one.
	//
	// They are func fields rather than a *apps.Registry because the Installer holds
	// no registry and should keep holding none — it needs two operations, not a
	// collaborator. The server wires both, and wires them to the **local** engine
	// specifically: a rollback has to be a rename, and restoring from a repository is
	// a download. Nil on either disables the rollback point, which is the correct
	// behaviour on a box with no Docker.
	BackupBeforeUpdate func(ctx context.Context, project string) (string, error)
	RollBack           func(ctx context.Context, project, name string) error

	// FetchBackup puts (project, engine, name) back as the app's folder before an
	// install runs over it — the store's install-from-backup path.
	//
	// A func field for the same reason the two above are: the Installer needs the
	// operation, not a registry. It has to be one at all rather than the direct
	// apps.RestoreBackup call it replaces, because that call read the data disk and
	// only the data disk — so on a box whose backups live in a repository the store
	// offered a list it could not install from, and mostly an empty one. Nil disables
	// install-from-backup rather than failing an install that asked for none.
	FetchBackup func(ctx context.Context, project, engine, name string, onProgress func(pct float64, msg string)) error

	mu       sync.Mutex
	installs map[string]*InstallState // project name -> live progress
}

// InstallState is a snapshot of one in-flight (or failed) install. It is overlaid
// onto the app list so the dashboard tile shows install progress even after the
// store panel is closed.
type InstallState struct {
	ID       string  `json:"id"`       // compose project name (== app tile id)
	Name     string  `json:"name"`     // display title, for the placeholder tile
	Icon     string  `json:"icon"`     // icon URL, for the placeholder tile
	Phase    string  `json:"phase"`    // pull | prepare | start | done | error
	Message  string  `json:"message"`  // human-readable detail
	Download float64 `json:"download"` // image-pull progress, 0-100
	Start    float64 `json:"start"`    // stack-start progress, 0-100
	Error    string  `json:"error"`    // set when Phase == error
}

// New creates an Installer. dx may be nil (pull progress is then skipped).
func New(cfg config.Config, store *appstore.Manager, dx *dockerx.Client) *Installer {
	return &Installer{cfg: cfg, store: store, dx: dx, installs: map[string]*InstallState{}}
}

// ProjectFor resolves the compose project name a store app would install as.
// The store panel needs it to look up that app's backups, and it cannot derive it
// itself: projectName prefers the compose file's own `name:`, which only the
// server has.
//
// A reference carrying a locator pins the lookup to that store instead of the
// merged catalog (see appstore.Manager.GetFrom).
func (in *Installer) ProjectFor(ctx context.Context, ref appstore.Ref) (string, error) {
	_, raw, err := in.store.GetFrom(ctx, ref)
	if err != nil {
		return "", err
	}
	f, err := composefile.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse compose: %w", err)
	}
	return projectName(f.Name, ref.ID), nil
}

// StartInstall launches a detached install of store app `id` and tracks its
// progress so it rides the live app list. The install runs on a background
// context, so it is NOT cancelled when the caller (e.g. the store panel) goes
// away. Idempotent: a second call while the same project is installing is a
// no-op. Returns the resolved compose project name (the app's tile id).
//
// from, when set, names one of this app's backups in the engine that holds it: it is
// restored as the app's folder before the install runs, so the app comes back with its
// old data and .env instead of a clean slate.
//
// A reference carrying a locator installs the app from that store rather than
// from the merged catalog — the store need not be a configured source. The whole
// reference is recorded as the app's update reference, so later updates keep
// coming from the same store *and* the same folder inside it.
func (in *Installer) StartInstall(ctx context.Context, ref appstore.Ref, from BackupRef) (string, error) {
	app, raw, err := in.store.GetFrom(ctx, ref)
	if err != nil {
		return "", err
	}
	f, err := composefile.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse compose: %w", err)
	}
	project := projectName(f.Name, ref.ID)

	in.mu.Lock()
	if _, running := in.installs[project]; running {
		in.mu.Unlock()
		return project, nil // already installing — attach, don't restart
	}
	name := app.Name
	if name == "" {
		name = ref.ID
	}
	in.installs[project] = &InstallState{ID: project, Name: name, Icon: app.Icon, Phase: "pull", Message: "Queued"}
	in.mu.Unlock()
	in.notify()

	go func() {
		// Deliberately not the caller's ctx: the install must outlive the request.
		err := in.Install(context.Background(), ref, from, func(ev Event) {
			in.mu.Lock()
			if st := in.installs[project]; st != nil {
				st.Phase, st.Message, st.Download, st.Start = ev.Phase, ev.Message, ev.Download, ev.Start
			}
			in.mu.Unlock()
			in.notify()
		})
		in.mu.Lock()
		if err != nil {
			log.Printf("install %s failed: %v", project, err)
			// Keep the entry so the failure stays visible on the tile until the
			// user retries (which clears it) or dismisses it.
			if st := in.installs[project]; st != nil {
				st.Phase, st.Error, st.Message = "error", err.Error(), err.Error()
			}
		} else {
			// Success: drop the overlay so the real, Docker-backed tile takes over.
			delete(in.installs, project)
		}
		in.mu.Unlock()
		in.notify()
	}()
	return project, nil
}

// Installs returns a snapshot of every tracked install (in-flight or errored).
func (in *Installer) Installs() []InstallState {
	in.mu.Lock()
	defer in.mu.Unlock()
	out := make([]InstallState, 0, len(in.installs))
	for _, st := range in.installs {
		out = append(out, *st)
	}
	return out
}

// ClearInstall drops a tracked install (used to dismiss a failed one).
func (in *Installer) ClearInstall(project string) {
	in.mu.Lock()
	_, existed := in.installs[project]
	delete(in.installs, project)
	in.mu.Unlock()
	if existed {
		in.notify()
	}
}

func (in *Installer) notify() {
	if in.OnUpdate != nil {
		in.OnUpdate()
	}
}

// Install fetches app `id`, transforms its compose, writes it, and brings the
// stack up — emitting progress events (safe to pass a nil emit).
//
// from, when set, restores that backup as the app's folder first (see StartInstall).
// A reference carrying a locator pins the app to that store.
func (in *Installer) Install(ctx context.Context, ref appstore.Ref, from BackupRef, emit func(Event)) error {
	if emit == nil {
		emit = func(Event) {}
	}

	app, raw, err := in.store.GetFrom(ctx, ref)
	if err != nil {
		return err
	}

	f, err := composefile.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse compose: %w", err)
	}

	project := projectName(f.Name, ref.ID)

	appDir := filepath.Join(in.cfg.AppsDir(), project)

	// Restore before anything writes to appDir: RestoreBackup refuses to overwrite
	// an existing folder, and the install below is deliberately non-destructive on
	// top of what it finds — the strict docker-compose.yml is refreshed from the
	// store, while the restored .env and data are left exactly as they were.
	if from.Name != "" {
		if in.FetchBackup == nil {
			return fmt.Errorf("restore backup: backups are not available on this box")
		}
		emit(Event{Phase: "prepare", Message: "Restoring backup " + from.Name})
		// Reported on the Download track. Restoring from a repository *is* a download,
		// it happens before the image pull that track normally carries, and an engine
		// that cannot measure its progress reports PctUnknown — which is negative, and
		// would render as a bar of negative width, so it floors at zero and the message
		// carries the detail instead.
		if err := in.FetchBackup(ctx, project, from.Engine, from.Name, func(pct float64, msg string) {
			emit(Event{Phase: "prepare", Message: msg, Download: max(pct, 0)})
		}); err != nil {
			return fmt.Errorf("restore backup: %w", err)
		}
	}

	if err := os.MkdirAll(appDir, 0o755); err != nil {
		return err
	}
	// The store's bytes, unchanged. Maison does not rewrite an app's compose — see
	// the package doc of internal/envinject and docs/app-model.md.
	composePath := filepath.Join(appDir, "docker-compose.yml")
	if err := os.WriteFile(composePath, raw, 0o644); err != nil {
		return err
	}

	// The store's seed tree, copied in beside the compose file so the app folder
	// carries everything it needs to converge later without the store (see
	// writeSeed and stackup.EnsureSeed). The files themselves are written out by
	// the converge below, not here.
	if err := writeSeed(app, appDir); err != nil {
		return err
	}

	// Prefill the app's .env from the deployment's .env.app (merged with the vars
	// Maison computes per app), so its compose resolves offline and the operator
	// can hand-edit it afterwards — see docs/app-model.md and internal/appenv.
	// Keys are ensured one by one, so a reinstall over a restored archive refreshes
	// what the deployment provides and keeps everything the user added.
	if err := appenv.Sync(in.cfg, project, appDir); err != nil {
		return err
	}

	// Record where this app came from so the per-app Update tab can pull a fresher
	// docker-compose.yml from the same store later. The reference lives in the
	// override's x-compose-app block (see docs/app-model.md and update.go).
	if err := writeUpdateRef(appDir, appstore.Ref{URL: app.StoreURL, AppsPath: app.AppsPath, ID: ref.ID}); err != nil {
		return err
	}

	files := []string{composePath}
	if override := filepath.Join(appDir, "docker-compose.override.yml"); fileExists(override) {
		files = append(files, override)
	}

	// Create the app's folders before anything else touches them — the pre_install
	// hook below routinely seeds config files into them. stackup.Up ensures them
	// again at up time (idempotent), so an app started later still gets them.
	spec := stackup.Load(files)
	if err := stackup.Prepare(in.cfg, project, appDir, spec); err != nil {
		return err
	}

	// Track 1 — Download: pull images with real progress (0 → 100%).
	in.pullImages(ctx, f, emit)

	// The install hooks run exactly once, here; the up hooks run inside stackup.Up
	// on this and every later start.
	if h := spec.Hooks.PreInstall; h != "" {
		emit(Event{Phase: "prepare", Message: "Running pre-install", Download: 100})
		if err := stackup.RunHook(ctx, in.cfg, project, appDir, h); err != nil {
			return fmt.Errorf("pre_install hook: %w", err)
		}
	}

	// Track 2 — Start: bring the stack up, then follow Docker until it is running.
	emit(Event{Phase: "start", Message: "Starting containers", Download: 100, Start: 15})
	if err := stackup.Up(ctx, in.cfg, project, appDir, files); err != nil {
		return err
	}
	if h := spec.Hooks.PostInstall; h != "" {
		if err := stackup.RunHook(ctx, in.cfg, project, appDir, h); err != nil {
			log.Printf("%s: post_install hook: %v", project, err)
		}
	}
	// `compose up -d` returns once containers are created/started; follow their
	// live Docker state (and health checks) so the Start bar reflects real
	// readiness rather than jumping straight to 100%.
	in.awaitStart(ctx, project, emit)

	emit(Event{Phase: "done", Message: "Installed", Download: 100, Start: 100})
	return nil
}

// pullImages pulls each service image, mapping per-image download progress onto
// the 0 → 100 Download bar.
func (in *Installer) pullImages(ctx context.Context, f *composefile.File, emit func(Event)) {
	var images []string
	for _, svc := range f.Services {
		if svc.Image != "" {
			images = append(images, svc.Image)
		}
	}
	if len(images) == 0 || in.dx == nil {
		emit(Event{Phase: "pull", Message: "Preparing images", Download: 100})
		return
	}
	span := 100.0 / float64(len(images))
	for i, img := range images {
		base := float64(i) * span
		emit(Event{Phase: "pull", Message: "Pulling " + img, Download: base})
		// Pull failures are non-fatal here — `compose up` will retry the pull.
		_ = in.dx.PullImage(ctx, img, func(pct float64, status string) {
			emit(Event{Phase: "pull", Message: img + ": " + status, Download: base + pct/100*span})
		})
		emit(Event{Phase: "pull", Message: "Pulled " + img, Download: base + span})
	}
}

// awaitStart polls the project's containers for up to ~30s after `compose up`,
// advancing the Start bar from Docker's live state: the running fraction (and,
// when containers declare health checks, the healthy fraction) drive it toward
// 100%. It returns early once every container is running and healthy. dx may be
// nil (no daemon) — the caller then just reports the stack as fully started.
func (in *Installer) awaitStart(ctx context.Context, project string, emit func(Event)) {
	if in.dx == nil {
		return
	}
	for i := 0; i < 30; i++ {
		svcs, err := in.dx.ProjectServices(ctx, project)
		if err == nil && len(svcs) > 0 {
			total := len(svcs)
			running, withHealth, healthy := 0, 0, 0
			for _, s := range svcs {
				if s.State == "running" {
					running++
				}
				if s.Health != "" {
					withHealth++
					if s.Health == "healthy" {
						healthy++
					}
				}
			}
			runFrac := float64(running) / float64(total)
			// 15 (up invoked) → 100. When health checks exist, blend running and
			// healthy fractions so the bar keeps moving through the "starting" wait.
			frac := runFrac
			if withHealth > 0 {
				frac = runFrac*0.5 + (float64(healthy)/float64(withHealth))*0.5
			}
			emit(Event{Phase: "start", Message: "Starting containers", Download: 100, Start: 15 + frac*85})
			if running == total && (withHealth == 0 || healthy == withHealth) {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
