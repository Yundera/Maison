package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/backup"
	"github.com/yundera/maison/internal/backup/kopia"
	"github.com/yundera/maison/internal/backupconfig"
	"github.com/yundera/maison/internal/config"
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
	applyChosenEngine(set, store)
	return set
}

// applyChosenEngine points the set's writer at whatever the configuration now says.
//
// It **mutates the set in place** rather than returning a new one, and that matters:
// the registry, the scheduler and the user-data coordinator all hold the same *Set, and
// none of them is re-handed it when the engine setting changes. Replacing the set here
// would leave every one of them writing through the previous instance — silently, since
// the old set is perfectly functional and merely wrong about which engine the user
// picked.
func applyChosenEngine(set *backup.Set, store *backupconfig.Store) {
	// The user's override wins; empty means "follow whatever the deployment
	// provisioned", which is inferred below rather than stored.
	if chosen := store.Get().Engine; chosen != "" {
		if err := set.SetWriter(chosen); err != nil {
			log.Printf("backup: %v (falling back to the local engine)", err)
		}
		return
	}
	// No override: prefer a remote engine that is actually connected. That inference
	// *is* the provisioning signal — a repository the host-side script has connected —
	// so there is no second file for the two sides to disagree about.
	//
	// The local engine is the fallback and is reasserted explicitly, because clearing an
	// override has to be able to move the writer *back* — not merely leave it wherever
	// the last explicit choice put it.
	if err := set.SetWriter(apps.EngineLocal); err != nil {
		log.Printf("backup: %v", err)
	}
	if k, ok := set.Get(kopia.ID); ok {
		if p, isKopia := k.(*kopia.Provider); isKopia && p.Status(context.Background()).Connected {
			_ = set.SetWriter(kopia.ID)
		}
	}
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
}

type engineInfo struct {
	ID        string `json:"id"`
	Connected bool   `json:"connected"`
	Detail    string `json:"detail,omitempty"`
	Offsite   bool   `json:"offsite"`
}

func (s *Server) handleBackupStatus(w http.ResponseWriter, r *http.Request) {
	if s.engines == nil {
		writeJSON(w, http.StatusOK, engineStatus{Active: apps.EngineLocal})
		return
	}
	out := engineStatus{
		Active: s.engines.Writer().ID(),
		Chosen: s.backupConf.Get().Engine,
		Config: s.backupConf.Get(),
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
			info.Connected, info.Detail = st.Connected, st.Detail
		}
		out.Engines = append(out.Engines, info)
	}
	if s.backupSched != nil {
		out.Run = s.backupSched.State()
		for _, t := range s.backupSched.Targets() {
			out.Targets = append(out.Targets, t.ID())
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
	if err := s.backupConf.Set(in); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// One path for both cases, mutating the set the rest of the process already holds:
	// an explicit choice and a cleared one are the same question asked of the same
	// object. See applyChosenEngine.
	if s.engines != nil {
		applyChosenEngine(s.engines, s.backupConf)
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
