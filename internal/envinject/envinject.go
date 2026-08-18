// Package envinject reproduces CasaOS/casa-img's environment and template
// handling for store apps: the base interpolation variables (consumed by
// `docker compose` interpolation) and the PCS structural transforms
// (DATA_ROOT volume rewrite, external APP_NET attach, PUID:PGID user).
//
// Ported from casa-img CasaOS-AppManagement: service/compose_service.go
// (baseInterpolationMap) and route/v2/appstore_pcs.go (modifyServices).
package envinject

import (
	"bytes"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

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
	return os.Expand(s, func(k string) string {
		if v, ok := vars[k]; ok {
			return v
		}
		return "${" + k + "}" // leave references we don't own untouched
	})
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

// Transform applies the PCS structural rewrites to a store compose file:
//   - rewrite volume sources under /DATA to ${DATA_ROOT}
//   - attach the main service to the external ${APP_NET} network (if set)
//
// Both are written as *references*, never as the resolved value. That is what lets
// an app survive a change to Maison's own deployment: the compose file says
// "wherever the data root is" and "whatever the app network is", and every
// `docker compose up` resolves that afresh against the .env SyncBaseVars keeps
// current. Baking the values in — as Maison once did — froze the app to the
// deployment it happened to be installed on, and left it unstartable, with
// reinstall as the only way out, the moment that deployment moved.
//
// It is therefore idempotent, and running it over an already-transformed file is
// how a stale one heals: a compose still carrying baked literals from an older
// Maison comes back in reference form. stackup.Normalize does exactly that,
// before every up.
//
// ${VAR} interpolation itself is left to `docker compose`.
func Transform(raw []byte, cfg config.Config, mainService string) ([]byte, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}

	services, _ := doc["services"].(map[string]any)

	rewriteDataRoot(services, cfg)

	if cfg.AppNet() != "" && services != nil {
		attachExternalNetwork(doc, services, mainService)
	}

	return yaml.Marshal(doc)
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

func addIf(m map[string]string, k, v string) {
	if v != "" {
		m[k] = v
	}
}

// rewriteDataRoot points every data-root volume source at ${DATA_ROOT}, so the
// host path is resolved by `docker compose` at up time rather than frozen into the
// file.
func rewriteDataRoot(services map[string]any, cfg config.Config) {
	for _, s := range services {
		svc, ok := s.(map[string]any)
		if !ok {
			continue
		}
		vols, ok := svc["volumes"].([]any)
		if !ok {
			continue
		}
		for i, v := range vols {
			switch vol := v.(type) {
			case string:
				vols[i] = refDataRoot(vol, cfg)
			case map[string]any:
				if src, ok := vol["source"].(string); ok {
					vol["source"] = refDataRoot(src, cfg)
				}
			}
		}
	}
}

// refDataRoot rewrites one bind source to the ${DATA_ROOT} form. It accepts the
// store spelling (/DATA/...) and the resolved host spelling a previous Transform
// under *this* config would have written — which is what makes Transform
// idempotent. A source under neither (/etc/localtime, a named volume) is returned
// untouched.
//
// A compose baked by a previous Maison whose DataHostPath differed from the
// current one cannot be recognised here: the old prefix is not something this
// process can know. Such a file is re-materialised from the store instead (see
// installer.ApplyUpdate), which is a one-time cost — no compose written from here
// on carries a literal to go stale.
func refDataRoot(src string, cfg config.Config) string {
	const ref = "${DATA_ROOT}"
	if strings.HasPrefix(src, ref) || strings.HasPrefix(src, "$DATA_ROOT") {
		return src // already a reference
	}
	if rest, ok := strings.CutPrefix(src, "/DATA"); ok {
		return ref + rest
	}
	if h := cfg.DataHostPath; h != "" && h != "/DATA" {
		if rest, ok := strings.CutPrefix(src, h); ok {
			return ref + rest
		}
	}
	return src
}

// AppNetKey is the compose-local key of the external network Maison attaches an
// app's main service to. It is deliberately *not* the network's name: the name is
// the deployment's (${APP_NET}, resolved at up time), while the key is a fixed
// handle inside the project, so that changing the network the deployment uses does
// not have to rewrite every service's `networks:` list.
const AppNetKey = "appnet"

// attachExternalNetwork joins the main service to the deployment's external
// network, referenced as ${APP_NET} rather than by its current name.
//
// It also drops the network a previous Maison attached, recognisable by its
// exact signature — an external network whose compose key *is* its name, which is
// what `networks[refNet] = {name: refNet, external: true}` used to produce. That
// entry names a network that may no longer exist (it names the one the app was
// installed against), and leaving it behind would keep the stack unstartable even
// once the right network is attached, since compose insists every declared
// external network exists.
func attachExternalNetwork(doc, services map[string]any, mainService string) {
	networks, _ := doc["networks"].(map[string]any)
	if networks == nil {
		networks = map[string]any{}
		doc["networks"] = networks
	}

	for _, key := range staleNetworks(networks) {
		delete(networks, key)
		detachNetwork(services, key)
	}

	networks[AppNetKey] = map[string]any{"name": "${APP_NET}", "external": true}

	// Default the main service to the first one if unspecified.
	if mainService == "" {
		names := make([]string, 0, len(services))
		for name := range services {
			names = append(names, name)
		}
		sort.Strings(names) // map order is random; the choice must not be
		if len(names) == 0 {
			return
		}
		mainService = names[0]
	}
	svc, ok := services[mainService].(map[string]any)
	if !ok {
		return
	}
	switch nets := svc["networks"].(type) {
	case []any:
		for _, n := range nets {
			if s, ok := n.(string); ok && s == AppNetKey {
				return // already attached — avoid a duplicate list entry
			}
		}
		svc["networks"] = append(nets, AppNetKey)
	case map[string]any:
		nets[AppNetKey] = nil // idempotent
	default:
		svc["networks"] = []any{AppNetKey}
	}
}

// staleNetworks names the external networks a previous Maison attached, which
// must be dropped before the current one is added. Left behind, they name a network
// that may no longer exist — and compose insists every declared external network
// does — so the stack would stay unstartable even once the right network is
// attached.
//
// The discrimination that matters is against an external network the *store app*
// declared, which we must never touch. It rests on how compose reads `name`: for an
// external network, an omitted `name` means "the network is called after the key".
// So a store app joining an existing network writes just
//
//	networks: {traefik: {external: true}}          → no `name` → not ours
//
// whereas every external network Maison has ever generated writes `name`
// explicitly, and in one of exactly two shapes:
//
//	name: pcs                                      → the key, resolved (the original bug)
//	name: ${APP_NET}  /  name: ${REF_NET}          → a reference (REF_NET being the old spelling)
//
// Hence: ours iff `name` is present and is either the key itself or an
// interpolation reference. A store app that both names its external network *and*
// names it after its own key would be misread — but that spelling is redundant, and
// no CasaOS store app writes it.
func staleNetworks(networks map[string]any) []string {
	var stale []string
	for key, n := range networks {
		if key == AppNetKey {
			continue // the one we are about to (re)write
		}
		net, ok := n.(map[string]any)
		if !ok {
			continue
		}
		if ext, _ := net["external"].(bool); !ext {
			continue
		}
		name, named := net["name"].(string)
		if !named {
			continue // the store app's own — it never sets `name`
		}
		if name == key || strings.HasPrefix(name, "${") {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale) // map order is random; the rewrite must not be
	return stale
}

// detachNetwork removes key from every service's `networks:`, so a network we drop
// leaves no dangling reference behind.
func detachNetwork(services map[string]any, key string) {
	for _, s := range services {
		svc, ok := s.(map[string]any)
		if !ok {
			continue
		}
		switch nets := svc["networks"].(type) {
		case []any:
			kept := make([]any, 0, len(nets))
			for _, n := range nets {
				if s, ok := n.(string); ok && s == key {
					continue
				}
				kept = append(kept, n)
			}
			if len(kept) == 0 {
				delete(svc, "networks")
			} else {
				svc["networks"] = kept
			}
		case map[string]any:
			delete(nets, key)
			if len(nets) == 0 {
				delete(svc, "networks")
			}
		}
	}
}
