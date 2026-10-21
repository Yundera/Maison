// Package routing is how a deployment reaches its apps, as one setting.
//
// A store listing says what an app needs to be reachable — routes: a service, a
// name, a port (xcomposeapp.Route) — and never how. How is the box's: this setting
// picks a renderer and its parameters, Table turns an app's routes into concrete
// targets under it, and internal/routes writes what the renderer needs into the
// app's override. The same listing therefore runs behind Caddy on a Yundera PCS,
// on published host ports at home, and behind an operator's own router.
package routing

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/yundera/maison/internal/domains"
	"github.com/yundera/maison/internal/routes/dialect"
)

// The renderers.
const (
	// Ports publishes each route's container port on the host. No proxy, no TLS:
	// what the container speaks on its port is what the host port speaks.
	Ports = "ports"
	// Labels writes proxy labels (a dialect) for every route on every domain.
	Labels = "labels"
	// External writes nothing for routing: the operator's own router reads the route
	// table (GET /api/routes) and routes the apps itself.
	External = "external"
)

// Defaults of the ports renderer.
const (
	DefaultBind     = "0.0.0.0"
	DefaultPortLow  = 20000
	DefaultPortHigh = 29999
)

// PrimaryTemplate is the primary domain of a deployment that has a domain and no
// routing setting: the `<app>-<domain>` shape every store listing has used.
const PrimaryTemplate = "{name}-${APP_DOMAIN}"

// Setting is the routing setting as stored and as edited.
type Setting struct {
	Renderer string `json:"renderer"`

	// ports
	Bind      string `json:"bind,omitempty"`       // host address ports are published on
	URLHost   string `json:"url-host,omitempty"`   // what goes in APP_URL and links; empty = learned
	PortRange [2]int `json:"port-range,omitempty"` // where ports below 1024 and taken ones go

	// labels
	Dialect string           `json:"dialect,omitempty"`
	Domains []domains.Domain `json:"domains,omitempty"` // [0] is the primary

	// external
	URLTemplate string `json:"url-template,omitempty"` // e.g. https://{name}.home.lan
}

// Primary is the primary domain of a labels setting.
func (s Setting) Primary() (domains.Domain, bool) {
	if len(s.Domains) == 0 {
		return domains.Domain{}, false
	}
	return s.Domains[0], true
}

// Normalize trims and checks a setting before it is stored. It is the one set of
// rules for every writer — the dashboard's editor and a deployment managing the
// setting through the CLI — so neither can store what the other would refuse.
//
// The fields of the renderers not chosen are kept: switching back and forth must
// not lose what was typed.
func Normalize(s Setting) (Setting, error) {
	s.Renderer = strings.TrimSpace(s.Renderer)
	s.Bind = strings.TrimSpace(s.Bind)
	s.URLHost = strings.TrimSpace(s.URLHost)
	s.Dialect = strings.TrimSpace(s.Dialect)
	s.URLTemplate = strings.TrimSpace(s.URLTemplate)

	switch s.Renderer {
	case Ports, Labels, External:
	default:
		return s, fmt.Errorf("renderer must be %s, %s or %s", Ports, Labels, External)
	}

	if s.Bind != "" && net.ParseIP(s.Bind) == nil {
		return s, fmt.Errorf("bind %q is not an IP address", s.Bind)
	}
	if s.URLHost != "" && (strings.Contains(s.URLHost, "/") || strings.ContainsAny(s.URLHost, " \t")) {
		return s, errors.New("url-host is a host name or address, without scheme or path")
	}
	if s.PortRange != [2]int{} {
		lo, hi := s.PortRange[0], s.PortRange[1]
		if lo < 1024 || hi > 65535 || lo > hi {
			return s, errors.New("port-range must be two ports between 1024 and 65535, low first")
		}
	}

	if s.Dialect != "" {
		if _, ok := dialect.Get(s.Dialect); !ok {
			return s, fmt.Errorf("unknown dialect %q (known: %s)", s.Dialect, strings.Join(dialect.Names(), ", "))
		}
	}
	doms, err := domains.Normalize(s.Domains)
	if err != nil {
		return s, err
	}
	for i := range doms {
		doms[i].Domain = doms[i].Template()
	}
	if err := checkOverlap(doms); err != nil {
		return s, err
	}
	s.Domains = doms
	if s.Renderer == Labels && len(s.Domains) == 0 {
		return s, errors.New("the labels renderer needs at least one domain: the first is the primary")
	}

	if s.URLTemplate != "" {
		if !strings.HasPrefix(s.URLTemplate, "http://") && !strings.HasPrefix(s.URLTemplate, "https://") {
			return s, errors.New("url-template must start with http:// or https://")
		}
		if !strings.Contains(s.URLTemplate, domains.NamePlaceholder) {
			return s, errors.New("url-template must contain {name}")
		}
	}
	return s, nil
}

// checkOverlap refuses two domains where one's hosts end with the other's suffix:
// a stale route on the shorter one would be recognised as the longer one's and
// cleaned up with it.
func checkOverlap(doms []domains.Domain) error {
	for i := range doms {
		for j := range doms {
			if i == j {
				continue
			}
			a, b := doms[i].Suffix(), doms[j].Suffix()
			if a != "" && b != "" && strings.HasSuffix(a, b) {
				return fmt.Errorf("domain %q is inside domain %q: their routes could not be told apart", doms[i].Name, doms[j].Name)
			}
		}
	}
	return nil
}

// Effective is the setting the box runs on: the stored one with its defaults
// filled in, or — when nothing is stored — the default for this deployment.
//
// A deployment with a domain and no setting routes the way it always has: labels
// on that one domain, which every store listing's own caddy labels already cover,
// so upgrading changes nothing until the deployment states its routing. One with no
// domain is a standalone install, which the ports renderer is for.
//
// seen is the host the dashboard was first opened at (see the server); it fills a
// ports setting's url-host when the operator has not set one.
func Effective(stored *Setting, env map[string]string, seen string) Setting {
	var s Setting
	switch {
	case stored != nil:
		s = *stored
	case env["APP_DOMAIN"] != "":
		s = Setting{Renderer: Labels, Domains: []domains.Domain{{Name: "primary", Domain: PrimaryTemplate}}}
	default:
		s = Setting{Renderer: Ports}
	}
	if s.Bind == "" {
		s.Bind = DefaultBind
	}
	if s.PortRange == [2]int{} {
		s.PortRange = [2]int{DefaultPortLow, DefaultPortHigh}
	}
	if s.Dialect == "" {
		s.Dialect = dialect.Caddy.Name()
	}
	if s.URLHost == "" {
		s.URLHost = seen
	}
	return s
}

// Migrate turns the old "additional domains" list (Settings › Domains, before the
// routing setting) into a routing setting: the labels renderer, the deployment's
// domain as the primary, and every old entry after it. An old entry has no {name}
// placeholder and Template reads it as `{name}-<host>`, which is what it meant.
func Migrate(old []domains.Domain) *Setting {
	if len(old) == 0 {
		return nil
	}
	doms := []domains.Domain{{Name: "primary", Domain: PrimaryTemplate}}
	for _, d := range old {
		d.Domain = d.Template()
		doms = append(doms, d)
	}
	return &Setting{Renderer: Labels, Dialect: dialect.Caddy.Name(), Domains: doms}
}
