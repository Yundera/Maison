package stackup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yundera/maison/internal/xcomposeapp"
)

// The declarations an app writes have to survive the whole path from YAML to
// Spec — including the two shapes YAML would otherwise retype (a quoted octal
// mode, a numeric variable) and both forms of a step's script entry.
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
  steps:
    - name: seed-db
      at: setup
      run: always
      images: [filebrowser/filebrowser:v2.63.23]
      script: |
        [ -e "$APP_DIR/db/database.db" ] && exit 0
        docker run --rm filebrowser/filebrowser:v2.63.23 config init
    - name: post-seed
      at: started
      file: scripts/post-seed.sh
      timeout: 5m
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

	if len(spec.Steps) != 2 {
		t.Fatalf("steps = %+v", spec.Steps)
	}
	seed, post := spec.Steps[0], spec.Steps[1]
	if seed.At != xcomposeapp.AtSetup || seed.RunMode() != xcomposeapp.RunAlways ||
		!strings.Contains(seed.Script.Script, "config init") || len(seed.Images) != 1 {
		t.Fatalf("setup step = %+v", seed)
	}
	if post.At != xcomposeapp.AtStarted || post.RunMode() != xcomposeapp.RunOnce || post.File != "scripts/post-seed.sh" || post.Timeout != "5m" {
		t.Fatalf("started step = %+v", post)
	}
}
