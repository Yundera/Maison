// Package routes publishes an app on the deployment's domains (see
// internal/domains) by generating the reverse-proxy labels for them.
//
// Maison always speaks Compose. It does not always sit behind Caddy, so nothing
// here knows Caddy's vocabulary: the label grammar is data, supplied by a Dialect
// the deployment configures (see dialect.go), and Caddy is only its default value.
//
// # Two sources, one mechanism
//
// An app states its web endpoints without naming a domain, a proxy or a TLS
// setting — a service, a name, a port (xcomposeapp.Route). For each configured
// domain, the dialect renders that into labels, which are written into the app's
// docker-compose.override.yml:
//
//	caddy_1: outline-${APP_PUBLIC_IP_DASH}.sslip.io
//	caddy_1.reverse_proxy: "{{upstreams 80}}"
//
// An app whose routing is genuinely proxy-specific — a handle_path tree, a
// forward_auth block — cannot be said that way, and is not asked to be. It writes
// its own labels, and those are *cloned* onto each further domain verbatim. Such
// an app is bound to its proxy and its own file says so; Maison is not.
//
// Both sources feed one reconciliation, sharing one record of which hosts are
// already routed, so an app can declare some routes and hand-write others.
//
// # What belongs to the domain
//
// The dialect names the keys a domain owns outright (Group.DomainKeys), and those
// are stripped from a clone whether or not the domain supplies its own. That is
// what keeps TLS with the domain rather than the app: the gateway and nip.io hosts
// are served with the deployment's own CA, while sslip.io deliberately carries
// nothing and falls through to Let's Encrypt — an absence the domain means, which
// is why the strip cannot be conditional on the domain having something to say.
// The app never states any of this, and Maison never learns what `import` means.
//
// Every other key follows the more specific source: an app's own label beats the
// domain's, and the domain's beats a dialect default. So an app that still writes
// its own routes keeps its own opinions on the domains it is cloned onto, and only
// gives them up when it stops expressing one.
//
// The host is emitted still-templated, so it resolves through ordinary Compose
// interpolation — and a bare `docker compose up -d` in the app's folder reproduces
// what Maison runs.
//
// # Why the override, and how it stays safe
//
// The base compose is byte-compared against the store's on every update check
// (internal/installer/update.go), so a label written there would read as a
// permanent "update available". The override is the only legal target — but it is
// also the operator's file. So generation is exact rather than heuristic: the
// keys Maison writes are recorded in a manifest, and the next run deletes
// precisely those before writing the new set. Nothing else in the file is read or
// touched, and it is patched through its node tree, so comments and key order
// survive.
package routes

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/yundera/maison/internal/brand"
	"github.com/yundera/maison/internal/domains"
	"github.com/yundera/maison/internal/yamlnode"
)

// ManifestKey names the generated-key manifest inside the override's
// x-compose-app block — the same block that already carries Maison's other
// bookkeeping (store, store-app-id, tips).
//
// The name is deliberately brand-free. It is written into files that belong to
// the operator and outlive any one release, so it is the one identifier a rename
// of the product must not be able to reach — see internal/brand.
const ManifestKey = "generated-routes"

// PrimaryToken is the canonical placeholder for the deployment's primary domain
// — the one a store app templates its single route with, and the one the settings
// editor shows as the domain every additional domain is added *to*. It is exported
// so the UI states the mechanism in the compose file's own vocabulary rather than
// re-inventing a name for it.
const PrimaryToken = "${APP_DOMAIN}"

// primaryTokens are the placeholders a compose file uses for the deployment's
// primary domain. A caddy label that references one of them is a route Maison
// can republish; one that doesn't (a hardcoded host, an already-generated
// sslip.io route) is left alone.
var primaryTokens = []string{PrimaryToken, "${DOMAIN}", "${domain}"}

var (
	caddyKey = regexp.MustCompile(`^caddy(_\d+)?$`)
	caddyIdx = regexp.MustCompile(`^caddy_(\d+)`)
	varRef   = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
)

// Resolver expands a host's ${VAR} references the way `docker compose` will when
// it reads the label off the file — the app's .env overlaid on the environment
// Maison runs compose with (see envinject.Render).
//
// It exists because "is this host already routed?" cannot be answered on the
// written text. A compose may reach the same host through different spellings of
// the same variable: Maison's own stack writes `maison-${PUBLIC_IP_DASH}.nip.io`
// while a configured domain is `${APP_PUBLIC_IP_DASH}.nip.io`, and both are
// `maison-<ip>.nip.io` by the time Caddy sees them. Compared as text they look
// like two hosts, and the app ends up published twice on one name with two
// different TLS settings.
//
// A reference it cannot resolve is left as written, so two genuinely unknown
// hosts never collapse into each other.
type Resolver func(string) string

// Sync reconciles the generated route groups in override against doms, using
// base for the routes to clone. It returns the new override, or nil when nothing
// is left in it and the file should be removed.
//
// resolve decides host identity (see Resolver); nil compares hosts as written,
// which is right for a caller that has no environment to resolve against.
//
// It is a pure function over the two files' bytes: everything that decides the
// output is in its arguments, which is what lets the whole rule be tested without
// Docker, a store, or a filesystem.
func Sync(base, override []byte, doms []domains.Domain, resolve Resolver) ([]byte, error) {
	if resolve == nil {
		resolve = func(s string) string { return s }
	}
	baseRoot, err := yamlnode.Root(base)
	if err != nil {
		return nil, fmt.Errorf("docker-compose.yml: %w", err)
	}
	overRoot, err := yamlnode.Root(override)
	if err != nil {
		return nil, fmt.Errorf("docker-compose.override.yml: %w", err)
	}

	dropped := dropGenerated(overRoot, doms, resolve)
	manifest := generate(baseRoot, overRoot, doms, resolve)

	// Nothing was generated before and nothing is now — an app with no routes on the
	// primary domain, or a deployment with no additional domains. Hand the override
	// back exactly as it came in: re-emitting it would rewrite the operator's file
	// (quoting, indent, key style) to say the same thing, and a file Maison has no
	// business in is a file it must not write.
	if !dropped && len(manifest) == 0 {
		if len(override) == 0 {
			return nil, nil
		}
		return override, nil
	}

	writeManifest(overRoot, manifest)
	prune(overRoot)

	if len(overRoot.Content) == 0 {
		return nil, nil
	}
	return yamlnode.Encode(overRoot)
}

// Vars lists the interpolation variables the app's routes reference, once the
// additional domains are in play (${APP_DOMAIN}, ${APP_PUBLIC_IP_DASH}, …).
//
// These come from the deployment's environment, not from Maison's own base
// variables, so they are absent from an app's prefilled .env — which means a
// route resolves under Maison and to an empty host under a hand-run
// `docker compose up -d`. The caller seeds them into the .env so the app folder
// stands on its own; see internal/stackup.
func Vars(base []byte, doms []domains.Domain) []string {
	seen := map[string]bool{}
	add := func(s string) {
		for _, m := range varRef.FindAllStringSubmatch(s, -1) {
			seen[m[1]] = true
		}
	}
	if root, err := yamlnode.Root(base); err == nil {
		svcs := yamlnode.Get(root, "services")
		for _, name := range yamlnode.Keys(svcs) {
			for _, p := range labelPairs(yamlnode.Get(yamlnode.Get(svcs, name), "labels")) {
				if strings.HasPrefix(p.key, "caddy") {
					add(p.val)
				}
			}
		}
	}
	for _, d := range doms {
		add(d.Domain)
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- generation ---

type kv struct{ key, val string }

// group is one caddy route group: the `caddy_N` label that names the host, plus
// every `caddy_N.*` directive hanging off it, in file order.
type group struct {
	key  string // "caddy_0"
	host string // "outline-${APP_DOMAIN}"
	subs []kv   // {"import", "gateway_tls"}, {"reverse_proxy", "{{upstreams 80}}"}, …
}

// generate clones every base route group that sits on the primary domain onto
// each additional domain, writing the clones into the override. It returns the
// manifest of what it wrote, keyed by service.
func generate(baseRoot, overRoot *yaml.Node, doms []domains.Domain, resolve Resolver) map[string][]string {
	manifest := map[string][]string{}
	if len(doms) == 0 {
		return manifest
	}

	baseSvcs := yamlnode.Get(baseRoot, "services")
	for _, name := range yamlnode.Keys(baseSvcs) {
		baseLabels := labelPairs(yamlnode.Get(yamlnode.Get(baseSvcs, name), "labels"))
		groups := routeGroups(baseLabels)
		if len(groups) == 0 {
			continue
		}
		overLabels := labelPairs(labelsNode(overRoot, name))

		// A host that is already routed — because the store still ships the sslip.io
		// label itself, or because the operator wrote it by hand — is left to whoever
		// declared it. This is what lets Maison run against a store that has not
		// been trimmed yet without publishing every app twice.
		//
		// Keyed on the *resolved* host, because that is what decides whether two
		// labels are the same route: `${PUBLIC_IP_DASH}` and `${APP_PUBLIC_IP_DASH}`
		// are two names for one address (see Resolver).
		routed := map[string]bool{}
		for _, p := range append(append([]kv{}, baseLabels...), overLabels...) {
			if caddyKey.MatchString(p.key) {
				routed[resolve(p.val)] = true
			}
		}
		next := nextIndex(baseLabels, overLabels)

		var written []string
		for _, g := range groups {
			token := primaryToken(g.host)
			if token == "" {
				continue // not a route on the primary domain — nothing to republish
			}
			for _, d := range doms {
				host := strings.ReplaceAll(g.host, token, d.Domain)
				if routed[resolve(host)] {
					continue
				}
				routed[resolve(host)] = true

				key := fmt.Sprintf("caddy_%d", next)
				next++
				labels := ensureLabels(overRoot, name)

				setLabel(labels, key, host)
				comment(labels, key, fmt.Sprintf("%s: %s — generated from %s (Settings › Domains)", brand.Name, d.Name, g.key))
				written = append(written, key)

				// The domain's own directives lead, as they do in a hand-written store
				// route (`caddy_0.import` above `caddy_0.reverse_proxy`), then the app's.
				for _, dk := range sortedKeys(d.Labels) {
					written = append(written, setLabel(labels, key+"."+dk, d.Labels[dk]))
				}
				for _, s := range g.subs {
					if domainOwned(s.key) {
						continue // the domain decides its own TLS, not the app
					}
					written = append(written, setLabel(labels, key+"."+s.key, s.val))
				}
			}
		}
		if len(written) > 0 {
			manifest[name] = written
		}
	}
	return manifest
}

// dropGenerated removes the previous run's output: the keys named in the
// manifest, plus — belt and braces, in case the manifest was edited away through
// the raw YAML view — any override route whose host sits on a configured domain.
// It reports whether it removed anything, so a run with nothing to do can leave
// the file alone entirely.
func dropGenerated(overRoot *yaml.Node, doms []domains.Domain, resolve Resolver) bool {
	dropped := false
	if prev := readManifest(overRoot); len(prev) > 0 {
		dropped = true
		for svc, keys := range prev {
			labels := labelsNode(overRoot, svc)
			for _, k := range keys {
				delLabel(labels, k)
			}
		}
		yamlnode.Delete(yamlnode.Get(overRoot, "x-compose-app"), ManifestKey)
	}

	svcs := yamlnode.Get(overRoot, "services")
	for _, name := range yamlnode.Keys(svcs) {
		labels := labelsNode(overRoot, name)
		for _, g := range routeGroups(labelPairs(labels)) {
			if !onAnyDomain(g.host, doms, resolve) {
				continue
			}
			dropped = true
			delLabel(labels, g.key)
			for _, s := range g.subs {
				delLabel(labels, g.key+"."+s.key)
			}
		}
	}
	return dropped
}

// onAnyDomain reports whether host sits on one of the configured domains, on the
// resolved host so that a route written with a different spelling of the same
// variable is still recognised — otherwise a generated route from an older run
// would never be cleaned up, which is the mirror image of the duplicate Resolver
// exists to prevent.
func onAnyDomain(host string, doms []domains.Domain, resolve Resolver) bool {
	host = resolve(host)
	for _, d := range doms {
		// The resolved suffix is checked too: a domain that resolves to nothing
		// would otherwise match every host and take the whole override with it.
		if suffix := resolve(d.Domain); d.Domain != "" && suffix != "" && strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// routeGroups collects the caddy route groups out of a service's labels, in file
// order. A group is keyed by its label ("caddy", "caddy_0", …) rather than by its
// index, so a file mixing the bare and numbered forms stays unambiguous.
func routeGroups(pairs []kv) []group {
	byKey := map[string]*group{}
	var order []string
	for _, p := range pairs {
		if !caddyKey.MatchString(p.key) {
			continue
		}
		byKey[p.key] = &group{key: p.key, host: p.val}
		order = append(order, p.key)
	}
	for _, p := range pairs {
		head, sub, ok := strings.Cut(p.key, ".")
		if !ok {
			continue
		}
		if g := byKey[head]; g != nil {
			g.subs = append(g.subs, kv{sub, p.val})
		}
	}
	out := make([]group, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	return out
}

// nextIndex is the first caddy_N index free across both files — generated routes
// must never land on an index either file already uses.
func nextIndex(sets ...[]kv) int {
	next := 0
	for _, pairs := range sets {
		for _, p := range pairs {
			m := caddyIdx.FindStringSubmatch(p.key)
			if m == nil {
				continue
			}
			if n, err := strconv.Atoi(m[1]); err == nil && n >= next {
				next = n + 1
			}
		}
	}
	return next
}

// primaryToken returns the primary-domain placeholder a host is templated with,
// or "" when it is not on the primary domain.
func primaryToken(host string) string {
	for _, t := range primaryTokens {
		if strings.Contains(host, t) {
			return t
		}
	}
	return ""
}

// domainOwned reports whether a directive belongs to the domain rather than to
// the app, and so must not be carried over from the cloned group.
func domainOwned(sub string) bool {
	return sub == "import" || sub == "tls" ||
		strings.HasPrefix(sub, "tls.") || strings.HasPrefix(sub, "tls_")
}

// --- the manifest ---

func readManifest(overRoot *yaml.Node) map[string][]string {
	node := yamlnode.Get(yamlnode.Get(overRoot, "x-compose-app"), ManifestKey)
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	out := map[string][]string{}
	for _, svc := range yamlnode.Keys(node) {
		seq := yamlnode.Get(node, svc)
		if seq == nil || seq.Kind != yaml.SequenceNode {
			continue
		}
		var keys []string
		for _, item := range seq.Content {
			keys = append(keys, item.Value)
		}
		out[svc] = keys
	}
	return out
}

func writeManifest(overRoot *yaml.Node, manifest map[string][]string) {
	xca := yamlnode.Get(overRoot, "x-compose-app")
	if len(manifest) == 0 {
		yamlnode.Delete(xca, ManifestKey)
		if xca != nil && len(xca.Content) == 0 {
			yamlnode.Delete(overRoot, "x-compose-app")
		}
		return
	}

	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, svc := range sortedKeys(manifest) {
		yamlnode.Set(node, svc, yamlnode.StringSeq(manifest[svc]))
	}
	xca = yamlnode.EnsureMap(overRoot, "x-compose-app")
	yamlnode.Set(xca, ManifestKey, node)
	comment(xca, ManifestKey, "generated by Maison — the routes it owns in this file, and will rewrite")
}

// --- label access (compose accepts labels in two shapes) ---

func labelsNode(overRoot *yaml.Node, svc string) *yaml.Node {
	return yamlnode.Get(yamlnode.Get(yamlnode.Get(overRoot, "services"), svc), "labels")
}

func ensureLabels(overRoot *yaml.Node, svc string) *yaml.Node {
	svcNode := yamlnode.EnsureMap(yamlnode.EnsureMap(overRoot, "services"), svc)
	// Only promote to a mapping when there is nothing there: an operator's
	// list-form labels are theirs, and setLabel writes into that shape rather than
	// replacing it.
	if labels := yamlnode.Get(svcNode, "labels"); labels != nil {
		return labels
	}
	return yamlnode.EnsureMap(svcNode, "labels")
}

// labelPairs normalizes a labels node to ordered key/value pairs, from either
// compose form: the `k: v` mapping or the `- "k=v"` list.
func labelPairs(n *yaml.Node) []kv {
	if n == nil {
		return nil
	}
	var out []kv
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			out = append(out, kv{n.Content[i].Value, n.Content[i+1].Value})
		}
	case yaml.SequenceNode:
		for _, item := range n.Content {
			k, v, _ := strings.Cut(item.Value, "=")
			out = append(out, kv{k, v})
		}
	}
	return out
}

// setLabel writes one label in whichever shape the node already uses, and returns
// the key it wrote (so the caller can record it in the manifest).
func setLabel(labels *yaml.Node, key, val string) string {
	switch {
	case labels == nil:
	case labels.Kind == yaml.SequenceNode:
		item := yamlnode.Scalar(key + "=" + val)
		for i, cur := range labels.Content {
			if k, _, _ := strings.Cut(cur.Value, "="); k == key {
				labels.Content[i] = item
				return key
			}
		}
		labels.Content = append(labels.Content, item)
	default:
		yamlnode.Set(labels, key, yamlnode.Scalar(val))
	}
	return key
}

func delLabel(labels *yaml.Node, key string) {
	switch {
	case labels == nil:
	case labels.Kind == yaml.SequenceNode:
		for i, cur := range labels.Content {
			if k, _, _ := strings.Cut(cur.Value, "="); k == key {
				labels.Content = append(labels.Content[:i], labels.Content[i+1:]...)
				return
			}
		}
	default:
		yamlnode.Delete(labels, key)
	}
}

// comment attaches an explanatory line above a generated key, so the file says
// what it is to whoever opens it. Mapping form only — there is nowhere to hang a
// comment on a list item that survives a rewrite.
func comment(m *yaml.Node, key, text string) {
	if m == nil || m.Kind != yaml.MappingNode {
		return
	}
	if k := yamlnode.KeyNode(m, key); k != nil {
		k.HeadComment = text
	}
}

// prune drops the containers Maison emptied out, so removing the last domain
// leaves an override no larger than it found it (and possibly no file at all).
func prune(overRoot *yaml.Node) {
	svcs := yamlnode.Get(overRoot, "services")
	for _, name := range yamlnode.Keys(svcs) {
		svc := yamlnode.Get(svcs, name)
		if labels := yamlnode.Get(svc, "labels"); labels != nil && len(labels.Content) == 0 {
			yamlnode.Delete(svc, "labels")
		}
		if len(svc.Content) == 0 {
			yamlnode.Delete(svcs, name)
		}
	}
	if svcs != nil && len(svcs.Content) == 0 {
		yamlnode.Delete(overRoot, "services")
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
