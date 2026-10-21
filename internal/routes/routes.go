// Package routes writes what the deployment's routing setting needs into an app's
// docker-compose.override.yml.
//
// What is written depends on the renderer (internal/routing):
//
//   - labels: one proxy label group per route per domain, spelled by a dialect
//     (internal/routes/dialect). An app whose listing still routes itself with
//     hand-written labels and declares no routes has those groups copied onto the
//     deployment's further domains instead (clone mode), as Maison always did.
//   - ports: a `ports:` entry per route, `${APP_BIND}:${APP_PORT_<NAME>}:<port>`.
//   - external: nothing but the network attachment below.
//
// Every routed service of a labels or external deployment is also attached to the
// deployment's app network, so the proxy can reach it, unless the listing already
// put it there.
//
// Nothing here knows a proxy's vocabulary: label grammar is the dialect's.
//
// # Why the override, and how it stays safe
//
// The base compose is byte-compared against the store's on every update check
// (internal/installer/update.go), so anything written there would read as a
// permanent "update available". The override is the only legal target — but it is
// also the operator's file. So generation is exact rather than heuristic: what
// Maison writes is recorded in a manifest, and the next run deletes precisely that
// before writing the new set. Nothing else in the file is read or touched, and it is
// patched through its node tree, so comments and key order survive.
//
// Hosts stay templated (`outline-${APP_DOMAIN}`), so a bare `docker compose up -d`
// in the app's folder reproduces what Maison runs.
package routes

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/yundera/maison/internal/brand"
	"github.com/yundera/maison/internal/domains"
	"github.com/yundera/maison/internal/routes/dialect"
	"github.com/yundera/maison/internal/routing"
	"github.com/yundera/maison/internal/yamlnode"
)

// ManifestKey names the generated-key manifest inside the override's
// x-compose-app block — the same block that already carries Maison's other
// bookkeeping (store-ref).
//
// The name is deliberately brand-free. It is written into files that belong to
// the operator and outlive any one release, so it is the one identifier a rename
// of the product must not be able to reach — see internal/brand.
const ManifestKey = "generated-routes"

// AppNetKey is the key the deployment's app network is attached under in an
// override, when the listing did not attach the service itself.
const AppNetKey = "app-net"

// Manifest entry forms beside plain label keys.
const (
	portsPrefix    = "ports:"    // a ports: entry, by value
	networksPrefix = "networks:" // a service's network attachment, by key
	topNetworks    = ".networks" // the manifest's pseudo-service for top-level networks (no service name starts with a dot)
)

// Resolver expands a host's ${VAR} references the way `docker compose` will when
// it reads the label off the file — the app's .env overlaid on the environment
// Maison runs compose with (see envinject.Render).
//
// It exists because "is this host already routed?" cannot be answered on the
// written text. A compose may reach the same host through different spellings of
// the same variable: Maison's own stack writes `maison-${PUBLIC_IP_DASH}.nip.io`
// while a configured domain is `{name}-${APP_PUBLIC_IP_DASH}.nip.io`, and both are
// `maison-<ip>.nip.io` by the time Caddy sees them.
//
// A reference it cannot resolve is left as written, so two genuinely unknown
// hosts never collapse into each other.
type Resolver = routing.Resolver

// Plan is what to write for one app.
type Plan struct {
	Renderer string
	// Dialect spells labels (labels renderer). Nil turns label generation off.
	Dialect dialect.Dialect
	// Domains are the labels renderer's domains; [0] is the primary. Clone mode
	// copies an app's own groups from the primary onto the rest.
	Domains []domains.Domain
	// Entries are the app's routes under the setting (routing.Table). Empty means
	// the app declares no routes.
	Entries []routing.Entry
	// Network is the deployment's app network as it resolves (`pcs`), and NetworkRef
	// how it is written into the override (`${APP_NET:-pcs}`). An empty Network
	// attaches nothing.
	Network    string
	NetworkRef string
}

// Sync reconciles the generated routing in override against p, using base for the
// app's services and its own labels. It returns the new override, or nil when
// nothing is left in it and the file should be removed.
//
// resolve decides host and network identity (see Resolver); nil compares as
// written.
//
// It is a pure function over the two files' bytes: everything that decides the
// output is in its arguments, which is what lets the whole rule be tested without
// Docker, a store, or a filesystem.
func Sync(base, override []byte, p Plan, resolve Resolver) ([]byte, error) {
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

	dropped := dropGenerated(overRoot, p, resolve)
	manifest := map[string][]string{}
	switch p.Renderer {
	case routing.Labels:
		if p.Dialect != nil {
			if len(p.Entries) > 0 {
				renderLabels(baseRoot, overRoot, p, resolve, manifest)
			} else {
				cloneLabels(baseRoot, overRoot, p, resolve, manifest)
			}
		}
	case routing.Ports:
		renderPorts(baseRoot, overRoot, p, manifest)
	}
	if (p.Renderer == routing.Labels || p.Renderer == routing.External) && p.Network != "" {
		attachNetwork(baseRoot, overRoot, p, resolve, manifest)
	}

	// Nothing was generated before and nothing is now. Hand the override back
	// exactly as it came in: re-emitting it would rewrite the operator's file
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

// --- labels ---

// renderLabels writes one group per route per domain.
func renderLabels(baseRoot, overRoot *yaml.Node, p Plan, resolve Resolver, manifest map[string][]string) {
	baseSvcs := yamlnode.Get(baseRoot, "services")
	for _, svc := range entryServices(p.Entries) {
		if yamlnode.Get(baseSvcs, svc) == nil {
			continue // a route on a service the compose does not have
		}
		baseLabels := labelPairs(yamlnode.Get(yamlnode.Get(baseSvcs, svc), "labels"))
		overLabels := labelPairs(labelsNode(overRoot, svc))
		routed := routedHosts(p.Dialect, resolve, baseLabels, overLabels)
		next := dialect.NextIndex(p.Dialect, baseLabels, overLabels)

		var written []string
		for _, e := range p.Entries {
			if e.Service != svc || routed[resolve(e.Host)] {
				continue
			}
			routed[resolve(e.Host)] = true
			group := p.Dialect.Render(next, e.Host, dialect.Upstream{Port: e.Port, Scheme: e.Scheme, Insecure: e.Insecure}, e.Labels)
			next++
			written = append(written, writeGroup(overRoot, svc, group, fmt.Sprintf("%s: route %s on %s", brand.Name, e.Name, e.Target))...)
		}
		if len(written) > 0 {
			manifest[svc] = append(manifest[svc], written...)
		}
	}
}

// cloneLabels copies every group an app's own labels put on the primary domain onto
// each further domain. A route group can be an entire handle_path tree or carry
// transport settings, and it has to keep working on the second domain, so the whole
// group is copied — except what the dialect says belongs to the domain (TLS).
//
// Every group on every service is copied, not just the web UI's: Outline's Dex
// sidecar has its own `outline-auth-${APP_DOMAIN}` route, and OIDC breaks if it is
// not reachable at the same names as the app.
func cloneLabels(baseRoot, overRoot *yaml.Node, p Plan, resolve Resolver, manifest map[string][]string) {
	if len(p.Domains) < 2 {
		return
	}
	primary := p.Domains[0]
	baseSvcs := yamlnode.Get(baseRoot, "services")
	for _, svc := range yamlnode.Keys(baseSvcs) {
		baseLabels := labelPairs(yamlnode.Get(yamlnode.Get(baseSvcs, svc), "labels"))
		groups := dialect.Groups(p.Dialect, baseLabels)
		if len(groups) == 0 {
			continue
		}
		overLabels := labelPairs(labelsNode(overRoot, svc))
		routed := routedHosts(p.Dialect, resolve, baseLabels, overLabels)
		next := dialect.NextIndex(p.Dialect, baseLabels, overLabels)

		var written []string
		for _, g := range groups {
			name, ok := primary.NameOf(primaryHost(g.Host))
			if !ok {
				continue // not a route on the primary domain — nothing to republish
			}
			for _, d := range p.Domains[1:] {
				host := d.Host(name)
				if routed[resolve(host)] {
					continue
				}
				routed[resolve(host)] = true
				group := p.Dialect.Clone(next, g, host, d.Labels)
				next++
				written = append(written, writeGroup(overRoot, svc, group, fmt.Sprintf("%s: %s — generated from %s", brand.Name, d.Name, g.Key))...)
			}
		}
		if len(written) > 0 {
			manifest[svc] = append(manifest[svc], written...)
		}
	}
}

// primaryHost reads the store's other spelling of the primary domain as the one
// the default primary template uses: `${domain}` is what older listings' webui-host
// was written with, and some labels copied it.
func primaryHost(host string) string {
	return strings.ReplaceAll(host, "${domain}", "${APP_DOMAIN}")
}

// routedHosts is every host either file already routes, resolved. A host that is
// already routed is left to whoever declared it: that is what lets the generator
// run against a listing that still carries its own labels without publishing the
// app twice.
func routedHosts(d dialect.Dialect, resolve Resolver, sets ...[]dialect.Label) map[string]bool {
	out := map[string]bool{}
	for _, labels := range sets {
		for _, l := range labels {
			if d.IsHost(l.Key) {
				out[resolve(l.Value)] = true
			}
		}
	}
	return out
}

// writeGroup writes a label group onto svc in the override and returns its keys.
func writeGroup(overRoot *yaml.Node, svc string, group []dialect.Label, note string) []string {
	labels := ensureLabels(overRoot, svc)
	keys := make([]string, 0, len(group))
	for i, l := range group {
		setLabel(labels, l.Key, l.Value)
		if i == 0 {
			comment(labels, l.Key, note)
		}
		keys = append(keys, l.Key)
	}
	return keys
}

// --- ports ---

// renderPorts publishes each route's port. The entry stays templated — the host
// port and the bind address are the app's .env values, so a hand-run compose
// publishes the same port Maison picked.
//
// A port the listing already publishes itself is left alone (routing.Table then
// reports that host port instead of allocating one).
func renderPorts(baseRoot, overRoot *yaml.Node, p Plan, manifest map[string][]string) {
	for _, e := range p.Entries {
		if e.HostPort == 0 || publishedPort(baseRoot, e.Service, e.Port) > 0 {
			continue
		}
		if yamlnode.Get(yamlnode.Get(baseRoot, "services"), e.Service) == nil {
			continue
		}
		entry := PortEntry(e.Name, e.Port)
		svcNode := yamlnode.EnsureMap(yamlnode.EnsureMap(overRoot, "services"), e.Service)
		seq := yamlnode.Get(svcNode, "ports")
		if seq == nil {
			seq = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			yamlnode.Set(svcNode, "ports", seq)
		}
		if seqIndex(seq, entry) < 0 {
			item := yamlnode.Scalar(entry)
			item.Style = yaml.DoubleQuotedStyle
			item.HeadComment = fmt.Sprintf("%s: route %s", brand.Name, e.Name)
			seq.Content = append(seq.Content, item)
		}
		manifest[e.Service] = append(manifest[e.Service], portsPrefix+entry)
	}
}

// PortEntry is the ports: entry a route is published with.
func PortEntry(name string, port int) string {
	return fmt.Sprintf("${APP_BIND}:${%s}:%d", routing.PortKey(name), port)
}

// PublishedPort is the host port svc publishes port on in a compose file, or 0.
// The ports renderer uses it to leave alone a port a listing publishes itself.
func PublishedPort(base []byte, svc string, port int) int {
	root, err := yamlnode.Root(base)
	if err != nil {
		return 0
	}
	return publishedPort(root, svc, port)
}

func publishedPort(root *yaml.Node, svc string, port int) int {
	seq := yamlnode.Get(yamlnode.Get(yamlnode.Get(root, "services"), svc), "ports")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return 0
	}
	for _, item := range seq.Content {
		switch item.Kind {
		case yaml.ScalarNode:
			// [ip:]published:target[/proto]
			v, _, _ := strings.Cut(item.Value, "/")
			parts := strings.Split(v, ":")
			if len(parts) < 2 {
				continue
			}
			target, err1 := strconv.Atoi(parts[len(parts)-1])
			pub, err2 := strconv.Atoi(parts[len(parts)-2])
			if err1 == nil && err2 == nil && target == port {
				return pub
			}
		case yaml.MappingNode:
			t := yamlnode.Get(item, "target")
			pb := yamlnode.Get(item, "published")
			if t == nil || pb == nil {
				continue
			}
			target, err1 := strconv.Atoi(t.Value)
			pub, err2 := strconv.Atoi(pb.Value)
			if err1 == nil && err2 == nil && target == port {
				return pub
			}
		}
	}
	return 0
}

// --- the app network ---

// attachNetwork puts every routed service on the deployment's app network, so the
// proxy (or the operator's router) can reach it. A service the listing already put
// there — or one with its own network_mode — is left alone.
//
// A service with no networks of its own is on the project's default network, and
// naming any network drops that: so the attachment then names `default` as well,
// or the service would lose its siblings.
func attachNetwork(baseRoot, overRoot *yaml.Node, p Plan, resolve Resolver, manifest map[string][]string) {
	attached := false
	for _, svc := range entryServices(p.Entries) {
		baseSvc := yamlnode.Get(yamlnode.Get(baseRoot, "services"), svc)
		if baseSvc == nil || yamlnode.Get(baseSvc, "network_mode") != nil {
			continue
		}
		overSvc := yamlnode.Get(yamlnode.Get(overRoot, "services"), svc)
		baseKeys := networkKeys(yamlnode.Get(baseSvc, "networks"))
		overKeys := networkKeys(yamlnode.Get(overSvc, "networks"))
		already := false
		for _, k := range append(append([]string{}, baseKeys...), overKeys...) {
			if networkName(baseRoot, overRoot, k, resolve) == p.Network {
				already = true
				break
			}
		}
		if already {
			continue
		}
		add := []string{AppNetKey}
		if len(baseKeys) == 0 && len(overKeys) == 0 {
			add = append([]string{"default"}, add...)
		}
		svcNode := yamlnode.EnsureMap(yamlnode.EnsureMap(overRoot, "services"), svc)
		nets := yamlnode.Get(svcNode, "networks")
		if nets == nil {
			nets = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			yamlnode.Set(svcNode, "networks", nets)
		}
		for _, k := range add {
			addNetworkRef(nets, k)
			manifest[svc] = append(manifest[svc], networksPrefix+k)
		}
		attached = true
	}
	if !attached {
		return
	}
	top := yamlnode.EnsureMap(overRoot, "networks")
	if yamlnode.Get(top, AppNetKey) == nil {
		def := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		yamlnode.Set(def, "name", yamlnode.Scalar(p.NetworkRef))
		yamlnode.Set(def, "external", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
		yamlnode.Set(top, AppNetKey, def)
		comment(top, AppNetKey, brand.Name+": the deployment's app network, for routing")
		manifest[topNetworks] = []string{AppNetKey}
	}
}

func addNetworkRef(nets *yaml.Node, key string) {
	switch nets.Kind {
	case yaml.SequenceNode:
		if seqIndex(nets, key) < 0 {
			nets.Content = append(nets.Content, yamlnode.Scalar(key))
		}
	case yaml.MappingNode:
		if yamlnode.Get(nets, key) == nil {
			yamlnode.Set(nets, key, &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Style: yaml.FlowStyle})
		}
	}
}

// networkKeys lists the networks a service names, in either compose form.
func networkKeys(n *yaml.Node) []string {
	if n == nil {
		return nil
	}
	switch n.Kind {
	case yaml.SequenceNode:
		var out []string
		for _, it := range n.Content {
			out = append(out, it.Value)
		}
		return out
	case yaml.MappingNode:
		return yamlnode.Keys(n)
	}
	return nil
}

// networkName is the Docker network a network key stands for: its `name:` when the
// definition gives one, the key itself for an external network without one. A
// project-local network gets a project-prefixed name, which is never the
// deployment's app network, so it reads as "".
func networkName(baseRoot, overRoot *yaml.Node, key string, resolve Resolver) string {
	for _, root := range []*yaml.Node{overRoot, baseRoot} {
		def := yamlnode.Get(yamlnode.Get(root, "networks"), key)
		if def == nil {
			continue
		}
		if n := yamlnode.Get(def, "name"); n != nil {
			return resolve(n.Value)
		}
		if ext := yamlnode.Get(def, "external"); ext != nil && ext.Value == "true" {
			return key
		}
	}
	return ""
}

// --- the manifest ---

// dropGenerated removes the previous run's output: what the manifest names, plus —
// belt and braces, in case the manifest was edited away through the raw YAML view —
// any label group in the override whose host sits on one of the further domains.
// It reports whether it removed anything, so a run with nothing to do can leave the
// file alone entirely.
func dropGenerated(overRoot *yaml.Node, p Plan, resolve Resolver) bool {
	dropped := false
	if prev := readManifest(overRoot); len(prev) > 0 {
		dropped = true
		for svc, keys := range prev {
			if svc == topNetworks {
				for _, k := range keys {
					yamlnode.Delete(yamlnode.Get(overRoot, "networks"), k)
				}
				continue
			}
			svcNode := yamlnode.Get(yamlnode.Get(overRoot, "services"), svc)
			for _, k := range keys {
				switch {
				case strings.HasPrefix(k, portsPrefix):
					removeSeqValue(yamlnode.Get(svcNode, "ports"), strings.TrimPrefix(k, portsPrefix))
				case strings.HasPrefix(k, networksPrefix):
					key := strings.TrimPrefix(k, networksPrefix)
					nets := yamlnode.Get(svcNode, "networks")
					if nets != nil && nets.Kind == yaml.SequenceNode {
						removeSeqValue(nets, key)
					} else {
						yamlnode.Delete(nets, key)
					}
				default:
					delLabel(labelsNode(overRoot, svc), k)
				}
			}
		}
		yamlnode.Delete(yamlnode.Get(overRoot, "x-compose-app"), ManifestKey)
	}

	if p.Dialect == nil || len(p.Domains) < 2 {
		return dropped
	}
	svcs := yamlnode.Get(overRoot, "services")
	for _, name := range yamlnode.Keys(svcs) {
		labels := labelsNode(overRoot, name)
		for _, g := range dialect.Groups(p.Dialect, labelPairs(labels)) {
			if !onFurtherDomain(g.Host, p.Domains[1:], resolve) {
				continue
			}
			dropped = true
			delLabel(labels, g.Key)
			for _, s := range g.Subs {
				delLabel(labels, g.Key+"."+s.Key)
			}
		}
	}
	return dropped
}

// onFurtherDomain reports whether host sits on one of doms, on the resolved host so
// that a route written with a different spelling of the same variable is still
// recognised — otherwise a generated route from an older run would never be
// cleaned up, which is the mirror image of the duplicate Resolver exists to prevent.
func onFurtherDomain(host string, doms []domains.Domain, resolve Resolver) bool {
	host = resolve(host)
	for _, d := range doms {
		// The resolved suffix is checked too: a suffix that resolves to nothing
		// would otherwise match every host and take the whole override with it.
		suf := resolve(d.Suffix())
		if strings.Trim(suf, "-.") != "" && strings.HasSuffix(host, suf) {
			return true
		}
	}
	return false
}

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
	comment(xca, ManifestKey, "generated by Maison — what it owns in this file, and will rewrite")
}

// --- node helpers (compose accepts labels in two shapes) ---

func entryServices(entries []routing.Entry) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		if !seen[e.Service] {
			seen[e.Service] = true
			out = append(out, e.Service)
		}
	}
	return out
}

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
func labelPairs(n *yaml.Node) []dialect.Label {
	if n == nil {
		return nil
	}
	var out []dialect.Label
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			out = append(out, dialect.Label{Key: n.Content[i].Value, Value: n.Content[i+1].Value})
		}
	case yaml.SequenceNode:
		for _, item := range n.Content {
			k, v, _ := strings.Cut(item.Value, "=")
			out = append(out, dialect.Label{Key: k, Value: v})
		}
	}
	return out
}

// setLabel writes one label in whichever shape the node already uses.
func setLabel(labels *yaml.Node, key, val string) {
	switch {
	case labels == nil:
	case labels.Kind == yaml.SequenceNode:
		item := yamlnode.Scalar(key + "=" + val)
		for i, cur := range labels.Content {
			if k, _, _ := strings.Cut(cur.Value, "="); k == key {
				labels.Content[i] = item
				return
			}
		}
		labels.Content = append(labels.Content, item)
	default:
		yamlnode.Set(labels, key, yamlnode.Scalar(val))
	}
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

func seqIndex(seq *yaml.Node, value string) int {
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return -1
	}
	for i, it := range seq.Content {
		if it.Value == value {
			return i
		}
	}
	return -1
}

func removeSeqValue(seq *yaml.Node, value string) {
	if i := seqIndex(seq, value); i >= 0 {
		seq.Content = append(seq.Content[:i], seq.Content[i+1:]...)
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

// prune drops the containers Maison emptied out, so removing the last route
// leaves an override no larger than it found it (and possibly no file at all).
func prune(overRoot *yaml.Node) {
	svcs := yamlnode.Get(overRoot, "services")
	for _, name := range yamlnode.Keys(svcs) {
		svc := yamlnode.Get(svcs, name)
		for _, k := range []string{"labels", "ports", "networks"} {
			if n := yamlnode.Get(svc, k); n != nil && len(n.Content) == 0 && n.Kind != yaml.ScalarNode {
				yamlnode.Delete(svc, k)
			}
		}
		if len(svc.Content) == 0 {
			yamlnode.Delete(svcs, name)
		}
	}
	if svcs != nil && len(svcs.Content) == 0 {
		yamlnode.Delete(overRoot, "services")
	}
	if nets := yamlnode.Get(overRoot, "networks"); nets != nil && len(nets.Content) == 0 {
		yamlnode.Delete(overRoot, "networks")
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
