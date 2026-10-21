package routing

import (
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/yundera/maison/internal/domains"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// Route is one of an app's routes, with its defaults applied and its port parsed.
type Route struct {
	Name     string `json:"name"`               // the host's leading label, the ROUTE_<NAME> key
	Service  string `json:"service"`            // the service that serves the port
	Port     int    `json:"upstream-port"`      // the port inside the container
	Scheme   string `json:"upstream-scheme"`    // "http" or "https": how the container serves it
	Insecure bool   `json:"insecure,omitempty"` // the container's certificate is not to be verified
	Path     string `json:"path"`               // where the web UI opens ("/")
}

// Routes reads an app's routes: name defaults to the app id, service to the app's
// only service. A route Maison cannot use — no port, no service to put it on, a name
// another route already has — is skipped, never fatal: `maison lint` is where the
// listing is told. services nil means they are not known (a caller after the
// address only), and a route's service is then left as written.
func Routes(app *xcomposeapp.App, appID string, services []string) []Route {
	if app == nil {
		return nil
	}
	var out []Route
	seen := map[string]bool{}
	for _, r := range app.Routes {
		rt := Route{
			Name:     strings.TrimSpace(r.Name),
			Service:  strings.TrimSpace(r.Service),
			Port:     r.RoutePort(),
			Scheme:   "http",
			Insecure: r.InsecureUpstream,
			Path:     strings.TrimSpace(r.Path),
		}
		if rt.Name == "" {
			rt.Name = strings.ToLower(appID)
		}
		if rt.Service == "" && len(services) == 1 {
			rt.Service = services[0]
		}
		if strings.TrimSpace(r.UpstreamScheme) == "https" {
			rt.Scheme = "https"
		}
		if rt.Path == "" || !strings.HasPrefix(rt.Path, "/") {
			rt.Path = "/"
		}
		if rt.Port == 0 || (rt.Service == "" && services != nil) || seen[rt.Name] {
			continue
		}
		seen[rt.Name] = true
		out = append(out, rt)
	}
	return out
}

// Entry is one route reached through one target: a domain (labels, external) or a
// published host port (ports).
type Entry struct {
	Route

	// Target is the domain's name, or "port" for the ports renderer.
	Target string `json:"target"`
	// Primary marks the entry the route's public address comes from: the primary
	// domain, or the published port.
	Primary bool `json:"primary"`

	// Host is the host, still templated (`outline-${APP_DOMAIN}`), as it goes into a
	// label for Compose to interpolate. Labels renderer only.
	Host string `json:"host,omitempty"`
	// Labels are the domain's own labels. Labels renderer only.
	Labels map[string]string `json:"-"`

	// HostPort is the published port. Ports renderer only.
	HostPort int `json:"host-port,omitempty"`

	// URL is the route's public origin, resolved (`https://outline-alice.nsl.sh`,
	// `http://192.168.1.10:20000`), and PublicHost its host[:port]. Both empty when
	// the address is not known — a domain variable the deployment does not set, a
	// ports box whose address has not been seen yet.
	URL        string `json:"url,omitempty"`
	PublicHost string `json:"public-host,omitempty"`
}

// Resolver expands ${VAR} references the way `docker compose` will.
type Resolver func(string) string

// PortFor returns the host port a route is published on (ports renderer).
type PortFor func(r Route) int

// Table is an app's routes under setting s. resolve turns a templated host into the
// one it is (nil leaves it as written); port picks a route's host port, and is only
// called by the ports renderer.
func Table(routes []Route, s Setting, resolve Resolver, port PortFor) []Entry {
	if resolve == nil {
		resolve = func(v string) string { return v }
	}
	var out []Entry
	for _, r := range routes {
		switch s.Renderer {
		case Ports:
			if port == nil {
				continue
			}
			hp := port(r)
			if hp == 0 {
				continue
			}
			e := Entry{Route: r, Target: "port", Primary: true, HostPort: hp}
			if s.URLHost != "" {
				e.URL, e.PublicHost = origin(r.Scheme, s.URLHost, hp)
			}
			out = append(out, e)
		case Labels:
			for i, d := range s.Domains {
				host := d.Host(r.Name)
				e := Entry{Route: r, Target: d.Name, Primary: i == 0, Host: host, Labels: d.Labels}
				if h := resolve(host); !strings.Contains(h, "${") && h != "" {
					// Behind a proxy that terminates TLS: the public scheme is https
					// whatever the container speaks.
					e.URL, e.PublicHost = "https://"+h, h
				}
				out = append(out, e)
			}
		case External:
			e := Entry{Route: r, Target: "external", Primary: true}
			if s.URLTemplate != "" {
				u := resolve(strings.ReplaceAll(s.URLTemplate, domains.NamePlaceholder, r.Name))
				if !strings.Contains(u, "${") {
					e.URL = strings.TrimRight(u, "/")
					e.PublicHost = strings.TrimPrefix(strings.TrimPrefix(e.URL, "https://"), "http://")
				}
			}
			out = append(out, e)
		}
	}
	return out
}

// origin builds scheme://host[:port], leaving out the scheme's default port the way
// a browser shows it.
func origin(scheme, host string, port int) (url, hostPort string) {
	hostPort = host
	if !(scheme == "http" && port == 80) && !(scheme == "https" && port == 443) {
		hostPort = host + ":" + strconv.Itoa(port)
	}
	return scheme + "://" + hostPort, hostPort
}

// Hosts lists the resolved hosts an app id is reached at on every domain of s —
// what the dashboard's host dispatch and the wake page match a request against.
func Hosts(s Setting, name string, resolve Resolver) []string {
	if s.Renderer != Labels {
		return nil
	}
	if resolve == nil {
		resolve = func(v string) string { return v }
	}
	var out []string
	for _, d := range s.Domains {
		if h := resolve(d.Host(name)); h != "" && !strings.Contains(h, "${") {
			out = append(out, strings.ToLower(h))
		}
	}
	sort.Strings(out)
	return out
}

// EnvResolver expands ${VAR} references from env — the deployment's .env.app —
// leaving a reference env does not carry as written.
func EnvResolver(env map[string]string) Resolver {
	return func(v string) string {
		return os.Expand(v, func(k string) string {
			if val, ok := env[k]; ok {
				return val
			}
			return "${" + k + "}"
		})
	}
}

// WebURL is the click URL of an app's first route under s: https on the primary
// domain, the external template, or "" under the ports renderer — there the tile is
// opened on the published port from the browser's own host.
func WebURL(rts []Route, s Setting, resolve Resolver) string {
	if len(rts) == 0 {
		return ""
	}
	for _, e := range Table(rts[:1], s, resolve, nil) {
		if e.Primary && e.URL != "" {
			return e.URL + rts[0].Path
		}
	}
	return ""
}
