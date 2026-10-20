package cgroupstat

import (
	"context"
	"sync"

	"github.com/yundera/maison/internal/dockerx"
)

// Target is one running container and the cgroup directory its counters live in.
type Target struct {
	ID      string
	Project string
	Service string
	Dir     string
}

// lister is the slice of dockerx.Client the tracker needs.
type lister interface {
	ListProjectContainers(ctx context.Context) ([]dockerx.Container, error)
}

// Tracker keeps the map from running compose containers to their cgroup
// directories, rebuilding it only when told the container set may have changed.
//
// Rebuilding costs one container LIST from the daemon (no stats, no inspect) and one
// walk of the cgroup tree. Between rebuilds a reading costs no daemon call at all,
// which is what makes per-app history affordable on the idle path. The server marks
// the tracker dirty from its Docker event watcher; a reader that finds a cgroup gone
// marks it dirty too, so a missed event costs one tick, not correctness.
//
// Safe for concurrent use. The target slice it hands out is never mutated after it
// is built — a rebuild replaces it — so callers may range over it without copying.
type Tracker struct {
	root string
	dx   lister

	refresh sync.Mutex // serialises rebuilds

	mu      sync.Mutex
	dirty   bool
	targets []Target
	byID    map[string]Target
}

// NewTracker returns a tracker over the cgroup tree at root. It starts dirty, so
// the first Targets call builds the map.
func NewTracker(root string, dx lister) *Tracker {
	return &Tracker{root: root, dx: dx, dirty: true}
}

// MarkDirty asks for a rebuild on the next Targets call.
func (t *Tracker) MarkDirty() {
	t.mu.Lock()
	t.dirty = true
	t.mu.Unlock()
}

// Targets returns every running compose container with a cgroup, rebuilding the map
// first if it is dirty. On a failed rebuild the previous map is kept and the
// tracker stays dirty.
func (t *Tracker) Targets(ctx context.Context) []Target {
	t.mu.Lock()
	dirty := t.dirty
	cur := t.targets
	t.mu.Unlock()
	if !dirty {
		return cur
	}

	t.refresh.Lock()
	defer t.refresh.Unlock()
	t.mu.Lock()
	if !t.dirty { // another caller rebuilt while we waited
		cur = t.targets
		t.mu.Unlock()
		return cur
	}
	t.dirty = false
	t.mu.Unlock()

	built, byID, err := t.build(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil {
		t.dirty = true
		return t.targets
	}
	t.targets, t.byID = built, byID
	return built
}

// Lookup returns the target for one container ID, rebuilding once if it is not
// known — a container started a moment ago may not have produced its event yet.
func (t *Tracker) Lookup(ctx context.Context, id string) (Target, bool) {
	t.Targets(ctx)
	t.mu.Lock()
	tg, ok := t.byID[id]
	t.mu.Unlock()
	if ok {
		return tg, true
	}
	t.MarkDirty()
	t.Targets(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	tg, ok = t.byID[id]
	return tg, ok
}

func (t *Tracker) build(ctx context.Context) ([]Target, map[string]Target, error) {
	list, err := t.dx.ListProjectContainers(ctx)
	if err != nil {
		return nil, nil, err
	}
	dirs, err := Index(t.root)
	if err != nil {
		return nil, nil, err
	}
	out := make([]Target, 0, len(list))
	byID := make(map[string]Target, len(list))
	for _, c := range list {
		if c.State != "running" {
			continue
		}
		dir, ok := dirs[c.ID]
		if !ok {
			continue
		}
		tg := Target{ID: c.ID, Project: c.Project, Service: c.Service, Dir: dir}
		out = append(out, tg)
		byID[c.ID] = tg
	}
	return out, byID, nil
}
