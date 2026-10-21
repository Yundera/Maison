package xcomposeapp

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// routeName is a DNS label: it becomes the leading part of a host.
var routeName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// RoutePort is the route's upstream port, or 0 when it is not a usable port.
func (r Route) RoutePort() int {
	n, err := strconv.Atoi(strings.TrimSpace(r.UpstreamPort))
	if err != nil || n < 1 || n > 65535 {
		return 0
	}
	return n
}

// ValidateRoutes checks an app's routes against the services its compose declares
// (service name → the ports it lists under `expose:`). It is what `maison lint`
// reports for a store listing; on a box an invalid route is skipped, never fatal.
func ValidateRoutes(routes []Route, services map[string][]string) []error {
	var errs []error
	seen := map[string]bool{}
	for i, r := range routes {
		label := fmt.Sprintf("route %d", i+1)
		if r.Name != "" {
			label = "route " + r.Name
			if !routeName.MatchString(r.Name) {
				errs = append(errs, fmt.Errorf("%s: name must be a lowercase DNS label (letters, digits, -)", label))
			}
			if seen[r.Name] {
				errs = append(errs, fmt.Errorf("%s: name is used by another route", label))
			}
			seen[r.Name] = true
		} else if i > 0 {
			errs = append(errs, fmt.Errorf("%s: name is required on every route but the first", label))
		}
		svc := r.Service
		switch {
		case svc == "" && len(services) != 1:
			errs = append(errs, fmt.Errorf("%s: service is required when the app has several", label))
		case svc != "":
			if _, ok := services[svc]; !ok {
				errs = append(errs, fmt.Errorf("%s: service %q is not in the compose", label, svc))
			}
		}
		if r.RoutePort() == 0 {
			errs = append(errs, fmt.Errorf("%s: upstream-port %q is not a port (1-65535)", label, r.UpstreamPort))
		}
		switch r.UpstreamScheme {
		case "", "http", "https":
		default:
			errs = append(errs, fmt.Errorf("%s: upstream-scheme must be http or https", label))
		}
		if r.InsecureUpstream && r.UpstreamScheme != "https" {
			errs = append(errs, fmt.Errorf("%s: insecure-upstream only applies to upstream-scheme: https", label))
		}
		if r.Path != "" && !strings.HasPrefix(r.Path, "/") {
			errs = append(errs, fmt.Errorf("%s: path must start with /", label))
		}
	}
	// The first route is the web UI; a port its service does not expose is almost
	// always a typo, and the tile would open onto nothing.
	if len(routes) > 0 {
		r := routes[0]
		svc := r.Service
		if svc == "" && len(services) == 1 {
			for k := range services {
				svc = k
			}
		}
		if exposed, ok := services[svc]; ok && r.RoutePort() > 0 && !containsPort(exposed, r.RoutePort()) {
			errs = append(errs, fmt.Errorf("the first route (the web UI) is on port %d, which service %q does not expose", r.RoutePort(), svc))
		}
	}
	return errs
}

func containsPort(list []string, port int) bool {
	for _, p := range list {
		p, _, _ = strings.Cut(strings.TrimSpace(p), "/")
		if n, err := strconv.Atoi(p); err == nil && n == port {
			return true
		}
	}
	return false
}
