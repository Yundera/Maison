package apps

import (
	"context"
	"errors"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yundera/maison/internal/dockerx"
)

// Listing the host's containers is the most expensive call on the dashboard's hot
// path, and it is not close. To answer `containers/json?all=1` the daemon resolves
// the image config of every container it returns, which on a PCS measures at
// roughly 150ms per container — 1.4s for twelve, 5.4s for thirty — and the app
// list is rebuilt by every broadcast and every REST read. The cost scales with the
// containers returned, so it grows with the box: the more apps a user installs,
// the slower their grid gets.
//
// There is no way to ask Docker for less. The list endpoint has no option to skip
// image resolution (ListOptions offers Size, All, Limit, Since, Before, Filters and
// nothing else), and Maison needs every compose container, so there is no set to
// narrow. What is left is to stop making the call so often, and to stop letting its
// latency reach the grid.
const (
	// containerTTL is how long a listing is served without touching Docker. It
	// exists to coalesce the burst of reads one page load produces, not to pace
	// the grid: a container event invalidates the cache outright, so this is never
	// how long a start or stop takes to show up.
	containerTTL = 2 * time.Second

	// containerRefreshTimeout is the refresh's own budget, spent on a context that
	// belongs to no request. Inheriting the caller's was the bug: the broadcast
	// snapshot allows five seconds, a slow daemon needs more, so the call was
	// cancelled a moment before it would have returned — Docker logged `request
	// cancelled by client`, threw away work that was nearly done, and the next tick
	// started the same call again from nothing.
	containerRefreshTimeout = 60 * time.Second

	// containerMaxStale is how long the last good listing stands in for a refresh
	// that is not arriving. Past it the cache admits it knows nothing and the grid
	// greys, which is the honest answer by then: a slow refresh takes seconds, so a
	// minute of silence means the daemon is not answering at all, and the apps
	// really are as stopped as the tiles would say.
	containerMaxStale = 60 * time.Second
)

// errNoDocker is what the cache reports when the Registry was built without a
// Docker client — the shape the backup and uninstall paths already guard for.
var errNoDocker = errors.New("docker client unavailable")

// containerCache keeps the last good container listing so that a slow Docker
// query costs freshness instead of costing the grid.
//
// Docker state only decorates the app list: existence comes from the filesystem
// (see Registry.List), and the container listing supplies running/stopped, health
// and published ports on top. Serving that a few seconds late is invisible.
// Serving none of it is not — every installed app greys out as stopped. That is
// what used to happen whenever the listing outran the caller's deadline, and
// because it outran it only sometimes, the grid flickered between correct and
// all-stopped for as long as the daemon stayed slow.
//
// So a read never waits on Docker when it has any recent answer to give. It
// returns what it has and leaves a refresh running behind it; when that refresh
// lands something different, onRefresh tells the server to broadcast again.
type containerCache struct {
	// list is the underlying Docker call, injected so the cache can be tested
	// without a daemon.
	list func(context.Context) ([]dockerx.Container, error)

	// onRefresh, if set, is called when a background refresh lands a listing that
	// differs from the one already handed out. Readers took the stale answer
	// without waiting, so this is the only thing that tells them to look again.
	onRefresh func()

	mu    sync.Mutex
	got   []dockerx.Container
	print string    // fingerprint of got, to spot a refresh that changed nothing
	at    time.Time // when got was fetched; zero means never
	err   error     // why the most recent refresh failed, if it did
	stale bool      // a container event landed after got was taken
	gen   uint64    // bumped by invalidate, to date a refresh already in flight
	busy  bool      // a refresh is in flight
	done  chan struct{}
}

// get returns the current listing, refreshing in the background once it has aged
// out. It blocks only when there is nothing worth serving — a cold start, or a
// daemon that has been silent past containerMaxStale.
func (c *containerCache) get(ctx context.Context) ([]dockerx.Container, error) {
	c.mu.Lock()
	if c.usableLocked(containerTTL) && !c.stale {
		got := c.got
		c.mu.Unlock()
		return got, nil
	}
	wait := c.startLocked()
	// Anything recent enough stands in while the refresh runs. The caller is a page
	// waiting to render, and a listing from a moment ago describes the box far
	// better than an empty one does.
	if c.usableLocked(containerMaxStale) {
		got := c.got
		c.mu.Unlock()
		return got, nil
	}
	c.mu.Unlock()

	select {
	case <-wait:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.usableLocked(containerMaxStale) {
		if c.err != nil {
			return nil, c.err
		}
		return nil, errNoDocker
	}
	return c.got, nil
}

// usableLocked reports whether a listing was taken recently enough to serve.
// Caller holds mu.
func (c *containerCache) usableLocked(within time.Duration) bool {
	return !c.at.IsZero() && time.Since(c.at) < within
}

// invalidate marks the listing out of date, so the next read refreshes it rather
// than waiting out containerTTL. Called when Docker reports a container event: the
// event is the news, and a listing taken before it shows the grid the state the
// event just replaced.
//
// The listing itself is kept. It is stale, not wrong — every app it names is still
// installed — and it remains the best thing to show until the refresh lands.
func (c *containerCache) invalidate() {
	c.mu.Lock()
	c.stale = true
	c.gen++
	c.mu.Unlock()
}

// startLocked begins a refresh unless one is already running, and returns the
// channel that closes when it finishes. Caller holds mu.
//
// One at a time, always. The reason this cache exists is that the call is slow and
// scales with the box, so letting a second one start behind the first would put
// exactly the load on the daemon that made it slow to begin with — and on a box
// where a listing takes longer than the interval between reads, that is not a rare
// race but the steady state.
func (c *containerCache) startLocked() <-chan struct{} {
	if c.busy {
		return c.done
	}
	c.busy = true
	c.done = make(chan struct{})
	gen, done := c.gen, c.done
	go c.refresh(gen, done)
	return done
}

// refresh performs one listing and publishes it.
func (c *containerCache) refresh(gen uint64, done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), containerRefreshTimeout)
	defer cancel()
	got, err := c.list(ctx)

	c.mu.Lock()
	c.err = err
	changed := false
	if err == nil {
		if print := fingerprint(got); print != c.print {
			c.print, changed = print, true
		}
		c.got, c.at = got, time.Now()
		// A refresh that began before a container event describes the box as it was
		// before it. Publish it — it is still newer than what we had — but leave it
		// marked stale, so the next read goes and gets the post-event picture.
		c.stale = c.gen != gen
	}
	c.busy = false
	c.mu.Unlock()
	close(done)

	if err != nil {
		log.Printf("apps: docker list failed, serving the last known container state: %v", err)
	}
	// Outside the lock: this reaches the server's broadcast, which reads the app
	// list, which reads this cache.
	if changed && c.onRefresh != nil {
		c.onRefresh()
	}
}

// fingerprint reduces a listing to what the grid actually renders from it: which
// project each container belongs to, whether it is up, and what its health check
// says. A refresh that changes none of that changes no tile and needs no
// broadcast.
//
// Published ports and the working directory are left out deliberately — neither
// can change without recreating the container, which changes its ID.
func fingerprint(list []dockerx.Container) string {
	keys := make([]string, 0, len(list))
	for _, c := range list {
		keys = append(keys, strings.Join([]string{c.ID, c.Project, c.State, c.Health}, "\x00"))
	}
	// Docker returns containers newest-first, which reorders on any recreate; sort
	// so that a reordering alone does not read as a change.
	sort.Strings(keys)
	return strings.Join(keys, "\x1e")
}
