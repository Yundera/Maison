// Package backup holds the set of backup engines a deployment knows about and the
// two rules that have to hold across them.
//
// The engine is a user-facing choice, which means a box can change it — and the
// moment it can, "which engine is selected" stops being a safe way to find a
// backup. Both rules below exist for that one reason. See docs/backup.md.
package backup

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/yundera/maison/internal/apps"
)

// Set is the engines this deployment knows about, plus which one currently
// receives writes.
//
// Engines are never removed once registered. An engine can stop being *offered* —
// dropped from the picker, superseded, deprecated — but it must stay registered
// read-only, because backups it wrote in the past are still on the box or still in
// a repository, and they have to remain listable, restorable and deletable.
type Set struct {
	mu      sync.RWMutex
	byID    map[string]apps.Provider
	order   []string // registration order, so listing is stable across restarts
	written string   // ID of the engine that receives new backups
}

// New builds a Set from the engines a deployment has. The first one registered is
// the initial write target, which makes the local engine — always present, needing
// no configuration — the natural default.
func New(providers ...apps.Provider) *Set {
	s := &Set{byID: map[string]apps.Provider{}}
	for _, p := range providers {
		s.Register(p)
	}
	return s
}

// Register adds an engine. Re-registering an ID replaces the instance but keeps its
// position, so a reconfigured engine does not jump around in the UI.
func (s *Set) Register(p apps.Provider) {
	if p == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := p.ID()
	if _, seen := s.byID[id]; !seen {
		s.order = append(s.order, id)
	}
	s.byID[id] = p
	if s.written == "" {
		s.written = id
	}
}

// IDs returns every registered engine ID in registration order.
func (s *Set) IDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.order...)
}

// Get returns one engine by ID.
func (s *Set) Get(id string) (apps.Provider, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.byID[id]
	return p, ok
}

// Writer is the engine new backups go to.
//
// **The selected engine governs writes only.** Nothing reads through it — see List
// and Locate — because a backup written by a previously selected engine is still
// the user's backup.
func (s *Set) Writer() apps.Provider {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byID[s.written]
}

// SetWriter picks the engine that receives new backups. It refuses an unknown ID
// rather than silently falling back, because silently writing somewhere other than
// where the user asked is how a user ends up believing their data is offsite when
// it is not.
func (s *Set) SetWriter(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[id]; !ok {
		return fmt.Errorf("unknown backup engine: %s", id)
	}
	s.written = id
	return nil
}

// List returns every backup of one app, from every engine, newest first.
//
// **Backups are NOT merged across engines.** A copy in the local archive and a
// snapshot in a repository are two backups that happen to share a stamp: they were
// written separately, they can be deleted separately, and restoring one is not
// restoring the other. Folding them into a single row meant the identity of a backup
// was (app, stamp), which only holds while exactly one engine is offsite — with two
// remote engines it silently conflated two unrelated snapshots and reported the pair
// as "on this disk + offsite", which was true of neither.
//
// So the identity of a backup is **(engine, app, stamp)**, and every caller that
// restores or deletes one says which engine it means. Engines run independently; the
// selected engine governs writes and nothing else.
//
// An engine that cannot answer is skipped rather than fatal — see logRead.
func (s *Set) List(ctx context.Context, app string) []apps.Backup {
	// Guarded here, not just in each engine: the name arrives from a URL and reaches a
	// subprocess argument in a remote engine. Nothing to list is the honest answer for
	// a name no app can have.
	if !apps.ValidProjectName(app) {
		return nil
	}
	var out []apps.Backup
	for _, p := range s.providers() {
		got, err := p.List(ctx, app)
		if err != nil {
			logRead(err, "list "+app, p)
			continue
		}
		out = append(out, got...)
	}
	return sortedBackups(out)
}

// sortedBackups orders newest first.
//
// Stable, and fed in registration order, so two engines holding the same stamp keep a
// fixed relative order — the local engine first, because it is registered first. A row
// order that changed between reloads would move the delete button under the cursor.
func sortedBackups(out []apps.Backup) []apps.Backup {
	// The stamp is fixed-width and lexically ordered, so comparing names is comparing
	// times. Name (not Stamp) breaks the tie between "<stamp>" and "<stamp>.zip".
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Stamp != out[j].Stamp {
			return out[i].Stamp > out[j].Stamp
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// logRead reports an engine that could not answer a read.
//
// An engine that is not configured contributes nothing and is not an error: that is
// the normal state of a remote engine on a box whose host-side setup has not run, and
// it must not stop the local engine's archives from being listed. Any other error is
// logged and skipped for the same reason — a broken repository should degrade the
// page, not empty it.
func logRead(err error, what string, p apps.Provider) {
	if !errors.Is(err, apps.ErrNotConfigured) {
		log.Printf("backup: %s from engine %s: %v", what, p.ID(), err)
	}
}

// ListAll is the global Backups view: every app with backups in any engine, grouped,
// newest first within each group, with local folder archives measured and totals
// summed.
//
// It asks each engine once — not once per app — because for a remote engine every
// call is a subprocess against the repository, and this page lists every app at once.
// That is the whole reason Provider.ListAll exists in the bulk shape it does.
//
// It is the engine-aware replacement for the old apps.ListAll, which could only ever
// see the data disk. The difference shows up in the case the page exists for — an app
// that is gone from this box — because after a rebuild the only thing that still knows
// the app existed is the repository.
//
// appsDir is consulted only to mark orphans; pass "" to skip that. backupsDir is
// needed to measure folder archives, which Measure leaves to the callers that
// actually display a size.
func (s *Set) ListAll(ctx context.Context, backupsDir, appsDir string) []apps.AppBackups {
	// app -> every engine's backups of it, concatenated in registration order. Not
	// deduped: see List for why (engine, app, stamp) is the identity.
	byApp := map[string][]apps.Backup{}
	for _, p := range s.providers() {
		got, err := p.ListAll(ctx)
		if err != nil {
			logRead(err, "list all", p)
			continue
		}
		for app, list := range got {
			byApp[app] = append(byApp[app], list...)
		}
	}

	return groupApps(byApp, backupsDir, appsDir)
}

// ListAllIn is ListAll for ONE engine — the shape the Backups page reads, because it
// shows a tab per engine and each tab is that engine's repository, not a view of a
// union. Totals are per engine for the same reason: a header summing across engines
// would describe a quantity of storage nobody is paying for.
func (s *Set) ListAllIn(ctx context.Context, engine, backupsDir, appsDir string) []apps.AppBackups {
	p, ok := s.Get(engine)
	if !ok {
		return nil
	}
	got, err := p.ListAll(ctx)
	if err != nil {
		logRead(err, "list all", p)
		return nil
	}
	return groupApps(got, backupsDir, appsDir)
}

// groupApps turns app -> backups into the page's per-app groups, measured and sorted.
func groupApps(byApp map[string][]apps.Backup, backupsDir, appsDir string) []apps.AppBackups {
	out := make([]apps.AppBackups, 0, len(byApp))
	for app, all := range byApp {
		list := apps.Measure(backupsDir, sortedBackups(all))
		if len(list) == 0 {
			continue
		}
		var total int64
		for i := range list {
			total += list[i].Size
		}
		orphan := false
		if appsDir != "" {
			_, err := os.Stat(filepath.Join(appsDir, app))
			orphan = err != nil
		}
		out = append(out, apps.AppBackups{App: app, Orphan: orphan, Backups: list, Total: total})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].App < out[j].App })
	return out
}

// LocateIn finds a backup in ONE named engine.
//
// This is the shape every user-initiated restore and delete uses, because the user
// picked a row and a row belongs to an engine. Locate below is the weaker form, for
// the one caller that genuinely has no engine to name.
func (s *Set) LocateIn(ctx context.Context, engine, app, stamp string) (apps.Provider, apps.Backup, error) {
	if err := validName(app, stamp); err != nil {
		return nil, apps.Backup{}, err
	}
	p, ok := s.Get(engine)
	if !ok {
		return nil, apps.Backup{}, fmt.Errorf("unknown backup engine: %s", engine)
	}
	got, err := p.List(ctx, app)
	if err != nil {
		return nil, apps.Backup{}, err
	}
	for _, b := range got {
		if b.Name == stamp {
			return p, b, nil
		}
	}
	return nil, apps.Backup{}, fmt.Errorf("backup not found in engine %s: %s", engine, stamp)
}

// Locate finds *an* engine that can serve a read of (app, stamp), with no engine
// named.
//
// It survives only for the store's install-from-backup path, where the user chose an
// app rather than a row and there is no engine in the request. Everything reached from
// the Backups UI goes through LocateIn instead.
//
// **Dispatch is on where the backup actually is, never on which engine is
// selected.** Without this, switching engine orphans every backup the previous one
// wrote — they would still exist, and nothing would be able to reach them.
//
// Among engines that have it, the one offering an instant restore wins: a local
// archive is a rename, so preferring it turns a download into no work at all.
func (s *Set) Locate(ctx context.Context, app, stamp string) (apps.Provider, apps.Backup, error) {
	if err := validName(app, stamp); err != nil {
		return nil, apps.Backup{}, err
	}
	var (
		best   apps.Provider
		bestB  apps.Backup
		bestOK bool
	)
	for _, p := range s.providers() {
		got, err := p.List(ctx, app)
		if err != nil {
			continue
		}
		for _, b := range got {
			if b.Name != stamp {
				continue
			}
			if !bestOK || (p.Caps().InstantRestore && !best.Caps().InstantRestore) {
				best, bestB, bestOK = p, b, true
			}
		}
	}
	if !bestOK {
		return nil, apps.Backup{}, fmt.Errorf("backup not found: %s", stamp)
	}
	return best, bestB, nil
}

// Delete removes one backup from ONE engine.
//
// It used to delete from every engine holding the stamp, which followed from rows
// being merged: one row meant one backup, so deleting it had to remove every copy or
// the row reappeared. Now that a row *is* an engine's backup, deleting the local
// archive must leave the offsite snapshot alone — that is the whole point of keeping
// both, and a delete that quietly took the offsite copy too would destroy the only
// disaster-recovery copy the user had.
func (s *Set) Delete(ctx context.Context, engine, app, stamp string) error {
	p, _, err := s.LocateIn(ctx, engine, app, stamp)
	if err != nil {
		return err
	}
	return p.Delete(ctx, app, stamp)
}

// validName rejects an app or backup name that is not one, before any engine is
// asked about it.
//
// The set is the single door every read and delete goes through, so this is where the
// traversal guard belongs: an engine must not be handed a name that could become a
// path, and "notes" must come back as a malformed request rather than as a backup
// that happens not to exist — the second invites a retry, the first says what is
// wrong. Each provider still validates for itself; this is the check that does not
// depend on every future engine remembering to.
func validName(app, stamp string) error {
	if !apps.ValidProjectName(app) {
		return fmt.Errorf("invalid app name: %s", app)
	}
	if _, ok := apps.ParseBackupName(app, stamp); !ok {
		return fmt.Errorf("not a backup name: %s", stamp)
	}
	return nil
}

// providers returns a snapshot of the registered engines in registration order, so
// callers iterate without holding the lock across engine calls that may block on a
// network.
func (s *Set) providers() []apps.Provider {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]apps.Provider, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.byID[id])
	}
	return out
}
