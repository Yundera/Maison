package xcomposeapp

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// Alias is an extra name the operator gives one of an app's routes on this box —
// `watch` for meta-watch's `metawatch` route, so it also answers on
// `watch.<box>` — declared in the override's x-compose-app, never by a listing:
// what a box calls an app is the box's choice, not the app's.
//
// Maison renders an alias exactly as it renders a route (the domain's labels, the
// manifest, the already-routed check), so it is generated output like any other
// and leaves no hand-written label for the next run to sweep. See
// docs/routing.md#aliases.
type Alias struct {
	// Name is the alias's leading label, in the place of the route's name in each
	// domain's host template: `watch` on `{name}.${APP_DOMAIN}`.
	Name string `yaml:"name,omitempty"`

	// Route names the route the alias answers for: a declared route's name or,
	// for a listing that carries its own labels, the name its group has on the
	// primary domain (`metawatch` for `metawatch-${APP_DOMAIN}`). Empty means the
	// app's first route.
	Route string `yaml:"route,omitempty"`

	// Domains are the routing setting's domain names the alias is published on
	// (`[dot]`). Empty means every domain.
	Domains []string `yaml:"domains,omitempty"`

	// Primary makes the alias the route's own address: the tile opens it and the
	// route's URL variable (APP_URL for the first route) names it, on the first of
	// its domains in the setting's order.
	Primary bool `yaml:"primary,omitempty"`
}

// UnmarshalYAML decodes an alias through tolerant scalars, as Route does: a type
// error here would cost the app its whole x-compose-app block. A single domain may
// be written bare (`domains: dot`).
func (a *Alias) UnmarshalYAML(n *yaml.Node) error {
	var raw struct {
		Name    text      `yaml:"name"`
		Route   text      `yaml:"route"`
		Domains yaml.Node `yaml:"domains"`
		Primary text      `yaml:"primary"`
	}
	if err := n.Decode(&raw); err != nil {
		return err
	}
	*a = Alias{Name: string(raw.Name), Route: string(raw.Route)}
	switch raw.Domains.Kind {
	case yaml.ScalarNode:
		if v := strings.TrimSpace(raw.Domains.Value); v != "" {
			a.Domains = []string{v}
		}
	case yaml.SequenceNode:
		for _, it := range raw.Domains.Content {
			if v := strings.TrimSpace(it.Value); it.Kind == yaml.ScalarNode && v != "" {
				a.Domains = append(a.Domains, v)
			}
		}
	}
	switch strings.ToLower(strings.TrimSpace(string(raw.Primary))) {
	case "true", "yes", "on", "1":
		a.Primary = true
	}
	return nil
}

// ValidName reports whether s can be a route's or an alias's name: a lowercase DNS
// label, since it becomes the leading part of a host.
func ValidName(s string) bool { return routeName.MatchString(s) }
