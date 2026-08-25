// Package stackup is the single path every `docker compose up` in Maison goes
// through — install, start, update and config-save all land here. It resolves an
// app's lifecycle spec from its compose files (x-compose-app `folders` / `hooks`,
// falling back to x-casaos install commands) and, in order:
//
//	ensure folders  →  pre_up hook  →  docker compose up -d  →  post_up hook
//
// The install-only hooks (pre_install / post_install) are run by the installer
// around this, since only it knows an app is being installed for the first time.
package stackup

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/yundera/maison/internal/appenv"
	"github.com/yundera/maison/internal/composecmd"
	"github.com/yundera/maison/internal/composefile"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/xcasaos"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// Spec is an app's lifecycle declaration, merged across its compose files.
type Spec struct {
	Folders   []xcomposeapp.Folder
	Hooks     xcomposeapp.Hooks
	Secrets   xcomposeapp.StringMap
	Variables xcomposeapp.StringMap
	Files     []xcomposeapp.File
	Init      []xcomposeapp.InitStep
}

// Load resolves the lifecycle spec from an app's compose files, in the order
// they are passed to `docker compose` (base, then override — later files win,
// key by key, matching Compose's own extension merge).
//
// x-compose-app `hooks` win over the x-casaos `pre-install-cmd` /
// `post-install-cmd` they generalise, so a store app carrying only x-casaos keeps
// working untouched.
func Load(files []string) Spec {
	var xa, xc map[string]any
	for _, path := range files {
		f, err := composefile.Load(path)
		if err != nil {
			continue
		}
		xa = merge(xa, f.XComposeApp)
		xc = merge(xc, f.XCasaOS)
	}

	var spec Spec
	if si, err := xcasaos.Parse(xc); err == nil && si != nil {
		spec.Hooks.PreInstall = si.PreInstallCmd
		spec.Hooks.PostInstall = si.PostInstallCmd
	}
	if ca, err := xcomposeapp.Parse(xa); err == nil && ca != nil {
		spec.Folders = ca.Folders
		if h := ca.Hooks.PreInstall; h != "" {
			spec.Hooks.PreInstall = h
		}
		if h := ca.Hooks.PostInstall; h != "" {
			spec.Hooks.PostInstall = h
		}
		spec.Hooks.PreUp, spec.Hooks.PostUp = ca.Hooks.PreUp, ca.Hooks.PostUp
		spec.Secrets, spec.Variables, spec.Files, spec.Init = ca.Secrets, ca.Variables, ca.Files, ca.Init
	}
	return spec
}

// merge layers over's keys on top of base (over wins). Either map may be nil.
func merge(base, over map[string]any) map[string]any {
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

// Up brings a managed app's stack up: it ensures the app's folders exist, runs
// its pre_up hook, invokes `docker compose up -d`, then runs its post_up hook.
//
// A failing pre_up aborts the up — the hook is the app's precondition, so a stack
// whose precondition doesn't hold must not start. A failing post_up is logged and
// swallowed: the stack is already running and tearing it back down would be worse
// than a broken after-the-fact tweak.
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
// below. That is what makes a hand-run `docker compose up -d` in the app's folder
// do exactly what this function does.
func Up(ctx context.Context, cfg config.Config, project, dir string, files []string) error {
	files = SyncRoutes(cfg, project, dir, files)
	// After SyncRoutes: the vars Maison owns win over its seeds.
	if err := appenv.Sync(cfg, project, dir); err != nil {
		log.Printf("%s: sync .env: %v", project, err)
	}
	spec := Load(files)

	captures, err := Converge(ctx, cfg, project, dir, spec)
	if err != nil {
		return err
	}
	if h := spec.Hooks.PreUp; h != "" {
		if err := RunHook(ctx, cfg, project, dir, h); err != nil {
			return fmt.Errorf("pre_up hook: %w", err)
		}
	}
	if err := composecmd.Up(ctx, dir, project, files, envinject.Env(cfg, project)); err != nil {
		return err
	}
	// After the stack is up: a seeder that needs the app's own network, or a
	// service of it to answer. Logged and swallowed like the post_up hook — the
	// app is already running, and taking a healthy stack back down over a failed
	// after-the-fact tweak is the worse outcome.
	if err := RunInit(ctx, cfg, project, dir, xcomposeapp.PhasePostUp, spec.Init, captures); err != nil {
		log.Printf("%s: %v", project, err)
	}
	if h := spec.Hooks.PostUp; h != "" {
		if err := RunHook(ctx, cfg, project, dir, h); err != nil {
			log.Printf("%s: post_up hook: %v", project, err)
		}
	}
	return nil
}

// Converge brings the app's declared state into being, in the one order every
// path into a stack goes through: directories, then the values its templates
// reference, then the files themselves.
//
//	folders → secrets → variables → init(pre_up) → seed → files
//
// Each step is idempotent and each is fatal: an app whose directory is missing,
// whose secret could not be generated, or whose config file could not be
// written must not start. Getting that wrong is what the shell version did —
// a failed substitution left a file that looked initialised, and the app came
// up broken and stayed that way.
//
// Later steps in this sequence (init, seed, files) are added by their own files
// in this package.
func Converge(ctx context.Context, cfg config.Config, project, dir string, spec Spec) (map[string]string, error) {
	if err := Prepare(cfg, project, dir, spec); err != nil {
		return nil, err
	}
	// What an init step prints lands here and overlays the app's .env for
	// everything rendered after it — the seed tree and `files`. It lives only for
	// this converge: a derived value is re-derived, not remembered.
	captures := map[string]string{}
	if err := EnsureSecrets(cfg, project, dir, spec.Secrets, captures); err != nil {
		return nil, err
	}
	if err := EnsureVariables(cfg, project, dir, spec.Variables, captures); err != nil {
		return nil, err
	}
	if err := RunInit(ctx, cfg, project, dir, xcomposeapp.PhasePreUp, spec.Init, captures); err != nil {
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
// Prepare is idempotent, so the installer can call it early — before the
// pre_install hook — and let Up call it again at up time.
func Prepare(cfg config.Config, project, dir string, spec Spec) error {
	return EnsureFolders(cfg, project, spec.Folders, readEnvFile(dir))
}

// RunHook runs a lifecycle hook. Hooks execute in Maison's own container
// (/bin/bash) but against the HOST Docker daemon, so `/DATA` and `${DATA_ROOT}`
// references are rewritten to host paths — a `docker run -v` in a hook must name
// a path the host daemon can resolve. Hooks that only need a directory to exist
// should declare it under `folders` instead: those are created container-side,
// through Maison's data mount, and are correct on both sides.
//
// The commands a hook may call are the curated set in hookBinDir; anything else
// fails the hook with a message naming the sanctioned alternative. See
// hookshell.go for why that list is an allowlist and why the verdict comes from
// a file rather than an exit status.
func RunHook(ctx context.Context, cfg config.Config, project, dir, script string) error {
	rejected, err := os.CreateTemp("", "maison-hook-rejected-")
	if err != nil {
		return fmt.Errorf("hook workspace: %w", err)
	}
	rejected.Close()
	defer os.Remove(rejected.Name())

	preamble, err := os.CreateTemp("", "maison-hook-preamble-*.sh")
	if err != nil {
		return fmt.Errorf("hook workspace: %w", err)
	}
	defer os.Remove(preamble.Name())
	if _, err := preamble.WriteString(hookPreamble()); err != nil {
		preamble.Close()
		return fmt.Errorf("hook workspace: %w", err)
	}
	preamble.Close()

	cmd := exec.CommandContext(ctx, "/bin/bash", "-c", envinject.RewriteToHostPath(script, cfg))
	cmd.Dir = dir
	cmd.Env = append(hookEnv(cfg, project, dir),
		"BASH_ENV="+preamble.Name(),
		hookRejectedVar+"="+rejected.Name(),
	)
	out, runErr := cmd.CombinedOutput()

	// Checked before runErr, and regardless of it: the failures this catches are
	// the ones that do not show up as a non-zero exit. A missing command inside
	// `"$(...)"` leaves the substitution empty and the hook exits 0, which is how
	// an app ships an empty secret and installs green.
	if names := rejectedCommands(rejected.Name()); len(names) > 0 {
		return fmt.Errorf("hook called %s, which app hooks may not use — run it in a pinned container instead (see %s): %s",
			strings.Join(names, ", "), hookDocRef, out)
	}
	if runErr != nil {
		return fmt.Errorf("%w: %s", runErr, out)
	}
	return nil
}

// hookEnv is the app's interpolation environment (base vars overlaid with its
// persisted .env, so a hook sees the same values its compose does) plus the few
// variables a hook needs to reach the host daemon and its own app directory.
func hookEnv(cfg config.Config, project, dir string) []string {
	env := envinject.Env(cfg, project)
	for k, v := range envinject.EnvFileVars(readEnvFile(dir)) {
		env = append(env, k+"="+v)
	}
	return append(env,
		"PATH="+hookPath(),
		"DOCKER_HOST=unix:///var/run/docker.sock",
		"AppID="+project,
		"APP_DIR="+envinject.HostPath(dir, cfg), // a real path, so map it — don't text-rewrite it
	)
}
