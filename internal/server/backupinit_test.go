package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/backup/backuptest"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/incident"
)

func postInit(t *testing.T, s *Server, engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Post("/api/backup/engines/{id}/init", s.handleInitEngine)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/api/backup/engines/"+engine+"/init", strings.NewReader(body)))
	return rec
}

func codeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	return e.Code
}

const goodPassword = "correct horse battery staple"

// Every answer the route can give, in the order a real owner meets them, with the code
// a script branches on.
func TestInitRouteStatuses(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	writeEnginePassword(t, cfg, "kopia", "placeholder") // creates the engine directory
	pwPath := filepath.Join(cfg.BackupEngineDir("kopia"), "repository.password")
	fake := backuptest.NewAwaitingSecret("kopia", pwPath)
	s := recoveringServer(t, cfg, fake, backuptest.NewRemote("restic"))

	for _, c := range []struct {
		name, engine, body string
		status             int
		code               string
	}{
		{"unknown engine", "nope", `{"key":"` + goodPassword + `","confirmed":true}`, http.StatusNotFound, codeUnknownEngine},
		{"engine without Caps.Init", "restic", `{"key":"` + goodPassword + `","confirmed":true}`, http.StatusNotImplemented, codeNotSupported},
		{"not json", "kopia", `{`, http.StatusBadRequest, codeInvalidBody},
		{"short password", "kopia", `{"key":"   fifteen chars  ","confirmed":true}`, http.StatusBadRequest, codeTooShort},
		{"not confirmed", "kopia", `{"key":"` + goodPassword + `"}`, http.StatusBadRequest, codeNotConfirmed},
	} {
		rec := postInit(t, s, c.engine, c.body)
		if rec.Code != c.status || codeOf(t, rec) != c.code {
			t.Errorf("%s: %d %s, want %d [%s]", c.name, rec.Code, rec.Body.String(), c.status, c.code)
		}
		if strings.Contains(rec.Body.String(), goodPassword) {
			t.Errorf("%s: the response repeats the password", c.name)
		}
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("a refused request reached the engine: %v", fake.Calls)
	}

	rec := postInit(t, s, "kopia", `{"key":"  `+goodPassword+`\n","confirmed":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("good password: %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	// Recorded as held by the owner, under the password the engine now holds: never
	// mailed back to them.
	if r, sent := readKeySent(cfg, "kopia"); !sent || !r.HeldByUser || r.Fingerprint != keyFingerprint(goodPassword) {
		t.Errorf("receipt = %+v, want held-by-user for the new password", r)
	}

	// The engine is no longer waiting: a second init is a conflict, not a re-create.
	if rec := postInit(t, s, "kopia", `{"key":"`+goodPassword+`","confirmed":true}`); rec.Code != http.StatusConflict || codeOf(t, rec) != codeNotAwaitingSecret {
		t.Errorf("an engine no longer waiting: %d %s, want 409 [not_awaiting_secret]", rec.Code, rec.Body.String())
	}
}

// A space that filled up between the status and the init: the engine refuses (exit 13)
// and the owner is pointed at recovery rather than told something broke.
func TestInitIntoAnExistingRepositoryIsAConflict(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	fake := backuptest.NewAwaitingSecret("kopia", "")
	fake.InitErr = apps.ErrRepositoryExists
	s := recoveringServer(t, cfg, fake)

	rec := postInit(t, s, "kopia", `{"key":"`+goodPassword+`","confirmed":true}`)
	if rec.Code != http.StatusConflict || codeOf(t, rec) != codeRepositoryExists {
		t.Fatalf("got %d %s, want 409 [repository_exists]", rec.Code, rec.Body.String())
	}
}

// The status reports whether the form is to be offered, and only for an engine that
// can take it.
func TestBackupStatusReportsNeedsSecret(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	s := recoveringServer(t, cfg, backuptest.NewAwaitingSecret("kopia", ""), backuptest.NewRemote("restic"))

	rec := httptest.NewRecorder()
	s.handleBackupStatus(rec, httptest.NewRequest("GET", "/api/backup/status", nil))
	var out struct {
		Engines []engineInfo `json:"engines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]engineInfo{}
	for _, e := range out.Engines {
		got[e.ID] = e
	}
	if k := got["kopia"]; !k.NeedsSecret || !k.CanInit {
		t.Errorf("kopia = needs_secret %v, can_init %v; want both", k.NeedsSecret, k.CanInit)
	}
	if r := got["restic"]; r.NeedsSecret || r.CanInit {
		t.Errorf("restic = needs_secret %v, can_init %v; want neither", r.NeedsSecret, r.CanInit)
	}
}

// The bell keeps asking for a password until one is set — whatever the schedule says,
// like recovery — and replaces "cannot be reached" rather than joining it.
func TestTheDetectorRaisesAndResolvesSecretMissing(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	fake := backuptest.NewAwaitingSecret("kopia", "")
	s := recoveringServer(t, cfg, fake)

	s.checkBackup(context.Background())
	id := incident.KindBackupSecretMissing + ":kopia"
	if !s.incidents.IsOpen(id) {
		t.Fatal("an engine waiting for a password raised nothing")
	}
	if s.incidents.IsOpen("backup.engine:kopia") || s.incidents.IsOpen(incident.KindBackupRecovery+":kopia") {
		t.Error("an engine waiting for a password was also reported unreachable or in recovery")
	}

	if rec := postInit(t, s, "kopia", `{"key":"`+goodPassword+`","confirmed":true}`); rec.Code != http.StatusOK {
		t.Fatalf("init: %d %s", rec.Code, rec.Body.String())
	}
	if s.incidents.IsOpen(id) {
		t.Error("the incident outlived the init")
	}
	s.checkBackup(context.Background())
	if s.incidents.IsOpen(id) {
		t.Error("the detector reopened the incident after the init")
	}
}

// The deployment's tools reach the same handlers over the admin socket, and get the
// same JSON the dashboard does.
func TestAdminSocketCarriesTheBackupRoutes(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	s := recoveringServer(t, cfg, backuptest.NewAwaitingSecret("kopia", ""))
	admin := s.adminRouter()

	if rec := do(t, admin, "GET", "/backup/status", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"needs_secret":true`) {
		t.Errorf("status over the socket: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, admin, "POST", "/backup/engines/kopia/recover", `{"key":"k"}`); rec.Code != http.StatusNotImplemented && rec.Code != http.StatusConflict {
		t.Errorf("recover over the socket: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, admin, "POST", "/backup/engines/kopia/init", `{"key":"`+goodPassword+`","confirmed":true}`); rec.Code != 200 {
		t.Errorf("init over the socket: %d %s", rec.Code, rec.Body.String())
	}
}
