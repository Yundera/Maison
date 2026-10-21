package usersettings

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yundera/maison/internal/notify"
	"github.com/yundera/maison/internal/routing"
)

func hasAll(string) bool { return true }

func TestUnmanagedValuesStayInTheShapeOlderImagesRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	legacy := `{"wallpaper":"/w.jpg","smtp":{"host":"relay","port":25,"from":"a@b","to":"c@d"},` +
		`"domains":[{"name":"lan","domain":"{name}.lan"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(path)
	got := s.Get()
	// The old domains list became the routing setting: the primary, then the entry.
	if got.SMTP == nil || got.SMTP.Host != "relay" || got.Routing == nil || len(got.Routing.Domains) != 2 || got.Wallpaper != "/w.jpg" {
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
	// Plain, not wrapped: an image before managed settings must still read it.
	var smtp notify.SMTP
	if err := json.Unmarshal(doc["smtp"], &smtp); err != nil || smtp.Host != "relay" || strings.Contains(string(doc["smtp"]), `"managed"`) {
		t.Fatalf("unmanaged smtp not written plain: %s", doc["smtp"])
	}
	var r routing.Setting
	if err := json.Unmarshal(doc["routing"], &r); err != nil || len(r.Domains) != 2 || strings.Contains(string(doc["routing"]), `"managed"`) {
		t.Fatalf("unmanaged routing not written plain: %s", doc["routing"])
	}
	if _, ok := doc["domains"]; ok {
		t.Fatalf("the migrated domains list was written back: %s", b)
	}
	if _, ok := doc["managed"]; ok {
		t.Fatalf("the API's managed map must not be written top-level: %s", b)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("settings.json mode = %v, want 0600", fi.Mode().Perm())
	}

	// And the rewritten file reads back to the same thing.
	again := New(path).Get()
	if again.SMTP == nil || again.SMTP.Host != "relay" || again.Language != "fr_fr" || again.Routing == nil || len(again.Routing.Domains) != 2 {
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
		{"onboarding", "yundera", `{"check_url":"/relative"}`, hasAll},
		// The pre-check shape is refused on the way in; on disk it still reads, as off.
		{"onboarding", "yundera", `{"url":"https://admin-box.example/"}`, hasAll},
		{"onboarding", "yundera", `{"check_url":"http://admin-app/api/onboarding/check","token":"${secret:onboarding_token}"}`, none},
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

func TestManagedRoutingIsNormalized(t *testing.T) {
	s := newStore(t)
	raw := json.RawMessage(`{"renderer":" labels ","domains":[{"name":" lan ","domain":" {name}.lan ","labels":{" ":"x","k":" v "}}]}`)
	if err := s.Manage("routing", "yundera", raw, hasAll); err != nil {
		t.Fatal(err)
	}
	got := s.Get().Routing
	if got == nil || got.Renderer != routing.Labels || len(got.Domains) != 1 {
		t.Fatalf("routing = %+v", got)
	}
	if d := got.Domains[0]; d.Name != "lan" || d.Domain != "{name}.lan" || d.Labels["k"] != "v" || len(d.Labels) != 1 {
		t.Fatalf("domain = %+v", d)
	}
	// The editor cannot change a managed setting.
	if err := s.SetRouting(nil); !errors.Is(err, ErrManaged) {
		t.Fatalf("SetRouting on a managed setting = %v, want ErrManaged", err)
	}
	if err := s.Manage("routing", "yundera", json.RawMessage(`{"renderer":"labels"}`), hasAll); err == nil {
		t.Fatal("a labels setting with no domain was accepted")
	}
}

// A managed domains list from before the routing setting keeps its owner.
func TestManagedLegacyDomainsMigrateWithTheirOwner(t *testing.T) {
	st, migrated, err := decodeFile([]byte(`{"domains":{"managed":true,"owner":"Yundera","value":[{"name":"nip","domain":"${APP_PUBLIC_IP_DASH}.nip.io"}]}}`))
	if err != nil || !migrated {
		t.Fatalf("decode: migrated=%v err=%v", migrated, err)
	}
	if st.Routing == nil || st.Managed["routing"] != "Yundera" {
		t.Fatalf("routing = %+v managed = %v", st.Routing, st.Managed)
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

// Maison 1.1.50 wrote unmanaged values wrapped with "managed": false. A file from it
// reads back unmanaged, and the next save writes it plain.
func TestWrappedUnmanagedFrom1150IsReadAndWrittenPlain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	old := `{"domains":{"managed":false,"value":[{"name":"nip","domain":"x.nip.io"}]},` +
		`"smtp":{"managed":true,"owner":"Yundera","value":{"host":"smtp","port":587}}}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(path)
	got := s.Get()
	if got.Routing == nil || len(got.Routing.Domains) != 2 || got.Managed["smtp"] != "Yundera" {
		t.Fatalf("1.1.50 file misread: %+v", got)
	}
	if _, ok := got.IsManaged("routing"); ok {
		t.Fatal("managed:false read as managed")
	}
	if err := s.Set(Settings{Language: "de_de"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(doc["routing"])), "{") || strings.Contains(string(doc["routing"]), `"managed"`) {
		t.Fatalf("routing not written plain: %s", doc["routing"])
	}
	if !strings.Contains(string(doc["smtp"]), `"managed":true`) && !strings.Contains(string(doc["smtp"]), `"managed": true`) {
		t.Fatalf("managed smtp lost its wrapper: %s", doc["smtp"])
	}
}

// A settings.json written by a Maison before the check carries {"url": …}. It must
// still parse — a box rolled forward must not lose its whole settings file — and it
// must read as the feature switched off, not as a check against nothing.
func TestTheOldOnboardingShapeReadsAsOff(t *testing.T) {
	st, _, err := decodeFile([]byte(`{"onboarding":{"managed":true,"owner":"yundera","value":{"url":"https://admin-box.example/"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if st.Onboarding == nil || st.Onboarding.CheckURL != "" {
		t.Fatalf("onboarding = %+v, want a value with no check_url", st.Onboarding)
	}
}

// The onboarding token is a secret reference like the feedback token, so releasing the
// setting hands it back for deletion.
func TestOnboardingTokenIsASecretRef(t *testing.T) {
	s := newStore(t)
	raw := json.RawMessage(`{"check_url":"http://admin-app/api/onboarding/check","token":"${secret:onboarding_token}"}`)
	if err := s.Manage("onboarding", "yundera", raw, hasAll); err != nil {
		t.Fatal(err)
	}
	refs, err := s.Release("onboarding")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0] != "onboarding_token" {
		t.Fatalf("release returned %v, want [onboarding_token]", refs)
	}
}
