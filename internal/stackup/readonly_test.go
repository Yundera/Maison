package stackup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yundera/maison/internal/appenv"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/domains"
)

// storeCompose is a store app in the shape the app stores ship: a shared external
// network the app declares itself and attaches to *two* services, /DATA binds, and
// a route on the primary domain for Maison to publish on the others.
const storeCompose = `name: outline

services:
  outline:
    image: outlinewiki/outline:0.82.0
    container_name: outline
    volumes:
      - ${DATA_ROOT:-/DATA}/AppData/outline/data:/var/lib/outline/data
    expose:
      - 3000
    labels:
      caddy_0: outline-${APP_DOMAIN}
      caddy_0.reverse_proxy: "{{upstreams 3000}}"
    networks: [pcs]

  outline-dex:
    image: dexidp/dex:v2.41.1
    container_name: outline-dex
    networks: [pcs]

networks:
  pcs:
    name: ${APP_NET:-pcs}
    external: true
`

// appFolder writes a store app to disk the way the installer does — the store's
// bytes, unchanged — and returns the app directory and a deployment around it.
func appFolder(t *testing.T) (config.Config, string) {
	t.Helper()
	cfg := testConfig(t)
	cfg.Domains = func() []domains.Domain {
		return []domains.Domain{{Name: "sslip", Domain: "${APP_PUBLIC_IP_DASH}.sslip.io"}}
	}
	// The deployment's .env.app, exactly as the orchestrator writes it on a PCS.
	envApp := appenv.Path(cfg)
	if err := os.MkdirAll(filepath.Dir(envApp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envApp, []byte("APP_NET=pcs\nAPP_DOMAIN=box.nsl.sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(cfg.DataRoot, "AppData", "outline")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(storeCompose), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg, dir
}

// Everything Up does to an app's folder before it hands off to `docker compose`,
// minus the compose run itself.
func prepareFolder(t *testing.T, cfg config.Config, dir string) {
	t.Helper()
	SyncRoutes(cfg, "outline", dir, []string{filepath.Join(dir, "docker-compose.yml")})
	if err := appenv.Sync(cfg, "outline", dir); err != nil {
		t.Fatalf("sync .env: %v", err)
	}
}

// The invariant: an app's docker-compose.yml is the store's file and Maison never
// writes to it. Maison used to rewrite it before every up — dropping the network
// the app declared and re-attaching only the main service, which left a sidecar
// unable to reach its own backend.
func TestUpNeverWritesTheStoresCompose(t *testing.T) {
	cfg, dir := appFolder(t)
	base := filepath.Join(dir, "docker-compose.yml")

	for range 2 { // twice: a rewrite that only happens on the second up is still a rewrite
		prepareFolder(t, cfg, dir)

		got, err := os.ReadFile(base)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != storeCompose {
			t.Fatalf("docker-compose.yml was rewritten:\n%s", got)
		}
	}

	// Both services keep the network the app put them on, and the app's own
	// declaration is still the one in the file.
	if n := strings.Count(storeCompose, "networks: [pcs]"); n != 2 {
		t.Fatalf("fixture no longer covers the multi-service case (%d attachments)", n)
	}
}

// What Maison does write instead: the override (its generated routes) and the
// app's .env (the deployment's variables). Those two plus the store's compose are
// the whole app folder, which is why a hand-run `docker compose up -d` in it
// reproduces what Maison does.
func TestUpWritesOnlyTheOverrideAndTheEnv(t *testing.T) {
	cfg, dir := appFolder(t)
	prepareFolder(t, cfg, dir)

	override, err := os.ReadFile(filepath.Join(dir, "docker-compose.override.yml"))
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	if !strings.Contains(string(override), "sslip.io") {
		t.Fatalf("override carries no generated route:\n%s", override)
	}

	env, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatalf(".env: %v", err)
	}
	// APP_NET is what the store compose's ${APP_NET:-pcs} resolves against, and it
	// reaches the app here rather than by rewriting the compose.
	for _, want := range []string{"APP_NET=pcs", "APP_DOMAIN=box.nsl.sh", "DATA_ROOT=/host/DATA"} {
		if !strings.Contains(string(env), want) {
			t.Fatalf(".env missing %q:\n%s", want, env)
		}
	}
}
