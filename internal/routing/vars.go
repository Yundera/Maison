package routing

import (
	"strconv"
	"strings"

	"github.com/yundera/maison/internal/domains"
)

// Vars are what Maison seeds into an app's .env so the app knows its own address:
//
//	APP_URL, APP_HOST                      the first route (the web UI), on the primary target
//	ROUTE_<NAME>_URL, ROUTE_<NAME>_HOST    every further route
//	APP_URLS, APP_HOSTS                    the first route on every target, primary first
//	ROUTE_<NAME>_URLS, ROUTE_<NAME>_HOSTS  the same for every further route
//	APP_HOST_SUFFIXES                      labels renderer: what follows `<name>-` on each domain
//	APP_BIND, APP_PORT_<NAME>              ports renderer: what the override's ports: entries read
//
// APP_URL is an origin — scheme://host[:port], no path, no trailing slash — so a
// listing appends its own path (`${APP_URL}/seafhttp`). Values are resolved, not
// templated: they are rewritten on every start, so a new domain or IP still
// follows, and the dialect's naming rule never has to live in an app's .env.
//
// The plural forms are comma-separated, for the allow-lists an app keeps of every
// address it is reached at (CORS and CSRF origins, trusted hosts). APP_HOST_SUFFIXES
// is the list an AppShield gate's REDIRECT_HOST_SUFFIXES takes; its first entry is the
// primary domain's, which the gate's OAuth issuer is derived from, so it only moves
// when the primary domain does.
//
// A route whose address is not known gets no URL keys: an app is better off with
// an unresolved ${APP_URL}, which compose reports, than a blank one.
func Vars(entries []Entry, s Setting) map[string]string {
	out := map[string]string{}
	first := ""
	urls, hosts := map[string][]string{}, map[string][]string{}
	var suffixes []string
	for _, e := range entries {
		if first == "" {
			first = e.Name
		}
		if s.Renderer == Ports && e.HostPort > 0 {
			out["APP_PORT_"+EnvName(e.Name)] = strconv.Itoa(e.HostPort)
		}
		if e.URL == "" {
			continue
		}
		// Primary first: Table emits a route's targets in domain order, the primary
		// one leading, and nothing reorders them here.
		urls[e.Name] = append(urls[e.Name], e.URL)
		hosts[e.Name] = append(hosts[e.Name], e.PublicHost)
		if e.Name == first {
			if suf, ok := hostSuffix(s, e); ok {
				suffixes = append(suffixes, suf)
			}
		}
		if !e.Primary {
			continue
		}
		if e.Name == first {
			out["APP_URL"], out["APP_HOST"] = e.URL, e.PublicHost
		} else {
			out["ROUTE_"+EnvName(e.Name)+"_URL"], out["ROUTE_"+EnvName(e.Name)+"_HOST"] = e.URL, e.PublicHost
		}
	}
	for name, list := range urls {
		prefix := "ROUTE_" + EnvName(name) + "_"
		if name == first {
			prefix = "APP_"
		}
		out[prefix+"URLS"] = strings.Join(list, ",")
		out[prefix+"HOSTS"] = strings.Join(hosts[name], ",")
	}
	if len(suffixes) > 0 {
		out["APP_HOST_SUFFIXES"] = strings.Join(suffixes, ",")
	}
	if s.Renderer == Ports && len(entries) > 0 {
		out["APP_BIND"] = s.Bind
	}
	return out
}

// hostSuffix is what follows `<name>-` in e's host, when e is on a labels domain
// whose template reads `{name}-<suffix>` — the only shape a suffix list can describe.
func hostSuffix(s Setting, e Entry) (string, bool) {
	if s.Renderer != Labels {
		return "", false
	}
	for _, d := range s.Domains {
		if d.Name != e.Target {
			continue
		}
		if !strings.HasPrefix(d.Template(), domains.NamePlaceholder+"-") {
			return "", false
		}
		suf, ok := strings.CutPrefix(e.PublicHost, e.Name+"-")
		return suf, ok && suf != ""
	}
	return "", false
}

// PortKey is the .env key a route's published port is kept under.
func PortKey(name string) string { return "APP_PORT_" + EnvName(name) }

// EnvName is a route name as it appears in a variable name: upper-cased, with every
// character a shell identifier cannot hold turned into _.
func EnvName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(name) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}
