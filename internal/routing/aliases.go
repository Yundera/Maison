package routing

import (
	"fmt"
	"strings"

	"github.com/yundera/maison/internal/xcomposeapp"
)

// Alias is an operator's extra name for one of an app's routes (xcomposeapp.Alias),
// trimmed and lower-cased.
type Alias struct {
	Name    string
	Route   string
	Domains []string
	Primary bool
}

// Aliases reads the aliases an app's x-compose-app declares. Nothing is validated
// here: WithAliases is where an alias is refused, so that the refusal can be told.
func Aliases(app *xcomposeapp.App) []Alias {
	if app == nil {
		return nil
	}
	var out []Alias
	for _, a := range app.Aliases {
		al := Alias{
			Name:    strings.ToLower(strings.TrimSpace(a.Name)),
			Route:   strings.ToLower(strings.TrimSpace(a.Route)),
			Primary: a.Primary,
		}
		for _, d := range a.Domains {
			if d = strings.TrimSpace(d); d != "" {
				al.Domains = append(al.Domains, d)
			}
		}
		out = append(out, al)
	}
	return out
}

// WithAliases adds an app's aliases to its route table: one entry per alias per
// domain it is published on, rendered like the route it answers for, and marked
// with the alias's name (Entry.Alias). entries is Table's output for routes.
//
// An alias that cannot be published is left out and reported, never fatal — the
// app still comes up on its own names:
//
//   - its name is not a DNS label, or another alias of the app already has it;
//   - it names a route the app does not have, or a domain the setting does not;
//   - one of its hosts is already this app's (its own route's name, on any domain),
//     or is in claimed: another app's, or the dashboard's. claimed maps a resolved
//     host to whoever holds it. Only that host is left out; the alias keeps its
//     others.
//
// A primary alias takes over its route's primary entry, on the first of its
// domains: the route's own primary entry is demoted, and the alias's entry leads the
// route's block, so Vars lists it first in the plural forms as well. A second
// primary alias for the same route is published as an ordinary one, and reported.
//
// Aliases exist under the labels renderer only — a name means nothing to a
// published port or an operator's own router — so under any other they are all
// reported and none is published.
func WithAliases(entries []Entry, routes []Route, aliases []Alias, s Setting, resolve Resolver, claimed map[string]string) ([]Entry, []error) {
	if len(aliases) == 0 {
		return entries, nil
	}
	if s.Renderer != Labels {
		return entries, []error{fmt.Errorf("aliases need the labels routing renderer (this box uses %s); none is published", s.Renderer)}
	}
	if resolve == nil {
		resolve = func(v string) string { return v }
	}
	if len(routes) == 0 {
		return entries, []error{fmt.Errorf("the app has no route an alias could answer for")}
	}

	byName := map[string]Route{}
	for _, r := range routes {
		byName[r.Name] = r
	}
	known := map[string]bool{}
	for _, d := range s.Domains {
		known[d.Name] = true
	}
	own := map[string]bool{}
	for _, e := range entries {
		if e.Host != "" {
			own[resolve(e.Host)] = true
		}
	}

	var errs []error
	added := map[string][]Entry{}    // route name → its alias entries
	primaryOf := map[string]string{} // route name → the alias that took it over
	seen := map[string]bool{}
	for _, a := range aliases {
		label := "alias " + a.Name
		switch {
		case !xcomposeapp.ValidName(a.Name):
			errs = append(errs, fmt.Errorf("alias %q: name must be a lowercase DNS label (letters, digits, -)", a.Name))
			continue
		case seen[a.Name]:
			errs = append(errs, fmt.Errorf("%s: declared twice", label))
			continue
		}
		seen[a.Name] = true

		routeName := a.Route
		if routeName == "" {
			routeName = routes[0].Name
		}
		r, ok := byName[routeName]
		if !ok {
			errs = append(errs, fmt.Errorf("%s: the app has no route %q", label, routeName))
			continue
		}

		allowed := map[string]bool{}
		for _, d := range a.Domains {
			if !known[d] {
				errs = append(errs, fmt.Errorf("%s: the routing setting has no domain %q", label, d))
				continue
			}
			allowed[d] = true
		}
		if len(a.Domains) > 0 && len(allowed) == 0 {
			continue // every domain it named is unknown: publishing it everywhere instead would be a guess
		}

		primary := a.Primary
		if primary && primaryOf[r.Name] != "" {
			errs = append(errs, fmt.Errorf("%s: route %s already takes its address from alias %s; published as an extra name", label, r.Name, primaryOf[r.Name]))
			primary = false
		}
		for _, d := range s.Domains {
			if len(allowed) > 0 && !allowed[d.Name] {
				continue
			}
			host := d.Host(a.Name)
			h := resolve(host)
			if own[h] {
				errs = append(errs, fmt.Errorf("%s: %s is already one of the app's own names", label, h))
				continue
			}
			if owner, taken := claimed[h]; taken {
				errs = append(errs, fmt.Errorf("%s: %s is already %s's", label, h, owner))
				continue
			}
			own[h] = true
			e := Entry{Route: r, Target: d.Name, Host: host, Labels: d.Labels, Alias: a.Name}
			if !strings.Contains(h, "${") && h != "" {
				e.URL, e.PublicHost = "https://"+h, h
			}
			if primary {
				e.Primary, primary = true, false
				primaryOf[r.Name] = a.Name
			}
			added[r.Name] = append(added[r.Name], e)
		}
	}

	// Each route's block: a primary alias's entry first, then the route's own
	// entries, then its other aliases' — route order kept as Table gave it.
	var out []Entry
	placed := map[string]bool{}
	emit := func(name string) {
		if placed[name] {
			return
		}
		placed[name] = true
		var lead, rest []Entry
		for _, e := range added[name] {
			if e.Primary {
				lead = append(lead, e)
			} else {
				rest = append(rest, e)
			}
		}
		out = append(out, lead...)
		for _, e := range entries {
			if e.Name != name {
				continue
			}
			if primaryOf[name] != "" {
				e.Primary = false
			}
			out = append(out, e)
		}
		out = append(out, rest...)
	}
	for _, e := range entries {
		emit(e.Name)
	}
	for _, r := range routes {
		emit(r.Name)
	}
	return out, errs
}

// PrimaryAlias is the first primary alias that can be published under s, and the
// templated host it takes over on: the first of its domains in the setting's
// order. It is the click URL of an app with no declared routes, whose tile has no
// route to look an alias up by (a declared app goes through WebURL). Other apps'
// names are not consulted.
func PrimaryAlias(aliases []Alias, s Setting) (Alias, string, bool) {
	if s.Renderer != Labels {
		return Alias{}, "", false
	}
	for _, a := range aliases {
		if !a.Primary || !xcomposeapp.ValidName(a.Name) {
			continue
		}
		for _, d := range s.Domains {
			if len(a.Domains) == 0 || contains(a.Domains, d.Name) {
				return a, d.Host(a.Name), true
			}
		}
	}
	return Alias{}, "", false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
