// Package backuptest provides a fake backup engine for tests.
//
// It is a real package rather than a _test.go file because two packages need it:
// internal/backup tests the rules that hold across engines, and internal/apps tests
// the orchestration that drives one. Duplicating it would let the two copies drift,
// and the whole point of a fake here is that both sides agree on what an engine is
// allowed to do.
package backuptest

import (
	"context"
	"sync"
	"time"

	"github.com/yundera/maison/internal/apps"
)

// Fake is a backup engine that keeps everything in memory, records what it was
// asked to do, and can be made to fail on demand.
//
// It exists to test sequencing — which calls happen, in what order, and what
// happens to the app when one of them fails — which is where the bugs in this
// feature will be. It deliberately stores no bytes.
type Fake struct {
	Name string
	Cap  apps.Caps

	// Errors to return. Set one to make that operation fail; the zero value succeeds.
	ListErr      error
	SnapshotErr  error
	CommitErr    error
	MaterialErr  error
	DeleteErr    error
	SnapshotHang time.Duration // block this long in Snapshot, to exercise timeouts

	mu    sync.Mutex
	store map[string][]apps.Backup // app -> committed backups
	// Calls records every operation in order, as "verb:app/stamp" (Snapshot carries
	// its pass, e.g. "snapshot:jellyfin/2026-01-01_000000#2"). Asserting on this is
	// how a test pins a sequence without reaching into the engine's state.
	Calls []string
}

// NewFake builds an engine with the given ID and capabilities.
func NewFake(id string, caps apps.Caps) *Fake {
	return &Fake{Name: id, Cap: caps, store: map[string][]apps.Backup{}}
}

// NewRemote builds a fake that behaves like an offsite engine: no local space
// needed, no instant restore, capable of restoring in place.
func NewRemote(id string) *Fake {
	return NewFake(id, apps.Caps{Offsite: true, InPlaceRestore: true, Retention: true})
}

// NewLocalLike builds a fake that behaves like the built-in local engine.
func NewLocalLike(id string) *Fake {
	return NewFake(id, apps.Caps{InstantRestore: true, NeedsLocalSpace: true})
}

func (f *Fake) ID() string      { return f.Name }
func (f *Fake) Caps() apps.Caps { return f.Cap }

// Seed adds an already-committed backup, for tests that need a starting state.
func (f *Fake) Seed(app string, stamps ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range stamps {
		f.store[app] = append(f.store[app], f.backup(app, s))
	}
}

func (f *Fake) backup(app, stamp string) apps.Backup {
	b, _ := apps.ParseBackupName(app, stamp)
	b.Tier = apps.TierRemote
	if f.Cap.InstantRestore {
		b.Tier = apps.TierLocal
	}
	b.Engine = f.Name
	return b
}

func (f *Fake) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, s)
}

func (f *Fake) Snapshot(ctx context.Context, app, stamp string, opts apps.SnapshotOpts, emit func(apps.Event)) error {
	f.record("snapshot:" + app + "/" + stamp + "#" + itoa(opts.Pass))
	if emit != nil {
		emit(apps.Event{Pct: 100, Message: "fake snapshot"})
	}
	if f.SnapshotHang > 0 {
		select {
		case <-time.After(f.SnapshotHang):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.SnapshotErr
}

func (f *Fake) Commit(ctx context.Context, app, stamp string, opts apps.SnapshotOpts, emit func(apps.Event)) (apps.Backup, error) {
	f.record("commit:" + app + "/" + stamp)
	if f.CommitErr != nil {
		return apps.Backup{}, f.CommitErr
	}
	b := f.backup(app, stamp)
	f.mu.Lock()
	f.store[app] = append(f.store[app], b)
	f.mu.Unlock()
	return b, nil
}

func (f *Fake) Abort(ctx context.Context, app, stamp string) error {
	f.record("abort:" + app + "/" + stamp)
	return nil
}

func (f *Fake) List(ctx context.Context, app string) ([]apps.Backup, error) {
	if f.ListErr != nil {
		return nil, f.ListErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]apps.Backup(nil), f.store[app]...), nil
}

func (f *Fake) Materialize(ctx context.Context, app, stamp string, emit func(apps.Event)) error {
	f.record("materialize:" + app + "/" + stamp)
	return f.MaterialErr
}

func (f *Fake) Delete(ctx context.Context, app, stamp string) error {
	f.record("delete:" + app + "/" + stamp)
	if f.DeleteErr != nil {
		return f.DeleteErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.store[app][:0]
	for _, b := range f.store[app] {
		if b.Name != stamp {
			kept = append(kept, b)
		}
	}
	f.store[app] = kept
	return nil
}

func itoa(n int) string {
	if n < 0 || n > 9 {
		return "?"
	}
	return string(rune('0' + n))
}
