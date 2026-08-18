// Package appenv owns .env.app — the deployment's statement of what every app
// receives.
//
// CasaOS mixes the variables it needs to run with the variables it forwards to the
// apps it manages, in one environment. Maison separates them: what Maison needs
// stays in Maison's own environment (DATA_ROOT, APPSTORE_URL, HTTP_ADDR, …)
// and is never forwarded; what an app receives is written in .env.app, which lives
// with the deployment's data rather than in Maison's compose. Nothing is in both,
// so there is no question of which one wins.
//
// The file is read on install and on every start, and each of its keys is ensured
// in the app's own .env (see Sync). It is the deployment's to write — Maison
// creates it once with a documented default and never overwrites it.
package appenv

import (
	_ "embed"
	"os"
	"path/filepath"

	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/envinject"
)

// defaultFile is the .env.app Maison writes when a deployment has none. It is
// heavily commented: it is the operator's primary interface to what apps receive.
//
//go:embed default.env.app
var defaultFile []byte

// Path is where the deployment's .env.app lives — inside Maison's own state
// directory (config.StateDir), beside its settings and store cache.
func Path(cfg config.Config) string {
	return filepath.Join(cfg.StateDir(), ".env.app")
}

// Ensure creates .env.app with the shipped default when the deployment has none,
// and leaves it alone otherwise. Called once at boot.
//
// It never rewrites an existing file. The file is the deployment's — a PCS writes
// it at provisioning, an operator edits it by hand — and Maison overwriting it on
// upgrade would silently revert their domain, network and credentials.
func Ensure(cfg config.Config) error {
	path := Path(cfg)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, defaultFile, 0o644)
}

// ReadRaw returns the deployment's .env.app verbatim — comments, blank lines and
// ordering included.
//
// The settings editor round-trips this text, so it has to be the file itself and
// not a rendering of Load's map: the comments are the file's documentation (see the
// package doc), the ordering is the deployment's, and an empty value means
// something Load's map cannot express (it drops the key entirely).
//
// A deployment whose file has not been created yet reads back the shipped default
// rather than an error, so the editor always opens on something worth reading. That
// text is exactly what Ensure would have written.
func ReadRaw(cfg config.Config) ([]byte, error) {
	raw, err := os.ReadFile(Path(cfg))
	if os.IsNotExist(err) {
		return defaultFile, nil
	}
	return raw, err
}

// WriteRaw replaces the deployment's .env.app with raw, after checking Maison
// can read it back as written (envinject.ValidateEnvFile). The check is the point:
// this replaces a file the apps on the box depend on, and a value silently dropped
// for a typo would surface much later, as an app that fails to start.
//
// Unlike Ensure, this is meant to overwrite — it is the operator asking. Note the
// file may have another writer: on a Yundera PCS the orchestrator provisions it,
// and may rewrite it again (the editor says so).
//
// Changing a value here does not reach an installed app until its next start, when
// Sync runs. Nothing is restarted from here.
func WriteRaw(cfg config.Config, raw []byte) error {
	if err := envinject.ValidateEnvFile(raw); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(Path(cfg)), 0o755); err != nil {
		return err
	}
	return os.WriteFile(Path(cfg), raw, 0o644)
}

// Load reads the deployment's app-facing variables. A key with an empty value is
// omitted: .env.app states what a deployment *has*, and an app is better off with
// an unresolved ${APP_DOMAIN} — which compose reports — than with a blank one,
// which silently routes it at nothing.
//
// A missing or unreadable file yields no variables rather than an error: apps then
// get only what Maison computes for them, which is a working local install.
func Load(cfg config.Config) map[string]string {
	raw, err := os.ReadFile(Path(cfg))
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, v := range envinject.ParseEnvFile(raw) {
		if v.Value != "" {
			out[v.Key] = v.Value
		}
	}
	return out
}

// Sync ensures every variable the app should have in the app's own .env: the
// deployment's (.env.app) merged with the ones Maison computes per app and per
// install (envinject.BaseVars — AppID, PUID/PGID/TZ, the data root).
//
// Each key is ensured independently: one already in the app's .env is set to the
// current value in the line it already occupies, one that is missing is appended.
// Neither file's ordering matters, and a key Maison does not forward is never
// touched — an app's .env is free to carry the operator's own variables.
//
// This is what keeps an installed app startable after the deployment moves. The
// app's compose refers to its surroundings only through ${APP_NET}, ${DATA_ROOT},
// ${APP_DOMAIN}, … — never a baked literal (see envinject.Transform) — so a new
// network, data root, domain or IP is picked up on the next start rather than
// stranding the app until it is reinstalled.
//
// Writing the values into the .env, rather than relying on the environment
// `docker compose` inherits from Maison, is the point: a `docker compose up -d`
// run by hand in the app's folder must bring the app up exactly as Maison does.
func Sync(cfg config.Config, appID, appDir string) error {
	vars := envinject.BaseVars(cfg, appID)
	for k, v := range Load(cfg) {
		vars[k] = v
	}
	return envinject.EnsureVars(filepath.Join(appDir, ".env"), vars)
}
