package dialect

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Caddy is the caddy-docker-proxy grammar: a route is a `caddy_N` label naming the
// host, and `caddy_N.<directive>` labels under it.
var Caddy Dialect = caddy{}

type caddy struct{}

var (
	caddyHost  = regexp.MustCompile(`^caddy(_\d+)?$`)
	caddyIndex = regexp.MustCompile(`^caddy_(\d+)$`)
)

func (caddy) Name() string { return "caddy" }

func (caddy) Group(key string) (string, bool) {
	head, _, _ := strings.Cut(key, ".")
	if !caddyHost.MatchString(head) {
		return "", false
	}
	return head, true
}

func (caddy) IsHost(key string) bool { return caddyHost.MatchString(key) }

func (caddy) Index(group string) int {
	m := caddyIndex.FindStringSubmatch(group)
	if m == nil {
		return -1
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return -1
	}
	return n
}

// Render: `reverse_proxy {{upstreams N}}`. A container serving HTTPS itself gets
// `{{upstreams https N}}`, pinned to HTTP/1.1 — over TLS Caddy would otherwise
// negotiate HTTP/2 with the container, and websockets do not pass over that (Crafty)
// — and, when its certificate is self-signed, verification switched off.
func (caddy) Render(index int, host string, up Upstream, domainLabels map[string]string) []Label {
	key := fmt.Sprintf("caddy_%d", index)
	var body []Label
	if up.Scheme == "https" {
		body = append(body,
			Label{"reverse_proxy", fmt.Sprintf("{{upstreams https %d}}", up.Port)},
			Label{"reverse_proxy.transport", "http"},
			Label{"reverse_proxy.transport.versions", "1.1"},
		)
		if up.Insecure {
			body = append(body, Label{"reverse_proxy.transport.tls_insecure_skip_verify", ""})
		}
	} else {
		body = append(body, Label{"reverse_proxy", fmt.Sprintf("{{upstreams %d}}", up.Port)})
	}
	return withDomain(key, host, body, domainLabels)
}

// Clone copies the whole group — a hand-written route may be an entire handle_path
// tree — except its TLS keys, which are the domain's to state: the gateway and ip.<zone>
// hosts carry the deployment's own CA, while sslip.io deliberately carries nothing
// and falls through to Let's Encrypt. That absence is something the domain means, so
// the strip cannot depend on the domain having labels of its own.
func (caddy) Clone(index int, g Group, host string, domainLabels map[string]string) []Label {
	key := fmt.Sprintf("caddy_%d", index)
	var body []Label
	for _, s := range g.Subs {
		if caddyTLS(s.Key) {
			continue
		}
		body = append(body, s)
	}
	return withDomain(key, host, body, domainLabels)
}

func caddyTLS(sub string) bool {
	return sub == "import" || sub == "tls" || strings.HasPrefix(sub, "tls.") || strings.HasPrefix(sub, "tls_")
}
