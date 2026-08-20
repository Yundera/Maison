package server

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yundera/maison/internal/backup/kopia"
	"github.com/yundera/maison/internal/backupconfig"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/notify"
)

// Handing the user their backup encryption key.
//
// The key is generated on the PCS and exists nowhere else: the box holds it, the
// user holds whatever copy they took, and Yundera holds nothing and cannot recover
// it. Everything here exists so that copy gets taken — there are two ways to take
// it, and they are deliberately different in kind:
//
//   - Showing it in the dashboard. Nothing leaves the box, so the security property
//     is preserved exactly; the user copies it into whatever they already trust.
//   - Mailing it. A plaintext secret in an inbox, indexed and retained — traded
//     against the far likelier failure, which is a user who never took a copy at all
//     and discovers it the day the disk dies.
//
// The mail is sent once, automatically, on the first boot where it can be sent (see
// EnsureKeyEmailed), and can be re-sent by hand. "Once" is enforced by a receipt file
// in Maison's state directory rather than by the caller remembering — that is the
// whole reason the file exists, and why it is written after the send rather than
// before.

// keySentRecord is the receipt: the proof that a copy of the key has already left the
// box, and what stops the automatic send repeating on every restart.
//
// It records the address and the engine as well as the time, because "was it sent?"
// is not the only question worth answering later — a key mailed to an address the
// user has since changed, or belonging to an engine they have since switched away
// from, is a copy of the wrong secret in the wrong inbox.
type keySentRecord struct {
	SentAt time.Time `json:"sent_at"`
	To     string    `json:"to,omitempty"`
	Engine string    `json:"engine,omitempty"`
	// Auto distinguishes the boot-time send from a button press, so a support
	// conversation can tell "we sent it" from "they asked for it".
	Auto bool `json:"auto,omitempty"`
}

// keySentPath is in StateDir, not in the engine directory: the engine directory is
// rendered by a host-side script that Maison only reads, and a receipt written into
// it would be outside Maison's own state and liable to be replaced under it.
func keySentPath(cfg config.Config) string {
	return filepath.Join(cfg.StateDir(), "backup-key-sent.json")
}

// readKeySent reports the receipt, if there is one.
//
// A malformed file is treated as "already sent" rather than as absent, deliberately:
// the failure mode of re-reading it wrong is mailing the key again on every boot,
// which is the one behaviour this file exists to prevent.
func readKeySent(cfg config.Config) (keySentRecord, bool) {
	b, err := os.ReadFile(keySentPath(cfg))
	if err != nil {
		return keySentRecord{}, false
	}
	var rec keySentRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return keySentRecord{}, true
	}
	return rec, true
}

// writeKeySent records that a copy has left the box. Through a temporary, like every
// other state file here, so an interrupted write cannot leave a truncated receipt
// that the next boot reads as "never sent".
func writeKeySent(cfg config.Config, rec keySentRecord) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	path := keySentPath(cfg)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".partial"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// keyMailBody is the message. Composed here rather than at the two call sites so the
// automatic mail and the hand-sent one cannot drift into saying different things
// about the same unrecoverable secret.
func keyMailBody(pw string) string {
	return "This is the encryption key for the backups on your server.\n\n" +
		pw + "\n\n" +
		"Store it somewhere safe and then delete this email.\n\n" +
		"Without it your backups cannot be decrypted, by you or by anyone else — " +
		"including Yundera, which never receives it. If you lose it there is no recovery path.\n"
}

// sendKeyMail mails the key and writes the receipt.
//
// The receipt is written only after a successful send, and a failure to write it is
// logged rather than returned: the mail is already gone, so reporting a failure to
// the user would be false. The cost of that choice is a duplicate mail on the next
// boot, which is the right way round.
func sendKeyMail(cfg config.Config, conf backupconfig.Config, pw string, auto bool) error {
	if err := notify.Send(conf.SMTP, "Your backup encryption key", keyMailBody(pw)); err != nil {
		return err
	}
	rec := keySentRecord{SentAt: time.Now(), To: conf.SMTP.To, Engine: kopia.ID, Auto: auto}
	if err := writeKeySent(cfg, rec); err != nil {
		log.Printf("backup: key mailed but the receipt could not be written: %v", err)
	}
	return nil
}

// EnsureKeyEmailed mails the key once, on the first boot where every precondition
// holds: a key exists, a mail server is configured, and no receipt says a copy has
// already left the box.
//
// It retries for a few minutes rather than testing once, because on a PCS the mail
// relay is a sibling container and boot order between the two is not guaranteed — a
// single attempt at t=0 would fail on exactly the deployments this is for, and then
// wait for a restart that may be months away.
//
// It gives up after that window instead of retrying forever: past the point where the
// relay would have come up, "not configured" is a real answer and not a race, and the
// next boot — or the Email me the key button — asks the question again.
func (s *Server) EnsureKeyEmailed() {
	if s.backupConf == nil {
		return
	}
	if _, sent := readKeySent(s.cfg); sent {
		return
	}
	pw, err := readEnginePassword(s.cfg, kopia.ID)
	if err != nil {
		// No repository on this box: nothing to hand over, and nothing to record.
		// A box provisioned later boots again before it has backups to lose.
		return
	}
	const attempts, wait = 10, 30 * time.Second
	for i := range attempts {
		if i > 0 {
			time.Sleep(wait)
		}
		conf := s.backupConf.Get()
		if !conf.SMTP.Configured() {
			continue
		}
		// Re-checked inside the loop: the user may have pressed the button, or a
		// second boot-time send may be in flight, during the wait.
		if _, sent := readKeySent(s.cfg); sent {
			return
		}
		if err := sendKeyMail(s.cfg, conf, pw, true); err != nil {
			log.Printf("backup: could not mail the encryption key: %v", err)
			continue
		}
		log.Printf("backup: mailed the encryption key to %s (first send on this box)", conf.SMTP.To)
		return
	}
	log.Printf("backup: the encryption key has never been mailed — no mail server is configured")
}

// handleEmailKey mails the repository password to the configured address.
//
// This is the only copy of that password that exists off the box, and it is the
// whole disaster-recovery story: the PCS holds it, the user holds a copy, Yundera
// holds nothing and cannot recover it.
//
// By hand it is unconditional — a user asking for the key again has a reason, and
// refusing because a receipt exists would leave them with no way to reach a secret
// that is theirs.
func (s *Server) handleEmailKey(w http.ResponseWriter, r *http.Request) {
	if s.backupConf == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "backups unavailable"})
		return
	}
	conf := s.backupConf.Get()
	if !conf.SMTP.Configured() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no mail server configured"})
		return
	}
	pw, err := readEnginePassword(s.cfg, kopia.ID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no repository password on this box"})
		return
	}
	if err := sendKeyMail(s.cfg, conf, pw, false); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

// handleShowKey returns the key for the dashboard to display.
//
// This is the copy path that costs nothing: the secret goes to the browser of
// someone already authenticated as the owner of the box and stops there, which is
// strictly less exposure than the mail it sits next to.
//
// It is a POST, not a GET, for that reason alone — a secret in a response to a
// navigable URL is a secret in a history entry, a prefetch and a shared link. The
// no-store header exists for the same reason.
func (s *Server) handleShowKey(w http.ResponseWriter, r *http.Request) {
	pw, err := readEnginePassword(s.cfg, kopia.ID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no repository password on this box"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"key": pw})
}

func readEnginePassword(cfg config.Config, engine string) (string, error) {
	b, err := os.ReadFile(filepath.Join(cfg.BackupEngineDir(engine), "repository.password"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
