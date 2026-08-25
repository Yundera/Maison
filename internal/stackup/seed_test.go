package stackup

import (
	"os"
	"path/filepath"
	"testing"
)

// seedApp makes an app folder whose .seed tree holds files, keyed by their
// path relative to .seed.
func seedApp(t *testing.T, env string, files map[string]string) string {
	t.Helper()
	dir := appDir(t, env)
	for rel, content := range files {
		path := filepath.Join(dir, SeedDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestEnsureSeedMirrorsTreeAndRendersTemplates(t *testing.T) {
	cfg := testConfig(t)
	dir := seedApp(t, "APP_DOMAIN=example.test\nSEARXNG_SECRET=s3cret\n", map[string]string{
		"Caddyfile":                 "root * /srv\n",
		"www/index.html":            "<h1>hi</h1>",
		"searxng/settings.yml.tmpl": "secret_key: \"${SEARXNG_SECRET}\"\nhost: ${APP_DOMAIN}\n",
	})

	if err := EnsureSeed(cfg, "caddy", dir, nil, nil); err != nil {
		t.Fatal(err)
	}

	// A path in the store is the path on disk — no destination declared anywhere.
	if got := read(t, filepath.Join(dir, "Caddyfile")); got != "root * /srv\n" {
		t.Fatalf("Caddyfile = %q", got)
	}
	if got := read(t, filepath.Join(dir, "www/index.html")); got != "<h1>hi</h1>" {
		t.Fatalf("index.html = %q", got)
	}
	// Rendered, and the .tmpl suffix is gone.
	want := "secret_key: \"s3cret\"\nhost: example.test\n"
	if got := read(t, filepath.Join(dir, "searxng/settings.yml")); got != want {
		t.Fatalf("settings.yml = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "searxng/settings.yml.tmpl")); err == nil {
		t.Fatal("the .tmpl suffix survived into the app folder")
	}
}

// The failure this whole mechanism exists to end: a template that references
// something nobody provides must fail the up, not write the literal ${VAR}.
func TestEnsureSeedFailsOnUnresolvedVariable(t *testing.T) {
	cfg := testConfig(t)
	dir := seedApp(t, "", map[string]string{
		"conf/app.yml.tmpl": "key: ${NOPE_NOT_PROVIDED}\n",
	})

	err := EnsureSeed(cfg, "app", dir, nil, nil)
	if err == nil {
		t.Fatal("want an error for an unresolved variable")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "conf/app.yml")); statErr == nil {
		t.Fatal("wrote the file anyway — the exact old behaviour")
	}
}

func TestEnsureSeedIsCreateIfAbsent(t *testing.T) {
	cfg := testConfig(t)
	dir := seedApp(t, "", map[string]string{"Caddyfile": "from the store\n"})

	if err := EnsureSeed(cfg, "caddy", dir, nil, nil); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "Caddyfile")
	if err := os.WriteFile(target, []byte("edited by the operator\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Every later up re-ensures, and must not clobber the edit.
	for i := 0; i < 3; i++ {
		if err := EnsureSeed(cfg, "caddy", dir, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := read(t, target); got != "edited by the operator\n" {
		t.Fatalf("an edit was overwritten: %q", got)
	}

	// ...but a file that has gone missing comes back. That is the accepted cost
	// of the ensure contract: a deliberate deletion returns too.
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSeed(cfg, "caddy", dir, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := read(t, target); got != "from the store\n" {
		t.Fatalf("a deleted file did not come back: %q", got)
	}
}

func TestEnsureSeedKeepsExecBitAndBinaryContent(t *testing.T) {
	cfg := testConfig(t)
	dir := seedApp(t, "", map[string]string{"db/init.sh": "#!/bin/sh\necho hi\n"})
	if err := os.Chmod(filepath.Join(dir, SeedDir, "db/init.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A "binary" the renderer must not touch: it carries a $ that is not a
	// variable, and no .tmpl suffix.
	binary := []byte{0x00, 0x01, '$', 'N', 'O', 'T', 0xff}
	if err := os.WriteFile(filepath.Join(dir, SeedDir, "db/dump.bin"), binary, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := EnsureSeed(cfg, "guacamole", dir, nil, nil); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, "db/init.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("exec bit lost: mode %v", info.Mode().Perm())
	}
	if got := read(t, filepath.Join(dir, "db/dump.bin")); got != string(binary) {
		t.Fatalf("binary content changed: %q", got)
	}
}

func TestEnsureSeedRefusesReservedAndEscapingTargets(t *testing.T) {
	cfg := testConfig(t)

	for _, name := range []string{"docker-compose.yml", "docker-compose.override.yml", ".env"} {
		dir := seedApp(t, "", map[string]string{name: "nope"})
		if err := EnsureSeed(cfg, "app", dir, nil, nil); err == nil {
			t.Errorf("%s: want an error, the app's identity is not the seed tree's to write", name)
		}
	}

	// A symlink in a downloaded tree can name anything on the filesystem.
	dir := seedApp(t, "", map[string]string{"keep": "x"})
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, SeedDir, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := EnsureSeed(cfg, "app", dir, nil, nil); err == nil {
		t.Error("want an error for a symlink in the seed tree")
	}
}

func TestEnsureSeedRefusesTwoEntriesForOneTarget(t *testing.T) {
	cfg := testConfig(t)
	dir := seedApp(t, "", map[string]string{
		"conf/app.yml":      "plain\n",
		"conf/app.yml.tmpl": "templated\n",
	})
	if err := EnsureSeed(cfg, "app", dir, nil, nil); err == nil {
		t.Fatal("want an error: which of the two wins would depend on walk order")
	}
}

// A path a files entry owns is left for it, so nothing is written twice and an
// ensure: always file is never briefly seeded with a stale render first.
func TestEnsureSeedSkipsClaimedPaths(t *testing.T) {
	cfg := testConfig(t)
	dir := seedApp(t, "", map[string]string{"conf/app.yml": "seeded\n"})
	claimed := map[string]bool{filepath.Join(dir, "conf/app.yml"): true}

	if err := EnsureSeed(cfg, "app", dir, claimed, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "conf/app.yml")); err == nil {
		t.Fatal("seed wrote a path that files owns")
	}
}

func TestEnsureSeedNoTreeIsNotAnError(t *testing.T) {
	if err := EnsureSeed(testConfig(t), "app", appDir(t, ""), nil, nil); err != nil {
		t.Fatalf("an app with no seed tree must converge cleanly: %v", err)
	}
}
