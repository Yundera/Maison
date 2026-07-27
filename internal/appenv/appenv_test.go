package appenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/envinject"
)

// newCfg gives a Config rooted at a temp dir, with a .env.app holding envApp.
func newCfg(t *testing.T, envApp string) config.Config {
	t.Helper()
	cfg := config.Config{
		DataRoot:     t.TempDir(),
		DataHostPath: "/host/DATA",
		PUID:         "1000",
		PGID:         "1000",
		TZ:           "Europe/Paris",
	}
	if err := os.MkdirAll(cfg.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if envApp != "" {
		if err := os.WriteFile(Path(cfg), []byte(envApp), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg.AppEnv = func() map[string]string { return Load(cfg) }
	return cfg
}

func appDir(t *testing.T, cfg config.Config, dotEnv string) string {
	t.Helper()
	dir := filepath.Join(cfg.AppsDir(), "psitransfer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if dotEnv != "" {
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(dotEnv), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func readEnv(t *testing.T, dir string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	return envinject.EnvFileVars(raw)
}

// Ensure writes the documented default once, and never overwrites the
// deployment's own file — overwriting on upgrade would silently revert its
// domain, network and credentials.
func TestEnsureSeedsDefaultAndNeverOverwrites(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}

	if err := Ensure(cfg); err != nil {
		t.Fatal(err)
	}
	if v := Load(cfg)["APP_NET"]; v != "mesh" {
		t.Errorf("default APP_NET = %q, want mesh", v)
	}

	if err := os.WriteFile(Path(cfg), []byte("APP_NET=pcs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(cfg); err != nil {
		t.Fatal(err)
	}
	if v := Load(cfg)["APP_NET"]; v != "pcs" {
		t.Errorf("Ensure clobbered the deployment's file: APP_NET = %q, want pcs", v)
	}
}

// The core contract: every key of .env.app is ensured in the app's .env, merged
// with the vars Maison computes — and nothing else in the file is disturbed.
func TestSyncEnsuresDeploymentAndComputedVars(t *testing.T) {
	cfg := newCfg(t, "APP_NET=pcs\nAPP_DOMAIN=example.com\n")
	// A stale value, a comment, and a key that is the operator's alone.
	dir := appDir(t, cfg, "# psitransfer\nAPP_DOMAIN=old.example\nPSI_RETENTION=7d\n")

	if err := Sync(cfg, "psitransfer", dir); err != nil {
		t.Fatal(err)
	}
	got := readEnv(t, dir)

	for k, want := range map[string]string{
		"APP_DOMAIN":     "example.com", // refreshed from .env.app
		"APP_NET":        "pcs",         // appended from .env.app
		"AppID":          "psitransfer", // computed by Maison
		"DATA_ROOT":      "/host/DATA",  // computed by Maison
		"DATA_HOST_PATH": "/host/DATA",
		"TZ":             "Europe/Paris",
		"PSI_RETENTION":  "7d", // the operator's — untouched
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}

	raw, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), "# psitransfer\n") {
		t.Errorf("the operator's comment must survive:\n%s", raw)
	}
}

// Keys are ensured one by one, so neither file's ordering can matter.
func TestSyncIsOrderIndependent(t *testing.T) {
	a := newCfg(t, "APP_NET=pcs\nAPP_DOMAIN=example.com\nAPP_EMAIL=x@y.z\n")
	b := newCfg(t, "APP_EMAIL=x@y.z\nAPP_DOMAIN=example.com\nAPP_NET=pcs\n")

	dirA := appDir(t, a, "APP_DOMAIN=old\nPSI=1\n")
	dirB := appDir(t, b, "APP_DOMAIN=old\nPSI=1\n")

	if err := Sync(a, "psitransfer", dirA); err != nil {
		t.Fatal(err)
	}
	if err := Sync(b, "psitransfer", dirB); err != nil {
		t.Fatal(err)
	}

	rawA, _ := os.ReadFile(filepath.Join(dirA, ".env"))
	rawB, _ := os.ReadFile(filepath.Join(dirB, ".env"))
	if string(rawA) != string(rawB) {
		t.Errorf(".env.app ordering leaked into the result:\n--- a ---\n%s\n--- b ---\n%s", rawA, rawB)
	}
}

func TestSyncIsIdempotent(t *testing.T) {
	cfg := newCfg(t, "APP_NET=pcs\nAPP_DOMAIN=example.com\n")
	dir := appDir(t, cfg, "APP_DOMAIN=old.example\n")

	if err := Sync(cfg, "psitransfer", dir); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(filepath.Join(dir, ".env"))
	if err := Sync(cfg, "psitransfer", dir); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(filepath.Join(dir, ".env"))

	if string(first) != string(second) {
		t.Errorf("Sync is not idempotent:\n--- 1 ---\n%s\n--- 2 ---\n%s", first, second)
	}
}

// An empty value in .env.app means "the deployment does not have this". Writing it
// blank would route an app at an empty host; leaving the reference unresolved at
// least makes compose say so.
func TestSyncSkipsEmptyValues(t *testing.T) {
	cfg := newCfg(t, "APP_DOMAIN=\nAPP_NET=pcs\n")
	dir := appDir(t, cfg, "")

	if err := Sync(cfg, "psitransfer", dir); err != nil {
		t.Fatal(err)
	}
	if v, ok := readEnv(t, dir)["APP_DOMAIN"]; ok {
		t.Errorf("empty APP_DOMAIN must be skipped, got %q", v)
	}
}

// Sync creates the .env for a fresh install rather than requiring one to exist.
func TestSyncCreatesEnvForAFreshInstall(t *testing.T) {
	cfg := newCfg(t, "APP_NET=pcs\n")
	dir := appDir(t, cfg, "")

	if err := Sync(cfg, "psitransfer", dir); err != nil {
		t.Fatal(err)
	}
	got := readEnv(t, dir)
	if got["AppID"] != "psitransfer" || got["APP_NET"] != "pcs" {
		t.Errorf("fresh .env not prefilled: %v", got)
	}
}

// The editor round-trips the file as text, so what it reads back must be the file
// byte for byte — comments, ordering and empty values included. Load's map keeps
// none of those (it drops empties entirely), which is why ReadRaw exists.
func TestReadRawIsVerbatim(t *testing.T) {
	const file = "# the deployment's note\n\nAPP_NET=pcs\nAPP_DOMAIN=\n"
	cfg := newCfg(t, file)

	got, err := ReadRaw(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != file {
		t.Errorf("ReadRaw() =\n%s\nwant\n%s", got, file)
	}
}

// A deployment that has not been through Ensure yet still opens the editor on the
// documented default rather than an error or a blank page.
func TestReadRawFallsBackToTheDefault(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}

	got, err := ReadRaw(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "APP_NET=mesh") {
		t.Errorf("ReadRaw() on a fresh deployment should be the shipped default, got:\n%s", got)
	}
}

// WriteRaw is the editor's save: it overwrites (unlike Ensure), and what it writes
// is what Load must then read.
func TestWriteRawReplacesAndLoads(t *testing.T) {
	cfg := newCfg(t, "APP_NET=mesh\n")

	if err := WriteRaw(cfg, []byte("# edited\nAPP_NET=pcs\nAPP_DOMAIN=example.com\n")); err != nil {
		t.Fatal(err)
	}
	if v := Load(cfg)["APP_NET"]; v != "pcs" {
		t.Errorf("APP_NET = %q, want pcs", v)
	}
	if v := Load(cfg)["APP_DOMAIN"]; v != "example.com" {
		t.Errorf("APP_DOMAIN = %q, want example.com", v)
	}
	raw, err := ReadRaw(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), "# edited\n") {
		t.Errorf("the operator's comment must survive the save:\n%s", raw)
	}
}

// A typo must not cost the operator their working file: the save is rejected and
// the file on disk is left exactly as it was.
func TestWriteRawRejectsBadTextWithoutTouchingTheFile(t *testing.T) {
	const before = "APP_NET=mesh\n"
	cfg := newCfg(t, before)

	if err := WriteRaw(cfg, []byte("APP_NET=mesh\nAPP_DOMAIN example.com\n")); err == nil {
		t.Fatal("WriteRaw() = nil, want an error for a line that is not KEY=VALUE")
	}
	raw, err := ReadRaw(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != before {
		t.Errorf("a rejected save must leave the file alone, got:\n%s", raw)
	}
}

// WriteRaw creates the file (and its directory) for a deployment that never had
// one — the editor is reachable before Ensure has ever run.
func TestWriteRawCreatesTheFile(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}

	if err := WriteRaw(cfg, []byte("APP_NET=pcs\n")); err != nil {
		t.Fatal(err)
	}
	if v := Load(cfg)["APP_NET"]; v != "pcs" {
		t.Errorf("APP_NET = %q, want pcs", v)
	}
}
