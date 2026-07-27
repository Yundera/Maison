package envinject

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/yundera/maison/internal/config"
)

func testCfg() config.Config {
	return config.Config{
		DataRoot:     "/DATA",
		DataHostPath: "/host/DATA",
		PUID:         "1000",
		PGID:         "1000",
		TZ:           "Europe/Paris",
		AppEnv: func() map[string]string {
			return map[string]string{"APP_NET": "mesh", "APP_DOMAIN": "example.com"}
		},
	}
}

// doc is the parsed YAML of a Transform result, for assertions.
func doc(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

const storeCompose = `
name: psitransfer
services:
  psitransfer:
    image: psitransfer:latest
    volumes:
      - /DATA/AppData/psitransfer/data/:/data
      - /DATA/Gallery/:/import/:ro
      - /etc/localtime:/etc/localtime:ro
`

// Transform must never write the resolved value of a deployment variable into an
// app's compose — that is what used to strand an app on the deployment it was
// installed against.
func TestTransformEmitsReferencesNotLiterals(t *testing.T) {
	out, err := Transform([]byte(storeCompose), testCfg(), "psitransfer")
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); strings.Contains(s, "/host/DATA") || strings.Contains(s, "name: mesh") {
		t.Fatalf("Transform baked a resolved value into the compose:\n%s", s)
	}

	d := doc(t, out)
	svc := d["services"].(map[string]any)["psitransfer"].(map[string]any)
	vols := svc["volumes"].([]any)
	if got, want := vols[0].(string), "${DATA_ROOT}/AppData/psitransfer/data/:/data"; got != want {
		t.Errorf("data-root bind: got %q, want %q", got, want)
	}
	if got, want := vols[1].(string), "${DATA_ROOT}/Gallery/:/import/:ro"; got != want {
		t.Errorf("data-root bind: got %q, want %q", got, want)
	}
	if got, want := vols[2].(string), "/etc/localtime:/etc/localtime:ro"; got != want {
		t.Errorf("non-data bind must be untouched: got %q, want %q", got, want)
	}

	net := d["networks"].(map[string]any)[AppNetKey].(map[string]any)
	if got, want := net["name"], "${APP_NET}"; got != want {
		t.Errorf("network name: got %q, want %q", got, want)
	}
	if ext, _ := net["external"].(bool); !ext {
		t.Error("network must be external")
	}
	if got, want := svc["networks"].([]any)[0].(string), AppNetKey; got != want {
		t.Errorf("main service network: got %q, want %q", got, want)
	}
}

func TestTransformIsIdempotent(t *testing.T) {
	once, err := Transform([]byte(storeCompose), testCfg(), "psitransfer")
	if err != nil {
		t.Fatal(err)
	}
	twice, err := Transform(once, testCfg(), "psitransfer")
	if err != nil {
		t.Fatal(err)
	}
	if string(once) != string(twice) {
		t.Errorf("Transform is not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
}

// The regression this whole change exists for: an app installed against one
// deployment (network `pcs`, data root `/old/DATA`) must come back in reference
// form — not keep a dangling external network that no longer exists.
func TestTransformHealsComposeBakedByAnOlderMaison(t *testing.T) {
	const baked = `
name: psitransfer
networks:
  pcs:
    external: true
    name: pcs
services:
  psitransfer:
    image: psitransfer:latest
    networks:
      - pcs
    volumes:
      - /old/DATA/AppData/psitransfer/data/:/data
`
	cfg := testCfg()
	cfg.DataHostPath = "/old/DATA" // the deployment it was baked against

	out, err := Transform([]byte(baked), cfg, "psitransfer")
	if err != nil {
		t.Fatal(err)
	}
	d := doc(t, out)

	nets := d["networks"].(map[string]any)
	if _, stale := nets["pcs"]; stale {
		t.Error("stale external network `pcs` must be dropped — it may no longer exist")
	}
	if got := nets[AppNetKey].(map[string]any)["name"]; got != "${APP_NET}" {
		t.Errorf("network name: got %q, want ${APP_NET}", got)
	}

	svc := d["services"].(map[string]any)["psitransfer"].(map[string]any)
	svcNets := svc["networks"].([]any)
	if len(svcNets) != 1 || svcNets[0].(string) != AppNetKey {
		t.Errorf("main service networks: got %v, want [%s]", svcNets, AppNetKey)
	}
	if got, want := svc["volumes"].([]any)[0].(string), "${DATA_ROOT}/AppData/psitransfer/data/:/data"; got != want {
		t.Errorf("baked host path: got %q, want %q", got, want)
	}
}

// Every external network Maison has ever generated must be recognised and
// replaced — including the intermediate ${REF_NET} spelling. Two live external
// networks would leave the app attached to a network that may not exist.
func TestTransformReplacesEveryGeneratedNetworkSpelling(t *testing.T) {
	const generated = `
name: psitransfer
networks:
  pcs:
    external: true
    name: pcs
  refnet:
    external: true
    name: ${REF_NET}
services:
  psitransfer:
    image: psitransfer:latest
    networks:
      - pcs
      - refnet
`
	out, err := Transform([]byte(generated), testCfg(), "psitransfer")
	if err != nil {
		t.Fatal(err)
	}
	d := doc(t, out)

	nets := d["networks"].(map[string]any)
	if len(nets) != 1 {
		t.Errorf("expected exactly one external network, got %v", nets)
	}
	if _, ok := nets[AppNetKey]; !ok {
		t.Errorf("the current network is missing: %v", nets)
	}

	svcNets := d["services"].(map[string]any)["psitransfer"].(map[string]any)["networks"].([]any)
	if len(svcNets) != 1 || svcNets[0].(string) != AppNetKey {
		t.Errorf("main service networks: got %v, want [%s]", svcNets, AppNetKey)
	}
}

// A store app that joins an external network of its own must keep it: it omits
// `name`, which is what tells it apart from anything Maison generated.
func TestTransformKeepsTheStoreAppsOwnExternalNetwork(t *testing.T) {
	const withOwnNet = `
name: psitransfer
networks:
  traefik:
    external: true
services:
  psitransfer:
    image: psitransfer:latest
    networks:
      - traefik
`
	out, err := Transform([]byte(withOwnNet), testCfg(), "psitransfer")
	if err != nil {
		t.Fatal(err)
	}
	d := doc(t, out)

	if _, ok := d["networks"].(map[string]any)["traefik"]; !ok {
		t.Error("the store app's own external network must not be dropped")
	}
	svcNets := d["services"].(map[string]any)["psitransfer"].(map[string]any)["networks"].([]any)
	if len(svcNets) != 2 {
		t.Errorf("main service should join both its own network and ours, got %v", svcNets)
	}
}

// No APP_NET in .env.app (a plain local run) means no external network at all.
func TestTransformWithoutAppNet(t *testing.T) {
	cfg := testCfg()
	cfg.AppEnv = func() map[string]string { return nil }
	out, err := Transform([]byte(storeCompose), cfg, "psitransfer")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := doc(t, out)["networks"]; ok {
		t.Error("no APP_NET configured: no network should be declared")
	}
}
