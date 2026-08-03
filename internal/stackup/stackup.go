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

	"github.com/yundera/maison/internal/composecmd"
	"github.com/yundera/maison/internal/composefile"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/xcasaos"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// Spec is an app's lifecycle declaration, merged across its compose files.
type Spec struct {
	Folders []xcomposeapp.Folder
	Hooks   xcomposeapp.Hooks
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
// The app's compose, .env and generated Caddy routes are reconciled with the
// current deployment first, so every path into the stack — install, start, store
// update, a config or .env save, an added domain — brings the app up against the
// deployment as it is now, not as it was when the app was installed. See Normalize
// and SyncRoutes.
func Up(ctx context.Context, cfg config.Config, project, dir string, files []string) error {
	files = SyncRoutes(cfg, project, dir, files)
	Normalize(cfg, project, dir) // after SyncRoutes: the vars Maison owns win over its seeds
	spec := Load(files)

	if err := Prepare(cfg, project, dir, spec); err != nil {
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
	if h := spec.Hooks.PostUp; h != "" {
		if err := RunHook(ctx, cfg, project, dir, h); err != nil {
			log.Printf("%s: post_up hook: %v", project, err)
		}
	}
	return nil
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
	envFile, _ := os.ReadFile(filepath.Join(dir, ".env"))
	return EnsureFolders(cfg, project, spec.Folders, envFile)
}

// RunHook runs a lifecycle hook. Hooks execute in Maison's own container
// (/bin/bash) but against the HOST Docker daemon, so `/DATA` and `${DATA_ROOT}`
// references are rewritten to host paths — a `docker run -v` in a hook must name
// a path the host daemon can resolve. Hooks that only need a directory to exist
// should declare it under `folders` instead: those are created container-side,
// through Maison's data mount, and are correct on both sides.
func RunHook(ctx context.Context, cfg config.Config, project, dir, script string) error {
	cmd := exec.CommandContext(ctx, "/bin/bash", "-c", envinject.RewriteToHostPath(script, cfg))
	cmd.Dir = dir
	cmd.Env = hookEnv(cfg, project, dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

// hookEnv is the app's interpolation environment (base vars overlaid with its
// persisted .env, so a hook sees the same values its compose does) plus the few
// variables a hook needs to reach the host daemon and its own app directory.
func hookEnv(cfg config.Config, project, dir string) []string {
	env := envinject.Env(cfg, project)
	if b, err := os.ReadFile(filepath.Join(dir, ".env")); err == nil {
		for k, v := range envinject.EnvFileVars(b) {
			env = append(env, k+"="+v)
		}
	}
	return append(env,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"DOCKER_HOST=unix:///var/run/docker.sock",
		"AppID="+project,
		"APP_DIR="+envinject.HostPath(dir, cfg), // a real path, so map it — don't text-rewrite it
	)
}
