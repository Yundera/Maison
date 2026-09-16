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
// about is what escrowKey finds in the SET, and a Server built by New() under a temp
// root discovers no engines at all. The fake stands in for the adapter a provisioned
// box would have registered; what it has to get right is the one thing escrowKey asks
// of an engine, which is Caps.KeyEscrow.
func sealedServer(t *testing.T, cfg config.Config) *Server {
	t.Helper()
	return &Server{
		cfg:        cfg,
		engines:    backup.New(apps.NewLocalProvider(cfg), backuptest.NewFake(escrowEngine, apps.Caps{Encrypted: true, KeyEscrow: true, Offsite: true})),
		backupConf: backupconfig.New(filepath.Join(cfg.StateDir(), "backup.json")),
	}
}

// writePassword renders the repository password the host-side script would have put
// there — the only thing that makes a box "provisioned" as far as this code is
// concerned. Which engine the escrow *chooses* is a capability question, asserted
// separately in TestEscrowKeyPicksTheEngineByCapability.
func writePassword(t *testing.T, cfg config.Config, pw string) {
	t.Helper()
	writeEnginePassword(t, cfg, escrowEngine, pw)
}

// The key has to be reachable from the dashboard, because showing it there is the copy
// path that keeps the secret on the box — the whole reason it sits next to the mail.
func TestShowKeyReturnsTheRepositoryPassword(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{DataRoot: root}
	writePassword(t, cfg, "correct horse battery staple")

	srv := sealedServer(t, cfg)
	rec := httptest.NewRecorder()
	srv.handleShowKey(rec, httptest.NewRequest("POST", "/api/backup/key", nil))

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

// The status page decides what to offer from these two facts, so both have to be
// true for the right reasons: has_key is what hides a button that would show
// something that does not exist, and key_sent is what lets the page say whether a
// copy has ever left the box.
func TestStatusReportsWhetherAKeyExistsAndHasBeenMailed(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{DataRoot: root}

	get := func() string {
		rec := httptest.NewRecorder()
		sealedServer(t, cfg).handleBackupStatus(rec, httptest.NewRequest("GET", "/api/backup/status", nil))
		return rec.Body.String()
	}

	if body := get(); !strings.Contains(body, `"has_key":false`) {
		t.Errorf("unprovisioned box reports %s, want has_key false", body)
	}
	writePassword(t, cfg, "s3cret")
	body := get()
	if !strings.Contains(body, `"has_key":true`) {
		t.Errorf("provisioned box reports %s, want has_key true", body)
	}
	if strings.Contains(body, `"key_sent"`) {
		t.Errorf("reports %s, want no receipt before anything has been mailed", body)
	}

	if err := writeKeySent(cfg, keySentRecord{SentAt: time.Now(), To: "u@example.com", Auto: true}); err != nil {
		t.Fatal(err)
	}
	if body := get(); !strings.Contains(body, `"to":"u@example.com"`) {
		t.Errorf("reports %s, want the receipt once a copy has been mailed", body)
	}
}

// The receipt is the only thing standing between "sent once" and "sent on every
// restart", so its read has to fail towards silence.
func TestAMalformedReceiptCountsAsAlreadySent(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	if _, sent := readKeySent(cfg); sent {
		t.Fatal("no receipt file reads as sent")
	}
	if err := os.MkdirAll(cfg.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keySentPath(cfg), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, sent := readKeySent(cfg); !sent {
		t.Error("a truncated receipt reads as never sent — which mails the key again every boot")
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
			if err := writeKeySent(cfg, keySentRecord{SentAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{DataRoot: t.TempDir()}
			tc.setup(cfg)
			s := &Server{cfg: cfg, backupConf: backupconfig.New(filepath.Join(cfg.StateDir(), "backup.json"))}

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

// Which engine's key gets escrowed comes from Caps.KeyEscrow, not from a named engine.
//
// This is what makes a second encrypting engine work without another call site
// learning its name — and what stops an engine that encrypts with a key the
// deployment already holds being mailed a secret nobody needed.
func TestEscrowKeyPicksTheEngineByCapability(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}

	// The escrowing engine is registered SECOND on purpose: picking the first engine
	// that happens to have a password file would pass a test where it is first.
	plain := backuptest.NewFake("plain", apps.Caps{})
	sealed := backuptest.NewFake("sealed", apps.Caps{Encrypted: true, KeyEscrow: true})
	srv := &Server{cfg: cfg, engines: backup.New(plain, sealed)}

	// A key sitting in the non-escrowing engine's directory must be ignored: it is not
	// the secret that is lost with the box.
	writeEnginePassword(t, cfg, "plain", "not the one")
	if _, _, err := srv.escrowKey(); err == nil {
		t.Fatal("escrowKey returned a key from an engine that declares no escrow")
	}

	writeEnginePassword(t, cfg, "sealed", "correct horse battery staple")
	engine, pw, err := srv.escrowKey()
	if err != nil {
		t.Fatalf("escrowKey: %v", err)
	}
	if engine != "sealed" {
		t.Errorf("engine = %q, want the engine that declares KeyEscrow", engine)
	}
	if pw != "correct horse battery staple" {
		t.Errorf("key = %q, want the sealed engine's password", pw)
	}
}

// writeEnginePassword renders one engine's password the way the host-side script would,
// with the trailing newline a shell writes — readEnginePassword's trim is what stops
// that newline becoming part of the key.
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
