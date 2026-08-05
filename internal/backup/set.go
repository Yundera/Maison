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

// List returns every backup of one app across every engine, newest first.
//
// The union is deduped on (app, name): the same backup can exist in more than one
// engine — an archive kept locally *and* pushed to a repository — and it is one
// backup, shown once, with Tier reporting that it is in both.
//
// An engine that is not configured contributes nothing and is not an error: that is
// the normal state of a remote engine on a box whose host-side setup has not run,
// and it must not stop the local engine's archives from being listed. Any other
// engine error is logged and skipped for the same reason — a broken repository
// should degrade the page, not empty it.
func (s *Set) List(ctx context.Context, app string) []apps.Backup {
	merged := map[string]apps.Backup{}
	for _, p := range s.providers() {
		got, err := p.List(ctx, app)
		if err != nil {
			if !errors.Is(err, apps.ErrNotConfigured) {
				log.Printf("backup: list %s from engine %s: %v", app, p.ID(), err)
			}
			continue
		}
		for _, b := range got {
			if prev, dup := merged[b.Name]; dup {
				merged[b.Name] = mergeTiers(prev, b)
				continue
			}
			merged[b.Name] = b
		}
	}
	out := make([]apps.Backup, 0, len(merged))
	for _, b := range merged {
		out = append(out, b)
	}
	// The stamp is fixed-width and lexically ordered, so comparing names is comparing
	// times. Name (not Stamp) breaks the tie between "<stamp>" and "<stamp>.zip".
	sort.Slice(out, func(i, j int) bool {
		if out[i].Stamp != out[j].Stamp {
			return out[i].Stamp > out[j].Stamp
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// mergeTiers folds the same backup seen in two engines into one row.
//
// The surviving Engine is the one that can restore it fastest, because that is the
// engine Locate will pick and the row should say where the restore will actually
// come from. Size prefers whichever engine measured one, since the local engine
// deliberately leaves folder archives at 0.
func mergeTiers(a, b apps.Backup) apps.Backup {
	out := a
	if b.Engine == apps.EngineLocal {
		out = b
	}
	out.Tier = apps.TierBoth
	if out.Size == 0 {
		if a.Size > b.Size {
			out.Size = a.Size
		} else {
			out.Size = b.Size
		}
	}
	return out
}

// Locate finds the engine that should serve a read of (app, stamp).
//
// **Dispatch is on where the backup actually is, never on which engine is
// selected.** Without this, switching engine orphans every backup the previous one
// wrote — they would still exist, and nothing would be able to reach them.
//
// Among engines that have it, the one offering an instant restore wins: a local
// archive is a rename, so preferring it turns a download into no work at all.
func (s *Set) Locate(ctx context.Context, app, stamp string) (apps.Provider, apps.Backup, error) {
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

// Delete removes a backup from every engine that holds it.
//
// It is deliberately not "delete from the engine that owns it": a backup present
// both locally and in a repository is one backup to the user, shown as one row, and
// deleting it has to mean what the row says. Partial failure is reported but does
// not stop the remaining engines — leaving a copy behind because a different engine
// errored would make the row reappear with no explanation.
func (s *Set) Delete(ctx context.Context, app, stamp string) error {
	found := false
	var errs []error
	for _, p := range s.providers() {
		got, err := p.List(ctx, app)
		if err != nil {
			continue
		}
		for _, b := range got {
			if b.Name != stamp {
				continue
			}
			found = true
			if err := p.Delete(ctx, app, stamp); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", p.ID(), err))
			}
		}
	}
	if !found {
		return fmt.Errorf("backup not found: %s", stamp)
	}
	return errors.Join(errs...)
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
