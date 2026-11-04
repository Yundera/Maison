package stackup

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yundera/maison/internal/certs"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/routing"
)

// ensureCertificates writes each certificate the app declares (x-compose-app
// `certificates`, see internal/certs) and returns the variables that name the
// host each one was picked for — `APP_TLS_HOST` for the app's first route,
// `ROUTE_<NAME>_TLS_HOST` for a further one, empty when the certificate is
// self-signed — and the services to restart because a file changed under them.
//
// A declaration that cannot be resolved (no such route, a path outside the data
// root) is an error, like a folder's: the app would start with a missing
// certificate.
func ensureCertificates(cfg config.Config, project, dir string, files []string) (map[string]string, []string, error) {
	spec := Load(files)
	if len(spec.Certificates) == 0 {
		return nil, nil, nil
	}
	base, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	if err != nil {
		return nil, nil, err
	}
	appEnv, _ := os.ReadFile(filepath.Join(dir, ".env"))
	resolve := resolver(cfg, project, appEnv)

	var s routing.Setting
	if cfg.Routing != nil {
		s = cfg.Routing()
	}
	rts := appRoutes(project, base)
	entries := routing.Table(rts, s, resolve, nil)
	store := ""
	if s.Renderer == routing.Labels && s.CertStore != "" {
		store = envinject.ContainerPath(resolve(s.CertStore), cfg)
	}
	boxIPs := publicIPs(resolve)

	vars := map[string]string{}
	var restart []string
	for _, c := range spec.Certificates {
		name := c.Route
		if name == "" && len(rts) > 0 {
			name = rts[0].Name
		}
		hosts, known := routeHosts(entries, rts, name)
		if !known {
			return nil, nil, fmt.Errorf("certificate: no route named %q", name)
		}
		path, err := resolvePath(envinject.Render(c.Path, cfg, project, appEnv), cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("certificate for %s: %w", name, err)
		}
		uid, err := resolveUID(envinject.Render(c.User, cfg, project, appEnv), cfg.PUID)
		if err != nil {
			return nil, nil, err
		}
		gid, err := resolveGID(envinject.Render(c.Group, cfg, project, appEnv), cfg.PGID)
		if err != nil {
			return nil, nil, err
		}

		key := "ROUTE_" + routing.EnvName(name) + "_TLS_HOST"
		if len(rts) > 0 && name == rts[0].Name {
			key = "APP_TLS_HOST"
		}
		// Sticky: the host clients were told to use is kept while it qualifies.
		prefer := envinject.EnvFileVars(appEnv)[key]
		res, err := certs.Pick(store, hosts, prefer, boxIPs, nil, time.Now())
		if err != nil {
			return nil, nil, fmt.Errorf("certificate for %s: %w", name, err)
		}
		_, statErr := os.Stat(filepath.Join(path, certs.CertFile))
		existed := statErr == nil
		changed, err := certs.Write(path, res, uid, gid)
		if err != nil {
			return nil, nil, fmt.Errorf("certificate for %s: %w", name, err)
		}
		if changed {
			log.Printf("%s: certificate for %s: %s", project, name, describe(res))
			// A first write needs no restart: whatever reads it starts after it.
			if existed {
				restart = append(restart, c.Restart...)
			}
		}
		vars[key] = res.Host
	}
	return vars, restart, nil
}

// RefreshCertificates re-picks an installed app's certificates — a renewal, a
// better host that has appeared — and returns the services to restart because
// their files changed. It is the periodic half of ensureCertificates, which runs
// on every up.
func RefreshCertificates(cfg config.Config, project, dir string) ([]string, error) {
	files := []string{filepath.Join(dir, "docker-compose.yml")}
	if _, err := os.Stat(filepath.Join(dir, "docker-compose.override.yml")); err == nil {
		files = append(files, filepath.Join(dir, "docker-compose.override.yml"))
	}
	vars, restart, err := ensureCertificates(cfg, project, dir, files)
	if err != nil {
		return nil, err
	}
	writeRouteVars(cfg, project, dir, vars)
	return restart, nil
}

// DeclaresCertificates reports whether the app's compose asks for a certificate,
// so the periodic check skips every other app without picking anything.
func DeclaresCertificates(dir string) bool {
	return len(Load([]string{filepath.Join(dir, "docker-compose.yml")}).Certificates) > 0
}

// routeHosts are the resolved hosts of route name, in domain order (primary
// first), and whether the app has such a route at all.
func routeHosts(entries []routing.Entry, rts []routing.Route, name string) ([]string, bool) {
	known := false
	for _, r := range rts {
		if r.Name == name {
			known = true
		}
	}
	var hosts []string
	for _, e := range entries {
		if e.Name == name && e.PublicHost != "" {
			hosts = append(hosts, strings.ToLower(e.PublicHost))
		}
	}
	return hosts, known
}

// publicIPs are the box's public addresses, as the deployment states them.
func publicIPs(resolve routing.Resolver) []net.IP {
	var out []net.IP
	for _, v := range []string{"${APP_PUBLIC_IPV4}", "${APP_PUBLIC_IPV6}", "${APP_PUBLIC_IP}"} {
		if ip := net.ParseIP(strings.TrimSpace(resolve(v))); ip != nil {
			out = append(out, ip)
		}
	}
	return out
}

func describe(r certs.Result) string {
	if r.Trusted() {
		return "using " + r.Host
	}
	return "no trusted certificate for its hosts, using a self-signed one"
}

// dedupe keeps the first of each name, in order.
func dedupe(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}
