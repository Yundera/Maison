// Package dialect is everything Maison knows about a reverse proxy's label grammar.
//
// Maison always speaks Compose; it does not always sit behind the same proxy. So
// the label vocabulary — how a route is spelled, which keys group together, which
// ones carry TLS — lives behind Dialect, and the rest of Maison (internal/routes,
// internal/routing) never names a proxy. Caddy is the only implementation today;
// a second one is a new file here and an entry in Get, not a change to the
// generator.
package dialect

import "sort"

// Label is one compose label, in file order.
type Label struct{ Key, Value string }

// Group is one route as a proxy's labels spell it: the label that names the host,
// and every label hanging off it. Subs are keyed relative to the group
// (`reverse_proxy`, not `caddy_0.reverse_proxy`).
type Group struct {
	Key   string // "caddy_0"
	Index int    // 0; -1 when the key carries no index ("caddy")
	Host  string // "outline-${APP_DOMAIN}"
	Subs  []Label
}

// Upstream is what a route proxies to, in proxy-neutral terms.
type Upstream struct {
	Port     int
	Scheme   string // "http" (default) or "https": the container terminates TLS itself
	Insecure bool   // do not verify the container's certificate (self-signed)
}

// Dialect is one proxy's label grammar.
type Dialect interface {
	// Name is the preset's name in the routing setting ("caddy").
	Name() string

	// Group returns the group a label key belongs to ("caddy_1.import" → "caddy_1"),
	// or ok=false when the key is not one of this proxy's route labels.
	Group(key string) (group string, ok bool)

	// IsHost reports whether key is a group's head — the label whose value is the host.
	IsHost(key string) bool

	// Index is the number a group key carries ("caddy_3" → 3), or -1.
	Index(group string) int

	// Render spells one route as a full label group at index: the host label, the
	// domain's labels, then the proxy directive for upstream.
	Render(index int, host string, up Upstream, domainLabels map[string]string) []Label

	// Clone respells a hand-written group onto another host at index, for a listing
	// that still routes itself (clone mode). Keys that belong to the domain rather
	// than the app — TLS, for Caddy — are dropped before the domain's own are added.
	Clone(index int, g Group, host string, domainLabels map[string]string) []Label
}

// Get returns the named preset.
func Get(name string) (Dialect, bool) {
	switch name {
	case "", Caddy.Name():
		return Caddy, true
	}
	return nil, false
}

// Names lists the presets this build carries, for validation and the settings UI.
func Names() []string { return []string{Caddy.Name()} }

// Groups collects d's route groups out of labels, in file order.
func Groups(d Dialect, labels []Label) []Group {
	byKey := map[string]*Group{}
	var order []string
	for _, l := range labels {
		if d.IsHost(l.Key) {
			byKey[l.Key] = &Group{Key: l.Key, Index: d.Index(l.Key), Host: l.Value}
			order = append(order, l.Key)
		}
	}
	for _, l := range labels {
		g, ok := d.Group(l.Key)
		if !ok || g == l.Key {
			continue
		}
		if grp := byKey[g]; grp != nil {
			grp.Subs = append(grp.Subs, Label{l.Key[len(g)+1:], l.Value})
		}
	}
	out := make([]Group, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	return out
}

// NextIndex is the first index free across every label set given: a generated
// group must never land on one either file already uses.
func NextIndex(d Dialect, sets ...[]Label) int {
	next := 0
	for _, labels := range sets {
		for _, l := range labels {
			if g, ok := d.Group(l.Key); ok {
				if n := d.Index(g); n >= next {
					next = n + 1
				}
			}
		}
	}
	return next
}

// withDomain assembles a group: the host label, then the domain's labels, then the
// route's own body. The domain wins on a key collision, and a domain label with an
// empty value deletes that key — which is how a domain says "nothing here" (an
// sslip.io domain dropping a TLS import) without Maison knowing what the key means.
func withDomain(key, host string, body []Label, domainLabels map[string]string) []Label {
	out := []Label{{key, host}}
	for _, k := range sortedKeys(domainLabels) {
		if v := domainLabels[k]; v != "" {
			out = append(out, Label{key + "." + k, v})
		}
	}
	for _, l := range body {
		if _, overridden := domainLabels[l.Key]; overridden {
			continue
		}
		out = append(out, Label{key + "." + l.Key, l.Value})
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
