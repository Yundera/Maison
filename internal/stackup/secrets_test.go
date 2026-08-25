package stackup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// appDir makes an app folder with the given .env content.
func appDir(t *testing.T, env string) string {
	t.Helper()
	dir := t.TempDir()
	if env != "" {
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func envVars(t *testing.T, dir string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatalf("read .env: %v", err)
	}
	return envinject.EnvFileVars(b)
}

func TestEnsureSecretsGeneratesOnceAndStaysStable(t *testing.T) {
	cfg := testConfig(t)
	dir := appDir(t, "APP_DOMAIN=example.test\n")
	secrets := xcomposeapp.StringMap{"SEARXNG_SECRET": "hex:32"}

	if err := EnsureSecrets(cfg, "odysseus", dir, secrets, nil); err != nil {
		t.Fatal(err)
	}
	first := envVars(t, dir)["SEARXNG_SECRET"]

	// The bug this replaces: a hook that "succeeded" while writing nothing.
	if len(first) != 64 {
		t.Fatalf("SEARXNG_SECRET = %q (len %d), want 64 hex chars", first, len(first))
	}

	// Three more converges — a restart, an update, a config save — must not
	// rotate a key the app has already encrypted things with.
	for i := 0; i < 3; i++ {
		if err := EnsureSecrets(cfg, "odysseus", dir, secrets, nil); err != nil {
			t.Fatal(err)
		}
		if got := envVars(t, dir)["SEARXNG_SECRET"]; got != first {
			t.Fatalf("converge %d rotated the secret: %q → %q", i+1, first, got)
		}
	}
}

func TestEnsureSecretsKeepsOperatorValueAndOtherLines(t *testing.T) {
	cfg := testConfig(t)
	dir := appDir(t, "# my notes\nOUTLINE_SECRET_KEY=pinned-by-hand\nCUSTOM=mine\n")

	err := EnsureSecrets(cfg, "outline", dir, xcomposeapp.StringMap{
		"OUTLINE_SECRET_KEY": "hex:32",
		"OUTLINE_OIDC":       "hex:32",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	vars := envVars(t, dir)
	if vars["OUTLINE_SECRET_KEY"] != "pinned-by-hand" {
		t.Fatalf("overwrote a pinned value: %q", vars["OUTLINE_SECRET_KEY"])
	}
	if len(vars["OUTLINE_OIDC"]) != 64 {
		t.Fatalf("missing secret was not generated: %q", vars["OUTLINE_OIDC"])
	}
	if vars["CUSTOM"] != "mine" {
		t.Fatal("an unrelated .env line was lost")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, ".env"))
	if !strings.Contains(string(raw), "# my notes") {
		t.Fatal("a comment line was lost")
	}
}

// An empty value is what a failed shell substitution used to leave behind, and
// it is indistinguishable from "never generated" — so it is refilled.
func TestEnsureSecretsRefillsEmptyValue(t *testing.T) {
	cfg := testConfig(t)
	dir := appDir(t, "APP_SECRET=\n")

	if err := EnsureSecrets(cfg, "docmost", dir, xcomposeapp.StringMap{"APP_SECRET": "hex:32"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := envVars(t, dir)["APP_SECRET"]; len(got) != 64 {
		t.Fatalf("APP_SECRET = %q, want a generated value", got)
	}
}

func TestEnsureSecretsBcryptRendersItsTemplate(t *testing.T) {
	cfg := testConfig(t)
	dir := appDir(t, "APP_DEFAULT_PASSWORD=hunter2\n")

	if err := EnsureSecrets(cfg, "outline", dir,
		xcomposeapp.StringMap{"DEX_HASH": "bcrypt:${APP_DEFAULT_PASSWORD}"}, nil); err != nil {
		t.Fatal(err)
	}
	got := envVars(t, dir)["DEX_HASH"]
	if !strings.HasPrefix(got, "$2a$") && !strings.HasPrefix(got, "$2b$") {
		t.Fatalf("DEX_HASH = %q, want a bcrypt hash", got)
	}
	if strings.Contains(got, "APP_DEFAULT_PASSWORD") {
		t.Fatal("hashed the literal template instead of its value")
	}
}

func TestEnsureSecretsFailsLoudly(t *testing.T) {
	cfg := testConfig(t)

	// An unresolvable reference must fail the up rather than hash an empty string.
	if err := EnsureSecrets(cfg, "app", appDir(t, ""),
		xcomposeapp.StringMap{"H": "bcrypt:${NOPE_NOT_SET}"}, nil); err == nil {
		t.Fatal("want an error for an unresolved variable")
	}
	// So must a generator nobody implements — the store author needs telling.
	if err := EnsureSecrets(cfg, "app", appDir(t, ""),
		xcomposeapp.StringMap{"K": "openssl rand -hex 32"}, nil); err == nil {
		t.Fatal("want an error for an unknown generator")
	}
}

func TestEnsureVariablesRefreshEveryConverge(t *testing.T) {
	cfg := testConfig(t)
	dir := appDir(t, "APP_DOMAIN=old.test\n")
	vars := xcomposeapp.StringMap{"OUTLINE_URL": "https://outline-${APP_DOMAIN}"}

	if err := EnsureVariables(cfg, "outline", dir, vars, nil); err != nil {
		t.Fatal(err)
	}
	if got := envVars(t, dir)["OUTLINE_URL"]; got != "https://outline-old.test" {
		t.Fatalf("OUTLINE_URL = %q", got)
	}

	// The deployment's domain changes; the derived value must follow it, which is
	// exactly what a once-at-install hook could not do.
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("APP_DOMAIN=new.test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureVariables(cfg, "outline", dir, vars, nil); err != nil {
		t.Fatal(err)
	}
	if got := envVars(t, dir)["OUTLINE_URL"]; got != "https://outline-new.test" {
		t.Fatalf("OUTLINE_URL did not follow the domain: %q", got)
	}
}

// Both renderers take the capture overlay, so a value an init step printed can
// be referenced wherever the app's own .env can be. (In the converge sequence
// init runs after these two, so in practice captures reach the seed tree and
// files; the overlay is threaded through here so the order can change without
// two of the four renderers silently not seeing it.)
func TestRenderersAcceptTheCaptureOverlay(t *testing.T) {
	cfg := testConfig(t)
	dir := appDir(t, "")
	captures := map[string]string{"RCLONE_PASS": "obscured-value"}

	if err := EnsureVariables(cfg, "seafile", dir,
		xcomposeapp.StringMap{"WEBDAV_PASS": "${RCLONE_PASS}"}, captures); err != nil {
		t.Fatal(err)
	}
	if got := envVars(t, dir)["WEBDAV_PASS"]; got != "obscured-value" {
		t.Fatalf("WEBDAV_PASS = %q", got)
	}
	if err := EnsureSecrets(cfg, "seafile", dir,
		xcomposeapp.StringMap{"H": "bcrypt:${RCLONE_PASS}"}, captures); err != nil {
		t.Fatal(err)
	}
}
