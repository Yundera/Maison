package stackup

import (
	"bytes"
	"log"
	"os"
	"path/filepath"

	"github.com/yundera/maison/internal/caddyroutes"
	"github.com/yundera/maison/internal/config"
)

// SyncRoutes reconciles the app's generated Caddy routes — the ones publishing it
// on the deployment's additional domains (Settings › Domains) — into its
// docker-compose.override.yml, and returns the compose files to bring the stack up
// with.
//
// It runs before *every* up, which is what makes the routes self-healing: adding a
// domain, removing one, reinstalling an app, restoring a backup or hand-editing
// the override all converge on the same file the next time the stack comes up. A
// domain change only reaches a running container through a recreate anyway, so
// there is no cheaper moment to do this.
//
// The returned file list matters: the override may not have existed before this
// call (an app with no user edits is generated one from nothing), or may have been
// emptied out by removing the last domain, and `docker compose` has to be handed
// exactly the files that now exist.
func SyncRoutes(cfg config.Config, project, dir string, files []string) []string {
	if cfg.Domains == nil {
		return files // feature unwired — never touch the operator's override
	}
	doms := cfg.Domains()

	basePath := filepath.Join(dir, "docker-compose.yml")
	overridePath := filepath.Join(dir, "docker-compose.override.yml")
	base, err := os.ReadFile(basePath)
	if err != nil {
		return files // not a managed app: no store compose to clone routes from
	}
	override, _ := os.ReadFile(overridePath)

	out, err := caddyroutes.Sync(base, override, doms)
	if err != nil {
		// A malformed override is the operator's to fix, and they can still see it in
		// the YAML editor. Failing the up over it would strand the app with no way
		// back, so carry on with what is on disk.
		log.Printf("%s: caddy routes: %v", project, err)
		return files
	}

	switch {
	case out == nil:
		// Nothing left in the override — the app had no edits of its own and the last
		// domain just went away.
		if err := os.Remove(overridePath); err != nil && !os.IsNotExist(err) {
			log.Printf("%s: caddy routes: %v", project, err)
		}
	case !bytes.Equal(out, override):
		if err := os.WriteFile(overridePath, out, 0o644); err != nil {
			log.Printf("%s: caddy routes: %v", project, err)
			return files
		}
	}

	// The variables these routes interpolate (${APP_DOMAIN}, ${APP_PUBLIC_IP_DASH}, …)
	// are not seeded here: they are the deployment's, and Normalize ensures them into
	// the app's .env from .env.app on this same up.

	return composeFiles(basePath, overridePath)
}

// composeFiles is the file list for an up: the base, plus the override when there
// is one.
func composeFiles(basePath, overridePath string) []string {
	files := []string{basePath}
	if _, err := os.Stat(overridePath); err == nil {
		files = append(files, overridePath)
	}
	return files
}
