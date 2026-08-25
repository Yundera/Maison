// Package envinject supplies the variables an app's compose file is interpolated
// with, and maps paths between this container and the Docker host.
//
// It does not touch an app's compose file. A store's docker-compose.yml is copied
// to disk byte-for-byte and read from there unchanged; everything the deployment
// contributes — the data root, the app network, the domain — reaches the app as a
// ${VAR} the app's own compose already references, resolved by `docker compose`
// against the .env that appenv.Sync keeps current. Maison used to rewrite the file
// instead, which is what made a hand-run `docker compose up` differ from an
// install.
package envinject

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/yundera/maison/internal/config"
)

// BaseVars returns the variables Maison computes for an app itself — the ones
// that depend on the app or on where Maison is installed, and so cannot be stated
// in the deployment's .env.app: the app's ID, the identity its files are owned by,
// and the data root.
//
// Everything else an app receives comes from .env.app (see internal/appenv), which
// merges these in. Maison's own configuration — APPSTORE_URL, the store cache,
// the listen address — is not here and is never forwarded to an app.
func BaseVars(cfg config.Config, appID string) map[string]string {
	tz := cfg.TZ
	if tz == "" {
		tz = "UTC"
	}
	// DATA_ROOT / DATA_HOST_PATH are exported as the HOST path: `${DATA_ROOT}` in
	// an app's compose is a bind-mount source, and the host daemon resolves it.
	return map[string]string{
		"AppID":          appID,
		"PUID":           cfg.PUID,
		"PGID":           cfg.PGID,
		"TZ":             tz,
		"DATA_ROOT":      cfg.DataHostPath,
		"DATA_HOST_PATH": cfg.DataHostPath,
	}
}

// DerivedKeys names the variables BaseVars computes — the ones Maison merges in
// after the deployment's .env.app, and which therefore cannot be stated there.
//
// It reads the names off BaseVars rather than listing them again, so the two can
// never drift: a variable added to BaseVars is reported as derived from that moment.
// The .env.app editor uses this to tell an operator that a key they typed will be
// overwritten, instead of letting them set a value that silently has no effect.
func DerivedKeys() []string {
	out := make([]string, 0, 6)
	for k := range BaseVars(config.Config{}, "") {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// EnsureVars ensures each of vars in an app's .env, key by key.
//
// A key already in the file is set to its current value, in the line it already
// occupies; a key that is missing is appended. Nothing is reordered, nothing else
// is rewritten, and nothing is ever removed — so the file's own ordering does not
// matter, neither does .env.app's, and a variable the operator added themselves is
// left exactly where it is.
//
// Appended keys are sorted, so a fresh .env is deterministic rather than in Go's
// map order.
func EnsureVars(envPath string, vars map[string]string) error {
	raw, err := os.ReadFile(envPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	out := []Var{}
	have := make(map[string]bool, len(vars))
	for _, v := range ParseEnvFile(raw) {
		if val, ok := vars[v.Key]; ok {
			v.Value = val // ours: refresh it where it stands
			have[v.Key] = true
		}
		out = append(out, v)
	}

	missing := make([]string, 0, len(vars))
	for k := range vars {
		if !have[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	for _, k := range missing {
		out = append(out, Var{Key: k, Value: vars[k]})
	}

	patched, err := PatchEnvFile(raw, out)
	if err != nil {
		return err
	}
	if bytes.Equal(patched, raw) {
		return nil // nothing drifted — don't touch the file's mtime
	}
	return os.WriteFile(envPath, patched, 0o644)
}

// Env returns the process environment plus the base interpolation variables so
// that `docker compose` resolves ${PUID}, ${DATA_ROOT}, ${AppID}, etc.
func Env(cfg config.Config, appID string) []string {
	env := os.Environ()
	for k, v := range BaseVars(cfg, appID) {
		env = append(env, k+"="+v)
	}
	return env
}

// Render substitutes ${VAR} / $VAR references in s using the same variables the
// install-time `docker compose` run sees (Env): the process environment, overlaid
// with the app's base interpolation variables (BaseVars), overlaid with the
// KEY=VALUE lines of its persisted .env (operator edits win). References we can't
// resolve are left intact. Used to render an app's tips for display — store tips
// routinely reference the ambient vars (APP_DEFAULT_PASSWORD, DOMAIN, …) that
// only exist in the process environment.
func Render(s string, cfg config.Config, appID string, envFile []byte) string {
	return expand(s, RenderVars(cfg, appID, envFile, nil), func(name string) string {
		return "${" + name + "}" // leave references we don't own untouched
	})
}

// RenderStrict renders s like Render but fails on a reference it cannot resolve,
// naming every one of them.
//
// The difference is the whole reason both exist. Leaving `${FOO}` in place is
// right for a tip, which a person reads and can shrug at; it is wrong for a file
// an app is about to parse, where the literal `${SEARXNG_SECRET}` lands in
// settings.yml and the app starts with a secret spelled as a variable name. That
// is the failure mode this package exists to end, so a template that references
// something nobody provides fails the up instead.
//
// extra overlays everything else — it carries the values captured from `init`
// steps, which exist only for the duration of one converge and are not in any
// file.
func RenderStrict(s string, cfg config.Config, appID string, envFile []byte, extra map[string]string) (string, error) {
	missing := map[string]bool{}
	out := expand(s, RenderVars(cfg, appID, envFile, extra), func(name string) string {
		missing[name] = true
		return ""
	})
	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for k := range missing {
			names = append(names, k)
		}
		sort.Strings(names)
		return "", fmt.Errorf("unresolved variable(s): ${%s}", strings.Join(names, "}, ${"))
	}
	return out, nil
}

// RenderVars is the variable set both renderers resolve against, lowest
// precedence first: the process environment, the app's base variables, its
// persisted .env, and finally extra (captures from `init`).
func RenderVars(cfg config.Config, appID string, envFile []byte, extra map[string]string) map[string]string {
	vars := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vars[k] = v
		}
	}
	for k, v := range BaseVars(cfg, appID) {
		vars[k] = v
	}
	for k, v := range EnvFileVars(envFile) {
		vars[k] = v
	}
	for k, v := range extra {
		vars[k] = v
	}
	return vars
}

// EnvFileVars parses simple KEY=VALUE lines (the format EnvFile writes),
// skipping blanks and # comments.
func EnvFileVars(b []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

// ContainerPath maps a host-side data path (the form written into an app's
// compose: `/DATA/...`, `${DATA_ROOT}/...`, or the literal host path) to the
// same location as seen INSIDE this container, so Maison can create it through
// its own data mount. Paths that don't live under the data root are returned
// unchanged — the caller decides whether to reject them.
func ContainerPath(src string, cfg config.Config) string {
	for _, tok := range []string{"${DATA_ROOT}", "$DATA_ROOT", "${DATA_HOST_PATH}", "$DATA_HOST_PATH"} {
		src = strings.ReplaceAll(src, tok, cfg.DataRoot)
	}
	if cfg.DataHostPath != "" && strings.HasPrefix(src, cfg.DataHostPath) {
		return cfg.DataRoot + src[len(cfg.DataHostPath):]
	}
	if strings.HasPrefix(src, "/DATA") {
		return cfg.DataRoot + src[len("/DATA"):]
	}
	return src
}

// HostPath maps a path inside this container's data mount to the same location as
// the Docker host sees it — the inverse of ContainerPath. Use it on real paths
// (an app's directory, a bind source); use RewriteToHostPath on script text, which
// carries the /DATA and ${DATA_ROOT} spellings instead of a resolved path.
func HostPath(p string, cfg config.Config) string {
	if cfg.DataRoot == "" || cfg.DataHostPath == "" || cfg.DataRoot == cfg.DataHostPath {
		return p
	}
	if strings.HasPrefix(p, cfg.DataRoot) {
		return cfg.DataHostPath + p[len(cfg.DataRoot):]
	}
	return p
}

// RewriteToHostPath replaces literal /DATA and ${DATA_ROOT} references with the
// host data path. Used on x-casaos install hooks, whose commands run against the
// host daemon (via DOCKER_HOST) and must therefore use host paths.
func RewriteToHostPath(s string, cfg config.Config) string {
	if cfg.DataHostPath == "" || cfg.DataHostPath == "/DATA" {
		return s
	}
	// One pass, not three ReplaceAll calls: a host path normally *ends* in /DATA
	// (e.g. /opt/maison/DATA), so expanding ${DATA_ROOT} first and then rewriting
	// /DATA would rewrite the path we just wrote — /opt/maison/opt/maison/DATA.
	// A Replacer scans left to right and never re-scans what it emitted.
	return strings.NewReplacer(
		"${DATA_ROOT}", cfg.DataHostPath,
		"$DATA_ROOT", cfg.DataHostPath,
		"/DATA", cfg.DataHostPath,
	).Replace(s)
}
