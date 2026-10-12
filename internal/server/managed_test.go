package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/notify"
	"github.com/yundera/maison/internal/secrets"
	"github.com/yundera/maison/internal/usersettings"
)

func newManagedServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Config{DataRoot: t.TempDir()}
	return &Server{
		cfg:      cfg,
		settings: usersettings.New(filepath.Join(cfg.StateDir(), "settings.json")),
		secrets:  secrets.New(filepath.Join(cfg.StateDir(), secrets.FileName)),
	}
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

func getSettings(t *testing.T, s *Server) (string, usersettings.Settings) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleGetSettings(rec, httptest.NewRequest("GET", "/api/settings", nil))
	var st usersettings.Settings
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return rec.Body.String(), st
}

func putSettings(t *testing.T, s *Server, st usersettings.Settings) {
	t.Helper()
	b, _ := json.Marshal(st)
	rec := httptest.NewRecorder()
	s.handlePutSettings(rec, httptest.NewRequest("PUT", "/api/settings", strings.NewReader(string(b))))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/settings = %d %s", rec.Code, rec.Body)
	}
}

// A box upgraded from before secrets.env carries its password in settings.json. The
// API must never return it, and the boot-time move puts it where it belongs.
func TestALiteralPasswordIsMaskedThenMovedToTheSecretStore(t *testing.T) {
	s := newManagedServer(t)
	if err := s.settings.Set(usersettings.Settings{SMTP: &notify.SMTP{Host: "relay", Pass: "hunter2"}}); err != nil {
		t.Fatal(err)
	}
	body, _ := getSettings(t, s)
	if strings.Contains(body, "hunter2") {
		t.Fatalf("GET returned the password: %s", body)
	}

	s.adoptLiteralSecrets()
	body, st := getSettings(t, s)
	if strings.Contains(body, "hunter2") || st.SMTP.Pass != secrets.Ref(userSMTPPass) {
		t.Fatalf("after the move GET should show the reference: %s", body)
	}
	if v, _ := s.secrets.Get(userSMTPPass); v != "hunter2" {
		t.Fatalf("secret store holds %q", v)
	}
	raw, _ := os.ReadFile(filepath.Join(s.cfg.StateDir(), "settings.json"))
	if strings.Contains(string(raw), "hunter2") {
		t.Fatalf("settings.json still holds the password: %s", raw)
	}
	if got := s.mailTransport().Pass; got != "hunter2" {
		t.Fatalf("mail transport resolved pass %q", got)
	}
}

// The dashboard sends back what GET gave it. An echoed reference keeps the password,
// a typed one replaces it, an emptied one removes it.
func TestUserEditsOfThePassword(t *testing.T) {
	s := newManagedServer(t)
	_, st := getSettings(t, s)
	st.SMTP = &notify.SMTP{Host: "relay", Port: 587, Pass: "first"}
	putSettings(t, s, st)

	_, st = getSettings(t, s)
	if st.SMTP.Pass != secrets.Ref(userSMTPPass) {
		t.Fatalf("typed password not replaced by a reference: %q", st.SMTP.Pass)
	}
	st.Wallpaper = "/other.jpg" // an unrelated edit echoing the reference back
	putSettings(t, s, st)
	if v, _ := s.secrets.Get(userSMTPPass); v != "first" {
		t.Fatalf("echoed reference changed the password to %q", v)
	}

	_, st = getSettings(t, s)
	st.SMTP.Pass = "${secret:something_else}" // a reference the client invented
	putSettings(t, s, st)
	if _, st2 := getSettings(t, s); st2.SMTP.Pass != secrets.Ref(userSMTPPass) {
		t.Fatalf("an invented reference was stored: %q", st2.SMTP.Pass)
	}

	_, st = getSettings(t, s)
	st.SMTP.Pass = ""
	putSettings(t, s, st)
	if s.secrets.Has(userSMTPPass) {
		t.Fatal("emptied password still in the secret store")
	}
}

// Nothing the open HTTP API receives can change a managed setting, or set the
// deployment-only ones at all.
func TestTheHTTPAPICannotTouchDeploymentSettings(t *testing.T) {
	s := newManagedServer(t)
	admin := s.adminRouter()
	if rec := do(t, admin, "PUT", "/settings/smtp?owner=mesh", `{"host":"smtp","port":587}`); rec.Code != 200 {
		t.Fatalf("manage smtp: %d %s", rec.Code, rec.Body)
	}

	_, st := getSettings(t, s)
	if st.Managed["smtp"] != "mesh" {
		t.Fatalf("GET does not report the owner: %v", st.Managed)
	}
	st.SMTP = &notify.SMTP{Host: "attacker", Port: 25, Pass: "x"}
	st.Feedback = &usersettings.Feedback{URL: "https://attacker.example", Token: "t"}
	st.Onboarding = &usersettings.Onboarding{URL: "https://attacker.example"}
	st.Managed = nil
	putSettings(t, s, st)

	got := s.settings.Get()
	if got.SMTP.Host != "smtp" || got.Feedback != nil || got.Onboarding != nil || got.Managed["smtp"] != "mesh" {
		t.Fatalf("a user edit reached deployment settings: %+v", got)
	}
	if s.secrets.Has(userSMTPPass) {
		t.Fatal("a password typed against a managed transport was stored")
	}

	if rec := do(t, admin, "PUT", "/settings/domains?owner=yundera", `[]`); rec.Code != 200 {
		t.Fatalf("manage domains: %d %s", rec.Code, rec.Body)
	}
	rec := httptest.NewRecorder()
	s.handlePutDomains(rec, httptest.NewRequest("PUT", "/api/settings/domains", strings.NewReader(`[]`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("PUT domains while managed = %d, want 409", rec.Code)
	}
}

func TestAdminManageReleaseAndSecrets(t *testing.T) {
	s := newManagedServer(t)
	admin := s.adminRouter()
	fb := `{"url":"https://ops.example/feedback","token":"${secret:feedback_token}"}`

	if rec := do(t, admin, "PUT", "/settings/feedback?owner=yundera", fb); rec.Code != http.StatusBadRequest {
		t.Fatalf("manage with a missing secret = %d, want 400", rec.Code)
	}
	if rec := do(t, admin, "PUT", "/settings/wallpaper?owner=yundera", `"x"`); rec.Code != http.StatusNotFound {
		t.Fatalf("manage an unknown setting = %d, want 404", rec.Code)
	}
	if rec := do(t, admin, "PUT", "/secrets/feedback_token", "tok-123\n"); rec.Code != 200 {
		t.Fatalf("set secret: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, admin, "PUT", "/settings/feedback?owner=yundera", fb); rec.Code != 200 {
		t.Fatalf("manage feedback: %d %s", rec.Code, rec.Body)
	}
	if c := s.feedbackClient(); c == nil {
		t.Fatal("feedback not enabled by the managed setting")
	}
	if v, _ := s.secrets.Get("feedback_token"); v != "tok-123" {
		t.Fatalf("secret stored as %q (trailing newline must be dropped)", v)
	}
	if body := do(t, admin, "GET", "/settings", "").Body.String(); strings.Contains(body, "tok-123") {
		t.Fatalf("admin GET leaked the token: %s", body)
	}

	if rec := do(t, admin, "DELETE", "/settings/feedback", ""); rec.Code != 200 {
		t.Fatalf("release: %d %s", rec.Code, rec.Body)
	}
	if s.secrets.Has("feedback_token") {
		t.Fatal("release kept the secret the managed value referenced")
	}
	if c := s.feedbackClient(); c != nil {
		t.Fatal("feedback still enabled after release")
	}
}

func TestOnboardingFollowsTheSettingAndFallsBackToTheFile(t *testing.T) {
	s := newManagedServer(t)
	if _, pending := s.onboardingURL(); pending {
		t.Fatal("pending with neither setting nor file")
	}
	file := filepath.Join(s.cfg.StateDir(), "onboarding.json")
	if err := os.WriteFile(file, []byte(`{"url":"https://admin-box.example/"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if u, pending := s.onboardingURL(); !pending || u != "https://admin-box.example/" {
		t.Fatalf("file fallback: %q %v", u, pending)
	}
	if rec := do(t, s.adminRouter(), "PUT", "/settings/onboarding?owner=yundera", `{"url":"https://claim.example/"}`); rec.Code != 200 {
		t.Fatalf("manage onboarding: %d %s", rec.Code, rec.Body)
	}
	if u, _ := s.onboardingURL(); u != "https://claim.example/" {
		t.Fatalf("setting should win over the file: %q", u)
	}
}

// The socket end to end: private, and answering the same routes.
func TestAdminSocket(t *testing.T) {
	s := newManagedServer(t)
	dir, err := os.MkdirTemp("", "msock") // short: unix socket paths are length-limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "admin.sock")
	s.startAdmin(path)

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, want 0600", fi.Mode().Perm())
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}}
	req, _ := http.NewRequest("PUT", "http://maison/settings/smtp?owner=mesh", strings.NewReader(`{"host":"smtp"}`))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("over the socket: %d %s", resp.StatusCode, b)
	}
	if got := s.settings.Get(); got.Managed["smtp"] != "mesh" || got.SMTP.Port != 587 {
		t.Fatalf("not applied: %+v", got)
	}
}
