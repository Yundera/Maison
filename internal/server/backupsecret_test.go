package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/backup/backuptest"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/incident"
)

const newSecret = "a-new-password-0123456789"

// changingBox is a provisioned box whose kopia engine can change its secret, holding
// "old-password" today.
func changingBox(t *testing.T) (*Server, *backuptest.Fake, config.Config) {
	t.Helper()
	cfg := config.Config{DataRoot: t.TempDir()}
	writeEnginePassword(t, cfg, "kopia", "old-password")
	fake := backuptest.NewChanging("kopia", filepath.Join(cfg.BackupEngineDir("kopia"), "repository.password"))
	return recoveringServer(t, cfg, fake, backuptest.NewRemote("restic")), fake, cfg
}

func postChange(t *testing.T, s *Server, engine, body string) int {
	t.Helper()
	rec := secretRoute(t, "/api/backup/engines/{id}/secret", s.handleChangeSecret,
		"/api/backup/engines/"+engine+"/secret", body)
	return rec.Code
}

// Every refusal the change route can give, then the change itself.
func TestChangeSecretRouteStatuses(t *testing.T) {
	s, fake, _ := changingBox(t)
	ok := `{"key":"` + newSecret + `","confirmed":true}`

	for _, tc := range []struct {
		name, engine, body string
		want               int
	}{
		{"unknown engine", "nope", ok, http.StatusNotFound},
		{"engine without the verb", "restic", ok, http.StatusNotImplemented},
		{"local engine", apps.EngineLocal, ok, http.StatusNotImplemented},
		{"no body", "kopia", ``, http.StatusBadRequest},
		{"too short", "kopia", `{"key":"short","confirmed":true}`, http.StatusBadRequest},
		{"not confirmed", "kopia", `{"key":"` + newSecret + `"}`, http.StatusBadRequest},
	} {
		if got := postChange(t, s, tc.engine, tc.body); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("a refused request reached the engine: %v", fake.Calls)
	}

	// A box waiting for recovery has no working key to change from.
	fake.Stat = &apps.EngineStatus{NeedsRecovery: true}
	if got := postChange(t, s, "kopia", ok); got != http.StatusConflict {
		t.Errorf("needs recovery: %d, want 409", got)
	}
	fake.Stat = nil

	fake.ChangeErr = apps.ErrWrongKey
	if got := postChange(t, s, "kopia", ok); got != http.StatusBadRequest {
		t.Errorf("stale current key: %d, want 400", got)
	}
	fake.ChangeErr = errors.New("storage unreachable")
	if got := postChange(t, s, "kopia", ok); got != http.StatusBadGateway {
		t.Errorf("engine failure: %d, want 502", got)
	}
	fake.ChangeErr = nil

	if got := postChange(t, s, "kopia", ok); got != http.StatusOK {
		t.Fatalf("change: %d, want 200", got)
	}
	es, _ := s.secretOf("kopia")
	if pw, _ := es.read(); pw != newSecret {
		t.Errorf("secret on disk = %q, want the new one", pw)
	}
}

// The new secret is the user's own copy, confirmed by them: recorded as held, under the
// new key's fingerprint, and never mailed by the detector.
func TestAChangedSecretIsHeldNotMailed(t *testing.T) {
	s, _, cfg := changingBox(t)
	if err := writeKeySent(cfg, "kopia", keySentRecord{SentAt: time.Now(), To: "u@example.com", Fingerprint: keyFingerprint("old-password")}); err != nil {
		t.Fatal(err)
	}
	if got := postChange(t, s, "kopia", `{"key":"`+newSecret+`","confirmed":true}`); got != http.StatusOK {
		t.Fatalf("change: %d", got)
	}
	rec, sent := readKeySent(cfg, "kopia")
	if !sent || !rec.HeldByUser || !rec.SentAt.IsZero() || rec.Fingerprint != keyFingerprint(newSecret) {
		t.Fatalf("receipt = %+v, want held by the user, for the new key", rec)
	}
	if keyNeedsMail(cfg, "kopia", newSecret) {
		t.Error("the changed secret reads as needing a mail")
	}
	if got := s.tryMailKey(context.Background()); got != keyMailNothing {
		t.Errorf("detector pass after a change: outcome %d, want nothing to mail", got)
	}
}

// The incident lifecycle: raised only after the state has held for consecutive passes,
// resolved by a mail receipt, raised again when the key changes under it, and resolved
// by a change the user confirmed.
func TestTheSecretIncident(t *testing.T) {
	s, fake, cfg := changingBox(t)
	id := secretIncidentID("kopia")
	d := &detector{streak: map[string]int{}}
	pass := func() {
		d.seen = map[string]bool{}
		s.checkBackupSecrets(context.Background(), d)
		d.prune()
	}

	pass()
	if s.incidents.IsOpen(id) {
		t.Fatal("raised on the first pass — a fresh box waiting for its relay would flash an incident")
	}
	pass()
	if !s.incidents.IsOpen(id) {
		t.Fatal("a secret that exists only on the box raised nothing")
	}

	if err := writeKeySent(cfg, "kopia", keySentRecord{SentAt: time.Now(), Fingerprint: keyFingerprint("old-password")}); err != nil {
		t.Fatal(err)
	}
	pass()
	if s.incidents.IsOpen(id) {
		t.Fatal("still open after the secret was mailed")
	}

	// A reset space mints a new key: the old receipt no longer covers it.
	writeEnginePassword(t, cfg, "kopia", "minted-by-a-reset")
	pass()
	pass()
	if !s.incidents.IsOpen(id) {
		t.Fatal("a new key nobody has a copy of raised nothing")
	}

	if got := postChange(t, s, "kopia", `{"key":"`+newSecret+`","confirmed":true}`); got != http.StatusOK {
		t.Fatalf("change: %d", got)
	}
	if s.incidents.IsOpen(id) {
		t.Error("still open after the user changed and confirmed the secret")
	}

	// An engine waiting for recovery is backup.recovery's to report, not this one's.
	if err := os.Remove(keySentPath(cfg, "kopia")); err != nil {
		t.Fatal(err)
	}
	fake.Stat = &apps.EngineStatus{NeedsRecovery: true}
	pass()
	pass()
	if s.incidents.IsOpen(id) {
		t.Error("raised for an engine waiting for recovery")
	}

	// An engine that leaves the set takes its incident with it.
	s.incidents.Report(incident.Report{ID: secretIncidentID("gone"), Kind: incident.KindBackupSecret, Severity: incident.Warning, Title: "x"})
	pass()
	if s.incidents.IsOpen(secretIncidentID("gone")) {
		t.Error("the incident of an engine no longer on the box was left open")
	}
}
