package apps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yundera/maison/internal/config"
)

func seedCredentialsApp(t *testing.T, r *Registry, id, base, override, env string) string {
	t.Helper()
	dir := filepath.Join(r.cfg.AppsDir(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if body == "" {
			return
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("docker-compose.yml", base)
	write("docker-compose.override.yml", override)
	write(".env", env)
	return dir
}

const credentialsBase = `services: {}
x-compose-app:
  title: Outline
  credentials:
    - label: Administrator
      username: admin
      password: ${ADMIN_PASSWORD}
      note: Change it after first login.
    - label: Empty
      username: ""
`

func TestRenderedCredentialsResolvesFromEnv(t *testing.T) {
	r := New(config.Config{DataRoot: t.TempDir()}, nil)
	seedCredentialsApp(t, r, "outline", credentialsBase, "", "ADMIN_PASSWORD=s3cret\n")

	got, err := r.RenderedCredentials("outline")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("credentials = %+v, want the empty entry dropped", got)
	}
	want := RenderedCredential{Label: "Administrator", Username: "admin", Password: "s3cret", Note: "Change it after first login."}
	if got[0] != want {
		t.Fatalf("got %+v, want %+v", got[0], want)
	}
}

func TestRenderedCredentialsUnresolvedStaysVisible(t *testing.T) {
	r := New(config.Config{DataRoot: t.TempDir()}, nil)
	seedCredentialsApp(t, r, "outline", credentialsBase, "", "")

	got, err := r.RenderedCredentials("outline")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0].Password, "ADMIN_PASSWORD") {
		t.Fatalf("an unresolved reference must stay visible, got %+v", got)
	}
}

func TestRenderedCredentialsOverrideReplacesList(t *testing.T) {
	r := New(config.Config{DataRoot: t.TempDir()}, nil)
	over := "x-compose-app:\n  credentials:\n    - username: root\n      password: hunter2\n"
	seedCredentialsApp(t, r, "outline", credentialsBase, over, "ADMIN_PASSWORD=s3cret\n")

	got, err := r.RenderedCredentials("outline")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Username != "root" || got[0].Password != "hunter2" {
		t.Fatalf("override should replace the base list, got %+v", got)
	}
}

func TestCredentialsDismissedRoundTrip(t *testing.T) {
	r := New(config.Config{DataRoot: t.TempDir()}, nil)
	dir := seedCredentialsApp(t, r, "outline", credentialsBase, "", "")

	if r.CredentialsDismissed("outline") {
		t.Fatal("dismissed before anything was recorded")
	}
	if err := r.SetCredentialsDismissed("outline", true); err != nil {
		t.Fatal(err)
	}
	if !r.CredentialsDismissed("outline") {
		t.Fatal("not dismissed after SetCredentialsDismissed(true)")
	}
	if _, err := os.Stat(filepath.Join(dir, credentialsDismissedFile)); err != nil {
		t.Fatalf("marker must live in the app folder (uninstall clears it): %v", err)
	}
	if err := r.SetCredentialsDismissed("outline", false); err != nil {
		t.Fatal(err)
	}
	if r.CredentialsDismissed("outline") {
		t.Fatal("still dismissed after SetCredentialsDismissed(false)")
	}
	if err := r.SetCredentialsDismissed("outline", false); err != nil {
		t.Fatalf("clearing twice must be idempotent: %v", err)
	}
	if err := r.SetCredentialsDismissed("missing", true); err == nil {
		t.Fatal("an app without a folder must be refused")
	}
	if err := r.SetCredentialsDismissed("../x", true); err == nil {
		t.Fatal("a path-like id must be refused")
	}
}
