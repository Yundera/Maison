// Package domains describes the domains a Maison deployment publishes its apps on.
//
// An app declares its web endpoints without naming a domain, a proxy, or a TLS
// setting — a service, a name, a port (see xcomposeapp.Route). Where those
// endpoints answer is a property of the *deployment*, not of the app, so it is
// configured here and Maison generates the labels (see internal/routes).
//
// The deployment's own domain is one of these entries like any other. Nothing in
// this package is privileged, which is what lets the same app compose run behind
// this deployment's gateway, behind someone else's, or behind nothing at all.
package domains

import "encoding/json"

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
	// It may carry a `{name}` placeholder for the app's own name, which is where
	// the deployment's naming convention lives: `{name}.lan.example.com` gives an
	// app a real subdomain, while a bare `${APP_DOMAIN}` keeps the prevailing
	// `{name}-${APP_DOMAIN}` shape. The join is a DNS choice, not a proxy one, so
	// it belongs to the domain rather than to the app or the dialect.
	Domain string `json:"domain"`

	// Labels are the labels this domain adds to every route generated on it, and
	// they win over the dialect's on a key collision. This is where the difference
	// between two domains lives: the same app, on one host with
	// `import: gateway_tls` and on another with nothing at all, so it falls
	// through to Let's Encrypt.
	//
	// Maison attaches no meaning to any key here — which keys belong to a domain
	// rather than to an app is the dialect's to declare (routes.Group.DomainKeys),
	// not this package's to know.
	Labels map[string]string `json:"labels,omitempty"`
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
