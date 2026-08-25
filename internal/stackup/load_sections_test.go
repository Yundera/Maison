package stackup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yundera/maison/internal/xcomposeapp"
)

// The declarations an app writes have to survive the whole path from YAML to
// Spec — including the two shapes YAML would otherwise retype (a quoted octal
// mode, a numeric variable) and the two compose spellings init accepts.
func TestLoadReadsTheDeclarativeSections(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(path, []byte(`
name: odysseus
services:
  odysseus:
    image: example/odysseus:1
x-compose-app:
  schema_version: 2
  secrets:
    SEARXNG_SECRET: hex:32
    DEX_HASH: bcrypt:${APP_DEFAULT_PASSWORD}
  variables:
    OUTLINE_URL: https://outline-${APP_DOMAIN}
    WORKERS: 4
  files:
    - path: /DATA/AppData/odysseus/element/config.json
      from: element/config.json.tmpl
      ensure: always
      mode: "0640"
  init:
    - name: seed-db
      image: filebrowser/filebrowser:v2.63.2
      command: config init --database /db/database.db
      user: "${PUID}:${PGID}"
      environment:
        LOG_LEVEL: debug
      volumes:
        - /DATA/AppData/odysseus/db:/db
      when: absent:/DATA/AppData/odysseus/db/database.db
    - name: post-seed
      image: example/seed:1
      entrypoint: ["/seed"]
      phase: post_up
      capture: SEED_TOKEN
`), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := Load([]string{path})

	if spec.Secrets["SEARXNG_SECRET"] != "hex:32" ||
		spec.Secrets["DEX_HASH"] != "bcrypt:${APP_DEFAULT_PASSWORD}" {
		t.Fatalf("secrets = %v", spec.Secrets)
	}
	// A number in a variable value must survive as text — YAML would type it as
	// an int, and .env has only strings.
	if spec.Variables["WORKERS"] != "4" {
		t.Fatalf("variables = %v", spec.Variables)
	}

	if len(spec.Files) != 1 {
		t.Fatalf("files = %v", spec.Files)
	}
	f := spec.Files[0]
	// The leading zero is the whole trap: a bare 0640 would arrive as 416.
	if f.Mode != "0640" || f.Ensure != xcomposeapp.EnsureAlways || f.From != "element/config.json.tmpl" {
		t.Fatalf("file = %+v", f)
	}

	if len(spec.Init) != 2 {
		t.Fatalf("init = %v", spec.Init)
	}
	// The string form of command is what an author types; it has to arrive as argv.
	wantCmd := []string{"config", "init", "--database", "/db/database.db"}
	got := spec.Init[0]
	if len(got.Command) != len(wantCmd) {
		t.Fatalf("command = %v, want %v", got.Command, wantCmd)
	}
	for i, w := range wantCmd {
		if got.Command[i] != w {
			t.Fatalf("command = %v, want %v", got.Command, wantCmd)
		}
	}
	if len(got.Env) != 1 || got.Env[0] != "LOG_LEVEL=debug" {
		t.Fatalf("environment = %v, want the mapping form normalised to KEY=VALUE", got.Env)
	}
	if got.When != "absent:/DATA/AppData/odysseus/db/database.db" {
		t.Fatalf("when = %q", got.When)
	}
	if spec.Init[1].Phase != xcomposeapp.PhasePostUp || spec.Init[1].Capture != "SEED_TOKEN" {
		t.Fatalf("post-up step = %+v", spec.Init[1])
	}
	if len(spec.Init[1].Entrypoint) != 1 || spec.Init[1].Entrypoint[0] != "/seed" {
		t.Fatalf("entrypoint = %v", spec.Init[1].Entrypoint)
	}
}
