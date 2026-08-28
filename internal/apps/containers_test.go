package apps

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/dockerx"
)

// running builds a listing of one container per project, all up.
func running(projects ...string) []dockerx.Container {
	out := make([]dockerx.Container, 0, len(projects))
	for _, p := range projects {
		out = append(out, dockerx.Container{ID: "c-" + p, Project: p, Service: "app", State: "running"})
	}
	return out
}

// age backdates the cached listing, standing in for the passage of time so the
// tests do not have to spend any.
func age(c *containerCache, d time.Duration) {
	c.mu.Lock()
	c.at = time.Now().Add(-d)
	c.mu.Unlock()
}

// settle waits for any in-flight refresh to finish.
func settle(t *testing.T, c *containerCache) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		c.mu.Lock()
		busy := c.busy
		c.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("a refresh never finished")
}

func projects(list []dockerx.Container) []string {
	out := make([]string, 0, len(list))
	for _, c := range list {
		out = append(out, c.Project)
	}
	return out
}

// The bug this cache exists for: a listing slower than the caller's deadline used
// to leave the grid with no container state at all, so every running app greyed out
// as stopped — and because it was slow only sometimes, the grid flickered between
// correct and all-stopped. A read must never wait on Docker when it has a recent
// answer to give.
func TestASlowListingCostsFreshnessNotTheGrid(t *testing.T) {
	wedged := make(chan struct{})
	var calls atomic.Int32
	c := &containerCache{list: func(ctx context.Context) ([]dockerx.Container, error) {
		if calls.Add(1) == 1 {
			return running("jellyfin"), nil
		}
		<-wedged // every later listing hangs, as a loaded daemon does
		return nil, nil
	}}
	defer close(wedged)

	if _, err := c.get(context.Background()); err != nil {
		t.Fatalf("first get: %v", err)
	}
	age(c, containerTTL*2) // due for a refresh, still well inside containerMaxStale

	served := make(chan []dockerx.Container, 1)
	go func() {
		got, _ := c.get(context.Background())
		served <- got
	}()
	select {
	case got := <-served:
		if p := projects(got); len(p) != 1 || p[0] != "jellyfin" {
			t.Fatalf("served %v, want the last good listing [jellyfin]", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("get blocked on a wedged refresh instead of serving the listing it already had")
	}
}

// The app list is rebuilt by every broadcast and every REST read, and the call
// behind it costs roughly 150ms per container. A burst of readers is one listing.
func TestReadsWithinTheTTLDoNotTouchDocker(t *testing.T) {
	var calls atomic.Int32
	c := &containerCache{list: func(ctx context.Context) ([]dockerx.Container, error) {
		calls.Add(1)
		return running("jellyfin", "nextcloud"), nil
	}}
	for i := 0; i < 5; i++ {
		got, err := c.get(context.Background())
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		if len(got) != 2 {
			t.Fatalf("get %d returned %d containers, want 2", i, len(got))
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("Docker was listed %d times for five reads, want 1", n)
	}
}

// Concurrent readers must not each start their own listing. On a box where one
// takes longer than the gap between reads, stacking them is not a rare race but the
// steady state — and it is precisely the load that makes the daemon slow.
func TestRefreshesRunOneAtATime(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	c := &containerCache{list: func(ctx context.Context) ([]dockerx.Container, error) {
		calls.Add(1)
		<-release
		return running("jellyfin"), nil
	}}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.get(context.Background())
		}()
	}
	time.Sleep(50 * time.Millisecond) // let them all pile up on the cold cache
	close(release)
	wg.Wait()

	if n := calls.Load(); n != 1 {
		t.Fatalf("eight concurrent readers started %d listings, want 1", n)
	}
}

// A container event is the news. The listing taken before it describes the box as
// it was, so the next read has to refresh — but the old listing is stale, not
// wrong, and every app it names is still installed, so it keeps being served in the
// meantime.
func TestAnEventRefreshesWithoutBlankingTheGrid(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	var calls atomic.Int32
	c := &containerCache{list: func(ctx context.Context) ([]dockerx.Container, error) {
		n := calls.Add(1)
		started <- struct{}{}
		if n == 1 {
			return running("jellyfin"), nil
		}
		<-release
		return running("jellyfin", "nextcloud"), nil
	}}
	defer close(release)

	c.get(context.Background())
	<-started
	c.invalidate()

	// Still inside the TTL, so only the invalidation can be what sends it back to
	// Docker — and it must answer from the old listing while it goes.
	got, err := c.get(context.Background())
	if err != nil {
		t.Fatalf("get after invalidate: %v", err)
	}
	if p := projects(got); len(p) != 1 || p[0] != "jellyfin" {
		t.Fatalf("served %v while refreshing, want the old listing [jellyfin]", p)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("invalidate left the cache fresh: the read never went back to Docker")
	}
}

// Serving the last good listing forever would claim a dead daemon's apps are
// running. A slow refresh takes seconds, so once Docker has been silent past
// containerMaxStale the greyed tiles are the truth and the error is reported.
func TestTheLastGoodListingIsNotServedForever(t *testing.T) {
	boom := errors.New("docker is not answering")
	var fail atomic.Bool
	c := &containerCache{list: func(ctx context.Context) ([]dockerx.Container, error) {
		if fail.Load() {
			return nil, boom
		}
		return running("jellyfin"), nil
	}}

	c.get(context.Background())
	fail.Store(true)

	// Old, but not yet too old to stand in.
	age(c, containerMaxStale/2)
	if got, err := c.get(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("get within containerMaxStale = (%v, %v), want the last good listing", projects(got), err)
	}
	settle(t, c)

	age(c, containerMaxStale*2)
	got, err := c.get(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("get past containerMaxStale = (%v, %v), want the listing error", projects(got), err)
	}
	if got != nil {
		t.Fatalf("got %v alongside the error, want nothing", projects(got))
	}
}

// Readers that took the stale answer were never told when the real one arrived, so
// a refresh that changes what the grid renders has to announce itself. One that
// changes nothing must stay quiet, or every refresh costs a broadcast.
func TestOnlyRefreshesThatChangeSomethingAreAnnounced(t *testing.T) {
	var mu sync.Mutex
	list := running("jellyfin")
	announced := make(chan struct{}, 8)
	c := &containerCache{
		list: func(ctx context.Context) ([]dockerx.Container, error) {
			mu.Lock()
			defer mu.Unlock()
			return list, nil
		},
		onRefresh: func() { announced <- struct{}{} },
	}

	// The first listing changes the grid from "nothing known" to something.
	c.get(context.Background())
	select {
	case <-announced:
	case <-time.After(2 * time.Second):
		t.Fatal("the first listing was never announced")
	}

	// The same containers again: no tile changes, so nobody is told.
	age(c, containerTTL*2)
	c.get(context.Background())
	settle(t, c)
	time.Sleep(50 * time.Millisecond)
	select {
	case <-announced:
		t.Fatal("a refresh that changed nothing was announced")
	default:
	}

	// One of them stops, which every tile of that app renders.
	mu.Lock()
	list = []dockerx.Container{{ID: "c-jellyfin", Project: "jellyfin", State: "exited"}}
	mu.Unlock()
	age(c, containerTTL*2)
	c.get(context.Background())
	select {
	case <-announced:
	case <-time.After(2 * time.Second):
		t.Fatal("a container that stopped was never announced")
	}
}

// Before any listing has landed there is nothing to serve, so a reader waits — but
// only as long as it said it would. The refresh outlives it on its own budget, so
// the wait is paid once rather than restarted by every caller that gives up.
func TestAColdCacheHonoursTheCallersDeadlineAndKeepsGoing(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	c := &containerCache{list: func(ctx context.Context) ([]dockerx.Container, error) {
		calls.Add(1)
		<-release
		return running("jellyfin"), nil
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.get(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cold get = %v, want the caller's deadline", err)
	}

	// The listing was not cancelled with the reader: it lands, and the next read
	// gets it without starting a second one.
	close(release)
	settle(t, c)
	got, err := c.get(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("get after the refresh landed = (%v, %v), want [jellyfin]", projects(got), err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("the abandoned refresh was restarted: %d listings, want 1", n)
	}
}

// A Registry built without Docker — the shape several tests use — must report that
// rather than dereference a nil client, which is what the old direct call did.
func TestARegistryWithoutDockerListsNoContainers(t *testing.T) {
	r := New(config.Config{}, nil)
	if _, err := r.containers.get(context.Background()); !errors.Is(err, errNoDocker) {
		t.Fatalf("container listing without a client = %v, want errNoDocker", err)
	}
}
