package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/incident"
	"github.com/yundera/maison/internal/notify"
)

// Handing the user the secret that opens their backups — per engine.
//
// An engine that encrypts with a key generated on the box holds the only copy: the box
// holds it, the user holds whatever copy they took, and Yundera holds nothing and cannot
// recover it. Everything here exists so that copy gets taken — there are two ways to
// take it, and they are deliberately different in kind:
//
//   - Showing it in the dashboard. Nothing leaves the box, so the security property
//     is preserved exactly; the user copies it into whatever they already trust.
//   - Mailing it. A plaintext secret in an inbox, indexed and retained — traded
//     against the far likelier failure, which is a user who never took a copy at all
//     and discovers it the day the disk dies.
//
// The mail is sent once per engine and per key, automatically, on the first boot where it
// can be sent (see EnsureKeyEmailed), and can be re-sent by hand. "Once" is enforced by a
// receipt file per engine in Maison's state directory rather than by the caller
// remembering — that is the whole reason the files exist, and why they are written after
// the send rather than before.
//
// **What the secret is, and what it is called, is the engine's to say** (apps.SecretSpec,
// declared in the engine's adapter.json). Maison owns the machinery — receipt, mail,
// incident, page — and the engine owns the words: whoever restores with kopia on another
// machine is asked for a "repository password", and the mail should have used that name.

// defaultSecretFile is where every engine that predates the declaration keeps its key.
const defaultSecretFile = "repository.password"

// engineSecret is one engine's secret, resolved to a file on disk.
type engineSecret struct {
	Engine string
	Spec   apps.SecretSpec
	Path   string
}

// read returns the secret. **It reads the file and never asks the engine**: the moment
// the secret matters is the moment the box is broken, and an escrow that needed a working
// engine container would fail exactly when it is needed.
func (es engineSecret) read() (string, error) {
	b, err := os.ReadFile(es.Path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// secretOf is the secret engine id holds, if it holds one.
//
// The engine's declaration wins. An engine that declares nothing — an adapter.json written
// before the declaration existed — falls back to what every such box has always meant: the
// key is repository.password, and it needs escrow when the engine says KeyEscrow. An
// engine that neither encrypts nor escrows (the local one) holds no secret at all.
func (s *Server) secretOf(id string) (engineSecret, bool) {
	p, ok := engineByID(s.engines, id)
	if !ok {
		return engineSecret{}, false
	}
	dir := s.cfg.BackupEngineDir(id)
	if h, ok := p.(apps.SecretHolder); ok {
		if spec, ok := h.Secret(); ok {
			return engineSecret{Engine: id, Spec: spec, Path: filepath.Join(dir, spec.File)}, true
		}
	}
	caps := p.Caps()
	if !caps.Encrypted && !caps.KeyEscrow {
		return engineSecret{}, false
	}
	spec := apps.SecretSpec{File: defaultSecretFile, Escrow: caps.KeyEscrow}
	return engineSecret{Engine: id, Spec: spec, Path: filepath.Join(dir, spec.File)}, true
}

// escrowSecrets is every engine secret that has to leave the box, in registration order.
// Every one of them, not the first: a second engine encrypting with a key only this box
// holds is exactly as unrecoverable as the first.
func (s *Server) escrowSecrets() []engineSecret {
	var out []engineSecret
	for _, id := range engineIDs(s.engines) {
		if es, ok := s.secretOf(id); ok && es.Spec.Escrow {
			out = append(out, es)
		}
	}
	return out
}

// keySentRecord is the receipt: the proof that a copy of one engine's secret has already
// left the box, and what stops the automatic send repeating on every restart.
//
// It records the address and the engine as well as the time, because "was it sent?"
// is not the only question worth answering later — a key mailed to an address the
// user has since changed is a copy of the secret in the wrong inbox.
type keySentRecord struct {
	SentAt time.Time `json:"sent_at"`
	To     string    `json:"to,omitempty"`
	Engine string    `json:"engine,omitempty"`
	// Auto distinguishes the boot-time send from a button press, so a support
	// conversation can tell "we sent it" from "they asked for it".
	Auto bool `json:"auto,omitempty"`

	// Fingerprint names WHICH key this receipt is for: the first 16 hex digits of the
	// secret's SHA-256. Not the key — sixty-four bits of a hash of a 264-bit random
	// secret gives nothing back — but enough to notice that the key on disk is no longer
	// the one that was mailed. That happens when the space is reset from the dashboard
	// and the box mints a fresh repository: without this the receipt would go on saying
	// "already sent" about a key that no longer opens anything, and the new one would
	// never leave the box.
	//
	// Absent in a receipt written before it existed, and that reads as MATCHING (see
	// keyNeedsMail): an upgrade must not mail every box's key a second time.
	Fingerprint string `json:"fingerprint,omitempty"`

	// HeldByUser marks a receipt written without a mail: the user typed this key in —
	// into the recovery form, or as the new password when changing it — so they
	// demonstrably hold a copy, and mailing it back would put a second plaintext copy in
	// an inbox for nothing. SentAt is left zero — nothing was sent — which is also what
	// keeps the page from claiming a mail.
	HeldByUser bool `json:"held_by_user,omitempty"`
}

// keyFingerprint is the receipt's name for a key. See keySentRecord.Fingerprint.
func keyFingerprint(pw string) string {
	sum := sha256.Sum256([]byte(pw))
	return hex.EncodeToString(sum[:])[:16]
}

// keyNeedsMail is whether an engine's key on disk still has to leave the box.
//
// Three answers, and the two that say "no" without a match are deliberate:
//   - no receipt: never sent, so yes;
//   - a receipt with no fingerprint — written before fingerprints, or malformed (see
//     readKeySent) — counts as covering whatever key is there now. Re-mailing on
//     upgrade, or on every boot for a damaged file, is exactly what the receipt exists
//     to prevent;
//   - a receipt for a different key: the key changed under it, so yes.
func keyNeedsMail(cfg config.Config, engine, pw string) bool {
	rec, sent := readKeySent(cfg, engine)
	if !sent {
		return true
	}
	if rec.Fingerprint == "" {
		return false
	}
	return rec.Fingerprint != keyFingerprint(pw)
}

// keySentPath is in StateDir, not in the engine directory: the engine directory is
// rendered by a host-side script that Maison only reads, and a receipt written into
// it would be outside Maison's own state and liable to be replaced under it.
func keySentPath(cfg config.Config, engine string) string {
	return filepath.Join(cfg.StateDir(), "backup-secret", engine+".json")
}

// legacyKeySentPath is the single box-wide receipt from before receipts were per engine.
// It is read and never written or moved: see readKeySent.
func legacyKeySentPath(cfg config.Config) string {
	return filepath.Join(cfg.StateDir(), "backup-key-sent.json")
}

// readKeySent reports an engine's receipt, if there is one.
//
// A malformed file is treated as "already sent" rather than as absent, deliberately:
// the failure mode of re-reading it wrong is mailing the key again on every boot,
// which is the one behaviour these files exist to prevent.
//
// An engine with no receipt of its own inherits the box-wide one written before receipts
// were per engine, when it names this engine or names none (every receipt from before the
// engine field — kopia was the only engine with a key). The legacy file is left where it
// is rather than migrated: the first receipt written for the engine supersedes it, and an
// upgrade that moved files could only add a way to lose one and mail a key twice.
func readKeySent(cfg config.Config, engine string) (keySentRecord, bool) {
	if rec, ok, exists := parseKeySent(keySentPath(cfg, engine)); exists {
		return rec, ok
	}
	rec, ok, exists := parseKeySent(legacyKeySentPath(cfg))
	if !exists {
		return keySentRecord{}, false
	}
	if rec.Engine != "" && rec.Engine != engine {
		return keySentRecord{}, false
	}
	return rec, ok
}

// parseKeySent reads one receipt file: exists is whether there is a file at all, and a
// malformed one reads as an empty receipt that exists.
func parseKeySent(path string) (rec keySentRecord, sent, exists bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return keySentRecord{}, false, false
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return keySentRecord{}, true, true
	}
	return rec, true, true
}

// writeKeySent records that a copy of an engine's secret has left the box. Through a
// temporary, like every other state file here, so an interrupted write cannot leave a
// truncated receipt that the next boot reads as "never sent".
func writeKeySent(cfg config.Config, engine string, rec keySentRecord) error {
	rec.Engine = engine
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	path := keySentPath(cfg, engine)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
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

// secretName is what to call an engine's secret in a sentence: its own name when it
// declared one, the generic one otherwise.
func secretName(spec apps.SecretSpec) string {
	if spec.Label != "" {
		return spec.Label
	}
	return "backup encryption key"
}

// keyMailSubject and keyMailBody are the message. Composed here rather than at the two
// call sites so the automatic mail and the hand-sent one cannot drift into saying
// different things about the same unrecoverable secret.
func keyMailSubject(spec apps.SecretSpec) string {
	return "Your " + secretName(spec)
}

func keyMailBody(spec apps.SecretSpec, pw string) string {
	body := "This is the " + secretName(spec) + " for the backups on your server.\n\n" +
		pw + "\n\n" +
		"Store it somewhere safe and then delete this email.\n\n"
	if spec.Label != "" {
		body += "Keep its name with it: \"" + spec.Label + "\" is what the backup software asks for " +
			"if you ever restore without this server.\n\n"
	}
	return body + "Without it your backups cannot be decrypted, by you or by anyone else — " +
		"including Yundera, which never receives it. If you lose it there is no recovery path.\n"
}

// sendKeyMail mails one engine's secret and writes its receipt.
//
// The receipt is written only after a successful send, and a failure to write it is
// logged rather than returned: the mail is already gone, so reporting a failure to
// the user would be false. The cost of that choice is a duplicate mail on the next
// boot, which is the right way round.
//
// The transport is passed in already resolved, so the receipt records the address the
// mail actually went to.
func sendKeyMail(cfg config.Config, smtp notify.SMTP, es engineSecret, pw string, auto bool) error {
	if err := notify.Send(smtp, keyMailSubject(es.Spec), keyMailBody(es.Spec, pw)); err != nil {
		return err
	}
	rec := keySentRecord{SentAt: time.Now(), To: smtp.To, Auto: auto, Fingerprint: keyFingerprint(pw)}
	if err := writeKeySent(cfg, es.Engine, rec); err != nil {
		log.Printf("backup: %s: secret mailed but the receipt could not be written: %v", es.Engine, err)
	}
	return nil
}

// EnsureKeyEmailed mails each engine's secret once, on the first boot where every
// precondition holds: a secret exists, a mail server is configured, and no receipt says a
// copy of THIS secret has already left the box.
//
// It retries for a few minutes rather than testing once, because on a PCS the mail
// relay is a sibling container and boot order between the two is not guaranteed — a
// single attempt at t=0 would fail on exactly the deployments this is for, and then
// wait for a restart that may be months away.
//
// It gives up after that window instead of retrying forever: past the point where the
// relay would have come up, "not configured" is a real answer and not a race. The
// detector loop asks again every five minutes (ensureKeyEmailedOnce), once per pass,
// which is also what catches a key that changes while Maison is running.
func (s *Server) EnsureKeyEmailed() {
	const attempts, wait = 10, 30 * time.Second
	for i := range attempts {
		if i > 0 {
			time.Sleep(wait)
		}
		switch s.tryMailKey(context.Background()) {
		case keyMailDone, keyMailNothing:
			return
		}
	}
	log.Printf("backup: the backup secret has not been mailed — no mail server is configured")
}

// ensureKeyEmailedOnce is one attempt, for the detector loop. It is silent when there is
// nothing to do, which is every pass on a box whose secrets have been mailed.
func (s *Server) ensureKeyEmailedOnce(ctx context.Context) {
	s.tryMailKey(ctx)
}

type keyMailOutcome int

const (
	keyMailNothing keyMailOutcome = iota // no secret, or every secret's copy has already left
	keyMailDone                          // mailed just now
	keyMailRetry                         // no relay yet, or a send failed
)

// tryMailKey is one attempt at the automatic send, for every escrowed secret.
//
// It holds keyMailMu across check-and-send, so the boot-time retries and a detector pass
// landing in the same instant cannot both decide a secret is unsent and mail it twice.
//
// Any engine still needing a retry makes the whole attempt a retry, so the boot loop
// keeps going for it even when another engine's mail went out.
func (s *Server) tryMailKey(ctx context.Context) keyMailOutcome {
	if s.backupConf == nil {
		return keyMailNothing
	}
	s.keyMailMu.Lock()
	defer s.keyMailMu.Unlock()

	outcome := keyMailNothing
	for _, es := range s.escrowSecrets() {
		switch s.tryMailSecret(ctx, es) {
		case keyMailRetry:
			outcome = keyMailRetry
		case keyMailDone:
			if outcome == keyMailNothing {
				outcome = keyMailDone
			}
		}
	}
	return outcome
}

// tryMailSecret is the automatic send for one engine. Called with keyMailMu held.
//
// An engine reporting NeedsRecovery is skipped even if a secret file is sitting in its
// directory: on that box any secret present is not the key to the user's backups, and
// mailing it would hand them a second, useless "your backup key" — the one mistake here
// worse than sending nothing.
func (s *Server) tryMailSecret(ctx context.Context, es engineSecret) keyMailOutcome {
	pw, err := es.read()
	if err != nil {
		// Not provisioned yet: nothing to hand over, and nothing to record. A box
		// provisioned later is asked again on the next pass.
		return keyMailNothing
	}
	if !keyNeedsMail(s.cfg, es.Engine, pw) {
		return keyMailNothing
	}
	if p, ok := engineByID(s.engines, es.Engine); ok && p.Status(ctx).NeedsRecovery {
		return keyMailNothing
	}
	if s.settings == nil {
		return keyMailRetry
	}
	smtp := s.settings.EffectiveSMTP(s.cfg.ProvisionedSMTP())
	if !smtp.Configured() {
		return keyMailRetry
	}
	_, hadReceipt := readKeySent(s.cfg, es.Engine)
	if err := sendKeyMail(s.cfg, smtp, es, pw, true); err != nil {
		log.Printf("backup: %s: could not mail the %s: %v", es.Engine, secretName(es.Spec), err)
		return keyMailRetry
	}
	if hadReceipt {
		log.Printf("backup: %s: mailed the %s to %s (it has changed since the last copy)", es.Engine, secretName(es.Spec), smtp.To)
	} else {
		log.Printf("backup: %s: mailed the %s to %s (first send on this box)", es.Engine, secretName(es.Spec), smtp.To)
	}
	return keyMailDone
}

// recordKeyHeldByUser writes the receipt for a secret the user has just typed in, so the
// automatic send does not mail it straight back to them. It carries the fingerprint of
// the secret now on disk — after a recovery or a change that is the key they typed,
// promoted by the engine — and no SentAt, because nothing was sent.
//
// Nothing is written when the secret cannot be read: a receipt with no fingerprint would
// read as covering whatever key appears later, which is the one receipt this must never
// leave behind.
func (s *Server) recordKeyHeldByUser(engine string) {
	es, ok := s.secretOf(engine)
	if !ok {
		return
	}
	pw, err := es.read()
	if err != nil {
		log.Printf("backup: %s: the %s is not readable yet — the receipt was not written: %v", engine, secretName(es.Spec), err)
		return
	}
	s.keyMailMu.Lock()
	defer s.keyMailMu.Unlock()
	rec := keySentRecord{HeldByUser: true, Fingerprint: keyFingerprint(pw)}
	if err := writeKeySent(s.cfg, engine, rec); err != nil {
		log.Printf("backup: %s: could not record that the user holds the %s: %v", engine, secretName(es.Spec), err)
	}
}

// secretCovered is whether a copy of the secret now on disk is known to exist off the
// box — mailed, or typed in by the user. The same reading of the receipt as keyNeedsMail.
func (s *Server) secretCovered(engine, pw string) bool {
	return !keyNeedsMail(s.cfg, engine, pw)
}

// requestSecret resolves the engine a secret route names, answering 404 for an unknown
// engine and 400 for one that holds no secret on this box.
func (s *Server) requestSecret(w http.ResponseWriter, r *http.Request) (engineSecret, string, bool) {
	id := chi.URLParam(r, "id")
	if _, ok := engineByID(s.engines, id); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown backup engine: " + id})
		return engineSecret{}, "", false
	}
	es, ok := s.secretOf(id)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "this backup engine holds no secret"})
		return engineSecret{}, "", false
	}
	pw, err := es.read()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no " + secretName(es.Spec) + " on this box"})
		return engineSecret{}, "", false
	}
	return es, pw, true
}

// handleEmailSecret mails an engine's secret to the configured address.
//
// This is the only copy of that secret that exists off the box, and it is the whole
// disaster-recovery story: the PCS holds it, the user holds a copy, Yundera holds nothing
// and cannot recover it.
//
// By hand it is unconditional — a user asking for the key again has a reason, and
// refusing because a receipt exists would leave them with no way to reach a secret
// that is theirs.
func (s *Server) handleEmailSecret(w http.ResponseWriter, r *http.Request) {
	if s.backupConf == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "backups unavailable"})
		return
	}
	smtp := s.settings.EffectiveSMTP(s.cfg.ProvisionedSMTP())
	if !smtp.Configured() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no mail server configured"})
		return
	}
	es, pw, ok := s.requestSecret(w, r)
	if !ok {
		return
	}
	s.keyMailMu.Lock()
	err := sendKeyMail(s.cfg, smtp, es, pw, false)
	s.keyMailMu.Unlock()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	s.resolveSecretIncident(es.Engine)
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

// handleShowSecret returns an engine's secret for the dashboard to display.
//
// This is the copy path that costs nothing: the secret goes to the browser of
// someone already authenticated as the owner of the box and stops there, which is
// strictly less exposure than the mail it sits next to.
//
// It is a POST, not a GET, for that reason alone — a secret in a response to a
// navigable URL is a secret in a history entry, a prefetch and a shared link. The
// no-store header exists for the same reason.
func (s *Server) handleShowSecret(w http.ResponseWriter, r *http.Request) {
	_, pw, ok := s.requestSecret(w, r)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"key": pw})
}

// minSecretLength is the shortest new secret the change route accepts. The page
// generates 44 characters; this only stops a typed one being trivially guessable,
// since the secret is all that stands between the storage provider and the backups.
const minSecretLength = 16

// handleChangeSecret replaces the secret that opens an engine's repository.
//
//	200 {status}   changed; every existing backup stays readable with the new secret
//	400 {error}    no body, a short secret, no confirmation, or the box's own secret no
//	               longer opens the repository
//	404            no such engine
//	409            a backup or restore is running, or the repository is not in a state
//	               to change (waiting for recovery, not configured)
//	501            the engine cannot change its secret (Caps.ChangeSecret)
//
// **The new secret is not mailed.** Changing it is how a user retires the copy that
// travelled in plaintext through a mail relay, and mailing the replacement the same way
// would undo the point. The page has the user take the copy themselves and confirm it,
// and the receipt records the secret as held by them. The mail button still works for
// a user who wants it anyway.
//
// It runs detached from the request, like recovery: a change the browser abandoned is
// still worth finishing, and the adapter is what makes an interrupted one safe.
func (s *Server) handleChangeSecret(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p, ok := engineByID(s.engines, id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown backup engine: " + id})
		return
	}
	ch, implements := p.(apps.SecretChanger)
	if !implements || !p.Caps().ChangeSecret {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "this backup engine cannot change its secret"})
		return
	}

	var in struct {
		Key       string `json:"key"`
		Confirmed bool   `json:"confirmed"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	key := strings.TrimSpace(in.Key)
	if len(key) < minSecretLength {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the new password must be at least 16 characters"})
		return
	}
	if !in.Confirmed {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "confirm that you have saved the new password first"})
		return
	}
	if s.backupBusy() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a backup or restore is running — try again when it has finished"})
		return
	}

	ctx := context.WithoutCancel(r.Context())
	if inv, ok := p.(statusInvalidator); ok {
		inv.Invalidate()
	}
	if st := p.Status(ctx); st.NeedsRecovery || !st.Configured {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this backup storage has no working password to change"})
		return
	}

	err := ch.ChangeSecret(ctx, key)
	switch {
	case errors.Is(err, apps.ErrWrongKey):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the password on this server no longer opens the backups"})
		return
	case errors.Is(err, apps.ErrNotConfigured):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this backup storage has no working password to change"})
		return
	case errors.Is(err, apps.ErrNotSupported):
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "this backup engine cannot change its secret"})
		return
	case err != nil:
		log.Printf("backup: %s: changing the secret failed: %v", id, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	log.Printf("backup: %s: the repository secret was changed", id)

	s.recordKeyHeldByUser(id)
	s.resolveSecretIncident(id)
	writeJSON(w, http.StatusOK, map[string]string{"status": "changed"})
}

// backupBusy is whether a backup run or a user-data restore is in progress. A secret
// change during either is refused rather than raced: the run would not fail, but a
// user watching both is better served by one thing happening at a time.
func (s *Server) backupBusy() bool {
	if s.backupSched != nil && s.backupSched.State().Running {
		return true
	}
	return s.userData != nil && s.userData.State().Running
}

// secretIncidentID is the "only on this box" incident for one engine.
func secretIncidentID(engine string) string { return incident.KindBackupSecret + ":" + engine }

func (s *Server) resolveSecretIncident(engine string) {
	if s.incidents != nil {
		s.incidents.Resolve(secretIncidentID(engine))
	}
}

// checkBackupSecrets keeps an escrowed secret that exists only on this box visible.
//
// A warning rather than critical: nothing is failing, and the box is backing up. But
// it is the state in which losing the server loses every backup with it, and the
// automatic mail can fail silently for as long as no relay is configured — so the bell
// says so until a copy is known to exist, mailed or taken by the user.
//
// Raised only once the state has held for consecutive passes, so the minutes a fresh
// box spends waiting for its relay before the first mail do not flash an incident.
// Skipped while the engine waits for recovery: the secret on that box, if any, is not
// the key to the user's backups, and backup.recovery already says what is wrong.
func (s *Server) checkBackupSecrets(ctx context.Context, d *detector) {
	if s.incidents == nil {
		return
	}
	escrowed := map[string]bool{}
	for _, es := range s.escrowSecrets() {
		escrowed[es.Engine] = true
		id := secretIncidentID(es.Engine)
		pw, err := es.read()
		alone := err == nil && !s.secretCovered(es.Engine, pw)
		if alone {
			if p, ok := engineByID(s.engines, es.Engine); ok && p.Status(ctx).NeedsRecovery {
				alone = false
			}
		}
		if !d.settled(id, alone) {
			if !alone {
				s.incidents.Resolve(id)
			}
			continue
		}
		name := secretName(es.Spec)
		s.incidents.Report(incident.Report{
			ID: id, Kind: incident.KindBackupSecret, Severity: incident.Warning,
			Title: "Your " + name + " exists only on this server",
			Detail: "The " + name + " is the only thing that can open your backups, and no copy of it has left this server. " +
				"If the server is lost, the backups are lost with it.\n\n" +
				"Open Settings → Backups to copy it somewhere safe, or to have it emailed to you.",
			Args: map[string]string{"engine": es.Engine, "secret": name},
		})
	}
	// An engine that stopped escrowing, or left the set, has nothing above to close its
	// incident.
	for _, inc := range s.incidents.Snapshot().Open {
		if inc.Kind != incident.KindBackupSecret {
			continue
		}
		if !escrowed[strings.TrimPrefix(inc.ID, incident.KindBackupSecret+":")] {
			s.incidents.Resolve(inc.ID)
		}
	}
}
