package usersettings

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yundera/maison/internal/domains"
	"github.com/yundera/maison/internal/notify"
)

func hasAll(string) bool { return true }

func TestLegacyFlatShapesLoadAndAreRewrittenWrapped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	legacy := `{"wallpaper":"/w.jpg","smtp":{"host":"relay","port":25,"from":"a@b","to":"c@d"},` +
		`"domains":[{"name":"lan","domain":"{name}.lan"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(path)
	got := s.Get()
	if got.SMTP == nil || got.SMTP.Host != "relay" || len(got.Domains) != 1 || got.Wallpaper != "/w.jpg" {
		t.Fatalf("legacy file not read: %+v", got)
	}
	if len(got.Managed) != 0 {
		t.Fatalf("legacy values must load unmanaged, got %v", got.Managed)
	}

	if err := s.Set(Settings{Language: "fr_fr"}); err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	var w wrapped
	if err := json.Unmarshal(doc["smtp"], &w); err != nil || w.Managed || !strings.Contains(string(w.Value), `"relay"`) {
		t.Fatalf("smtp not rewritten wrapped: %s", doc["smtp"])
	}
	if _, ok := doc["managed"]; ok {
		t.Fatalf("the API's managed map must not be written top-level: %s", b)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("settings.json mode = %v, want 0600", fi.Mode().Perm())
	}

	// And the rewritten file reads back to the same thing.
	again := New(path).Get()
	if again.SMTP == nil || again.SMTP.Host != "relay" || again.Language != "fr_fr" || len(again.Domains) != 1 {
		t.Fatalf("round trip lost data: %+v", again)
	}
}

func TestManagedFieldIsImmuneToUserEdits(t *testing.T) {
	s := newStore(t)
	if err := s.Manage("smtp", "mesh", json.RawMessage(`{"host":"smtp","port":587,"from":"","to":""}`), hasAll); err != nil {
		t.Fatal(err)
	}
	// What the dashboard does: send the whole object back with a changed field.
	edit := s.Get()
	edit.Wallpaper = "/new.jpg"
	edit.SMTP = &notify.SMTP{Host: "evil", Port: 25}
	edit.Managed = map[string]string{} // a client trying to drop the flag
	if err := s.Set(edit); err != nil {
		t.Fatal(err)
	}
	got := s.Get()
	if got.Wallpaper != "/new.jpg" {
		t.Fatalf("the unmanaged part of the edit was lost: %q", got.Wallpaper)
	}
	if got.SMTP.Host != "smtp" {
		t.Fatalf("a user edit changed a managed setting: %+v", got.SMTP)
	}
	if o, ok := got.IsManaged("smtp"); !ok || o != "mesh" {
		t.Fatalf("managed flag lost: %v", got.Managed)
	}

	// And it survives a reload.
	re := New(s.path).Get()
	if o, ok := re.IsManaged("smtp"); !ok || o != "mesh" || re.SMTP.Host != "smtp" {
		t.Fatalf("managed state not persisted: %+v", re)
	}
}

func TestReleaseClearsAndReturnsSecretsButLeavesUserValuesAlone(t *testing.T) {
	s := newStore(t)

	// Not managed: a user's own value survives a release (the nightly no-op case).
	if err := s.Set(Settings{SMTP: &notify.SMTP{Host: "mine"}}); err != nil {
		t.Fatal(err)
	}
	if refs, err := s.Release("smtp"); err != nil || refs != nil {
		t.Fatalf("release of an unmanaged setting: %v, %v", refs, err)
	}
	if s.Get().SMTP == nil || s.Get().SMTP.Host != "mine" {
		t.Fatal("release cleared a value the user owns")
	}

	raw := json.RawMessage(`{"url":"https://ops.example/fb","token":"${secret:feedback_token}"}`)
	if err := s.Manage("feedback", "yundera", raw, hasAll); err != nil {
		t.Fatal(err)
	}
	refs, err := s.Release("feedback")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0] != "feedback_token" {
		t.Fatalf("refs = %v, want [feedback_token]", refs)
	}
	got := s.Get()
	if got.Feedback != nil {
		t.Fatalf("released value not cleared: %+v", got.Feedback)
	}
	if _, ok := got.IsManaged("feedback"); ok {
		t.Fatal("released setting still managed")
	}
}

func TestManageRefusesBadInput(t *testing.T) {
	s := newStore(t)
	none := func(string) bool { return false }
	cases := []struct {
		name, owner, raw string
		has              func(string) bool
	}{
		{"wallpaper", "yundera", `"x"`, hasAll},                                // not manageable
		{"smtp", "Yundera Team", `{"host":"smtp"}`, hasAll},                    // bad owner
		{"smtp", "mesh", `{"host":""}`, hasAll},                                // no host
		{"smtp", "mesh", `{"host":"smtp","colour":"blue"}`, hasAll},            // unknown field
		{"smtp", "mesh", `{"host":"smtp","pass":"${secret:smtp_pass}"}`, none}, // secret missing
		{"feedback", "yundera", `{"url":"javascript:alert(1)","token":"t"}`, hasAll},
		{"onboarding", "yundera", `{"url":"/relative"}`, hasAll},
		{"domains", "yundera", `[{"name":"a","domain":"x"},{"name":"b","domain":"x"}]`, hasAll},
	}
	for _, c := range cases {
		if err := s.Manage(c.name, c.owner, json.RawMessage(c.raw), c.has); err == nil {
			t.Errorf("Manage(%s, %s, %s) accepted", c.name, c.owner, c.raw)
		}
	}
	if _, err := s.Release("wallpaper"); !errors.Is(err, ErrUnknownSetting) {
		t.Errorf("Release(wallpaper) err = %v, want ErrUnknownSetting", err)
	}
	if len(s.Get().Managed) != 0 {
		t.Fatalf("a refused Manage left state behind: %v", s.Get().Managed)
	}
}

func TestManagedDomainsAreNormalized(t *testing.T) {
	s := newStore(t)
	raw := json.RawMessage(`[{"name":" lan ","domain":" {name}.lan ","labels":{" ":"x","k":" v "}}]`)
	if err := s.Manage("domains", "yundera", raw, hasAll); err != nil {
		t.Fatal(err)
	}
	want := domains.Domain{Name: "lan", Domain: "{name}.lan", Labels: map[string]string{"k": "v"}}
	got := s.Get().Domains
	if len(got) != 1 || got[0].Name != want.Name || got[0].Domain != want.Domain || got[0].Labels["k"] != "v" || len(got[0].Labels) != 1 {
		t.Fatalf("domains = %+v, want %+v", got, want)
	}
}

func TestManagedSMTPIsNotLayeredOverTheEnv(t *testing.T) {
	s := newStore(t)
	if err := s.Manage("smtp", "mesh", json.RawMessage(`{"host":"smtp","port":587}`), hasAll); err != nil {
		t.Fatal(err)
	}
	prov := notify.SMTP{Host: "env-relay", Port: 25, User: "envuser", From: "noreply@box", To: "owner@box"}
	got := s.EffectiveSMTP(prov)
	if got.Host != "smtp" || got.User != "" {
		t.Fatalf("env transport leaked into a managed one: %+v", got)
	}
	if got.From != "noreply@box" || got.To != "owner@box" {
		t.Fatalf("addresses should still fall back to the deployment identity: %+v", got)
	}
}
