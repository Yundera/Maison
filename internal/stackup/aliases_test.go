package stackup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/domains"
	"github.com/yundera/maison/internal/incident"
	"github.com/yundera/maison/internal/routing"
)

// A listing that routes itself (clone mode), as MetaWatchShared does: its own
// group on the primary domain, nothing declared under routes.
const clonedWatch = `name: metawatch
services:
  metawatch-app:
    image: example/meta-watch:1.3.14
    labels:
      caddy_0: metawatch-${APP_DOMAIN}
      caddy_0.import: gateway_tls
      caddy_0.reverse_proxy: "{{upstreams 3000}}"
x-compose-app:
  webui-host: metawatch-${domain}
`

// The operator's override on metastar: the custom name stated once, as intent.
const watchAliasOverride = `x-compose-app:
  aliases:
    - { name: watch, domains: [dot], primary: true }
`

// metastarBox is a labels box with metastar's domains: the gateway primary, the
// dot domain whose suffix covers every subdomain of the box, and sslip.
func metastarBox(t *testing.T) (config.Config, *[]incident.Report, *[]string) {
	t.Helper()
	cfg := testConfig(t)
	cfg.Routing = func() routing.Setting {
		return routing.Setting{Renderer: routing.Labels, Dialect: "caddy", Domains: []domains.Domain{
			{Name: "gateway", Domain: "{name}-${APP_DOMAIN}", Labels: map[string]string{"import": "gateway_tls"}},
			{Name: "dot", Domain: "{name}.${APP_DOMAIN}"},
			{Name: "sslip", Domain: "{name}-${APP_PUBLIC_IP_DASH}.sslip.io"},
		}}
	}
	cfg.AppEnv = func() map[string]string {
		return map[string]string{"APP_DOMAIN": "box.example.com", "APP_PUBLIC_IP_DASH": "10-0-0-1"}
	}
	var reports []incident.Report
	var resolved []string
	cfg.Report = func(r incident.Report) { reports = append(reports, r) }
	cfg.Resolve = func(id string) { resolved = append(resolved, id) }
	return cfg, &reports, &resolved
}

func installApp(t *testing.T, cfg config.Config, id, base, override string) string {
	t.Helper()
	dir := filepath.Join(cfg.AppsDir(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	if override != "" {
		if err := os.WriteFile(filepath.Join(dir, "docker-compose.override.yml"), []byte(override), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func syncApp(t *testing.T, cfg config.Config, id, dir string) (string, map[string]string) {
	t.Helper()
	_, vars := SyncRoutes(cfg, id, dir, []string{filepath.Join(dir, "docker-compose.yml")})
	raw, _ := os.ReadFile(filepath.Join(dir, "docker-compose.override.yml"))
	return string(raw), vars
}

// The case aliases exist for: a custom name on the dot domain is generated output,
// so it survives every up — where a hand-written group on that domain was swept as
// a stale route on the very next one.
func TestAliasOnTheDotDomainSurvivesEveryUp(t *testing.T) {
	cfg, reports, _ := metastarBox(t)
	dir := installApp(t, cfg, "metawatch", clonedWatch, watchAliasOverride)

	once, vars := syncApp(t, cfg, "metawatch", dir)
	for _, want := range []string{
		"caddy_1: metawatch.${APP_DOMAIN}",
		": watch.${APP_DOMAIN}",
		"alias watch for metawatch on dot",
		"aliases:", // the operator's intent stays where it was written
	} {
		if !strings.Contains(once, want) {
			t.Errorf("missing %q in:\n%s", want, once)
		}
	}
	if got := vars["APP_URL"]; got != "https://watch.box.example.com" {
		t.Errorf("APP_URL = %q, want the primary alias", got)
	}
	if got := vars["APP_URLS"]; !strings.HasPrefix(got, "https://watch.box.example.com,") || !strings.Contains(got, "https://metawatch-box.example.com") {
		t.Errorf("APP_URLS = %q, want the alias first, then the route's own names", got)
	}
	if len(*reports) != 0 {
		t.Errorf("unexpected incident: %+v", *reports)
	}

	twice, _ := syncApp(t, cfg, "metawatch", dir)
	if once != twice {
		t.Errorf("second up changed the override:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}

	// Taking the intent away takes the name away: the manifest owned it.
	stripped := strings.Replace(twice, "  aliases:\n    - {name: watch, domains: [dot], primary: true}\n", "", 1)
	if stripped == twice {
		stripped = removeAliases(t, twice)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.override.yml"), []byte(stripped), 0o644); err != nil {
		t.Fatal(err)
	}
	after, _ := syncApp(t, cfg, "metawatch", dir)
	if strings.Contains(after, "watch.${APP_DOMAIN}") && !strings.Contains(after, "metawatch.${APP_DOMAIN}") {
		t.Fatalf("lost the app's own dot route:\n%s", after)
	}
	if strings.Contains(after, ": watch.${APP_DOMAIN}") {
		t.Errorf("alias group outlived its alias:\n%s", after)
	}
}

// removeAliases drops the aliases block however the encoder spelled it.
func removeAliases(t *testing.T, override string) string {
	t.Helper()
	var out []string
	skip := false
	for _, line := range strings.Split(override, "\n") {
		if strings.HasPrefix(line, "  aliases:") {
			skip = true
			continue
		}
		if skip && strings.HasPrefix(line, "    ") {
			continue
		}
		skip = false
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// A hand-written group on the dot domain is still swept: the sweep is what keeps
// that namespace Maison's, and aliases are the way to put a name there.
func TestHandWrittenDotLabelIsStillSwept(t *testing.T) {
	cfg, _, _ := metastarBox(t)
	dir := installApp(t, cfg, "metawatch", clonedWatch, `services:
  metawatch-app:
    labels:
      caddy_10: watch.${APP_DOMAIN}
      caddy_10.reverse_proxy: "{{upstreams 3000}}"
`)
	got, _ := syncApp(t, cfg, "metawatch", dir)
	if strings.Contains(got, "watch.${APP_DOMAIN}") && strings.Contains(got, "caddy_10") {
		t.Errorf("hand-written dot label survived:\n%s", got)
	}
}

// Two apps that alias the same name both lose it, and each is told why.
func TestAliasClaimedByAnotherAppIsRefused(t *testing.T) {
	cfg, reports, _ := metastarBox(t)
	dir := installApp(t, cfg, "metawatch", clonedWatch, watchAliasOverride)
	installApp(t, cfg, "jellyfin", `name: jellyfin
services:
  jellyfin:
    image: jellyfin/jellyfin:10.10.0
    expose: ["8096"]
x-compose-app:
  routes:
    - upstream-port: 8096
`, `x-compose-app:
  aliases:
    - { name: watch, domains: [dot] }
`)
	got, vars := syncApp(t, cfg, "metawatch", dir)
	if strings.Contains(got, ": watch.${APP_DOMAIN}") {
		t.Errorf("a contested alias was published:\n%s", got)
	}
	if vars["APP_URL"] == "https://watch.box.example.com" {
		t.Errorf("APP_URL names an alias that was refused")
	}
	if len(*reports) != 1 || !strings.Contains((*reports)[0].Detail, "jellyfin") {
		t.Fatalf("want one incident naming jellyfin, got %+v", *reports)
	}
	if (*reports)[0].ID != "app.address.alias:metawatch" {
		t.Errorf("incident id = %q", (*reports)[0].ID)
	}
}

// An app's own id on another app's alias is refused too: `jellyfin.<box>` is
// jellyfin's on the dot domain whether or not anyone aliased it.
func TestAliasCannotTakeAnotherAppsName(t *testing.T) {
	cfg, reports, _ := metastarBox(t)
	dir := installApp(t, cfg, "metawatch", clonedWatch, `x-compose-app:
  aliases:
    - { name: jellyfin }
`)
	installApp(t, cfg, "jellyfin", "name: jellyfin\nservices:\n  jellyfin:\n    image: x\n", "")
	got, _ := syncApp(t, cfg, "metawatch", dir)
	if strings.Contains(got, ": jellyfin.${") || strings.Contains(got, ": jellyfin-${") {
		t.Errorf("alias took another app's name:\n%s", got)
	}
	if len(*reports) != 1 {
		t.Fatalf("want one incident, got %+v", *reports)
	}
}

// An up with nothing to refuse clears the incident a previous one raised.
func TestAliasIncidentClears(t *testing.T) {
	cfg, _, resolved := metastarBox(t)
	dir := installApp(t, cfg, "metawatch", clonedWatch, watchAliasOverride)
	syncApp(t, cfg, "metawatch", dir)
	if len(*resolved) == 0 || (*resolved)[len(*resolved)-1] != "app.address.alias:metawatch" {
		t.Errorf("incident not cleared: %v", *resolved)
	}
}
