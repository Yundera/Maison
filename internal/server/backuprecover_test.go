package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/backup"
	"github.com/yundera/maison/internal/backup/backuptest"
	"github.com/yundera/maison/internal/backupconfig"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/incident"
	"github.com/yundera/maison/internal/usersettings"
)

// recoveringServer is a rebuilt box: one engine whose storage holds backups this box
// has no key for, recoverable with "the right key".
func recoveringServer(t *testing.T, cfg config.Config, engines ...apps.Provider) *Server {
	t.Helper()
	store := backupconfig.New(filepath.Join(cfg.StateDir(), "backup.json"))
	set := backup.New(append([]apps.Provider{apps.NewLocalProvider(cfg)}, engines...)...)
	return &Server{
		cfg:         cfg,
		engines:     set,
		backupConf:  store,
		backupSched: backup.NewScheduler(cfg, nil, set, store),
		incidents:   incident.New(filepath.Join(cfg.StateDir(), "incidents.json")),
		settings:    usersettings.New(filepath.Join(cfg.StateDir(), "settings.json")),
	}
}

func postRecover(t *testing.T, s *Server, engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Post("/api/backup/engines/{id}/recover", s.handleRecoverEngine)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/api/backup/engines/"+engine+"/recover", strings.NewReader(body)))
	return rec
}

// Every answer the route can give, in the order a real user meets them: a typo, an
// empty field, the right key, and then the same key again once the box is already
// reconnected.
func TestRecoverRouteStatuses(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	fake := backuptest.NewRecovering("kopia", "the right key")
	fake.RecoverResult = apps.RecoverResult{Snapshots: 42, Pinned: 42}
	s := recoveringServer(t, cfg, fake, backuptest.NewRemote("restic"))

	if rec := postRecover(t, s, "nope", `{"key":"x"}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown engine: %d, want 404", rec.Code)
	}
	// An engine that does not declare the capability is not offered the form, and the
	// route agrees with the page rather than trying anyway.
	if rec := postRecover(t, s, "restic", `{"key":"x"}`); rec.Code != http.StatusNotImplemented {
		t.Errorf("engine without Caps.Recover: %d, want 501", rec.Code)
	}
	if rec := postRecover(t, s, "kopia", `{"key":"   "}`); rec.Code != http.StatusBadRequest {
		t.Errorf("blank key: %d, want 400", rec.Code)
	}
	rec := postRecover(t, s, "kopia", `{"key":"a wrong key"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong key: %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	// The response is about the key; it must never echo it back.
	if strings.Contains(rec.Body.String(), "a wrong key") {
		t.Errorf("the error repeats the key: %s", rec.Body.String())
	}

	// What the adapter does on success: promote the candidate to the password file.
	writeEnginePassword(t, cfg, "kopia", "the right key")
	rec = postRecover(t, s, "kopia", `{"key":"  the right key\n"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("right key: %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out apps.RecoverResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Snapshots != 42 || out.Pinned != 42 {
		t.Errorf("result = %+v, want the engine's counts", out)
	}

	if rec := postRecover(t, s, "kopia", `{"key":"the right key"}`); rec.Code != http.StatusConflict {
		t.Errorf("an engine no longer waiting for a key: %d, want 409", rec.Code)
	}
}

// A key the user just typed must not be mailed straight back to them — and the receipt
// that says so has to name THIS key, or the next reset's new key would never be sent.
func TestARecoveredKeyIsRecordedAsHeldNotMailed(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	s := recoveringServer(t, cfg, backuptest.NewRecovering("kopia", "k"))
	writeEnginePassword(t, cfg, "kopia", "k")

	if rec := postRecover(t, s, "kopia", `{"key":"k"}`); rec.Code != http.StatusOK {
		t.Fatalf("recover: %d (%s)", rec.Code, rec.Body.String())
	}
	rec, sent := readKeySent(cfg, "kopia")
	if !sent || !rec.HeldByUser || !rec.SentAt.IsZero() {
		t.Fatalf("receipt = %+v, want held-by-user with no send date", rec)
	}
	if rec.Fingerprint != keyFingerprint("k") {
		t.Errorf("receipt fingerprint = %q, want the recovered key's", rec.Fingerprint)
	}
	if keyNeedsMail(cfg, "kopia", "k") {
		t.Error("the recovered key still reads as needing a mail")
	}
}

// The fingerprint rules, one by one. The two "no" answers without a match are what keep
// an upgrade from mailing every box's key a second time.
func TestKeyNeedsMailFollowsTheFingerprint(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	if !keyNeedsMail(cfg, "kopia", "k1") {
		t.Error("no receipt: want a send")
	}

	// A receipt from before fingerprints existed covers whatever key is there.
	if err := writeKeySent(cfg, "kopia", keySentRecord{SentAt: time.Now(), To: "u@example.com"}); err != nil {
		t.Fatal(err)
	}
	if keyNeedsMail(cfg, "kopia", "k1") {
		t.Error("an old receipt without a fingerprint triggered a re-send on upgrade")
	}

	if err := writeKeySent(cfg, "kopia", keySentRecord{SentAt: time.Now(), Fingerprint: keyFingerprint("k1")}); err != nil {
		t.Fatal(err)
	}
	if keyNeedsMail(cfg, "kopia", "k1") {
		t.Error("the key that was mailed reads as needing another mail")
	}
	if !keyNeedsMail(cfg, "kopia", "k2") {
		t.Error("a changed key — a reset space — was not re-sent")
	}

	// Malformed still reads as sent.
	if err := os.WriteFile(keySentPath(cfg, "kopia"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if keyNeedsMail(cfg, "kopia", "k2") {
		t.Error("a malformed receipt triggered a send")
	}
}

// One attempt per detector pass: a changed key is noticed and attempted (here, with no
// relay, it waits for the next pass), a matching one is left alone, and an engine still
// asking for its key never has a password mailed in its name.
func TestTheDetectorPassMailsAChangedKeyOnce(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	sealed := backuptest.NewFake("kopia", apps.Caps{Encrypted: true, KeyEscrow: true, Offsite: true})
	s := recoveringServer(t, cfg, sealed)
	writeEnginePassword(t, cfg, "kopia", "new key")
	if err := writeKeySent(cfg, "kopia", keySentRecord{SentAt: time.Now(), Fingerprint: keyFingerprint("old key")}); err != nil {
		t.Fatal(err)
	}

	if got := s.tryMailKey(context.Background()); got != keyMailRetry {
		t.Errorf("changed key, no relay: outcome %d, want a retry on the next pass", got)
	}

	if err := writeKeySent(cfg, "kopia", keySentRecord{SentAt: time.Now(), Fingerprint: keyFingerprint("new key")}); err != nil {
		t.Fatal(err)
	}
	if got := s.tryMailKey(context.Background()); got != keyMailNothing {
		t.Errorf("matching key: outcome %d, want nothing to do", got)
	}

	// The same box, but its engine says the storage is someone else's repository: any
	// password lying in its directory is not the user's key.
	if err := os.Remove(keySentPath(cfg, "kopia")); err != nil {
		t.Fatal(err)
	}
	sealed.Stat = &apps.EngineStatus{NeedsRecovery: true}
	if got := s.tryMailKey(context.Background()); got != keyMailNothing {
		t.Errorf("engine in recovery: outcome %d, want no mail", got)
	}
}

// The recovery incident is raised whatever the schedule says — on a rebuilt box the
// schedule is usually off and the engine receives nothing — and closes once the key is
// in.
func TestTheDetectorRaisesAndResolvesRecovery(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	fake := backuptest.NewRecovering("kopia", "k")
	s := recoveringServer(t, cfg, fake)
	if s.backupConf.Get().Enabled {
		t.Fatal("fixture drifted: this is meant to be a box with the schedule off")
	}

	s.checkBackup(context.Background())
	id := incident.KindBackupRecovery + ":kopia"
	if !s.incidents.IsOpen(id) {
		t.Fatal("an engine asking for its key raised nothing")
	}
	// The recovery incident replaces "cannot be reached", not joins it.
	if s.incidents.IsOpen("backup.engine:kopia") {
		t.Error("a recovering engine was also reported as unreachable")
	}

	fake.Stat = &apps.EngineStatus{Configured: true, Connected: true}
	s.checkBackup(context.Background())
	if s.incidents.IsOpen(id) {
		t.Error("the recovery incident outlived the recovery")
	}
}

// A pause stays visible for as long as it lasts, and only while backups are on: off is
// a decision, not a pause.
func TestTheDetectorRaisesAndResolvesPaused(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	s := recoveringServer(t, cfg)
	now := time.Now()

	set := func(c backupconfig.Config) {
		t.Helper()
		if err := s.backupConf.Set(c); err != nil {
			t.Fatal(err)
		}
		s.checkBackup(context.Background())
	}

	set(backupconfig.Config{Enabled: true, Paused: true, PausedAt: &now})
	if !s.incidents.IsOpen(incident.IDBackupPaused) {
		t.Fatal("a paused schedule raised nothing")
	}
	set(backupconfig.Config{Enabled: true})
	if s.incidents.IsOpen(incident.IDBackupPaused) {
		t.Error("resuming left the pause incident open")
	}
	set(backupconfig.Config{Enabled: false, Paused: true})
	if s.incidents.IsOpen(incident.IDBackupPaused) {
		t.Error("a box with backups switched off was reported as paused")
	}
}

// The page needs three facts about a recovering engine to render the form.
func TestStatusReportsRecoveryToThePage(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	fake := backuptest.NewRecovering("kopia", "k")
	fake.Stat.RecoveryHelpURL = "https://app.example/dashboard/backup"
	s := recoveringServer(t, cfg, fake)

	rec := httptest.NewRecorder()
	s.handleBackupStatus(rec, httptest.NewRequest("GET", "/api/backup/status", nil))
	var out struct {
		Engines []struct {
			ID              string `json:"id"`
			NeedsRecovery   bool   `json:"needs_recovery"`
			CanRecover      bool   `json:"can_recover"`
			RecoveryHelpURL string `json:"recovery_help_url"`
		} `json:"engines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, e := range out.Engines {
		if e.ID != "kopia" {
			if e.NeedsRecovery || e.CanRecover {
				t.Errorf("%s claims recovery: %+v", e.ID, e)
			}
			continue
		}
		if !e.NeedsRecovery || !e.CanRecover || e.RecoveryHelpURL != "https://app.example/dashboard/backup" {
			t.Errorf("kopia = %+v, want needs/can recover and the help link", e)
		}
		return
	}
	t.Fatal("the recovering engine is missing from the status")
}
