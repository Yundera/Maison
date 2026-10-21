// Package stackup is the single path every `docker compose up` in Maison goes
// through — install, start, update and config-save all land here. It resolves an
// app's lifecycle spec from its compose files (x-compose-app, or translated
// x-casaos) and, in order:
//
//	folders → secrets → variables → [migrations] → setup steps → seed → files
//	        → docker compose up -d → started steps
//
// An install is this same sequence: nothing runs only on install. Every piece of
// app-supplied code — a step, a migration step, a credential change — is a script
// entry run by RunScript.
package stackup

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/yundera/maison/internal/appenv"
	"github.com/yundera/maison/internal/composecmd"
	"github.com/yundera/maison/internal/composefile"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/incident"
	"github.com/yundera/maison/internal/routing"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// Spec is an app's lifecycle declaration, merged across its compose files.
type Spec struct {
	Folders   []xcomposeapp.Folder
	Secrets   xcomposeapp.StringMap
	Variables xcomposeapp.StringMap
	Files     []xcomposeapp.File
	Steps     []xcomposeapp.Step
}

// Load resolves the lifecycle spec from an app's compose files, in the order
// they are passed to `docker compose` (base, then override — later files win,
// key by key, matching Compose's own extension merge). The metadata is
// composefile.Resolve's: a store app carrying only x-casaos gets its
// `pre-install-cmd` / `post-install-cmd` as `once` steps, and one carrying
// x-compose-app gets x-compose-app's alone.
func Load(files []string) Spec {
	var fs []*composefile.File
	for _, path := range files {
		if f, err := composefile.Load(path); err == nil {
			fs = append(fs, f)
		}
	}
	var spec Spec
	if ca, err := composefile.Resolve(fs...); err == nil && ca != nil {
		spec.Folders = ca.Folders
		spec.Secrets, spec.Variables, spec.Files, spec.Steps = ca.Secrets, ca.Variables, ca.Files, ca.Steps
	}
	return spec
}

// Up brings a managed app's stack up: it converges the app's declarations (folders,
// secrets, variables, setup steps, seed tree, files), invokes `docker compose up
// -d --remove-orphans`, then runs its started steps.
//
// A failing setup step aborts the up — it is the app's precondition, so a stack
// whose precondition doesn't hold must not start. A failing started step is logged
// and swallowed: the stack is already running and tearing it back down would be
// worse than a broken after-the-fact tweak.
//
// The app's .env and its generated Caddy routes are reconciled with the current
// deployment first, so every path into the stack — install, start, store update, a
// config or .env save, an added domain — brings the app up against the deployment
// as it is now, not as it was when the app was installed. See SyncRoutes and
// appenv.Sync.
//
// What is *not* reconciled is the app's docker-compose.yml. It is the store's file,
// byte-for-byte, and Maison never writes to it: the deployment reaches the app
// through the ${VAR}s that file already references — ${APP_NET}, ${DATA_ROOT},
// ${APP_DOMAIN} — resolved afresh by `docker compose` on every up against the .env
// below. That is what makes a hand-run `docker compose up -d --remove-orphans` in
// the app's folder do exactly what this function does.
func Up(ctx context.Context, cfg config.Config, project, dir string, files []string) error {
	return UpWithPrior(ctx, cfg, project, dir, files, readEnvFile(dir))
}

// UpWithPrior is Up for a caller that has already synced the app's .env itself —
// the installer — and so must say what the file held before it did. prior is what
// a seeded secret (`${APP_DEFAULT_PASSWORD}|alnum:32`) reads: nil for a fresh
// install, which then generates, rather than the password Sync just wrote in.
func UpWithPrior(ctx context.Context, cfg config.Config, project, dir string, files []string, prior []byte) error {
	return UpWith(ctx, cfg, project, dir, files, UpOptions{Prior: prior})
}

// UpOptions are what the callers of Up that know more than "bring it up" pass in.
type UpOptions struct {
	// Prior is the app's .env before the caller synced it (see UpWithPrior).
	Prior []byte
	// Migrate puts the up in update mode: the plan's steps run after `variables`
	// and before the setup steps, the seed tree and `files`, which write create-if-absent
	// defaults for the new layout and would otherwise take the path a migration is
	// about to move the user's real data into. Only an update sets it — only an
	// update has a backup to undo a step with.
	Migrate *MigratePlan
	// Progress is told about each migration step as it starts.
	Progress MigrateProgress
}

// UpWith is the one up every path goes through; Up and UpWithPrior are its
// common spellings.
func UpWith(ctx context.Context, cfg config.Config, project, dir string, files []string, opts UpOptions) error {
	prior := opts.Prior
	// Before anything is written: a stack whose data is in another shape than its
	// compose expects must not start, and nothing about it should change either.
	// An update instead checks the data is where its plan starts from — the compose
	// on disk is already the new one.
	if opts.Migrate != nil {
		if v, ok, err := ReadDataVersion(dir); err != nil {
			return err
		} else if ok && v != opts.Migrate.From {
			return fmt.Errorf("the app's data is at version %d, not %d as the update planned", v, opts.Migrate.From)
		}
	} else if err := CheckDataVersion(dir); err != nil {
		cfg.ReportIncident(dataIncident(project, err))
		return err
	}
	cfg.ResolveIncident(dataIncidentID(project))

	var migrate func(context.Context, map[string]string) error
	if opts.Migrate != nil {
		migrate = func(ctx context.Context, captures map[string]string) error {
			return RunMigrations(ctx, cfg, project, dir, *opts.Migrate, captures, opts.Progress)
		}
	}
	// Everything before `compose up`, as one pass: it runs again whole when the up
	// has to move a route off a port, since the app's address is in what it renders.
	prepare := func(avoid map[int]bool, migrate func(context.Context, map[string]string) error) (Spec, map[string]string, []string, map[string]string, error) {
		upFiles, routeVars := syncRoutes(cfg, project, dir, files, avoid)
		// After the routes: the vars Maison owns win over its seeds.
		if err := appenv.Sync(cfg, project, dir); err != nil {
			log.Printf("%s: sync .env: %v", project, err)
			cfg.ReportIncident(hookIncident(project, ".env could not be written", err))
		}
		// The app's own address, before converge: `files` and steps render with the
		// .env, and a config file that names the app's URL (Dex's issuer) must see it.
		writeRouteVars(cfg, project, dir, routeVars)
		spec := Load(upFiles)
		outputs, err := converge(ctx, cfg, project, dir, spec, envinject.EnvFileVars(prior), migrate)
		return spec, outputs, upFiles, routeVars, err
	}

	spec, outputs, upFiles, routeVars, err := prepare(nil, migrate)
	if err != nil {
		return err
	}
	if err := composecmd.Up(ctx, dir, project, upFiles, envinject.Env(cfg, project)); err != nil {
		// A host port Maison handed out that something outside Docker holds: Maison
		// cannot see host processes, so compose is the real check. Pick another port
		// and go through the whole pass once more — migrations excepted, they have
		// run — saying so.
		port, ok := portInUse(err)
		if !ok || cfg.Routing == nil || cfg.Routing().Renderer != routing.Ports {
			return err
		}
		var retryVars map[string]string
		var perr error
		spec, outputs, upFiles, retryVars, perr = prepare(map[int]bool{port: true}, nil)
		if perr != nil {
			return perr
		}
		key, now, moved := movedOff(routeVars, retryVars, port)
		if !moved {
			return err
		}
		if err := composecmd.Up(ctx, dir, project, upFiles, envinject.Env(cfg, project)); err != nil {
			return err
		}
		cfg.ReportIncident(portIncident(project, port, key, now))
	}
	// After the stack is up: work that needs the app's own network, or a service
	// of it to answer. Logged and swallowed — the app is already running, and
	// taking a healthy stack back down over a failed after-the-fact tweak is the
	// worse outcome.
	if err := RunSteps(ctx, cfg, project, dir, xcomposeapp.AtStarted, spec.Steps, outputs); err != nil {
		log.Printf("%s: %v", project, err)
		cfg.ReportIncident(hookIncident(project, "one of its setup steps did not complete", err))
	}
	return nil
}

// Converge brings the app's declared state into being, in the one order every
// path into a stack goes through: directories, then the values its templates
// reference, then the files themselves.
//
//	folders → secrets → variables → [migrations] → setup steps → seed → files
//
// Each step is idempotent and each is fatal: an app whose directory is missing,
// whose secret could not be generated, or whose config file could not be
// written must not start. Getting that wrong is what the shell version did —
// a failed substitution left a file that looked initialised, and the app came
// up broken and stayed that way.
//
// Later stages of this sequence (steps, seed, files) live in their own files in
// this package.
//
// prior is the app's .env as it stood before this operation synced it — what a
// seeded secret reads (see EnsureSecrets).
func Converge(ctx context.Context, cfg config.Config, project, dir string, spec Spec, prior map[string]string) (map[string]string, error) {
	return converge(ctx, cfg, project, dir, spec, prior, nil)
}

// converge is Converge with an update's data migrations slotted in after
// `variables` (see UpOptions.Migrate). migrate is nil on every other path.
func converge(ctx context.Context, cfg config.Config, project, dir string, spec Spec, prior map[string]string, migrate func(context.Context, map[string]string) error) (map[string]string, error) {
	if err := Prepare(cfg, project, dir, spec); err != nil {
		return nil, err
	}
	// What a script writes to $MAISON_OUTPUT lands here and overlays the app's
	// .env for everything run or rendered after it — later scripts, the seed tree
	// and `files`. It lives only for this converge: a derived value is re-derived,
	// not remembered.
	captures := map[string]string{}
	if err := EnsureSecrets(cfg, project, dir, spec.Secrets, prior, captures); err != nil {
		return nil, err
	}
	if err := EnsureVariables(cfg, project, dir, spec.Variables, captures); err != nil {
		return nil, err
	}
	if migrate != nil {
		if err := migrate(ctx, captures); err != nil {
			return nil, err
		}
	}
	if err := RunSteps(ctx, cfg, project, dir, xcomposeapp.AtSetup, spec.Steps, captures); err != nil {
		return nil, err
	}
	// The seed tree leaves a files entry's target alone: files owns it, and an
	// ensure: always file must not be briefly seeded with a stale render first.
	if err := EnsureSeed(cfg, project, dir, ClaimedPaths(cfg, project, dir, spec.Files), captures); err != nil {
		return nil, err
	}
	if err := EnsureFiles(cfg, project, dir, spec.Files, captures); err != nil {
		return nil, err
	}
	return captures, nil
}

// readEnvFile reads an app's .env, or returns nil when it has none yet — every
// caller here treats "no file" and "empty file" the same way.
func readEnvFile(dir string) []byte {
	b, _ := os.ReadFile(filepath.Join(dir, ".env"))
	return b
}

// Prepare creates the directories the app needs before anything touches its
// stack: exactly the ones declared in x-compose-app `folders`, and nothing else.
//
// Maison does not guess. A bind mount whose source is missing is Docker's to
// create — root-owned, as always — and an app that needs it to exist with a
// given owner says so under `folders`. There is no inference from the compose
// file to fall back on, because any such inference has to decide whether a bind
// source is a file or a directory with no information to decide it on, and gets
// it wrong in both directions: a directory created where the app wanted a config
// file, or a silently root-owned data dir. The declaration is the only contract.
//
// Prepare is idempotent, so the installer can call it early — before the image
// pull — and let Up call it again at up time.
func Prepare(cfg config.Config, project, dir string, spec Spec) error {
	return EnsureFolders(cfg, project, spec.Folders, readEnvFile(dir))
}

// hookIncident describes a setup step that failed after the stack was already up.
//
// Every caller of this deliberately swallows the error — the app is running, and
// taking a healthy stack back down over an after-the-fact tweak is the worse outcome.
// Swallowing it is not the same as hiding it, though: the result is an app that looks
// installed and behaves oddly, and until now the only evidence was one line in a
// container log nobody reads.
//
// One ID per app, not per step: an app whose setup is failing usually fails the same
// way every time it starts, and three records saying so is not three problems.
func hookIncident(project, what string, err error) incident.Report {
	return incident.Report{
		ID:       "app.hook:" + project,
		Kind:     incident.KindAppStackup,
		Severity: incident.Warning,
		Title:    "A setup step failed for " + project,
		Detail: project + " is running, but " + what + ":\n" + err.Error() +
			"\n\nThe app may be only partly configured. Restarting it runs the step again.",
		Args: map[string]string{"app": project},
	}
}
