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

	// SharedDirPath overrides where cross-tool state lives (see SharedDir). Empty
	// means the default, ${DataRoot}/AppDataShared. Set SHARED_DIR to move it —
	// which is how a test or a local `go run` points the backup engines at a scratch
	// tree instead of the real one.
	SharedDirPath string

	PUID string
	PGID string
	TZ   string

	StoreURLs []string // app-store zip URLs (multi-store)

	// Domains returns the additional domains every app is published on, beyond the
	// primary one its compose already routes (see internal/routes). It is a
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

// FromEnv builds a Config from environment variables with sensible defaults.
func FromEnv() Config {
	dataRoot := envOr("DATA_ROOT", "/DATA")
	c := Config{
		Addr:          envOr("HTTP_ADDR", ":8080"),
		DataRoot:      dataRoot,
		DataHostPath:  envOr("DATA_HOST_PATH", dataRoot),
		StateDirPath:  os.Getenv("STATE_DIR"),
		SharedDirPath: os.Getenv("SHARED_DIR"),
		PUID:          envOr("PUID", "1000"),
		PGID:          envOr("PGID", "1000"),
		TZ:            os.Getenv("TZ"),
		StoreURLs: splitList(envOr("APPSTORE_URL",
			"https://github.com/Yundera/AppStore/archive/refs/heads/main.zip")),
	}
	return c
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

// SharedDir holds state that belongs to the deployment rather than to any one app:
// ${DataRoot}/AppDataShared. It sits *beside* AppData, not inside it, so the app
// model never mistakes it for an app folder (managedDirs only reads AppData) and an
// app's backup never sweeps it up.
//
// It is deliberately inside the user-data backup set, so a box running two backup
// engines has each engine's backup carrying the other's configuration — see
// docs/backup.md. The engines' caches and logs are excluded by pattern, at the
// engine's own policy level rather than here.
//
// SHARED_DIR overrides it, exactly as STATE_DIR overrides StateDir. That override is
// what lets a test or a local `go run` point the backup engines at a scratch tree.
func (c Config) SharedDir() string {
	if c.SharedDirPath != "" {
		return c.SharedDirPath
	}
	return filepath.Join(c.DataRoot, "AppDataShared")
}

// BackupEngineDir is where one backup engine keeps its repository config, its
// password, its cache and its logs: ${SharedDir}/backup/<engine>.
//
// One directory per engine rather than one shared directory, because the engine is
// a user-flippable choice and a box that has switched must still be able to read
// what the previous engine wrote — listing and restore dispatch on where a backup
// actually is, never on which engine is currently selected.
//
// Maison does not create or write this directory: a self-check script on the host
// renders the credentials into it, and Maison only reads. An absent directory is the
// normal "not configured" state, not an error.
func (c Config) BackupEngineDir(engine string) string {
	return filepath.Join(c.SharedDir(), "backup", engine)
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
