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
	"os"
	"path/filepath"
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
	RestoreErr   error
	DeleteErr    error
	SnapshotHang time.Duration // block this long in Snapshot, to exercise timeouts

	// MaterializeInto, when set, is the .backups directory this fake writes into when
	// asked to bring a backup down — enough of a real download for the paths above it
	// (restore, and the store's install-from-backup) to be exercised end to end against
	// an engine whose backups are not already on the disk. Left empty, Materialize only
	// records the call.
	MaterializeInto string

	mu    sync.Mutex
	store map[string][]apps.Backup // app -> committed backups
	// ListCalls counts per-app listings, and ListAllCalls bulk ones. They are counters
	// rather than Calls entries because what matters about them is *how many*: for a
	// remote engine each one is a subprocess against the repository, so the pages that
	// list many apps must cost one regardless of how many they show — the global page,
	// and the store's install picker, which reads the catalog through ListAll for the
	// same reason.
	ListCalls    int
	ListAllCalls int
	// Calls records every operation in order, as "verb:app/stamp" (Snapshot carries
	// its pass, e.g. "snapshot:jellyfin/2026-01-01_000000#2", and appends "+consume"
	// when the caller offered it the app folder outright). Asserting on this is how a
	// test pins a sequence without reaching into the engine's state.
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

// Snapshot records the call and stores nothing. It ignores SnapshotOpts.Consume
// beyond recording it, which is exactly what a streaming engine does: it reads the app
// folder and leaves it, and the registry removes it after Commit. That makes this fake
// the remote half of the Consume contract, and the assertion that the registry cleans
// up after such an engine rather than assuming the folder is gone.
func (f *Fake) Snapshot(ctx context.Context, app, stamp string, opts apps.SnapshotOpts, emit func(apps.Event)) error {
	call := "snapshot:" + app + "/" + stamp + "#" + itoa(opts.Pass)
	if opts.Consume {
		call += "+consume"
	}
	f.record(call)
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
	f.mu.Lock()
	f.ListCalls++
	f.mu.Unlock()
	if f.ListErr != nil {
		return nil, f.ListErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]apps.Backup(nil), f.store[app]...), nil
}

// ListAll returns everything this fake holds, grouped by app. It honours ListErr too:
// a test that makes listing fail is testing a broken engine, and a broken engine
// cannot answer this either.
func (f *Fake) ListAll(ctx context.Context) (map[string][]apps.Backup, error) {
	f.mu.Lock()
	f.ListAllCalls++
	f.mu.Unlock()
	if f.ListErr != nil {
		return nil, f.ListErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]apps.Backup{}
	for app, list := range f.store {
		if len(list) > 0 {
			out[app] = append([]apps.Backup(nil), list...)
		}
	}
	return out, nil
}

func (f *Fake) Materialize(ctx context.Context, app, stamp string, emit func(apps.Event)) error {
	f.record("materialize:" + app + "/" + stamp)
	if f.MaterialErr != nil || f.MaterializeInto == "" {
		return f.MaterialErr
	}
	dir := filepath.Join(apps.AppBackupDir(f.MaterializeInto, app), stamp)
	if err := os.MkdirAll(filepath.Join(dir, "db"), 0o755); err != nil {
		return err
	}
	// The same shape the real fixtures use, so what lands is recognisably the app.
	return os.WriteFile(filepath.Join(dir, "db", "data.sqlite"), []byte("rows"), 0o644)
}

func (f *Fake) RestoreInPlace(ctx context.Context, app, stamp, dst string, emit func(apps.Event)) error {
	f.record("restore-in-place:" + app + "/" + stamp)
	if !f.Cap.InPlaceRestore {
		return apps.ErrNotSupported
	}
	return f.RestoreErr
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
