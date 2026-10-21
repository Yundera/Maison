package stackup

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/yundera/maison/internal/composefile"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/incident"
	"github.com/yundera/maison/internal/routes"
	"github.com/yundera/maison/internal/routes/dialect"
	"github.com/yundera/maison/internal/routing"
)

// AppNetRef is how the deployment's app network is written into an override: the
// same spelling store listings use, so a hand-run compose without the variable
// still lands on the PCS network.
const AppNetRef = "${APP_NET:-pcs}"

// SyncRoutes reconciles the app's routing — what the deployment's routing setting
// needs written for its routes — into its docker-compose.override.yml, and returns
// the compose files to bring the stack up with plus the variables to seed into the
// app's .env (APP_URL, APP_HOST, ROUTE_<NAME>_*, and in ports mode APP_BIND and
// APP_PORT_<NAME>; see routing.Vars).
//
// It runs before *every* up, which is what makes routing self-healing: a new
// setting, a reinstall, a restored backup or a hand-edited override all converge on
// the same file the next time the stack comes up. A routing change only reaches a
// running container through a recreate anyway, so there is no cheaper moment.
//
// The returned file list matters: the override may not have existed before this
// call, or may have been emptied out, and `docker compose` has to be handed exactly
// the files that now exist.
func SyncRoutes(cfg config.Config, project, dir string, files []string) ([]string, map[string]string) {
	return syncRoutes(cfg, project, dir, files, nil)
}

// syncRoutes is SyncRoutes with ports to stay off (the retry after compose found a
// port taken that Maison could not see).
func syncRoutes(cfg config.Config, project, dir string, files []string, avoid map[int]bool) ([]string, map[string]string) {
	if cfg.Routing == nil {
		return files, nil // feature unwired — never touch the operator's override
	}
	s := cfg.Routing()

	basePath := filepath.Join(dir, "docker-compose.yml")
	overridePath := filepath.Join(dir, "docker-compose.override.yml")
	base, err := os.ReadFile(basePath)
	if err != nil {
		return files, nil // not a managed app: no store compose to route
	}
	override, _ := os.ReadFile(overridePath)
	appEnv, _ := os.ReadFile(filepath.Join(dir, ".env"))
	resolve := resolver(cfg, project, appEnv)

	entries := table(cfg, s, project, base, appEnv, resolve, avoid)
	d, _ := dialect.Get(s.Dialect)
	plan := routes.Plan{
		Renderer:   s.Renderer,
		Dialect:    d,
		Domains:    s.Domains,
		Entries:    entries,
		Network:    resolve(AppNetRef),
		NetworkRef: AppNetRef,
	}

	out, err := routes.Sync(base, override, plan, resolve)
	if err != nil {
		// A malformed override is the operator's to fix, and they can still see it in
		// the YAML editor. Failing the up over it would strand the app with no way
		// back, so carry on with what is on disk.
		log.Printf("%s: routes: %v", project, err)
		return files, nil
	}
	switch {
	case out == nil:
		if err := os.Remove(overridePath); err != nil && !os.IsNotExist(err) {
			log.Printf("%s: routes: %v", project, err)
		}
	case !bytes.Equal(out, override):
		if err := os.WriteFile(overridePath, out, 0o644); err != nil {
			log.Printf("%s: routes: %v", project, err)
			return files, nil
		}
	}
	recordRoutes(cfg, project, entries)
	return composeFiles(basePath, overridePath), routing.Vars(entries, s)
}

// Table is an app's route table as it stands, without allocating anything: a port
// the app has not been given yet is simply absent. It is what GET /api/routes and
// the click URL read.
func Table(cfg config.Config, project, dir string) []routing.Entry {
	if cfg.Routing == nil {
		return nil
	}
	base, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	if err != nil {
		return nil
	}
	appEnv, _ := os.ReadFile(filepath.Join(dir, ".env"))
	s := cfg.Routing()
	rts, have := appRoutes(project, base), envinject.EnvFileVars(appEnv)
	return routing.Table(rts, s, resolver(cfg, project, appEnv), func(r routing.Route) int {
		if hp := routes.PublishedPort(base, r.Service, r.Port); hp > 0 {
			return hp
		}
		n, _ := strconv.Atoi(have[routing.PortKey(r.Name)])
		return n
	})
}

func table(cfg config.Config, s routing.Setting, project string, base, appEnv []byte, resolve routing.Resolver, avoid map[int]bool) []routing.Entry {
	rts := appRoutes(project, base)
	var port routing.PortFor
	if s.Renderer == routing.Ports {
		have := envinject.EnvFileVars(appEnv)
		picker := &routes.PortPicker{Taken: takenPorts(cfg, project, avoid), Low: s.PortRange[0], High: s.PortRange[1]}
		port = func(r routing.Route) int {
			if hp := routes.PublishedPort(base, r.Service, r.Port); hp > 0 {
				return hp
			}
			kept, _ := strconv.Atoi(have[routing.PortKey(r.Name)])
			if avoid[kept] {
				kept = 0
			}
			return picker.Pick(kept, r.Port)
		}
	}
	return routing.Table(rts, s, resolve, port)
}

// appRoutes reads the routes an app's store compose declares.
func appRoutes(project string, base []byte) []routing.Route {
	f, err := composefile.Parse(base)
	if err != nil {
		return nil
	}
	a, err := f.App()
	if err != nil {
		return nil
	}
	services := make([]string, 0, len(f.Services))
	for name := range f.Services {
		services = append(services, name)
	}
	sort.Strings(services)
	return routing.Routes(a, project, services)
}

// resolver expands ${VAR} references with the variables `docker compose` will
// interpolate the override with (envinject.Render: the process environment, Maison's
// base variables, then the app's own .env), with the deployment's .env.app beneath
// the app's .env — on a first install the app's .env may not carry a domain
// variable yet. Host identity is decided on the resolved host: without it a route
// written as `${PUBLIC_IP_DASH}.nip.io` reads as a different host from a domain
// configured as `{name}-${APP_PUBLIC_IP_DASH}.nip.io`, and the app is published
// twice on one name.
func resolver(cfg config.Config, project string, appEnv []byte) routing.Resolver {
	var layered bytes.Buffer
	if cfg.AppEnv != nil {
		keys := make([]string, 0)
		env := cfg.AppEnv()
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			layered.WriteString(k + "=" + env[k] + "\n")
		}
	}
	layered.Write(appEnv)
	raw := layered.Bytes()
	return func(v string) string { return envinject.Render(v, cfg, project, raw) }
}

// takenPorts is every host port another app holds: published by its containers
// (Docker), or kept in its .env for when it next starts.
func takenPorts(cfg config.Config, project string, avoid map[int]bool) map[int]bool {
	taken := map[int]bool{}
	if cfg.PortsInUse != nil {
		for p := range cfg.PortsInUse(project) {
			taken[p] = true
		}
	}
	for p := range avoid {
		taken[p] = true
	}
	envs, _ := filepath.Glob(filepath.Join(cfg.AppsDir(), "*", ".env"))
	for _, path := range envs {
		if filepath.Base(filepath.Dir(path)) == project {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for k, v := range envinject.EnvFileVars(raw) {
			if strings.HasPrefix(k, "APP_PORT_") {
				if n, err := strconv.Atoi(v); err == nil {
					taken[n] = true
				}
			}
		}
	}
	return taken
}

// allocatedPort reads the host port out of Docker's "already in use" failure:
//
//	Bind for 0.0.0.0:5556 failed: port is already allocated
//	failed to bind host port 0.0.0.0:5556/tcp: address already in use
var allocatedPort = regexp.MustCompile(`:(\d+)(?:/(?:tcp|udp))?(?: failed)?: (?:port is already allocated|address already in use)`)

func portInUse(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	m := allocatedPort.FindStringSubmatch(err.Error())
	if m == nil {
		return 0, false
	}
	n, convErr := strconv.Atoi(m[1])
	return n, convErr == nil
}

// recordRoutes keeps <state>/routes.json — every app's route table — current, for
// an operator's own router to read (the external renderer). The same content is
// served at GET /api/routes.
var routesFileMu sync.Mutex

func recordRoutes(cfg config.Config, project string, entries []routing.Entry) {
	if cfg.StateDir() == "" {
		return
	}
	routesFileMu.Lock()
	defer routesFileMu.Unlock()
	path := filepath.Join(cfg.StateDir(), "routes.json")
	all := map[string][]routing.Entry{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &all)
	}
	if len(entries) == 0 {
		delete(all, project)
	} else {
		all[project] = entries
	}
	b, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		log.Printf("routes.json: %v", err)
	}
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

// writeRouteVars seeds the app's own address into its .env. Maison's to own: every
// key is rewritten on every up, so a new domain, IP or port follows.
func writeRouteVars(cfg config.Config, project, dir string, vars map[string]string) {
	if len(vars) == 0 {
		return
	}
	if err := envinject.EnsureVars(filepath.Join(dir, ".env"), vars); err != nil {
		log.Printf("%s: route variables: %v", project, err)
		cfg.ReportIncident(hookIncident(project, "its address could not be written to .env", err))
	}
}

// movedOff reports which route the retry moved off port, and the port it is on
// now: the same port again would only fail the same way.
func movedOff(before, after map[string]string, port int) (key, now string, ok bool) {
	held := strconv.Itoa(port)
	for k, v := range before {
		if strings.HasPrefix(k, "APP_PORT_") && v == held && after[k] != "" && after[k] != held {
			return k, after[k], true
		}
	}
	return "", "", false
}

// portIncident tells the operator a route's address changed under them.
func portIncident(project string, port int, key, now string) incident.Report {
	route := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(key, "APP_PORT_"), "_", "-"))
	return incident.Report{
		ID:       "app.address.port:" + project,
		Kind:     incident.KindAppAddress,
		Severity: incident.Warning,
		Title:    project + " moved off port " + strconv.Itoa(port),
		Detail: "Port " + strconv.Itoa(port) + " was already in use on this machine, by something outside Maison, so " +
			project + "'s route " + route + " is now published on port " + now + ". To pick another, change " + key +
			" in the app's Config tab.",
		Args: map[string]string{"app": project},
	}
}
