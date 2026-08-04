// Package config holds runtime configuration derived from the environment and
// the persisted settings file.
package config

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/yundera/maison/internal/brand"
	"github.com/yundera/maison/internal/domains"
)

// Config is the process-wide runtime configuration.
type Config struct {
	Addr string // listen address, e.g. ":8080"

	// DataRoot is the data folder as seen INSIDE this container (the bind-mount
	// target). Maison reads/writes its own files here (compose projects, store
	// cache, settings).
	DataRoot string

	// DataHostPath is the SAME data folder's path on the Docker host. Because
	// app bind-mount sources are resolved by the host daemon (not inside this
	// container), generated app compose files must reference host paths. Defaults
	// to DataRoot when the container mount point equals the host path.
	DataHostPath string

	// StateDirPath overrides where Maison keeps everything it owns (see StateDir).
	// Empty means the default, ${DataRoot}/AppData/maison. Set STATE_DIR
	// to move it — e.g. onto a different volume, or out of AppData entirely so the
	// dashboard's own folder is not also an app folder.
	StateDirPath string

	PUID string
	PGID string
	TZ   string

	StoreURLs []string // app-store zip URLs (multi-store)

	// ProtectedApps names apps the user must not uninstall from the dashboard —
	// the platform's own pieces (maison, casaos, …), which appear as ordinary
	// tiles but whose Uninstall entry is hidden and whose DELETE is refused. An
	// entry matches an app's store ID (x-casaos store_app_id / x-compose-app id)
	// or, failing that, its compose project name. Case-insensitive.
	ProtectedApps []string

	// Domains returns the additional domains every app is published on, beyond the
	// primary one its compose already routes (see internal/caddyroutes). It is a
	// function, not a slice, because the operator edits the list at runtime while
	// Config is a value copied once at boot — reading it live is what lets a
	// settings change reach the next `docker compose up` without a restart.
	//
	// nil means the feature is unwired (a Config built outside the server), and no
	// override is ever touched.
	Domains func() []domains.Domain

	// AppEnv returns the deployment's app-facing variables — the contents of
	// .env.app (see internal/appenv), which Maison forwards into every app's .env.
	// It is a function, not a map, for the same reason Domains is: the file belongs
	// to the deployment and is edited while Maison runs, so reading it live is
	// what lets a new domain or IP reach the next `docker compose up` without a
	// restart.
	//
	// nil means the feature is unwired (a Config built outside the server); apps
	// then get only the variables Maison computes for itself.
	AppEnv func() map[string]string
}

// appEnv is AppEnv, tolerating a Config that never wired it.
func (c Config) appEnv() map[string]string {
	if c.AppEnv == nil {
		return nil
	}
	return c.AppEnv()
}

// AppDomain is the deployment's base domain (.env.app's APP_DOMAIN). Maison uses
// it for two things of its own: resolving an app's click-through URL
// (xcomposeapp.WebURL) and recognising an app's gateway host in the dashboard's
// host-based dispatch (server/gate.go). Empty when the deployment has no domain,
// in which case apps simply have no reachable web address.
func (c Config) AppDomain() string { return c.appEnv()["APP_DOMAIN"] }

// AppNet is the external Docker network Maison attaches every app's main service
// to (.env.app's APP_NET). Empty means no network is attached.
func (c Config) AppNet() string { return c.appEnv()["APP_NET"] }

// FromEnv builds a Config from environment variables with sensible defaults.
func FromEnv() Config {
	dataRoot := envOr("DATA_ROOT", "/DATA")
	c := Config{
		Addr:         envOr("HTTP_ADDR", ":8080"),
		DataRoot:     dataRoot,
		DataHostPath: envOr("DATA_HOST_PATH", dataRoot),
		StateDirPath: os.Getenv("STATE_DIR"),
		PUID:         envOr("PUID", "1000"),
		PGID:         envOr("PGID", "1000"),
		TZ:           os.Getenv("TZ"),
		StoreURLs: splitList(envOr("APPSTORE_URL",
			"https://github.com/Yundera/AppStore/archive/refs/heads/main.zip")),
		ProtectedApps: splitList(os.Getenv("PROTECTED_APPS")),
	}
	return c
}

// IsProtected reports whether an app is exempt from uninstall. Both identifiers
// are tested: storeID (the store's app id, e.g. "maison") and project (the
// compose project / folder name), so a protected app is caught whether or not it
// carries store metadata.
func (c Config) IsProtected(storeID, project string) bool {
	for _, p := range c.ProtectedApps {
		if storeID != "" && strings.EqualFold(p, storeID) {
			return true
		}
		if project != "" && strings.EqualFold(p, project) {
			return true
		}
	}
	return false
}

// AppsDir is the flat root that holds one directory per app
// (${DATA_ROOT}/AppData/<app>). Each app directory carries its own
// docker-compose.yml, docker-compose.override.yml, .env, and data — the folder's
// presence is what makes an app appear on the dashboard. See docs/app-model.md.
func (c Config) AppsDir() string {
	return filepath.Join(c.DataRoot, "AppData")
}

// BackupsDir holds every app archive, one sub-directory per app
// (${DATA_ROOT}/AppData/.backups/<app>/<stamp>). Archives land here whether they
// came from an uninstall or from an on-demand backup.
//
// It sits inside AppData rather than beside it so that an archive is always on
// the same filesystem as the app it came from — which is what keeps uninstall's
// archive step, and restore's swap, an instantaneous rename instead of a copy.
// The leading dot keeps it off the dashboard for free: a name containing a dot is
// never a tile (see apps.Registry.managedDirs and docs/app-model.md).
func (c Config) BackupsDir() string {
	return filepath.Join(c.DataRoot, "AppData", ".backups")
}

// StateDir is where everything Maison owns lives: its settings, its store cache,
// and the deployment's .env.app. It defaults to Maison's own app directory —
// ${DataRoot}/AppData/maison — the same folder a deployment installs the dashboard's
// compose stack into, so there is one place to look for anything Maison, and no
// hidden sibling.
//
// The default name carries no dot, so the app model's "a dot in the name hides it"
// rule does NOT hide it: when a deployment puts a docker-compose.yml here, Maison
// tiles itself, which is intended. On a standalone install there is no compose file
// here and the folder holds state alone — isManaged requires a docker-compose.yml, so
// it stays off the dashboard rather than rendering an empty tile.
//
// STATE_DIR overrides it. It is a path INSIDE this container, like DataRoot:
// point it outside AppData and the state stops sharing a folder with an app; point it
// at another volume and it moves off the data disk entirely. A deployment that sets it
// must put .env.app there too — that is where Maison will look.
func (c Config) StateDir() string {
	if c.StateDirPath != "" {
		return c.StateDirPath
	}
	return filepath.Join(c.DataRoot, "AppData", brand.Slug)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
