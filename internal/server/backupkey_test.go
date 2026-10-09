package server

import (
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
)

// escrowEngine is the engine these fixtures provision. Any name will do, and that is
// the point: Maison no longer ships an engine, so there is no longer a real one to
// reach for here. A box's engines come from the adapter descriptors the host side
// wrote, none of which exists under t.TempDir(), so the set a real Server builds in a
// test holds the local engine alone — and the local engine escrows nothing.
const escrowEngine = "sealed"

// sealedServer is a box provisioned with one encrypting engine.
//
// It builds the Server directly rather than through New() because what these tests are
// about is what the escrow finds in the SET, and a Server built by New() under a temp
// root discovers no engines at all. The fake stands in for the adapter a provisioned
// box would have registered.
func sealedServer(t *testing.T, cfg config.Config, engines ...apps.Provider) *Server {
	t.Helper()
	if len(engines) == 0 {
		engines = []apps.Provider{backuptest.NewFake(escrowEngine, apps.Caps{Encrypted: true, KeyEscrow: true, Offsite: true})}
	}
	return &Server{
		cfg:        cfg,
		engines:    backup.New(append([]apps.Provider{apps.NewLocalProvider(cfg)}, engines...)...),
		backupConf: backupconfig.New(filepath.Join(cfg.StateDir(), "backup.json")),
	}
}

// writePassword renders the repository password the host-side script would have put
// there — the only thing that makes a box "provisioned" as far as this code is
// concerned.
func writePassword(t *testing.T, cfg config.Config, pw string) {
	t.Helper()
	writeEnginePassword(t, cfg, escrowEngine, pw)
}

// secretRoute serves one per-engine secret route the way the router does.
func secretRoute(t *testing.T, pattern string, h http.HandlerFunc, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Post(pattern, h)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
	return rec
}

func showSecret(t *testing.T, s *Server, engine string) *httptest.ResponseRecorder {
	t.Helper()
	return secretRoute(t, "/api/backup/engines/{id}/secret/show", s.handleShowSecret,
		"/api/backup/engines/"+engine+"/secret/show", "")
}

// The key has to be reachable from the dashboard, because showing it there is the copy
// path that keeps the secret on the box — the whole reason it sits next to the mail.
func TestShowSecretReturnsTheRepositoryPassword(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	writePassword(t, cfg, "correct horse battery staple")

	rec := showSecret(t, sealedServer(t, cfg), escrowEngine)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["key"] != "correct horse battery staple" {
		t.Errorf("key = %q, want the password with its trailing newline trimmed", out["key"])
	}
	// A response carrying the one unrecoverable secret on the box must not be
	// storable: a cached copy outlives the tab it was read in.
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("Cache-Control = %q, want it to forbid storing", got)
	}
}

// Each engine answers for its own secret: an unknown engine is a 404, one that holds no
// secret (the local engine) and one not yet provisioned are a 400.
func TestShowSecretIsPerEngine(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	s := sealedServer(t, cfg)
	for _, tc := range []struct {
		engine string
		want   int
	}{{"nope", http.StatusNotFound}, {apps.EngineLocal, http.StatusBadRequest}, {escrowEngine, http.StatusBadRequest}} {
		if rec := showSecret(t, s, tc.engine); rec.Code != tc.want {
			t.Errorf("%s: %d, want %d (%s)", tc.engine, rec.Code, tc.want, rec.Body.String())
		}
	}
}

// The status page decides what to offer from these facts, so each has to be true for
// the right reasons, and per engine: has_key hides a button that would show something
// that does not exist, secret_sent says whether a copy has ever left the box, and the
// label is the engine's own name for the secret.
func TestStatusReportsEachEnginesSecret(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	sealed := backuptest.NewFake(escrowEngine, apps.Caps{Encrypted: true, KeyEscrow: true, Offsite: true})
	sealed.Declared = &apps.SecretSpec{Label: "Kopia repository password", File: "repository.password", Escrow: true}

	engine := func() engineInfo {
		rec := httptest.NewRecorder()
		sealedServer(t, cfg, sealed).handleBackupStatus(rec, httptest.NewRequest("GET", "/api/backup/status", nil))
		var out engineStatus
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		for _, e := range out.Engines {
			if e.ID == escrowEngine {
				return e
			}
		}
		t.Fatalf("no %s engine in %s", escrowEngine, rec.Body.String())
		return engineInfo{}
	}

	if e := engine(); e.HasKey || !e.Escrow || e.SecretLabel != "Kopia repository password" {
		t.Errorf("unprovisioned engine = %+v, want no key, escrow, and the declared label", e)
	}
	writePassword(t, cfg, "s3cret")
	if e := engine(); !e.HasKey || e.SecretSent != nil {
		t.Errorf("provisioned engine = %+v, want a key and no receipt yet", e)
	}
	if err := writeKeySent(cfg, escrowEngine, keySentRecord{SentAt: time.Now(), To: "u@example.com", Fingerprint: keyFingerprint("s3cret")}); err != nil {
		t.Fatal(err)
	}
	if e := engine(); e.SecretSent == nil || e.SecretSent.To != "u@example.com" {
		t.Errorf("engine = %+v, want the receipt once a copy has been mailed", e)
	}
	// A receipt for a key that has since changed describes a copy that opens nothing.
	writePassword(t, cfg, "changed")
	if e := engine(); e.SecretSent != nil {
		t.Errorf("engine = %+v, want no receipt for a key that is no longer the one on disk", e)
	}
}

// The receipt is the only thing standing between "sent once" and "sent on every
// restart", so its read has to fail towards silence.
func TestAMalformedReceiptCountsAsAlreadySent(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	if _, sent := readKeySent(cfg, escrowEngine); sent {
		t.Fatal("no receipt file reads as sent")
	}
	path := keySentPath(cfg, escrowEngine)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, sent := readKeySent(cfg, escrowEngine); !sent {
		t.Error("a truncated receipt reads as never sent — which mails the key again every boot")
	}
}

// The box-wide receipt from before receipts were per engine still counts, so an upgrade
// mails nothing: for the engine it names, for every engine when it names none (written
// before the engine field), and — malformed — for every engine. A receipt naming another
// engine says nothing about this one, and the engine's own receipt wins once written.
func TestTheLegacyReceiptStillCounts(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	writeLegacy := func(body string) {
		t.Helper()
		if err := os.MkdirAll(cfg.StateDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(legacyKeySentPath(cfg), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	writeLegacy(`{"sent_at":"2026-01-01T00:00:00Z","engine":"kopia","fingerprint":"` + keyFingerprint("k") + `"}`)
	if keyNeedsMail(cfg, "kopia", "k") {
		t.Error("the legacy receipt for this engine and key did not count — an upgrade would re-mail it")
	}
	if !keyNeedsMail(cfg, "other", "k") {
		t.Error("a legacy receipt naming another engine covered this one")
	}

	writeLegacy(`{"sent_at":"2026-01-01T00:00:00Z"}`)
	if keyNeedsMail(cfg, "kopia", "anything") {
		t.Error("a legacy receipt from before the engine field did not count")
	}
	writeLegacy(`{not json`)
	if keyNeedsMail(cfg, "kopia", "anything") {
		t.Error("a malformed legacy receipt triggered a send")
	}

	if err := writeKeySent(cfg, "kopia", keySentRecord{SentAt: time.Now(), Fingerprint: keyFingerprint("old")}); err != nil {
		t.Fatal(err)
	}
	if !keyNeedsMail(cfg, "kopia", "new") {
		t.Error("the engine's own receipt for an older key was overridden by the legacy one")
	}
	if _, err := os.Stat(legacyKeySentPath(cfg)); err != nil {
		t.Errorf("the legacy receipt was moved or removed: %v", err)
	}
}

// EnsureKeyEmailed must return rather than block when there is nothing it can do:
// it runs on the boot path, and both of these are the ordinary state of a box.
func TestEnsureKeyEmailedReturnsWhenThereIsNothingToSend(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(config.Config)
	}{
		{"no repository on the box", func(config.Config) {}},
		{"a copy has already been mailed", func(cfg config.Config) {
			writePassword(t, cfg, "s3cret")
			if err := writeKeySent(cfg, escrowEngine, keySentRecord{SentAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{DataRoot: t.TempDir()}
			tc.setup(cfg)
			s := sealedServer(t, cfg)

			done := make(chan struct{})
			go func() { s.EnsureKeyEmailed(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("EnsureKeyEmailed blocked; it must not wait on a send it cannot make")
			}
		})
	}
}

// Which engines' secrets get escrowed comes from what each engine declares — or, for one
// that declares nothing, from Caps.KeyEscrow — never from a named engine, and never just
// the first: a second engine with a key only this box holds is exactly as unrecoverable.
func TestEscrowCoversEveryEngineThatNeedsIt(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}

	plain := backuptest.NewFake("plain", apps.Caps{})
	sealed := backuptest.NewFake("sealed", apps.Caps{Encrypted: true, KeyEscrow: true})
	declared := backuptest.NewFake("declared", apps.Caps{Encrypted: true})
	declared.Declared = &apps.SecretSpec{Label: "Restic repository password", File: "restic.key", Escrow: true}
	held := backuptest.NewFake("held", apps.Caps{Encrypted: true, KeyEscrow: true})
	held.Declared = &apps.SecretSpec{File: "repository.password", Escrow: false}
	srv := &Server{cfg: cfg, engines: backup.New(plain, sealed, declared, held)}

	got := map[string]engineSecret{}
	for _, es := range srv.escrowSecrets() {
		got[es.Engine] = es
	}
	if len(got) != 2 {
		t.Fatalf("escrowed %v, want exactly sealed and declared", got)
	}
	if es := got["sealed"]; es.Path != filepath.Join(cfg.BackupEngineDir("sealed"), "repository.password") {
		t.Errorf("sealed: path %q, want the default repository.password", es.Path)
	}
	if es := got["declared"]; es.Path != filepath.Join(cfg.BackupEngineDir("declared"), "restic.key") || es.Spec.Label == "" {
		t.Errorf("declared: %+v, want the declared file and label", es)
	}

	writeEnginePassword(t, cfg, "sealed", "correct horse battery staple")
	if pw, err := got["sealed"].read(); err != nil || pw != "correct horse battery staple" {
		t.Errorf("read = %q, %v", pw, err)
	}
}

// The mail carries the engine's own name for its secret, so whoever restores with the
// engine's tools on another machine recognises what they are being asked for.
func TestTheMailUsesTheEnginesName(t *testing.T) {
	spec := apps.SecretSpec{Label: "Kopia repository password"}
	if got := keyMailSubject(spec); got != "Your Kopia repository password" {
		t.Errorf("subject = %q", got)
	}
	if body := keyMailBody(spec, "pw"); !strings.Contains(body, "Kopia repository password") || !strings.Contains(body, "pw") {
		t.Errorf("body = %q", body)
	}
	if got := keyMailSubject(apps.SecretSpec{}); got != "Your backup encryption key" {
		t.Errorf("undeclared subject = %q, want the generic one", got)
	}
}

// writeEnginePassword renders one engine's password the way the host-side script would,
// with the trailing newline a shell writes — the read's trim is what stops that newline
// becoming part of the key.
func writeEnginePassword(t *testing.T, cfg config.Config, engine, pw string) {
	t.Helper()
	dir := cfg.BackupEngineDir(engine)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "repository.password"), []byte(pw+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
