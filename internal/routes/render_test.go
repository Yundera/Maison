package routes

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/yundera/maison/internal/composefile"
	"github.com/yundera/maison/internal/domains"
	"github.com/yundera/maison/internal/routes/dialect"
	"github.com/yundera/maison/internal/routing"
)

// outlineRoutes is the worked example's listing: no labels, two routes, both
// services already on the app network.
const outlineRoutes = `name: outline
services:
  outline:
    image: outlinewiki/outline:1.6.1
    expose: ["80"]
    networks: [outline-internal, pcs]
  outline-dex:
    image: ghcr.io/dexidp/dex:v2.45.1
    expose: ["5556"]
    networks: [outline-internal, pcs]
networks:
  outline-internal:
  pcs:
    name: ${APP_NET:-pcs}
    external: true
x-compose-app:
  routes:
    - service: outline
      name: outline
      upstream-port: 80
    - service: outline-dex
      name: outline-auth
      upstream-port: 5556
`

// pcsRouting is the routing setting a Yundera PCS states.
var pcsRouting = routing.Setting{
	Renderer: routing.Labels,
	Dialect:  "caddy",
	Domains: []domains.Domain{
		{Name: "gateway", Domain: "{name}-${APP_DOMAIN}", Labels: map[string]string{"import": "gateway_tls"}},
		{Name: "nip", Domain: "{name}-${APP_PUBLIC_IP_DASH}.nip.io", Labels: map[string]string{"import": "gateway_tls"}},
		{Name: "sslip", Domain: "{name}-${APP_PUBLIC_IP_DASH}.sslip.io"},
	},
}

var aliceVars = map[string]string{"APP_DOMAIN": "alice.nsl.sh", "APP_PUBLIC_IP_DASH": "203-0-113-10"}

func outlineTable(t *testing.T, s routing.Setting, port routing.PortFor) []routing.Entry {
	t.Helper()
	f, err := composefile.Parse([]byte(outlineRoutes))
	if err != nil {
		t.Fatal(err)
	}
	a, err := f.App()
	if err != nil {
		t.Fatal(err)
	}
	rts := routing.Routes(a, "outline", []string{"outline", "outline-dex"})
	return routing.Table(rts, s, routing.EnvResolver(aliceVars), port)
}

func planFor(s routing.Setting, entries []routing.Entry) Plan {
	d, _ := dialect.Get(s.Dialect)
	return Plan{Renderer: s.Renderer, Dialect: d, Domains: s.Domains, Entries: entries,
		Network: "${APP_NET:-pcs}", NetworkRef: "${APP_NET:-pcs}"}
}

func labelsOf(t *testing.T, override, svc string) map[string]string {
	t.Helper()
	var doc struct {
		Services map[string]struct {
			Labels map[string]string `yaml:"labels"`
			Ports  []string          `yaml:"ports"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(override), &doc); err != nil {
		t.Fatalf("override does not parse: %v\n%s", err, override)
	}
	return doc.Services[svc].Labels
}

// The worked example §3: 2 routes × 3 domains = the 18 lines Outline's listing
// carries by hand today.
func TestRenderLabelsReproducesTheWorkedExample(t *testing.T) {
	entries := outlineTable(t, pcsRouting, nil)
	out, err := Sync([]byte(outlineRoutes), nil, planFor(pcsRouting, entries), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)

	want := map[string]map[string]string{
		"outline": {
			"caddy_0": "outline-${APP_DOMAIN}", "caddy_0.import": "gateway_tls", "caddy_0.reverse_proxy": "{{upstreams 80}}",
			"caddy_1": "outline-${APP_PUBLIC_IP_DASH}.nip.io", "caddy_1.import": "gateway_tls", "caddy_1.reverse_proxy": "{{upstreams 80}}",
			"caddy_2": "outline-${APP_PUBLIC_IP_DASH}.sslip.io", "caddy_2.reverse_proxy": "{{upstreams 80}}",
		},
		"outline-dex": {
			"caddy_0": "outline-auth-${APP_DOMAIN}", "caddy_0.import": "gateway_tls", "caddy_0.reverse_proxy": "{{upstreams 5556}}",
			"caddy_1": "outline-auth-${APP_PUBLIC_IP_DASH}.nip.io", "caddy_1.import": "gateway_tls", "caddy_1.reverse_proxy": "{{upstreams 5556}}",
			"caddy_2": "outline-auth-${APP_PUBLIC_IP_DASH}.sslip.io", "caddy_2.reverse_proxy": "{{upstreams 5556}}",
		},
	}
	for svc, labels := range want {
		have := labelsOf(t, got, svc)
		if len(have) != len(labels) {
			t.Errorf("%s: %d labels, want %d:\n%s", svc, len(have), len(labels), got)
		}
		for k, v := range labels {
			if have[k] != v {
				t.Errorf("%s: %s = %q, want %q", svc, k, have[k], v)
			}
		}
	}
	for _, want := range []string{"route outline on gateway", "route outline-auth on sslip", ManifestKey} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// Both services are already on the app network: nothing to attach.
	if strings.Contains(got, AppNetKey) {
		t.Errorf("attached a network the listing already joins:\n%s", got)
	}

	again, err := Sync([]byte(outlineRoutes), out, planFor(pcsRouting, entries), nil)
	if err != nil || string(again) != got {
		t.Errorf("second sync changed the file (err %v):\n%s", err, again)
	}
}

// The table resolves every route's public address — what APP_URL and the click
// URL are made of.
func TestTableResolvesTheAddresses(t *testing.T) {
	vars := routing.Vars(outlineTable(t, pcsRouting, nil), pcsRouting)
	want := map[string]string{
		"APP_URL":                 "https://outline-alice.nsl.sh",
		"APP_HOST":                "outline-alice.nsl.sh",
		"ROUTE_OUTLINE_AUTH_URL":  "https://outline-auth-alice.nsl.sh",
		"ROUTE_OUTLINE_AUTH_HOST": "outline-auth-alice.nsl.sh",
	}
	if len(vars) != len(want) {
		t.Errorf("vars = %v, want %v", vars, want)
	}
	for k, v := range want {
		if vars[k] != v {
			t.Errorf("%s = %q, want %q", k, vars[k], v)
		}
	}
}

// The worked example §4: the same listing on a home box.
func TestRenderPortsPublishesEachRoute(t *testing.T) {
	home := routing.Effective(&routing.Setting{Renderer: routing.Ports, URLHost: "192.168.1.10"}, nil, "")
	picker := &PortPicker{Low: home.PortRange[0], High: home.PortRange[1]}
	entries := outlineTable(t, home, func(r routing.Route) int { return picker.Pick(0, r.Port) })

	out, err := Sync([]byte(outlineRoutes), nil, planFor(home, entries), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{
		`"${APP_BIND}:${APP_PORT_OUTLINE}:80"`,
		`"${APP_BIND}:${APP_PORT_OUTLINE_AUTH}:5556"`,
		"ports:${APP_BIND}:${APP_PORT_OUTLINE}:80",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "caddy") || strings.Contains(got, AppNetKey) {
		t.Errorf("ports mode wrote labels or a network:\n%s", got)
	}

	vars := routing.Vars(entries, home)
	// Port 80 is below 1024, so it goes to the range; 5556 is free and kept.
	want := map[string]string{
		"APP_BIND": "0.0.0.0", "APP_PORT_OUTLINE": "20000", "APP_PORT_OUTLINE_AUTH": "5556",
		"APP_URL": "http://192.168.1.10:20000", "APP_HOST": "192.168.1.10:20000",
		"ROUTE_OUTLINE_AUTH_URL": "http://192.168.1.10:5556", "ROUTE_OUTLINE_AUTH_HOST": "192.168.1.10:5556",
	}
	for k, v := range want {
		if vars[k] != v {
			t.Errorf("%s = %q, want %q (all: %v)", k, vars[k], v, vars)
		}
	}

	// Switching the box to labels removes every port Maison published.
	switched, err := Sync([]byte(outlineRoutes), out, planFor(pcsRouting, outlineTable(t, pcsRouting, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(switched), "APP_PORT_") {
		t.Errorf("ports survived the switch to labels:\n%s", switched)
	}
	if !strings.Contains(string(switched), "caddy_0: outline-${APP_DOMAIN}") {
		t.Errorf("no labels after the switch:\n%s", switched)
	}
}

// A port the listing publishes itself is left to it.
func TestRenderPortsSkipsAPortTheListingPublishes(t *testing.T) {
	base := `services:
  jellyfin:
    image: jellyfin/jellyfin:10
    ports: ["8096:8096"]
x-compose-app:
  routes:
    - upstream-port: 8096
`
	if hp := PublishedPort([]byte(base), "jellyfin", 8096); hp != 8096 {
		t.Fatalf("PublishedPort = %d, want 8096", hp)
	}
	entries := []routing.Entry{{Route: routing.Route{Name: "jellyfin", Service: "jellyfin", Port: 8096, Scheme: "http", Path: "/"}, Target: "port", Primary: true, HostPort: 8096}}
	out, err := Sync([]byte(base), nil, Plan{Renderer: routing.Ports, Entries: entries}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		t.Errorf("republished a port the listing publishes:\n%s", out)
	}
}

// A routed service the listing leaves off the app network is attached to it — with
// `default` beside it, or it would lose its siblings on the project network.
func TestAttachNetwork(t *testing.T) {
	base := `services:
  web:
    image: web:1
    expose: ["8080"]
x-compose-app:
  routes:
    - upstream-port: 8080
`
	entries := []routing.Entry{{Route: routing.Route{Name: "web", Service: "web", Port: 8080, Scheme: "http", Path: "/"}, Target: "external", Primary: true}}
	p := Plan{Renderer: routing.External, Entries: entries, Network: "pcs", NetworkRef: "${APP_NET:-pcs}"}
	out, err := Sync([]byte(base), nil, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	var doc struct {
		Services map[string]struct {
			Networks map[string]any `yaml:"networks"`
		} `yaml:"services"`
		Networks map[string]struct {
			Name     string `yaml:"name"`
			External bool   `yaml:"external"`
		} `yaml:"networks"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("%v\n%s", err, got)
	}
	nets := doc.Services["web"].Networks
	if _, ok := nets["default"]; !ok {
		t.Errorf("default network not kept:\n%s", got)
	}
	if _, ok := nets[AppNetKey]; !ok {
		t.Errorf("app network not attached:\n%s", got)
	}
	if n := doc.Networks[AppNetKey]; n.Name != "${APP_NET:-pcs}" || !n.External {
		t.Errorf("app network definition = %+v:\n%s", n, got)
	}

	// Removing the route takes the attachment with it and leaves no file.
	gone, err := Sync([]byte(base), out, Plan{Renderer: routing.External, Network: "pcs"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if gone != nil {
		t.Errorf("attachment outlived its route:\n%s", gone)
	}
}

// An HTTPS upstream gets the TLS upstream, pinned to HTTP/1.1, and verification
// switched off only when asked — Crafty's and Hubs' hand-written labels.
func TestCaddyRendersAnHTTPSUpstream(t *testing.T) {
	got := map[string]string{}
	for _, l := range dialect.Caddy.Render(0, "crafty-${APP_DOMAIN}", dialect.Upstream{Port: 8443, Scheme: "https", Insecure: true}, map[string]string{"import": "gateway_tls"}) {
		got[l.Key] = l.Value
	}
	want := map[string]string{
		"caddy_0": "crafty-${APP_DOMAIN}", "caddy_0.import": "gateway_tls",
		"caddy_0.reverse_proxy":                                    "{{upstreams https 8443}}",
		"caddy_0.reverse_proxy.transport":                          "http",
		"caddy_0.reverse_proxy.transport.versions":                 "1.1",
		"caddy_0.reverse_proxy.transport.tls_insecure_skip_verify": "",
	}
	if len(got) != len(want) {
		t.Errorf("got %v", got)
	}
	for k, v := range want {
		if gv, ok := got[k]; !ok || gv != v {
			t.Errorf("%s = %q, want %q", k, gv, v)
		}
	}
}

// A domain's label wins on a collision, and an empty one deletes the key.
func TestDomainLabelsWinAndEmptyDeletes(t *testing.T) {
	got := map[string]string{}
	for _, l := range dialect.Caddy.Render(2, "app.lan", dialect.Upstream{Port: 80}, map[string]string{
		"reverse_proxy": "{{upstreams 81}}", "header": ">Strict-Transport-Security max-age=15552000", "import": "",
	}) {
		got[l.Key] = l.Value
	}
	if got["caddy_2.reverse_proxy"] != "{{upstreams 81}}" {
		t.Errorf("domain label lost the collision: %v", got)
	}
	if _, ok := got["caddy_2.import"]; ok {
		t.Errorf("an empty domain label did not delete its key: %v", got)
	}
	if got["caddy_2.header"] == "" {
		t.Errorf("HSTS on the domain was dropped: %v", got)
	}
}
