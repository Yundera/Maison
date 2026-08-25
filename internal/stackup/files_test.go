package stackup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// dataRootApp makes an app folder at the place a real one lives —
// ${DataRoot}/AppData/<project> — so declared paths spelled /DATA/... resolve
// onto it the way they do on a box.
func dataRootApp(t *testing.T, project, env string) (config.Config, string) {
	t.Helper()
	cfg := testConfig(t)
	dir := filepath.Join(cfg.AppsDir(), project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if env != "" {
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return cfg, dir
}

func TestEnsureFilesAlwaysFollowsTheDeployment(t *testing.T) {
	cfg, dir := dataRootApp(t, "tuwunel", "APP_DOMAIN=old.test\n")
	files := []xcomposeapp.File{{
		Path:    "/DATA/AppData/${AppID}/element/config.json",
		Content: "{\"base_url\": \"https://matrix-${APP_DOMAIN}\"}",
		Ensure:  xcomposeapp.EnsureAlways,
	}}
	target := filepath.Join(dir, "element/config.json")

	if err := EnsureFiles(cfg, "tuwunel", dir, files, nil); err != nil {
		t.Fatal(err)
	}
	if got := read(t, target); got != "{\"base_url\": \"https://matrix-old.test\"}" {
		t.Fatalf("config.json = %q", got)
	}

	// The deployment moves. A hook wrote this once at install and never revisited
	// it; ensure: always is the fix.
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("APP_DOMAIN=new.test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureFiles(cfg, "tuwunel", dir, files, nil); err != nil {
		t.Fatal(err)
	}
	if got := read(t, target); got != "{\"base_url\": \"https://matrix-new.test\"}" {
		t.Fatalf("config.json did not follow the domain: %q", got)
	}
}

func TestEnsureFilesOnceLeavesAnExistingFileAlone(t *testing.T) {
	cfg, dir := dataRootApp(t, "odoo", "")
	files := []xcomposeapp.File{{
		Path:    "/DATA/AppData/${AppID}/odoo.conf",
		Content: "from the declaration\n",
	}}

	if err := EnsureFiles(cfg, "odoo", dir, files, nil); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "odoo.conf")
	if err := os.WriteFile(target, []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureFiles(cfg, "odoo", dir, files, nil); err != nil {
		t.Fatal(err)
	}
	if got := read(t, target); got != "edited\n" {
		t.Fatalf("ensure: once overwrote an edit: %q", got)
	}
}

// An always-file that already says the right thing must not be rewritten, so a
// converge does not churn its mtime on every start.
func TestEnsureFilesAlwaysDoesNotRewriteUnchanged(t *testing.T) {
	cfg, dir := dataRootApp(t, "app", "")
	files := []xcomposeapp.File{{
		Path:    "/DATA/AppData/${AppID}/conf.yml",
		Content: "stable\n",
		Ensure:  xcomposeapp.EnsureAlways,
	}}
	if err := EnsureFiles(cfg, "app", dir, files, nil); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "conf.yml")
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureFiles(cfg, "app", dir, files, nil); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("rewrote a file whose content had not changed")
	}
}

func TestEnsureFilesFromSeedTree(t *testing.T) {
	cfg, dir := dataRootApp(t, "guacamole", "APP_DOMAIN=x.test\n")
	seed := filepath.Join(dir, SeedDir, "db")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "init.sh"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := EnsureFiles(cfg, "guacamole", dir, []xcomposeapp.File{{
		Path: "/DATA/AppData/${AppID}/db/init.sh",
		From: "db/init.sh",
		Mode: "0755",
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "db/init.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755", info.Mode().Perm())
	}
}

func TestEnsureFilesDeclarationErrors(t *testing.T) {
	cfg, dir := dataRootApp(t, "app", "")

	cases := []struct {
		name string
		file xcomposeapp.File
	}{
		{"neither from nor content", xcomposeapp.File{Path: "/DATA/AppData/app/a"}},
		{"both from and content", xcomposeapp.File{
			Path: "/DATA/AppData/app/b", From: "a", Content: "b"}},
		{"unresolved variable in the path", xcomposeapp.File{
			Path: "/DATA/AppData/${NOPE}/c", Content: "y"}},
		{"relative path", xcomposeapp.File{Path: "conf/d", Content: "y"}},
		{"outside the data root", xcomposeapp.File{Path: "/etc/cron.d/e", Content: "y"}},
		{"mode that is not octal", xcomposeapp.File{
			Path: "/DATA/AppData/app/f", Content: "y", Mode: "999"}},
		{"unknown ensure", xcomposeapp.File{
			Path: "/DATA/AppData/app/g", Content: "y", Ensure: "sometimes"}},
		// Each case gets its own path: with ensure: once an existing file is
		// skipped before anything is rendered, so a shared path would hide this.
		{"unresolved variable in the body", xcomposeapp.File{
			Path: "/DATA/AppData/app/h", Content: "k: ${NOPE_NOT_SET}"}},
		{"missing seed source", xcomposeapp.File{
			Path: "/DATA/AppData/app/i", From: "not/in/the/tree"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := EnsureFiles(cfg, "app", dir, []xcomposeapp.File{c.file}, nil); err == nil {
				t.Fatal("want a declaration error")
			}
		})
	}
}

func TestClaimedPathsMatchesWhatSeedWouldWrite(t *testing.T) {
	cfg, dir := dataRootApp(t, "app", "")
	claimed := ClaimedPaths(cfg, "app", dir, []xcomposeapp.File{
		{Path: "/DATA/AppData/${AppID}/conf/app.yml", Content: "x"},
		{Path: "/DATA/AppData/${NOPE}/broken", Content: "x"}, // skipped, not fatal
	})
	if !claimed[filepath.Join(dir, "conf/app.yml")] {
		t.Fatalf("claimed = %v, want the app-dir path seed compares against", claimed)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed = %v, want only the resolvable entry", claimed)
	}
}
