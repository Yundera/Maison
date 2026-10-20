package apps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yundera/maison/internal/appenv"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/envinject"
)

const secretsBase = `services: {}
x-compose-app:
  title: {en_us: Outline}
  secrets:
    DB_PASSWORD: alnum:32
    ADMIN_HASH: bcrypt:${ADMIN_PASSWORD}
  credentials:
    - label: {en_us: Administrator}
      username: admin
      password: ${ADMIN_PASSWORD}
`

const secretsEnv = `PUID=1000
DB_PASSWORD=dbsecretvalue
ADMIN_PASSWORD=adminsecretvalue
ADMIN_HASH=$2a$10$hashhashhash
SMTP_TOKEN=tokenvalue
LOG_LEVEL=debug
`

func newSecretsRegistry(t *testing.T) *Registry {
	t.Helper()
	r := New(config.Config{DataRoot: t.TempDir()}, nil)
	seedCredentialsApp(t, r, "outline", secretsBase, "", secretsEnv)
	return r
}

func TestGetEnvMasksAndClassifiesSecrets(t *testing.T) {
	r := newSecretsRegistry(t)
	got, err := r.GetEnv("outline")
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]EnvEntry{}
	for _, e := range got {
		byKey[e.Key] = e
	}
	for _, k := range []string{"DB_PASSWORD", "ADMIN_PASSWORD", "ADMIN_HASH"} {
		e := byKey[k]
		if e.Source != appenv.SourceSecret || !e.Masked || e.Value != "" {
			t.Errorf("%s = %+v, want a masked secret with no value", k, e)
		}
	}
	if e := byKey["SMTP_TOKEN"]; e.Source != "" || !e.Masked || e.Value != "" {
		t.Errorf("SMTP_TOKEN = %+v, want masked by name, not locked", e)
	}
	if e := byKey["LOG_LEVEL"]; e.Masked || e.Value != "debug" {
		t.Errorf("LOG_LEVEL = %+v, want shown as is", e)
	}
	if e := byKey["PUID"]; e.Source != appenv.SourceMaison || e.Masked {
		t.Errorf("PUID = %+v, want maison and unmasked", e)
	}
}

func TestProtectEnvKeepsLockedAndMaskedValues(t *testing.T) {
	r := newSecretsRegistry(t)
	old, err := os.ReadFile(filepath.Join(r.cfg.AppsDir(), "outline", ".env"))
	if err != nil {
		t.Fatal(err)
	}
	// What a client sends after: blanking every masked row (as GetEnv served them),
	// trying to change one secret, dropping another, and editing its own variable.
	in := []envinject.Var{
		{Key: "PUID", Value: "0"},
		{Key: "DB_PASSWORD", Value: "hijacked"},
		{Key: "ADMIN_HASH", Value: ""},
		{Key: "SMTP_TOKEN", Value: ""},
		{Key: "LOG_LEVEL", Value: "info"},
		{Key: "NEW_VAR", Value: "x"},
	}
	got := map[string]string{}
	for _, v := range r.protectEnv("outline", old, in) {
		got[v.Key] = v.Value
	}
	want := map[string]string{
		"PUID":           "1000",
		"DB_PASSWORD":    "dbsecretvalue",
		"ADMIN_PASSWORD": "adminsecretvalue", // dropped by the client, kept
		"ADMIN_HASH":     "$2a$10$hashhashhash",
		"SMTP_TOKEN":     "tokenvalue", // masked, sent blank = unchanged
		"LOG_LEVEL":      "info",
		"NEW_VAR":        "x",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestProtectEnvLetsMaskedUnlockedRowsChangeAndGo(t *testing.T) {
	r := newSecretsRegistry(t)
	old, _ := os.ReadFile(filepath.Join(r.cfg.AppsDir(), "outline", ".env"))

	got := r.protectEnv("outline", old, []envinject.Var{{Key: "SMTP_TOKEN", Value: "newtoken"}})
	vals := map[string]string{}
	for _, v := range got {
		vals[v.Key] = v.Value
	}
	if vals["SMTP_TOKEN"] != "newtoken" {
		t.Errorf("SMTP_TOKEN = %q, want the typed value", vals["SMTP_TOKEN"])
	}
	if _, ok := vals["LOG_LEVEL"]; ok {
		t.Error("LOG_LEVEL was left out and must be deleted")
	}
	got = r.protectEnv("outline", old, []envinject.Var{})
	for _, v := range got {
		if v.Key == "SMTP_TOKEN" {
			t.Error("SMTP_TOKEN was left out and must be deleted: it is masked, not locked")
		}
	}
}

func TestRedactEnvHidesSecretValues(t *testing.T) {
	r := newSecretsRegistry(t)
	in := "DB: dbsecretvalue\nADMIN: adminsecretvalue\nHASH: $2a$10$hashhashhash\nLEVEL: debug\nUID: 1000\n"
	out := r.redactEnv("outline", in)
	for _, v := range []string{"dbsecretvalue", "adminsecretvalue", "hashhashhash"} {
		if strings.Contains(out, v) {
			t.Errorf("%q left in:\n%s", v, out)
		}
	}
	if !strings.Contains(out, "LEVEL: debug") || !strings.Contains(out, "UID: 1000") {
		t.Errorf("non-secret values must survive:\n%s", out)
	}
}

func TestRedactValuesLongestFirstAndSkipsShort(t *testing.T) {
	got := redactValues("abcdef abcd abc", []string{"abcd", "abcdef", "abc"})
	if got != redactMask+" "+redactMask+" abc" {
		t.Fatalf("got %q", got)
	}
}
