package caddyroutes

import (
	"strings"
	"testing"

	"github.com/yundera/maison/internal/domains"
)

// The deployment's two additional domains, exactly as the Yundera Caddyfile
// treats them: nip.io goes through the gateway's custom CA, sslip.io deliberately
// does not (it falls through to Let's Encrypt).
var yundera = []domains.Domain{
	{Name: "nip", Domain: "${APP_PUBLIC_IP_DASH}.nip.io", Directives: map[string]string{"import": "gateway_tls"}},
	{Name: "sslip", Domain: "${APP_PUBLIC_IP_DASH}.sslip.io"},
}

// outlineBase is a trimmed store compose after the store drops its nip/sslip
// labels: one route group, on the primary domain.
const outlineBase = `name: outline
services:
  outline:
    image: outlinewiki/outline:1.6.1
    labels:
      caddy_0: outline-${APP_DOMAIN}
      caddy_0.import: gateway_tls
      caddy_0.reverse_proxy: "{{upstreams 80}}"
`

// deployVars are the interpolation variables of the box these tests describe —
// the two spellings of its public IP that a real PCS carries at once, and the two
// of its domain. syncEnv resolves against them; sync does not resolve at all.
var deployVars = map[string]string{
	"APP_PUBLIC_IP_DASH": "10-0-0-1",
	"PUBLIC_IP_DASH":     "10-0-0-1",
	"APP_DOMAIN":         "box.example.com",
	"DOMAIN":             "box.example.com",
}

func resolveWith(vars map[string]string) Resolver {
	return func(host string) string {
		return varRef.ReplaceAllStringFunc(host, func(ref string) string {
			if v, ok := vars[strings.Trim(ref, "${}")]; ok {
				return v
			}
			return ref
		})
	}
}

func sync(t *testing.T, base, override string, doms []domains.Domain) string {
	t.Helper()
	return syncWith(t, base, override, doms, nil)
}

func syncWith(t *testing.T, base, override string, doms []domains.Domain, resolve Resolver) string {
	t.Helper()
	out, err := Sync([]byte(base), []byte(override), doms, resolve)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return string(out)
}

// The acceptance test: what Maison generates is what the store used to ship by
// hand — the same hosts, the same upstream, gateway_tls on nip and absent on sslip.
func TestSyncReproducesTheStoresRoutes(t *testing.T) {
	got := sync(t, outlineBase, "", yundera)

	for _, want := range []string{
		"caddy_1: outline-${APP_PUBLIC_IP_DASH}.nip.io",
		"caddy_1.reverse_proxy: '{{upstreams 80}}'",
		"caddy_1.import: gateway_tls",
		"caddy_2: outline-${APP_PUBLIC_IP_DASH}.sslip.io",
		"caddy_2.reverse_proxy: '{{upstreams 80}}'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// sslip.io must NOT inherit the app's gateway_tls: TLS belongs to the domain.
	if strings.Contains(got, "caddy_2.import") {
		t.Errorf("sslip route inherited the app's TLS directive:\n%s", got)
	}
	if !strings.Contains(got, ManifestKey) {
		t.Errorf("no manifest written:\n%s", got)
	}
}

func TestSyncIsIdempotent(t *testing.T) {
	once := sync(t, outlineBase, "", yundera)
	twice := sync(t, outlineBase, once, yundera)
	if once != twice {
		t.Errorf("second sync changed the file:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
}

// Removing every domain must leave the override exactly as Maison found it —
// here, with nothing of its own, so the file goes away entirely.
func TestSyncRemovesItsOwnRoutes(t *testing.T) {
	generated := sync(t, outlineBase, "", yundera)

	out, err := Sync([]byte(outlineBase), []byte(generated), nil, nil)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if out != nil {
		t.Errorf("override survived the last domain being removed:\n%s", out)
	}
}

// The operator's content is not Maison's to touch — including a route they
// wrote themselves on an index we could otherwise have used.
func TestSyncKeepsTheOperatorsOverride(t *testing.T) {
	override := `services:
  outline:
    # keep this app off the CPU
    cpus: "0.5"
    labels:
      caddy_9: outline-internal.example.com
      caddy_9.reverse_proxy: "{{upstreams 80}}"
x-compose-app:
  store-app-id: outline
`
	got := sync(t, outlineBase, override, yundera)

	for _, want := range []string{
		"# keep this app off the CPU",
		`cpus: "0.5"`,
		"caddy_9: outline-internal.example.com",
		"store-app-id: outline",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("clobbered %q in:\n%s", want, got)
		}
	}
	// caddy_9 is taken, so the generated routes start after it.
	if !strings.Contains(got, "caddy_10: outline-${APP_PUBLIC_IP_DASH}.nip.io") {
		t.Errorf("generated route collided with the operator's index:\n%s", got)
	}

	// And removing the domains again leaves their file untouched.
	out, err := Sync([]byte(outlineBase), []byte(got), nil, nil)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	for _, want := range []string{"# keep this app off the CPU", "caddy_9: outline-internal.example.com"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("removing the domains took %q with it:\n%s", want, out)
		}
	}
	if strings.Contains(string(out), "sslip.io") || strings.Contains(string(out), ManifestKey) {
		t.Errorf("generated routes outlived their domain:\n%s", out)
	}
}

// A store that still ships its own nip/sslip labels must not be published twice —
// this is what lets Maison roll out before the store is trimmed.
func TestSyncSkipsRoutesTheStoreAlreadyShips(t *testing.T) {
	base := outlineBase + `      caddy_1: outline-${APP_PUBLIC_IP_DASH}.nip.io
      caddy_1.import: gateway_tls
      caddy_1.reverse_proxy: "{{upstreams 80}}"
      caddy_2: outline-${APP_PUBLIC_IP_DASH}.sslip.io
      caddy_2.reverse_proxy: "{{upstreams 80}}"
`
	if out := sync(t, base, "", yundera); out != "" {
		t.Errorf("republished a route the store already ships:\n%s", out)
	}
}

// A route can be a whole handle_path tree (Seafile's is), and it has to keep
// working on the second domain — so the clone copies every directive, not just
// the reverse_proxy.
func TestSyncClonesNestedDirectives(t *testing.T) {
	base := `name: seafile
services:
  seafile:
    labels:
      caddy_0: seafile-${APP_DOMAIN}
      caddy_0.import: gateway_tls
      caddy_0.0_handle_path: "/notification/*"
      caddy_0.0_handle_path.reverse_proxy: "notification-server:8083"
      caddy_0.1_handle_path: "/socket.io/*"
      caddy_0.1_handle_path.0_rewrite: "* /socket.io{uri}"
      caddy_0.4_reverse_proxy: "{{upstreams 80}}"
`
	got := sync(t, base, "", []domains.Domain{yundera[1]}) // sslip only

	for _, want := range []string{
		"caddy_1: seafile-${APP_PUBLIC_IP_DASH}.sslip.io",
		"caddy_1.0_handle_path: /notification/*",
		"caddy_1.0_handle_path.reverse_proxy: notification-server:8083",
		"caddy_1.1_handle_path.0_rewrite: '* /socket.io{uri}'",
		"caddy_1.4_reverse_proxy: '{{upstreams 80}}'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// Outline's Dex sidecar has its own route on the primary domain, and needs the
// same variants — so every route group is cloned, not just the app's web UI one.
func TestSyncClonesEveryServicesRoutes(t *testing.T) {
	base := outlineBase + `  outline-dex:
    labels:
      caddy_0: outline-auth-${APP_DOMAIN}
      caddy_0.reverse_proxy: "{{upstreams 5556}}"
`
	got := sync(t, base, "", []domains.Domain{yundera[1]})

	for _, want := range []string{
		"caddy_1: outline-${APP_PUBLIC_IP_DASH}.sslip.io",
		"caddy_1: outline-auth-${APP_PUBLIC_IP_DASH}.sslip.io",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// A host with no primary-domain placeholder is not ours to republish.
func TestSyncIgnoresRoutesOffThePrimaryDomain(t *testing.T) {
	base := `services:
  nas:
    labels:
      caddy_0: nas.example.com
      caddy_0.reverse_proxy: "{{upstreams 80}}"
`
	if out := sync(t, base, "", yundera); out != "" {
		t.Errorf("republished a hardcoded host:\n%s", out)
	}
}

// An app with no routes at all (headless, or not behind the gateway) is left alone.
func TestSyncLeavesRoutelessAppsAlone(t *testing.T) {
	base := "services:\n  worker:\n    image: worker:1\n"
	if out := sync(t, base, "", yundera); out != "" {
		t.Errorf("wrote an override for a routeless app:\n%s", out)
	}
}

// …and so is that app's override. With nothing to generate there is nothing to
// write, and re-emitting the file would rewrite the operator's YAML — quoting,
// indent, tags — to say exactly what it already said.
func TestSyncDoesNotRewriteAnOverrideItHasNoBusinessIn(t *testing.T) {
	base := "services:\n  worker:\n    image: worker:1\n"
	override := `services:
  worker:
    ports: !override
      - "18192:80"    # hand-written, and it stays that way
    environment:
      TZ: UTC
`
	if got := sync(t, base, override, yundera); got != override {
		t.Errorf("rewrote an untouched override:\n--- want ---\n%s\n--- got ---\n%s", override, got)
	}
}

func TestVarsCoversTheLabelsCompose(t *testing.T) {
	got := Vars([]byte(outlineBase), yundera)
	want := []string{"APP_DOMAIN", "APP_PUBLIC_IP_DASH"}

	if len(got) != len(want) {
		t.Fatalf("Vars() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Vars() = %v, want %v", got, want)
		}
	}
}

// maisonBase is Maison's own stack as a PCS writes it — the older, un-prefixed
// spelling of the deployment's variables. It routes the same three hosts a store
// app does, under different names for the same values.
const maisonBase = `name: maison
services:
  maison:
    image: rg.fr-par.scw.cloud/aptero/maison:1.0.0
    labels:
      caddy_0: maison-${DOMAIN}
      caddy_0.import: gateway_tls
      caddy_0.reverse_proxy: "{{upstreams 80}}"
      caddy_1: maison-${PUBLIC_IP_DASH}.nip.io
      caddy_1.import: gateway_tls
      caddy_1.reverse_proxy: "{{upstreams 80}}"
      caddy_2: maison-${PUBLIC_IP_DASH}.sslip.io
      caddy_2.reverse_proxy: "{{upstreams 80}}"
`

// TestSyncSkipsAHostAlreadyRoutedUnderAnotherSpelling is the bug this resolution
// exists for. `${PUBLIC_IP_DASH}` and `${APP_PUBLIC_IP_DASH}` are one address, so
// a compose already carrying `maison-${PUBLIC_IP_DASH}.nip.io` is already on the
// configured nip domain — generating for it published the dashboard twice on
// `maison-10-0-0-1.nip.io`, once with gateway_tls and once without, and left
// whichever Caddy picked to decide the TLS.
func TestSyncSkipsAHostAlreadyRoutedUnderAnotherSpelling(t *testing.T) {
	got := syncWith(t, maisonBase, "", yundera, resolveWith(deployVars))

	if strings.Contains(got, "caddy_3") || strings.Contains(got, "caddy_4") {
		t.Errorf("generated a route onto a host the base already serves:\n%s", got)
	}
	if got != "" {
		t.Errorf("nothing to generate and no override to keep, want an empty file:\n%s", got)
	}
}

// The other half: resolution must not make Maison blind to a domain that is
// genuinely new. Same base, same variables, a domain the box does not serve yet.
func TestSyncStillGeneratesForAnUnroutedDomain(t *testing.T) {
	lan := []domains.Domain{{Name: "lan", Domain: "lan.example.com"}}
	got := syncWith(t, maisonBase, "", lan, resolveWith(deployVars))

	if !strings.Contains(got, "caddy_3: maison-lan.example.com") {
		t.Errorf("want a generated route on the new domain:\n%s", got)
	}
}

// A previously generated route written with the other spelling must still be
// recognised as sitting on a configured domain, or removing the domain would
// leave it behind forever.
func TestDropRecognisesAGeneratedRouteUnderAnotherSpelling(t *testing.T) {
	stale := `services:
  outline:
    labels:
      caddy_9: outline-${PUBLIC_IP_DASH}.nip.io
      caddy_9.reverse_proxy: "{{upstreams 80}}"
`
	got := syncWith(t, outlineBase, stale, yundera, resolveWith(deployVars))

	if strings.Contains(got, "caddy_9") {
		t.Errorf("stale route on a configured domain survived:\n%s", got)
	}
}
