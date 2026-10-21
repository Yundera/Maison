package stackup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/routing"
)

const routedCompose = `name: outline
services:
  outline:
    image: outlinewiki/outline:1.6.1
    expose: ["80"]
  outline-dex:
    image: ghcr.io/dexidp/dex:v2.45.1
    expose: ["5556"]
x-compose-app:
  routes:
    - service: outline
      upstream-port: 80
    - service: outline-dex
      name: outline-auth
      upstream-port: 5556
`

func portsBox(t *testing.T, inUse map[int]bool) (config.Config, string) {
	t.Helper()
	cfg := testConfig(t)
	cfg.Routing = func() routing.Setting {
		return routing.Effective(&routing.Setting{Renderer: routing.Ports}, nil, "192.168.1.10")
	}
	cfg.PortsInUse = func(string) map[int]bool { return inUse }
	dir := filepath.Join(cfg.AppsDir(), "outline")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(routedCompose), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg, dir
}

func up(t *testing.T, cfg config.Config, dir string) map[string]string {
	t.Helper()
	_, vars := SyncRoutes(cfg, "outline", dir, []string{filepath.Join(dir, "docker-compose.yml")})
	writeRouteVars(cfg, "outline", dir, vars)
	raw, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	return envinject.EnvFileVars(raw)
}

// Ports mode, end to end on the folder: a port another app's container publishes
// and one another app keeps in its .env are both skipped, the result is saved, and
// the next up keeps it.
func TestPortsModeAllocatesOnceAndKeeps(t *testing.T) {
	cfg, dir := portsBox(t, map[int]bool{20000: true}) // a container holds 20000
	other := filepath.Join(cfg.AppsDir(), "wiki")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, ".env"), []byte("APP_PORT_WIKI=5556\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := up(t, cfg, dir)
	if env["APP_PORT_OUTLINE"] != "20001" || env["APP_PORT_OUTLINE_AUTH"] != "20002" {
		t.Fatalf("ports = %s / %s, want 20001 (80 is below 1024, 20000 is taken) and 20002 (5556 is kept by another app)",
			env["APP_PORT_OUTLINE"], env["APP_PORT_OUTLINE_AUTH"])
	}
	if env["APP_URL"] != "http://192.168.1.10:20001" || env["APP_BIND"] != "0.0.0.0" {
		t.Fatalf(".env = %v", env)
	}
	override, err := os.ReadFile(filepath.Join(dir, "docker-compose.override.yml"))
	if err != nil || !strings.Contains(string(override), "${APP_BIND}:${APP_PORT_OUTLINE}:80") {
		t.Fatalf("override (err %v):\n%s", err, override)
	}

	// Next up, with 20001 now in use by this very app's container: it is kept.
	cfg.PortsInUse = func(string) map[int]bool { return map[int]bool{20000: true} }
	if again := up(t, cfg, dir); again["APP_PORT_OUTLINE"] != "20001" {
		t.Fatalf("a published port was renumbered: %v", again)
	}

	// routes.json lists the app for an external router.
	if b, err := os.ReadFile(filepath.Join(cfg.StateDir(), "routes.json")); err != nil || !strings.Contains(string(b), `"outline-auth"`) {
		t.Fatalf("routes.json (err %v): %s", err, b)
	}
}

// The retry path: Docker reports the port, and a re-sync avoiding it moves the
// route that held it.
func TestPortInUseIsMovedOff(t *testing.T) {
	for _, msg := range []string{
		"Error response from daemon: driver failed programming external connectivity: Bind for 0.0.0.0:20001 failed: port is already allocated",
		"failed to bind host port 0.0.0.0:20001/tcp: address already in use",
	} {
		if p, ok := portInUse(errors.New(msg)); !ok || p != 20001 {
			t.Errorf("portInUse(%q) = %d, %v", msg, p, ok)
		}
	}

	cfg, dir := portsBox(t, nil)
	before := up(t, cfg, dir)
	_, after := syncRoutes(cfg, "outline", dir, nil, map[int]bool{20000: true})
	key, now, ok := movedOff(before, after, 20000)
	if !ok || key != "APP_PORT_OUTLINE" || now == "20000" {
		t.Fatalf("before %v, after %v: the route on 20000 did not move", before, after)
	}
	if d := portIncident("outline", 20000, key, now).Detail; !strings.Contains(d, "route outline is now published on port "+now) {
		t.Errorf("incident = %q", d)
	}
}
