package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/yundera/maison/internal/backup/kopia"
	"github.com/yundera/maison/internal/backupconfig"
	"github.com/yundera/maison/internal/config"
)

// writePassword renders the repository password the host-side script would have put
// there — the only thing that makes a box "provisioned" as far as this code is
// concerned.
func writePassword(t *testing.T, cfg config.Config, pw string) {
	t.Helper()
	dir := cfg.BackupEngineDir(kopia.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// With a trailing newline, because that is how a shell script writes it and
	// readEnginePassword's trim is what stops it becoming part of the key.
	if err := os.WriteFile(filepath.Join(dir, "repository.password"), []byte(pw+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The key has to be reachable from the dashboard, because showing it there is the copy
// path that keeps the secret on the box — the whole reason it sits next to the mail.
func TestShowKeyReturnsTheRepositoryPassword(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{DataRoot: root}
	writePassword(t, cfg, "correct horse battery staple")

	h := New(cfg, fstest.MapFS{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/backup/key", nil))

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
		h := New(cfg, fstest.MapFS{})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/backup/status", nil))
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
