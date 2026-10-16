package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/backupconfig"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/engine"
)

// Timeouts. Everything that moves real bytes is unbounded and everything that reads
// metadata is not — a metadata call that hangs is a page that hangs, while a snapshot
// that takes six hours is a snapshot.
const (
	metaTimeout   = 5 * time.Minute
	statusTimeout = 2 * time.Minute
	deleteTimeout = 10 * time.Minute
	capsTimeout   = 1 * time.Minute
	// recoverTimeout covers a connect plus a pin per snapshot, in batches. Bounded,
	// because a user is watching a spinner; generous, because a space with years of
	// nightly history is thousands of pins.
	recoverTimeout = 15 * time.Minute
	// initTimeout covers creating an empty repository: a handful of blob writes.
	initTimeout = 3 * time.Minute
	// probeTimeout covers one throwaway connect attempt.
	probeTimeout = 1 * time.Minute
	// changeSecretTimeout covers one rewrite of the repository's key blob — seconds —
	// and, after an ambiguous failure, the two probes that settle which key it took.
	changeSecretTimeout = 3 * time.Minute
	unbounded           = 0
)

// engineCaps is the root this engine runs as, narrowed.
//
// PUID:PGID cannot read an app's own private data — postgres' 0700 pgdata is the usual
// one — and CHOWN/FOWNER/FSETID are what let a restore put recorded ownership and setuid
// bits back, so a restored database starts instead of coming back as a directory
// postgres refuses. It mirrors what the engine stack declares for its resident
// container; the two must not drift.
var engineCaps = engine.Caps{
	Drop: []string{"ALL"},
	Add:  []string{"DAC_READ_SEARCH", "DAC_OVERRIDE", "CHOWN", "FOWNER", "FSETID"},
}

// defaultUserDataKeep is the retention the user-data set gets. It is not configurable
// per-set today; the box-wide tiers govern apps.
var defaultUserDataKeep = backupconfig.Keep{Latest: 2, Daily: 7, Weekly: 4, Monthly: 12, Annual: 0}

// Provider is one adapter-backed engine.
type Provider struct {
	cfg    config.Config
	desc   Descriptor
	runner *engine.Runner

	mu       sync.Mutex
	status   apps.EngineStatus
	statusAt time.Time
	caps     *apps.Caps

	// secretMu serialises Recover, InitSecret and ChangeSecret. There is one candidate,
	// one .initial and one .next file per engine directory, and two attempts racing
	// would have one install or test the other's key.
	secretMu sync.Mutex

	// approved says whether the owner has enabled this engine. Nil is always enabled,
	// which is what New builds; the server builds every engine with NewGated.
	approved func() bool
}

// ErrNotApproved is every verb of an engine the owner has not enabled.
//
// It is returned BEFORE anything is run. Approving an engine is approving the image
// Maison will exec as root with the data root in scope, so an engine nobody approved
// must not cost so much as a `capabilities` call into it.
var ErrNotApproved = errors.New("this backup engine is not enabled — enable it in Settings › Backups")

// New builds a provider from a descriptor. It performs no I/O: a provider is
// constructed at boot, including on a box whose engine container is not running.
func New(cfg config.Config, d Descriptor) *Provider {
	return &Provider{cfg: cfg, desc: d, runner: engine.New(cfg)}
}

// NewGated is New for an engine that runs only while approved() says so.
//
// The engine is registered either way: an engine that is not enabled still has a tab,
// an incident and an Enable button, and one that was enabled and is not any more keeps
// its history in the list even though reading it now fails with ErrNotApproved.
func NewGated(cfg config.Config, d Descriptor, approved func() bool) *Provider {
	p := New(cfg, d)
	p.approved = approved
	return p
}

// Descriptor is what this provider was built from. The server compares it against a
// fresh Discover to tell a changed descriptor from an unchanged one.
func (p *Provider) Descriptor() Descriptor { return p.desc }

// Approved reports whether the owner has enabled this engine.
func (p *Provider) Approved() bool { return p.approved == nil || p.approved() }

func (p *Provider) ID() string { return p.desc.EngineID }

// dir is the engine's own directory, which the host side owns and Maison only reads.
// The path is container-side, and the engine container mounts the data root at the same
// place, so the same string is valid on both sides.
func (p *Provider) dir() string { return p.cfg.BackupEngineDir(p.desc.EngineID) }

// --- capabilities ------------------------------------------------------------

// Caps asks the adapter what it can do, once.
//
// The answer is cached for the life of this provider because it is a property of a pinned
// image: it cannot change without the image changing, and a changed image means a changed
// descriptor, which the server answers with a fresh provider (rescanEngines).
//
// A failure is NOT cached, and returns the zero value — "this engine cannot" for every
// field, which is the conservative answer and the one that keeps a transient failure
// from being recorded as a permanent incapacity.
func (p *Provider) Caps() apps.Caps {
	if !p.Approved() {
		return apps.Caps{}
	}
	p.mu.Lock()
	if p.caps != nil {
		defer p.mu.Unlock()
		return *p.caps
	}
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), capsTimeout)
	defer cancel()

	var wc wireCaps
	if err := p.call(ctx, capsTimeout, nil, &wc, "capabilities"); err != nil {
		log.Printf("backup: %s: could not read capabilities: %v", p.ID(), err)
		return apps.Caps{}
	}
	if wc.Protocol != Protocol {
		// Refused rather than guessed at. An adapter speaking a dialect this build does
		// not know is exactly the case where "try it and see" costs a backup.
		log.Printf("backup: %s: adapter speaks protocol %q, this build speaks %q — engine disabled",
			p.ID(), wc.Protocol, Protocol)
		return apps.Caps{}
	}
	if wc.EngineID != p.desc.EngineID {
		log.Printf("backup: %s: adapter identifies as %q — engine disabled", p.ID(), wc.EngineID)
		return apps.Caps{}
	}

	c := wc.caps()
	p.mu.Lock()
	p.caps = &c
	p.mu.Unlock()
	return c
}

// --- status ------------------------------------------------------------------

// repoState is the host-written description of the provisioned space — the half of the
// status an engine cannot be asked, because it describes the SPACE rather than the
// engine pointed at it. A PCS provisioned by Yundera says so; a self-hoster pointing the
// same engine at their own bucket must not be told they are using someone's service.
type repoState struct {
	Label string `json:"label"`
	// RecoveryHelpURL is where a user without their key can start over. Absent on a
	// box whose deployment offers no such place, and on a template that predates it.
	RecoveryHelpURL string `json:"recoveryHelpUrl,omitempty"`
}

func (p *Provider) readState() repoState {
	var st repoState
	b, err := os.ReadFile(filepath.Join(p.dir(), "state.json"))
	if err != nil {
		return st
	}
	if err := json.Unmarshal(b, &st); err != nil {
		log.Printf("backup: %s: unreadable state.json: %v", p.ID(), err)
		return repoState{}
	}
	return st
}

// Status is cached briefly, because both the settings page and every app's Backups tab
// ask and answering costs a round trip into the engine.
func (p *Provider) Status(ctx context.Context) apps.EngineStatus {
	if !p.Approved() {
		// Answered from files alone. Not cached either: the moment the owner clicks
		// Enable, the next page load has to stop saying it.
		state := p.readState()
		return apps.EngineStatus{
			Label:         state.Label,
			Detail:        ErrNotApproved.Error(),
			NeedsApproval: true,
		}
	}
	p.mu.Lock()
	if time.Since(p.statusAt) < 30*time.Second {
		defer p.mu.Unlock()
		return p.status
	}
	p.mu.Unlock()

	st := p.probe(ctx)

	p.mu.Lock()
	p.status, p.statusAt = st, time.Now()
	p.mu.Unlock()
	return st
}

// Invalidate drops the cached status, so the next Status asks the engine again.
//
// For the one change Maison itself causes rather than observes: a recovery turns a box
// with no key into a connected one, and a page that re-read a thirty-second-old "needs
// your key" straight after the user typed it would tell them it had not worked.
func (p *Provider) Invalidate() {
	p.mu.Lock()
	p.statusAt = time.Time{}
	p.mu.Unlock()
}

func (p *Provider) probe(ctx context.Context) apps.EngineStatus {
	// The label is read first and kept whatever the probe says: a box that has been
	// issued a space but has not connected to it yet should still be able to say whose
	// space it is, rather than being described by its engine while it is being set up.
	state := p.readState()

	var ws wireStatus
	raw, err := p.callRaw(ctx, statusTimeout, nil, "status")
	if err != nil {
		st := apps.EngineStatus{Label: state.Label, RecoveryHelpURL: state.RecoveryHelpURL, Detail: err.Error()}
		// "Not configured" is still read for a recovery flag. The protocol has the
		// adapter answer needsRecovery with exit 0, but an adapter that also exits 10
		// for it is saying something true, and collapsing it to plain "not configured"
		// would hide every backup the user owns behind the words for a fresh box.
		// needsSecret is read the same way, for the same reason.
		if errors.Is(err, apps.ErrNotConfigured) && len(raw) > 0 &&
			json.Unmarshal(raw, &ws) == nil && (ws.NeedsRecovery || ws.NeedsSecret) {
			st.NeedsRecovery = ws.NeedsRecovery
			st.NeedsSecret = ws.NeedsSecret && !ws.NeedsRecovery
			if ws.Detail != "" {
				st.Detail = ws.Detail
			}
		}
		return st
	}
	if err := unmarshalResult(raw, &ws); err != nil {
		return apps.EngineStatus{Label: state.Label, RecoveryHelpURL: state.RecoveryHelpURL, Detail: err.Error()}
	}
	// A repository that already holds backups is never one to create: recovery wins if
	// an engine ever says both, and a configured engine has nothing left to create.
	needsSecret := ws.NeedsSecret && !ws.NeedsRecovery && !ws.Configured
	return apps.EngineStatus{
		Configured:      ws.Configured,
		Connected:       ws.Connected,
		Label:           state.Label,
		Identity:        ws.Identity,
		Detail:          ws.Detail,
		NeedsRecovery:   ws.NeedsRecovery,
		NeedsSecret:     needsSecret,
		RecoveryHelpURL: state.RecoveryHelpURL,
	}
}

// --- sources -----------------------------------------------------------------

// The two source ids the protocol defines. Maison owns the mapping from a source to its
// path and passes both; the adapter never derives one from the other.
const (
	appPrefix  = "app:"
	userDataID = "userdata"
)

func appSource(app string) string { return appPrefix + app }

func (p *Provider) appPath(app string) string { return filepath.Join(p.cfg.AppsDir(), app) }

func (p *Provider) sourcePath(sourceID string) string {
	if sourceID == userDataID {
		return p.cfg.DataRoot
	}
	return p.appPath(strings.TrimPrefix(sourceID, appPrefix))
}

// appOf recovers the app name from a source id, rejecting the user-data set and
// anything that is not a valid project name.
//
// The id came back from a repository, which is untrusted input, and it feeds path
// construction — so it is re-validated on the way in rather than on the way out.
func appOf(sourceID string) (string, bool) {
	if !strings.HasPrefix(sourceID, appPrefix) {
		return "", false
	}
	app := strings.TrimPrefix(sourceID, appPrefix)
	if app == "" || !apps.ValidProjectName(app) {
		return "", false
	}
	return app, true
}
