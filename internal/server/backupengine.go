package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/backup"
	"github.com/yundera/maison/internal/backup/adapter"
	"github.com/yundera/maison/internal/backupconfig"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/incident"
	"github.com/yundera/maison/internal/notify"
	"github.com/yundera/maison/internal/usersettings"
)

// Backup engines, their configuration, and the schedule.
//
// These routes live under /api/backup/ — deliberately a different prefix from the
// existing /api/backups, which lists archives. Two prefixes one character apart is
// not lovely, but it keeps this entirely away from the /api/apps/{id}/{action}
// catch-all, and the alternative (nesting under /api/backups) would put settings
// under a path whose every other member is an archive.
//
// Like the global archive routes, none of these require Docker: an unconfigured
// engine and a box with no daemon must both render as a page that explains itself
// rather than a 503.

// buildEngines assembles the engine set and applies the user's choice.
//
// The local engine is registered first and therefore is the default writer: it needs
// no configuration and is always available, which is what makes it the right default
// for an install that has never been provisioned. Remote engines are registered
// whether or not they are configured — an engine with no repository still has to be
// able to *list* what it wrote before, which is the rule that stops a user's history
// disappearing when they switch away from it.
func buildEngines(cfg config.Config, store *backupconfig.Store) *backup.Set {
	// The local engine first, and therefore the default writer.
	providers := []apps.Provider{apps.NewLocalProvider(cfg)}

	// Then every engine the host side declared an adapter for. This is the whole of
	// "which engines does this box have": an engine with no descriptor does not exist
	// as far as Maison is concerned, which is what keeps the answer out of this build.
	//
	// THERE IS NO COMPILED ENGINE ANY MORE. A kopia provider used to be registered here
	// whenever no descriptor had claimed the name, so that a box whose host side had not
	// caught up kept backing up through the engine baked into this binary. That crutch is
	// gone, and with it the last line of Maison that knew what kopia is: every engine
	// now arrives as an adapter image the deployment names in adapter.json.
	//
	// What made the removal safe is an ordering the host already guarantees —
	// ensure-backup-config.sh writes the descriptor, and ensure-maison-stack.sh deploys
	// this binary, in that order in scripts-config.txt, with the Maison pin living inside
	// the same template — so a box cannot reach this code without having been handed a
	// descriptor first. What makes it honest when that ordering is nonetheless broken is
	// checkBackup: an engine the configuration names and this set does not have is
	// reported, not quietly dropped. See detect.go.
	for _, d := range adapter.Discover(cfg) {
		providers = append(providers, adapter.New(cfg, d))
	}

	set := backup.New(providers...)
	applyEngineSettings(set, store)
	return set
}

// An adapter is a full engine: it satisfies the Provider interface and all three of the
// narrow ones its consumers declare. Asserted here, at the composition root, because
// this is the one package that already imports both sides — and because a missing
// method would otherwise surface as a silently absent capability at runtime rather than
// as a build failure.
var (
	_ apps.Provider                = (*adapter.Provider)(nil)
	_ backup.UserDataEngine        = (*adapter.Provider)(nil)
	_ backup.UserDataRestoreEngine = (*adapter.Provider)(nil)
	_ backup.RetentionEngine       = (*adapter.Provider)(nil)
	_ apps.Recoverer               = (*adapter.Provider)(nil)
	_ statusInvalidator            = (*adapter.Provider)(nil)
)

// statusInvalidator is an engine whose Status is cached and can be told to forget it.
// Asked for by the recovery route, which has to know the engine's state NOW rather than
// thirty seconds ago — see handleRecoverEngine.
type statusInvalidator interface{ Invalidate() }

// applyEngineSettings points the set's write sets at whatever the configuration now
// says, one set per trigger.
//
// It **mutates the set in place** rather than returning a new one, and that matters:
// the registry, the scheduler and the user-data coordinator all hold the same *Set, and
// none of them is re-handed it when the settings change. Replacing the set here would
// leave every one of them writing through the previous instance — silently, since the
// old set is perfectly functional and merely wrong about where the user wants backups.
//
// THE MIGRATION LIVES HERE, and only here. A box that has never opened the settings page
// carries no ticks at all, and must keep writing exactly where it was provisioned to
// rather than reading as "no destination for anything". So when nobody has stated a
// preference for any engine, the legacy single writer — the user's chosen engine, or the
// connected-repository inference below — receives every trigger, which is what Maison
// has always done. The first tick anyone sets switches the box to the new model.
func applyEngineSettings(set *backup.Set, store *backupconfig.Store) {
	conf := store.Get()

	stated := false
	for _, id := range set.IDs() {
		if conf.Effective(id, backupconfig.Provisioned{}).Stated {
			stated = true
			break
		}
	}

	if stated {
		for _, t := range apps.Triggers {
			var ids []string
			for _, id := range set.IDs() {
				r := conf.Effective(id, backupconfig.Provisioned{})
				if (t == apps.TriggerSchedule && r.Schedule) || (t == apps.TriggerUninstall && r.Uninstall) {
					ids = append(ids, id)
				}
			}
			if err := set.SetWriters(t, ids); err != nil {
				log.Printf("backup: %v", err)
			}
		}
		return
	}

	for _, t := range apps.Triggers {
		if err := set.SetWriters(t, []string{legacyWriter(set, conf)}); err != nil {
			log.Printf("backup: %v", err)
		}
	}
}

// legacyWriter is the single engine a box wrote everything to before engines could be
// ticked individually. It is the compatibility answer, not a preference anyone stated.
func legacyWriter(set *backup.Set, conf backupconfig.Config) string {
	// The user's override wins; empty means "follow whatever the deployment
	// provisioned", which is inferred below rather than stored.
	if chosen := conf.Engine; chosen != "" {
		if _, ok := set.Get(chosen); ok {
			return chosen
		}
		// Not a typo — PUT /api/backup/config refuses an engine the box does not have,
		// so the only way to get here is an engine that has DISAPPEARED: a descriptor
		// the host side stopped writing, or an engine app that was uninstalled. Backups
		// keep being taken, locally, because a copy on the wrong disk still beats no
		// copy — but the box must not be left believing they are going offsite, so
		// checkBackup raises backup.missing for exactly this. The log line is the
		// operator's copy of it.
		log.Printf("backup: %q is not declared on this box any more — backups are landing on the local disk instead", chosen)
		return apps.EngineLocal
	}
	// No override: prefer an offsite engine that is actually connected. That inference
	// *is* the provisioning signal — a repository the host-side script has connected —
	// so there is no second file for the two sides to disagree about.
	//
	// Asked of capabilities rather than of a named engine, in registration order, so a
	// second offsite engine does not need a branch here.
	for _, id := range set.IDs() {
		p, ok := set.Get(id)
		if !ok || !p.Caps().Offsite {
			continue
		}
		if p.Status(context.Background()).Connected {
			return id
		}
	}
	return apps.EngineLocal
}

// adoptLegacySMTP moves a mail configuration written under backup.json's `smtp` key
// into settings.json, where it now lives, and clears it from the old place.
//
// Two callers, one rule: at boot, for a box upgraded across the move, and on PUT
// /api/backup/config, for a client still sending it there. Both are one-way, and the
// settings store wins if it already holds one — a value the user has since set in the
// new place must not be reverted by the stale copy left in the old.
//
// It reports whether it changed conf, so the caller knows to persist the cleared
// document rather than leaving the old key on disk to be adopted again next boot.
func adoptLegacySMTP(settings *usersettings.Store, conf *backupconfig.Config) bool {
	if conf.LegacySMTP == nil || *conf.LegacySMTP == (notify.SMTP{}) {
		return false
	}
	legacy := *conf.LegacySMTP
	conf.LegacySMTP = nil
	if settings == nil || settings.Get().SMTP != nil {
		return true // already configured in the new place: drop the stale copy
	}
	if err := settings.Set(usersettings.Settings{SMTP: &legacy}); err != nil {
		// Keep the value where it is rather than losing it: leaving the old key on
		// disk means the next boot tries again, which is the better failure.
		log.Printf("settings: adopting the mail configuration from backup.json: %v", err)
		conf.LegacySMTP = &legacy
		return false
	}
	log.Printf("settings: moved the mail configuration from backup.json into settings.json")
	return true
}

// engineStatus is what the settings page renders.
type engineStatus struct {
	Engines []engineInfo        `json:"engines"`
	Active  string              `json:"active"`
	Chosen  string              `json:"chosen,omitempty"` // the user's override, if any
	Run     backup.RunState     `json:"run"`
	Config  backupconfig.Config `json:"config"`
	Targets []string            `json:"targets"`

	// HasKey is whether this box has an encryption key at all — false on a box whose
	// repository has never been provisioned, where offering to show or mail a key
	// would be offering something that does not exist.
	HasKey bool `json:"has_key"`
	// KeySent is the receipt: when a copy of the key last left the box by mail, and
	// where to. Absent means no copy has ever been mailed, which is what the page
	// says out loud rather than leaving the user to wonder.
	KeySent *keySentRecord `json:"key_sent,omitempty"`

	// LastRun and NextRun are the two facts the settings page leads with, and neither
	// can be derived from Run above: RunState is in memory, is wiped at the start of
	// the next run, and knows nothing about the schedule. Absent means "never run" and
	// "the schedule is off" respectively — both real states, both worth saying.
	LastRun *lastRunView `json:"last_run,omitempty"`
	NextRun *time.Time   `json:"next_run,omitempty"`
}

// lastRunView is when the schedule last ran and whether it was failing.
type lastRunView struct {
	At     time.Time `json:"at"`
	Failed bool      `json:"failed"`
}

// retentionView is one engine's resolved retention, with the layer that decided it.
//
// Sent per engine rather than once for the box because retention *is* per engine: a
// repository expires snapshots under its own policy while local archives are counted
// by Maison, and the numbers genuinely differ. Source lets the page say "your
// deployment chose this" instead of presenting it as something the user typed.
type retentionView struct {
	Mode       string            `json:"mode"`
	Keep       backupconfig.Keep `json:"keep"`
	Count      int               `json:"count,omitempty"`
	MaxAgeDays int               `json:"max_age_days,omitempty"`
	Source     string            `json:"source"`
	Locked     []string          `json:"locked,omitempty"`

	// SelfExpiring is whether the engine applies this itself. It is the difference
	// between "the repository enforces it" and "Maison deletes them", which is the one
	// thing a user reading two different sets of numbers needs to know.
	SelfExpiring bool `json:"self_expiring"`

	// Tiered is whether grandfather-father-son is a sound thing to ask of this engine,
	// and it is a question about storage rather than about policy: an engine that needs
	// local space per backup keeps FULL COPIES, so 7 daily + 4 weekly + 12 monthly is
	// twenty-three complete copies of an app on one disk. Effective collapses tiers to
	// a count for such an engine; this is how the page knows not to offer them in the
	// first place, instead of offering a choice the server will quietly rewrite.
	Tiered bool `json:"tiered"`
}

type engineInfo struct {
	ID string `json:"id"`

	// Name is what to call this engine on screen, when something on the box knows a
	// better answer than its ID.
	//
	// The ID is permanent and is recorded on every backup it writes (apps.Backup.Engine),
	// which is exactly why it must stay a bare engine name: it is machine identity, and
	// a deployment's branding has no business in it. The display name comes from the
	// provisioning side instead — for kopia, the host-written state file — so a PCS can
	// say "Yundera Backup Storage" while a self-hoster running the same engine against
	// their own bucket sees no such claim. Empty means nobody named it and the client
	// falls back to describing the engine itself.
	Name      string `json:"name,omitempty"`
	Connected bool   `json:"connected"`
	Detail    string `json:"detail,omitempty"`
	Offsite   bool   `json:"offsite"`

	// Encrypted is whether this engine's backups are encrypted at rest — a capability
	// of the engine, false for the local one whose archives are plain folders on the
	// data disk.
	Encrypted bool `json:"encrypted"`

	// HasKey is whether a key for this engine actually exists on the box. Separate
	// from Encrypted on purpose: a repository engine on a box that has never been
	// provisioned still encrypts, it simply has nothing to encrypt with yet, and
	// "not encrypted" is the wrong thing to say about it.
	HasKey bool `json:"has_key"`

	// Retention is what this engine has been told to keep, resolved for it alone.
	Retention *retentionView `json:"retention,omitempty"`

	// ReceivesSchedule and ReceivesUninstall are the triggers this engine is set to
	// receive. They replaced a single box-wide "default engine": a backup can land in
	// several engines at once, and each says for itself what it is for.
	ReceivesSchedule  bool `json:"receives_schedule"`
	ReceivesUninstall bool `json:"receives_uninstall"`

	// LastRun is when this engine last actually took a backup, and whether it is
	// failing. Per engine because the run's own verdict cannot describe a night where
	// one destination succeeded and another did not — which is the state the page's
	// per-engine rows exist to show.
	LastRun *lastRunView `json:"last_run,omitempty"`

	// NeedsRecovery is the rebuilt box: the storage holds backups this box has no key
	// for. The page leads with it, because it is the one state where everything the user
	// owns is present and unreadable. See apps.EngineStatus.NeedsRecovery.
	NeedsRecovery bool `json:"needs_recovery"`
	// CanRecover is whether this engine can take the key from the page. False for an
	// adapter that predates the verb, which hides the form rather than offering a
	// button that would fail.
	CanRecover bool `json:"can_recover"`
	// RecoveryHelpURL is where a user without the key can start over, when the
	// deployment offers such a place. Absent, the page offers no link.
	RecoveryHelpURL string `json:"recovery_help_url,omitempty"`

	// ReceivesRollback is whether the rollback point an update takes lands here.
	//
	// It is always and only the local engine (server.go wires BackupBeforeUpdate to a
	// local provider explicitly, whatever the chosen engine is) because rolling back
	// has to be a rename: restoring from a repository is a download, and the app would
	// be broken for its duration. Sent as a fact rather than inferred from a capability
	// because that is what it is — a hardcoded wiring the page should be able to state.
	ReceivesRollback bool `json:"receives_rollback"`
}

func (s *Server) handleBackupStatus(w http.ResponseWriter, r *http.Request) {
	if s.engines == nil {
		writeJSON(w, http.StatusOK, engineStatus{Active: apps.EngineLocal})
		return
	}
	conf := s.backupConf.Get()
	out := engineStatus{
		Chosen: conf.Engine,
		Config: conf,
	}
	// The first engine the schedule writes to, kept because the page still wants one
	// engine to open its tabs on and to mark. It is no longer "the" destination —
	// engineInfo.Receives* below is what actually says where a backup goes.
	if w := s.engines.Writers(apps.TriggerSchedule); len(w) > 0 {
		out.Active = w[0].ID()
	}
	if _, _, err := s.escrowKey(); err == nil {
		out.HasKey = true
	}
	// A receipt the user earned by typing the key in carries no send date, and is shown
	// all the same: "you entered this key yourself" is the honest replacement for "no
	// copy has ever been mailed", which would be true and misleading.
	if rec, sent := readKeySent(s.cfg); sent && (!rec.SentAt.IsZero() || rec.HeldByUser) {
		out.KeySent = &rec
	}
	for _, id := range s.engines.IDs() {
		p, _ := s.engines.Get(id)
		info := engineInfo{ID: id, Offsite: p.Caps().Offsite}
		// Every engine is asked the same question. The local engine answers that it is
		// connected because it is the data disk, so it needs no special case — which is
		// what stops a second engine having to be added to a branch here.
		st := p.Status(r.Context())
		info.Connected, info.Detail, info.Name = st.Connected, st.Detail, st.Label
		info.NeedsRecovery, info.RecoveryHelpURL = st.NeedsRecovery, st.RecoveryHelpURL
		info.CanRecover = canRecover(p)
		// Resolved for this engine alone. Provisioned{} is empty because nothing
		// renders that layer onto a box yet; when the host-side script does, this is
		// the one call site that has to learn about it.
		caps := p.Caps()
		info.ReceivesRollback = id == apps.EngineLocal
		// Asked of the SET, not of the configuration, so that the page shows what the
		// box is actually doing — including the legacy fallback a box that has never
		// been configured is still running on. See applyEngineSettings.
		info.ReceivesSchedule = writesTo(s.engines, apps.TriggerSchedule, id)
		info.ReceivesUninstall = writesTo(s.engines, apps.TriggerUninstall, id)
		if s.backupSched != nil {
			if at, failed, ok := s.backupSched.LastRunIn(id); ok {
				info.LastRun = &lastRunView{At: at, Failed: failed}
			}
		}
		info.Retention = resolvedView(conf.Effective(id, backupconfig.Provisioned{}), caps)
		info.Encrypted = caps.Encrypted
		_, err := readEnginePassword(s.cfg, id)
		info.HasKey = err == nil
		out.Engines = append(out.Engines, info)
	}
	if s.backupSched != nil {
		out.Run = s.backupSched.State()
		for _, t := range s.backupSched.Targets() {
			out.Targets = append(out.Targets, t.ID())
		}
		if at, failed, ok := s.backupSched.LastRun(); ok {
			out.LastRun = &lastRunView{At: at, Failed: failed}
		}
		if next := s.backupSched.NextRun(); !next.IsZero() {
			out.NextRun = &next
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePutBackupConfig replaces the whole configuration. There is no merge here for
// the same reason there is none in the store: a partial update is how a field nobody
// remembered gets silently reset.
func (s *Server) handlePutBackupConfig(w http.ResponseWriter, r *http.Request) {
	if s.backupConf == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "backup configuration unavailable"})
		return
	}
	var in backupconfig.Config
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	// An engine the box does not have is a refusal, not a fallback: silently writing
	// backups somewhere other than where the user asked is how someone ends up
	// believing their data is offsite when it is not.
	if in.Engine != "" && s.engines != nil {
		if _, ok := s.engines.Get(in.Engine); !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown backup engine: " + in.Engine})
			return
		}
	}
	// A pause is stamped here when the client did not say when, so the incident and the
	// banner can always say since when. The client normally sends it; this is the
	// fallback, not the clock of record.
	if in.Paused && in.PausedAt == nil {
		now := time.Now()
		in.PausedAt = &now
	}
	// A client still sending `smtp` here is honoured once and moved, rather than
	// having its mail configuration stored where nothing reads it any more.
	adoptLegacySMTP(s.settings, &in)
	if err := s.backupConf.Set(in); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// One path for both cases, mutating the set the rest of the process already holds:
	// an explicit choice and a cleared one are the same question asked of the same
	// object. See applyEngineSettings.
	if s.engines != nil {
		applyEngineSettings(s.engines, s.backupConf)
	}
	// A changed schedule must take effect without a restart.
	if s.backupSched != nil {
		s.backupSched.Reload()
	}
	writeJSON(w, http.StatusOK, s.backupConf.Get())
}

// handleRunBackup starts a run by hand. It returns immediately: a run takes as long
// as it takes, and its progress rides the live channel like every other long
// operation in Maison.
func (s *Server) handleRunBackup(w http.ResponseWriter, r *http.Request) {
	if s.backupSched == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "backups unavailable"})
		return
	}
	go func() {
		if err := s.backupSched.RunAll(context.Background()); err != nil {
			log.Printf("backup: manual run: %v", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// canRecover is whether an engine both declares the recover capability and implements
// the method. Both, because either alone is a button that fails: a declared capability
// with no method is a build that lost it, and a method with no capability is an adapter
// image that predates the verb.
func canRecover(p apps.Provider) bool {
	_, ok := p.(apps.Recoverer)
	return ok && p.Caps().Recover
}

// handleRecoverEngine reconnects a rebuilt box to the repository its space already
// holds, with the key the user was mailed.
//
//	200 {snapshots, pinned}   reconnected; every existing snapshot pinned against retention
//	400 {error}               an empty or wrong key — the user's typo, not a fault
//	404                       no such engine
//	409                       the engine is not waiting for a key
//	501                       the engine cannot take one (Caps.Recover)
//
// **The key never reaches a log**, here or below: it goes from the body into the
// engine's candidate file and nowhere else, and nothing on this path prints the body.
//
// The state is checked fresh, with the status cache dropped first. A box whose
// recovery has just finished — in another tab, or by the host side — must answer 409
// rather than run the verb again against a repository it is already connected to.
//
// It runs detached from the request: a reconnect that the browser abandoned halfway is
// still worth finishing, and an interrupted one is worth less than either outcome.
func (s *Server) handleRecoverEngine(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p, ok := engineByID(s.engines, id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown backup engine: " + id})
		return
	}
	rec, implements := p.(apps.Recoverer)
	if !implements || !p.Caps().Recover {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "this backup engine cannot take a recovery key"})
		return
	}

	var in struct {
		Key string `json:"key"`
	}
	// A key is tens of bytes. The cap is only so a pasted novel is a 400 rather than an
	// allocation.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	key := strings.TrimSpace(in.Key)
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "enter your backup key"})
		return
	}

	ctx := context.WithoutCancel(r.Context())
	if inv, ok := p.(statusInvalidator); ok {
		inv.Invalidate()
	}
	if !p.Status(ctx).NeedsRecovery {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this backup storage is not waiting for a key"})
		return
	}

	res, err := rec.Recover(ctx, key)
	switch {
	case errors.Is(err, apps.ErrWrongKey):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "that key does not open these backups"})
		return
	case errors.Is(err, apps.ErrNotConfigured):
		// The engine found nothing to recover after all — the host side moved on between
		// the status above and the verb.
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this backup storage is not waiting for a key"})
		return
	case err != nil:
		log.Printf("backup: %s: recovery failed: %v", id, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	log.Printf("backup: %s: reconnected to the existing repository (%d snapshots, %d pinned)", id, res.Snapshots, res.Pinned)

	// The user just typed this key, so they hold a copy: record that instead of letting
	// the automatic send mail it straight back to them.
	s.recordKeyHeldByUser(id)
	// A connected offsite engine is what the legacy fallback writes to, so the write
	// sets are recomputed now rather than at the next restart.
	if s.backupConf != nil {
		applyEngineSettings(s.engines, s.backupConf)
	}
	// Closed now rather than at the next detector pass, so the bell does not go on
	// asking for a key the user has just entered.
	if s.incidents != nil {
		s.incidents.Resolve(incident.KindBackupRecovery + ":" + id)
	}
	writeJSON(w, http.StatusOK, res)
}

// resolvedView renders one engine's resolved retention for the settings page.
//
// MaxAge comes back as days because that is what the user typed and what the picker
// shows; a duration in nanoseconds would make the client convert back and guess at
// rounding.
func resolvedView(r backupconfig.Resolved, caps apps.Caps) *retentionView {
	return &retentionView{
		Mode:         string(r.Mode),
		Keep:         r.Keep,
		Count:        r.Count,
		MaxAgeDays:   int(r.MaxAge / (24 * time.Hour)),
		Source:       string(r.Source),
		Locked:       r.Locked,
		SelfExpiring: caps.Retention,
		Tiered:       !caps.NeedsLocalSpace,
	}
}

// writesTo reports whether a trigger currently writes to one engine.
func writesTo(set *backup.Set, t apps.Trigger, id string) bool {
	for _, p := range set.Writers(t) {
		if p.ID() == id {
			return true
		}
	}
	return false
}
