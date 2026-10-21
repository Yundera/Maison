package routing

import (
	"strconv"
	"strings"
)

// Vars are what Maison seeds into an app's .env so the app knows its own address:
//
//	APP_URL, APP_HOST                    the first route (the web UI), on the primary target
//	ROUTE_<NAME>_URL, ROUTE_<NAME>_HOST  every further route
//	APP_BIND, APP_PORT_<NAME>            ports renderer: what the override's ports: entries read
//
// APP_URL is an origin — scheme://host[:port], no path, no trailing slash — so a
// listing appends its own path (`${APP_URL}/seafhttp`). Values are resolved, not
// templated: they are rewritten on every start, so a new domain or IP still
// follows, and the dialect's naming rule never has to live in an app's .env.
//
// A route whose address is not known gets no URL keys: an app is better off with
// an unresolved ${APP_URL}, which compose reports, than a blank one.
func Vars(entries []Entry, s Setting) map[string]string {
	out := map[string]string{}
	first := ""
	for _, e := range entries {
		if first == "" {
			first = e.Name
		}
		if s.Renderer == Ports && e.HostPort > 0 {
			out["APP_PORT_"+EnvName(e.Name)] = strconv.Itoa(e.HostPort)
		}
		if !e.Primary || e.URL == "" {
			continue
		}
		if e.Name == first {
			out["APP_URL"], out["APP_HOST"] = e.URL, e.PublicHost
		} else {
			out["ROUTE_"+EnvName(e.Name)+"_URL"], out["ROUTE_"+EnvName(e.Name)+"_HOST"] = e.URL, e.PublicHost
		}
	}
	if s.Renderer == Ports && len(entries) > 0 {
		out["APP_BIND"] = s.Bind
	}
	return out
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
