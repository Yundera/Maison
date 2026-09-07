package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/backup"
	"github.com/yundera/maison/internal/backup/kopia"
	"github.com/yundera/maison/internal/backupconfig"
	"github.com/yundera/maison/internal/config"
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
	set := backup.New(
		apps.NewLocalProvider(cfg),
		kopia.New(cfg),
	)
	applyEngineSettings(set, store)
	return set
}

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
		log.Printf("backup: unknown backup engine %q (falling back to the local engine)", chosen)
		return apps.EngineLocal
	}
	// No override: prefer a remote engine that is actually connected. That inference
	// *is* the provisioning signal — a repository the host-side script has connected —
	// so there is no second file for the two sides to disagree about.
	if k, ok := set.Get(kopia.ID); ok {
		if p, isKopia := k.(*kopia.Provider); isKopia && p.Status(context.Background()).Connected {
			return kopia.ID
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
	if _, err := readEnginePassword(s.cfg, kopia.ID); err == nil {
		out.HasKey = true
	}
	if rec, sent := readKeySent(s.cfg); sent && !rec.SentAt.IsZero() {
		out.KeySent = &rec
	}
	for _, id := range s.engines.IDs() {
		p, _ := s.engines.Get(id)
		info := engineInfo{ID: id, Offsite: p.Caps().Offsite}
		// The local engine is always usable; a remote one has to be asked.
		info.Connected = true
		if k, ok := p.(*kopia.Provider); ok {
			st := k.Status(r.Context())
			info.Connected, info.Detail, info.Name = st.Connected, st.Detail, st.Label
		}
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
