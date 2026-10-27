// Package domains describes the domains a Maison deployment publishes its apps on.
//
// An app declares its web endpoints without naming a domain, a proxy, or a TLS
// setting — a service, a name, a port (xcomposeapp.Route). Where those endpoints
// answer is a property of the *deployment*: the routing setting's domain list
// (internal/routing), which this package describes, and from which internal/routes
// generates the labels.
//
// The deployment's own domain is one of these entries like any other — the first
// one — which is what lets the same app compose run behind this deployment's
// gateway, behind someone else's, or behind nothing at all.
package domains

import (
	"encoding/json"
	"errors"
	"strings"
)

// Domain is one domain every app is published on.
type Domain struct {
	// Name identifies the entry in the settings UI ("sslip", "nip", "lan").
	Name string `json:"name"`

	// Domain is the host, and stays *templated*: it is copied verbatim into the
	// generated label and resolved by Compose interpolation. Keeping it a template
	// (rather than baking the resolved string in) is what makes the label survive
	// the box changing IP — `${APP_PUBLIC_IP_DASH}.sslip.io` re-resolves on every
	// up.
	//
	// It carries a `{name}` placeholder for the route's name, which is where the
	// deployment's naming convention lives: `{name}.lan.example.com` gives an app a
	// real subdomain, while `{name}-${APP_DOMAIN}` is the Yundera shape. The join is
	// a DNS choice, not a proxy one, so it belongs to the domain rather than to the
	// app or the dialect. A host written without the placeholder (every entry from
	// before it existed) reads as `{name}-<host>`; see Template.
	Domain string `json:"domain"`

	// Labels are the labels this domain adds to every route generated on it, and
	// they win over the dialect's on a key collision. This is where the difference
	// between two domains lives: the same app, on one host with
	// `import: gateway_tls` and on another with nothing at all, so it falls
	// through to Let's Encrypt.
	//
	// A label with an empty value deletes that key from the generated group. Maison
	// attaches no meaning to any key here: which keys a cloned, hand-written group
	// gives up to its domain (TLS, for Caddy) is the dialect's to know
	// (internal/routes/dialect), not this package's.
	Labels map[string]string `json:"labels,omitempty"`
}

// NamePlaceholder is the route's name inside a domain's host template.
const NamePlaceholder = "{name}"

// Template is d's host template with the name placeholder guaranteed: a host
// written without one is read as `{name}-<host>`, the shape every domain had before
// the placeholder existed (`${APP_PUBLIC_IP_DASH}.sslip.io` meant
// `<app>-${APP_PUBLIC_IP_DASH}.sslip.io`).
func (d Domain) Template() string {
	if strings.Contains(d.Domain, NamePlaceholder) {
		return d.Domain
	}
	return NamePlaceholder + "-" + d.Domain
}

// Host is the host route name answers on, on this domain — still templated
// (`outline-${APP_DOMAIN}`), for Compose to interpolate.
func (d Domain) Host(name string) string {
	return strings.ReplaceAll(d.Template(), NamePlaceholder, name)
}

// NameOf reverses Host: the route name a host on this domain carries, when the host
// is on it at all.
func (d Domain) NameOf(host string) (string, bool) {
	pre, suf, _ := strings.Cut(d.Template(), NamePlaceholder)
	if len(host) <= len(pre)+len(suf) || !strings.HasPrefix(host, pre) || !strings.HasSuffix(host, suf) {
		return "", false
	}
	return host[len(pre) : len(host)-len(suf)], true
}

// Suffix is what every host on this domain ends with: the template after its name
// (`-${APP_PUBLIC_IP_DASH}.sslip.io`). Used to recognise a route on this domain that a
// manifest no longer lists.
func (d Domain) Suffix() string {
	_, suf, _ := strings.Cut(d.Template(), NamePlaceholder)
	return suf
}

// UnmarshalJSON accepts the field's former name, `directives`, so a settings.json
// written before the rename still loads. The name changed because "directive" is
// Caddy's word for these and they are simply labels — but a file on an operator's
// disk outlives the vocabulary we describe it with, and silently dropping their
// TLS configuration on upgrade is not an acceptable cost for a better noun.
func (d *Domain) UnmarshalJSON(b []byte) error {
	var raw struct {
		Name       string            `json:"name"`
		Domain     string            `json:"domain"`
		Labels     map[string]string `json:"labels"`
		Directives map[string]string `json:"directives"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*d = Domain{Name: raw.Name, Domain: raw.Domain, Labels: raw.Labels}
	if d.Labels == nil {
		d.Labels = raw.Directives
	}
	return nil
}

// Normalize trims and checks a domain list before it is stored, and returns the
// cleaned copy. It is the one set of rules for every writer — the dashboard's
// editor and a deployment managing the list — so neither can store a list the
// other would have refused.
//
// Each entry needs a name and a host, a host has no spaces, and a host appears
// once: generation skips a host that is already routed, so two entries on one host
// would fight over the same generated route. Labels come from a key/value editor,
// which is free to hand back the blank row someone opened and did not fill in;
// those are dropped rather than written as an empty label into every app.
func Normalize(in []Domain) ([]Domain, error) {
	if in == nil {
		return nil, nil // absent stays absent: only an explicit [] clears the list
	}
	out := make([]Domain, len(in))
	seen := map[string]bool{}
	for i, d := range in {
		d.Name = strings.TrimSpace(d.Name)
		d.Domain = strings.TrimSpace(d.Domain)
		if d.Name == "" || d.Domain == "" {
			return nil, errors.New("each domain needs a name and a host")
		}
		if strings.ContainsAny(d.Domain, " \t") {
			return nil, errors.New("a host cannot contain spaces: " + d.Domain)
		}
		if seen[d.Domain] {
			return nil, errors.New("that host is already in the list: " + d.Domain)
		}
		seen[d.Domain] = true
		clean := map[string]string{}
		for k, v := range d.Labels {
			if k = strings.TrimSpace(k); k != "" {
				clean[k] = strings.TrimSpace(v)
			}
		}
		if len(clean) == 0 {
			clean = nil
		}
		d.Labels = clean
		out[i] = d
	}
	return out, nil
}
