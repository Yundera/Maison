package stackup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// testConfig points the data root at a temp dir and gives it a distinct host
// path, so the tests also cover the host→container path mapping a real
// deployment does (an app's compose names /DATA/..., Maison creates it under
// its own mount).
func testConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		DataRoot:     t.TempDir(),
		DataHostPath: "/host/DATA",
		PUID:         "1000",
		PGID:         "1000",
	}
}

func TestEnsureFoldersCreatesAndInterpolates(t *testing.T) {
	cfg := testConfig(t)
	folders := []xcomposeapp.Folder{
		{Path: "/DATA/AppData/${AppID}/config", Mode: "0750"},
		{Path: "${DATA_ROOT}/Media/movies"},         // host-path placeholder form
		{Path: "/host/DATA/AppData/jellyfin/cache"}, // literal host path
	}
	if err := EnsureFolders(cfg, "jellyfin", folders, nil); err != nil {
		t.Fatalf("EnsureFolders: %v", err)
	}

	for _, want := range []string{"AppData/jellyfin/config", "Media/movies", "AppData/jellyfin/cache"} {
		st, err := os.Stat(filepath.Join(cfg.DataRoot, want))
		if err != nil {
			t.Fatalf("%s: %v", want, err)
		}
		if !st.IsDir() {
			t.Fatalf("%s is not a directory", want)
		}
	}
	st, _ := os.Stat(filepath.Join(cfg.DataRoot, "AppData/jellyfin/config"))
	if got := st.Mode().Perm(); got != 0o750 {
		t.Fatalf("mode = %o, want 0750", got)
	}
	st, _ = os.Stat(filepath.Join(cfg.DataRoot, "Media/movies"))
	if got := st.Mode().Perm(); got != DefaultFolderMode {
		t.Fatalf("default mode = %o, want %o", got, DefaultFolderMode)
	}
}

// An existing folder keeps its content and gets its declared mode pinned —
// MkdirAll alone is a no-op on an existing directory.
func TestEnsureFoldersIsIdempotent(t *testing.T) {
	cfg := testConfig(t)
	dir := filepath.Join(cfg.DataRoot, "AppData/app/config")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(keep, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	f := []xcomposeapp.Folder{{Path: "/DATA/AppData/app/config", Mode: "0755"}}
	if err := EnsureFolders(cfg, "app", f, nil); err != nil {
		t.Fatalf("EnsureFolders: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("existing content lost: %v", err)
	}
	st, _ := os.Stat(dir)
	if got := st.Mode().Perm(); got != 0o755 {
		t.Fatalf("mode = %o, want 0755 (not pinned on an existing dir)", got)
	}
}

func TestEnsureFoldersRejects(t *testing.T) {
	cfg := testConfig(t)
	cases := []struct {
		name   string
		folder xcomposeapp.Folder
	}{
		{"escapes the data root", xcomposeapp.Folder{Path: "/etc/cron.d"}},
		{"traverses out of the data root", xcomposeapp.Folder{Path: "/DATA/../etc"}},
		{"relative path", xcomposeapp.Folder{Path: "AppData/app"}},
		{"unresolved variable", xcomposeapp.Folder{Path: "/DATA/AppData/${NOPE}/x"}},
		{"unquoted mode lost its leading zero", xcomposeapp.Folder{Path: "/DATA/AppData/app", Mode: "493"}},
		{"unknown user", xcomposeapp.Folder{Path: "/DATA/AppData/app", User: "nosuchuser"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := EnsureFolders(cfg, "app", []xcomposeapp.Folder{c.folder}, nil); err == nil {
				t.Fatalf("EnsureFolders(%+v) = nil, want an error", c.folder)
			}
		})
	}
	if _, err := os.Stat("/etc/cron.d"); err == nil {
		t.Fatal("a rejected folder was created anyway")
	}
}

// The .env is the app's persistent variable record, so a folder path may
// reference variables the operator set there.
func TestEnsureFoldersReadsEnvFile(t *testing.T) {
	cfg := testConfig(t)
	f := []xcomposeapp.Folder{{Path: "/DATA/AppData/app/${LIBRARY}"}}
	if err := EnsureFolders(cfg, "app", f, []byte("LIBRARY=photos\n")); err != nil {
		t.Fatalf("EnsureFolders: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataRoot, "AppData/app/photos")); err != nil {
		t.Fatalf("env-file variable not interpolated: %v", err)
	}
}

func TestLoadMergesSpec(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "docker-compose.yml")
	override := filepath.Join(dir, "docker-compose.override.yml")

	// The base carries x-casaos install commands beside its x-compose-app block,
	// which makes them dead: the override re-pins the steps and the folder list.
	if err := os.WriteFile(base, []byte(`
services:
  app:
    image: app:1
x-casaos:
  pre-install-cmd: casaos-pre
  post-install-cmd: casaos-post
x-compose-app:
  folders:
    - /DATA/AppData/app/config
  steps:
    - {name: base, at: setup, script: base-step}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(override, []byte(`
x-compose-app:
  folders:
    - path: /DATA/AppData/app/data
      recursive: true
  steps:
    - {name: mine, at: started, run: always, script: override-step}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := Load([]string{base, override})

	// One block or the other: an x-casaos command beside an x-compose-app block
	// never runs, and the override replaces the step list wholesale.
	if len(spec.Steps) != 1 || spec.Steps[0].Name != "mine" || spec.Steps[0].Script.Script != "override-step" {
		t.Fatalf("steps = %+v, want only the override's", spec.Steps)
	}
	// The override replaces the folder list wholesale (Compose extension merge is
	// key-by-key, not element-wise).
	want := []xcomposeapp.Folder{{Path: "/DATA/AppData/app/data", Recursive: true}}
	if len(spec.Folders) != 1 || spec.Folders[0] != want[0] {
		t.Fatalf("folders = %+v, want %+v", spec.Folders, want)
	}
}

// A store app with no x-compose-app block at all keeps its CasaOS install
// commands, as lenient `once` steps.
func TestLoadXCasaOSOnly(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(base, []byte(`
services:
  app:
    image: app:1
x-casaos:
  pre-install-cmd: casaos-pre
`), 0o644); err != nil {
		t.Fatal(err)
	}
	// What Maison itself writes into the override's x-compose-app does not turn
	// the app into a native one that has lost its install commands.
	override := filepath.Join(dir, "docker-compose.override.yml")
	if err := os.WriteFile(override, []byte("x-compose-app:\n  store-ref: https://store.example/-/Apps/app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := Load([]string{base, override})
	if len(spec.Steps) != 1 || spec.Steps[0].Script.Script != "casaos-pre" || spec.Steps[0].At != xcomposeapp.AtSetup || !spec.Steps[0].Lenient {
		t.Fatalf("steps = %+v, want casaos-pre as a lenient setup step", spec.Steps)
	}
	if spec.Folders != nil {
		t.Fatalf("folders = %+v, want none", spec.Folders)
	}
}

// Prepare creates declared folders and ONLY declared folders. A bind mount whose
// source Maison was never told about is left to Docker — Maison does not inspect
// `volumes:` and guess which sources are directories, because nothing in a compose
// file says whether a bind source is a directory or a file.
func TestPrepareCreatesDeclaredFoldersOnly(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	base := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(base, []byte(`
services:
  app:
    image: app:1
    volumes:
      - /DATA/AppData/app/config:/config
      - /DATA/AppData/app/Caddyfile:/etc/caddy/Caddyfile
x-compose-app:
  schema_version: 2
  folders:
    - /DATA/AppData/app/config
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Prepare(cfg, "app", dir, Load([]string{base})); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataRoot, "AppData/app/config")); err != nil {
		t.Fatalf("declared folder not created: %v", err)
	}
	// The undeclared file-style bind must be left alone: creating a directory here
	// is what breaks an app whose bind source is a config file.
	if _, err := os.Stat(filepath.Join(cfg.DataRoot, "AppData/app/Caddyfile")); !os.IsNotExist(err) {
		t.Fatalf("undeclared bind source was created (err=%v), want it left to Docker", err)
	}
}

// A script sees the app's variables (base + .env), its AppID, and APP_DIR /
// DATA_ROOT as HOST paths — what a `docker run -v` needs, valid in the shell too
// through the alias Maison creates at startup. Nothing in the text is rewritten.
// COMPOSE_PROJECT_NAME and COMPOSE_FILE point `compose` at the app's own stack.
func TestRunScriptEnvironment(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no /bin/bash")
	}
	cfg := testConfig(t)
	cfg.DataHostPath = "/hostroot"

	appDir := filepath.Join(cfg.DataRoot, "AppData", "jellyfin")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, ".env"), []byte("SECRET=s3cr3t\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	script := `
test "$AppID" = "jellyfin"
test "$PUID" = "1000"
test "$SECRET" = "s3cr3t"
test "$APP_DIR" = "/hostroot/AppData/jellyfin"
test "${DATA_ROOT}" = "/hostroot"
test "$COMPOSE_PROJECT_NAME" = "jellyfin"
test "$COMPOSE_FILE" = "` + filepath.Join(appDir, "docker-compose.yml") + `"
test "/DATA" = "/DATA"
`
	if err := runScript(t, cfg, "jellyfin", appDir, script); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if err := runScript(t, cfg, "jellyfin", appDir, "exit 3"); err == nil {
		t.Fatal("a failing script returned nil")
	}
}
