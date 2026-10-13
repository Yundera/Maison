package stackup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/xcomposeapp"
	"golang.org/x/crypto/bcrypt"
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

	if err := EnsureSecrets(cfg, "odysseus", dir, secrets, nil, nil); err != nil {
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
		if err := EnsureSecrets(cfg, "odysseus", dir, secrets, nil, nil); err != nil {
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
	}, nil, nil)
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

	if err := EnsureSecrets(cfg, "docmost", dir, xcomposeapp.StringMap{"APP_SECRET": "hex:32"}, nil, nil); err != nil {
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
		xcomposeapp.StringMap{"DEX_HASH": "bcrypt:${APP_DEFAULT_PASSWORD}"}, nil, nil); err != nil {
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
		xcomposeapp.StringMap{"H": "bcrypt:${NOPE_NOT_SET}"}, nil, nil); err == nil {
		t.Fatal("want an error for an unresolved variable")
	}
	// So must a generator nobody implements — the store author needs telling.
	if err := EnsureSecrets(cfg, "app", appDir(t, ""),
		xcomposeapp.StringMap{"K": "openssl rand -hex 32"}, nil, nil); err == nil {
		t.Fatal("want an error for an unknown generator")
	}
}

// An installed app moved to a seeded spec keeps the shared password its database
// was initialised with; a fresh install of the same listing gets its own.
func TestEnsureSecretsSeedFromPrior(t *testing.T) {
	cfg := testConfig(t)
	secrets := xcomposeapp.StringMap{"DB_PASSWORD": "${APP_DEFAULT_PASSWORD}|alnum:32"}

	// Update of an installed app: its .env had the shared password before this up.
	prior := map[string]string{"APP_DEFAULT_PASSWORD": "old|shared:pw"}
	dir := appDir(t, "APP_DEFAULT_PASSWORD=old|shared:pw\n")
	if err := EnsureSecrets(cfg, "immich", dir, secrets, prior, nil); err != nil {
		t.Fatal(err)
	}
	if got := envVars(t, dir)["DB_PASSWORD"]; got != "old|shared:pw" {
		t.Fatalf("update: DB_PASSWORD = %q, want the prior shared password", got)
	}

	// Fresh install: Sync has written APP_DEFAULT_PASSWORD into the .env by now,
	// but there was no .env before — so the generator runs.
	dir = appDir(t, "APP_DEFAULT_PASSWORD=old|shared:pw\n")
	if err := EnsureSecrets(cfg, "immich", dir, secrets, nil, nil); err != nil {
		t.Fatal(err)
	}
	fresh := envVars(t, dir)["DB_PASSWORD"]
	if len(fresh) != 32 || fresh == "old|shared:pw" {
		t.Fatalf("fresh install: DB_PASSWORD = %q, want 32 generated characters", fresh)
	}

	// Once there, it is an ordinary secret: a later prior never replaces it.
	if err := EnsureSecrets(cfg, "immich", dir, secrets, prior, nil); err != nil {
		t.Fatal(err)
	}
	if got := envVars(t, dir)["DB_PASSWORD"]; got != fresh {
		t.Fatalf("seed replaced an existing secret: %q → %q", fresh, got)
	}

	// An empty prior value counts as absent, and the next seed is tried.
	dir = appDir(t, "")
	if err := EnsureSecrets(cfg, "immich", dir,
		xcomposeapp.StringMap{"K": "${A}|${B}|hex:4"},
		map[string]string{"A": "", "B": "from-b"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := envVars(t, dir)["K"]; got != "from-b" {
		t.Fatalf("K = %q, want the first non-empty seed", got)
	}
}

// A hash of a sibling secret works on a fresh install, though the hash's key
// sorts first and the sibling is not in the .env yet.
func TestEnsureSecretsBcryptOfASibling(t *testing.T) {
	cfg := testConfig(t)
	dir := appDir(t, "")
	if err := EnsureSecrets(cfg, "outline", dir, xcomposeapp.StringMap{
		"A_ADMIN_HASH":     "bcrypt:${Z_ADMIN_PASSWORD}",
		"Z_ADMIN_PASSWORD": "password:24",
	}, nil, nil); err != nil {
		t.Fatal(err)
	}
	got := envVars(t, dir)
	if err := bcrypt.CompareHashAndPassword([]byte(got["A_ADMIN_HASH"]), []byte(got["Z_ADMIN_PASSWORD"])); err != nil {
		t.Fatalf("A_ADMIN_HASH is not the hash of Z_ADMIN_PASSWORD: %v", err)
	}
}

// A seed value is a password: it must never surface in an error.
func TestEnsureSecretsSeedNeverInErrors(t *testing.T) {
	cfg := testConfig(t)
	err := EnsureSecrets(cfg, "app", appDir(t, "APP_DEFAULT_PASSWORD=s3cret-value\n"),
		xcomposeapp.StringMap{"K": "${APP_DEFAULT_PASSWORD}|rsa:2048"}, nil, nil)
	if err == nil {
		t.Fatal("want an error for an unknown generator")
	}
	if strings.Contains(err.Error(), "s3cret-value") {
		t.Fatalf("error leaks the seed value: %v", err)
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
		xcomposeapp.StringMap{"H": "bcrypt:${RCLONE_PASS}"}, nil, captures); err != nil {
		t.Fatal(err)
	}
}
