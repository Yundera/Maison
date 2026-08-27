// Package live is the WebSocket hub that pushes real-time data (system stats,
// app status, per-app logs/stats) to connected clients. To keep the idle
// footprint near zero, the system sampler only runs while a client is
// subscribed to the "system" channel.
package live

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/yundera/maison/internal/system"
)

// Channel names clients can subscribe to.
const (
	ChannelSystem = "system"
	ChannelApps   = "apps"
	// ChannelBackup carries the whole-box run and the user-data restore.
	//
	// Separate from ChannelApps because it is the progress of things that have no
	// tile: the user-data set is the largest target on the box and is not an app, and
	// a run's plan — what is still to come — exists nowhere in the app list. Its
	// payload is also cheap, where the app list is a host-wide container listing plus
	// a YAML parse per app, so tying the two together would have made a byte counter
	// the most expensive thing on the dashboard.
	ChannelBackup = "backup"
	// ChannelAppStats carries per-app CPU/memory for the monitor panel: one row
	// per compose project, sampled only while that panel is open.
	//
	// Separate from ChannelSystem, which it sits behind in the UI, because it is
	// far more expensive to produce — a container listing plus a blocking stats
	// read per running container — and the gauges must keep ticking at their own
	// rate whether or not anyone has opened the breakdown.
	ChannelAppStats = "appstats"
	// ChannelResources carries the host breakdown behind the Resources page:
	// per-interface throughput, per-device IO, the filesystem table and the process
	// list.
	//
	// Separate from ChannelSystem, whose two gauges every dashboard visitor
	// subscribes to, because this payload costs a great deal more to produce — the
	// process table alone is a /proc read per process on the box — and only one
	// page ever wants it. Nothing here is sampled while that page is closed.
	ChannelResources = "resources"
)

const sampleInterval = 2 * time.Second

// appStatsInterval is slower than the gauges on purpose: one round costs a
// second or so of daemon round-trips (see appstats.Sampler), and a per-app
// figure that moves every three seconds reads as live enough.
const appStatsInterval = 3 * time.Second

// Envelope is the wire format in both directions.
type Envelope struct {
	Type    string          `json:"type"`
	Channel string          `json:"channel,omitempty"`
	ID      string          `json:"id,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Hub tracks connected clients and fans out messages.
type Hub struct {
	collector *system.Collector

	mu      sync.Mutex
	clients map[*client]struct{}

	// AppsSnapshot, if set, returns the current app list for the "apps" channel.
	AppsSnapshot func() any

	// BackupSnapshot, if set, returns the current backup run/restore state for the
	// "backup" channel.
	BackupSnapshot func() any

	// AppStatsSnapshot, if set, returns per-app usage for the "appstats" channel.
	AppStatsSnapshot func() any

	// ResourcesSnapshot, if set, returns the host breakdown for the "resources"
	// channel.
	ResourcesSnapshot func() any
}

// NewHub creates a hub sampling utilization via the given collector.
func NewHub(collector *system.Collector) *Hub {
	h := &Hub{
		collector: collector,
		clients:   make(map[*client]struct{}),
	}
	go h.sampleLoop()
	go h.appStatsLoop()
	go h.resourcesLoop()
	return h
}

// BroadcastLazy sends the result of produce to every client subscribed to
// channel, and calls produce only if there is at least one. Building an "apps"
// payload means a host-wide container list plus a YAML parse per installed app,
// so an eager Broadcast would pay that on every Docker event even with nobody
// connected — and Docker events include the health checks of every container on
// the host, so on an idle box that is a steady drip of work for no reader.
func (h *Hub) BroadcastLazy(channel string, produce func() any) {
	if !h.anySubscribed(channel) {
		return
	}
	h.Broadcast(channel, produce())
}

// Broadcast sends data to every client subscribed to channel.
func (h *Hub) Broadcast(channel string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	env := Envelope{Type: channel, Channel: channel, Data: raw}
	msg, err := json.Marshal(env)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.subscribed(channel) {
			c.trySend(msg)
		}
	}
}

// sampleLoop pushes system stats whenever at least one client wants them.
func (h *Hub) sampleLoop() {
	ticker := time.NewTicker(sampleInterval)
	defer ticker.Stop()
	for range ticker.C {
		if !h.anySubscribed(ChannelSystem) {
			continue
		}
		h.Broadcast(ChannelSystem, h.collector.Sample())
	}
}

// appStatsLoop pushes per-app usage while the monitor panel is open. Sampling is
// synchronous here, so a round that outlives a tick simply delays the next one
// rather than stacking a second scan of the daemon on top of the first.
func (h *Hub) appStatsLoop() {
	ticker := time.NewTicker(appStatsInterval)
	defer ticker.Stop()
	for range ticker.C {
		if h.AppStatsSnapshot == nil {
			continue
		}
		h.BroadcastLazy(ChannelAppStats, h.AppStatsSnapshot)
	}
}

// resourcesLoop pushes the host breakdown while the Resources page is open. It
// shares the gauges' cadence — the two are read side by side on that page, and a
// per-interface figure that lags the CPU dial is more confusing than one that
// moves with it.
func (h *Hub) resourcesLoop() {
	ticker := time.NewTicker(sampleInterval)
	defer ticker.Stop()
	for range ticker.C {
		if h.ResourcesSnapshot == nil {
			continue
		}
		h.BroadcastLazy(ChannelResources, h.ResourcesSnapshot)
	}
}

func (h *Hub) anySubscribed(channel string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.subscribed(channel) {
			return true
		}
	}
	return false
}

func (h *Hub) add(c *client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) remove(c *client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

// client is a single WebSocket connection.
type client struct {
	conn *websocket.Conn
	send chan []byte

	mu   sync.Mutex
	subs map[string]bool
}

func (c *client) subscribed(channel string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.subs[channel]
}

func (c *client) setSub(channel string, on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if on {
		c.subs[channel] = true
	} else {
		delete(c.subs, channel)
	}
}

func (c *client) trySend(msg []byte) {
	select {
	case c.send <- msg:
	default:
		// Slow client: drop the message rather than block the hub.
	}
}

// ServeWS upgrades an HTTP request to a WebSocket and runs the read/write pumps.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		return
	}
	c := &client{
		conn: conn,
		send: make(chan []byte, 32),
		subs: make(map[string]bool),
	}
	h.add(c)
	defer func() {
		h.remove(c)
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	go c.writePump(ctx)
	c.readPump(ctx, h)
}

func (c *client) readPump(ctx context.Context, h *Hub) {
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return
		}
		var env Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		switch env.Type {
		case "subscribe":
			c.setSub(env.Channel, true)
			// Send an immediate snapshot so the UI doesn't wait a full tick.
			c.snapshot(h, env.Channel)
		case "unsubscribe":
			c.setSub(env.Channel, false)
		}
	}
}

func (c *client) snapshot(h *Hub, channel string) {
	switch channel {
	case ChannelSystem:
		if raw, err := json.Marshal(Envelope{Type: channel, Channel: channel,
			Data: mustJSON(h.collector.Sample())}); err == nil {
			c.trySend(raw)
		}
	case ChannelApps:
		if h.AppsSnapshot != nil {
			if raw, err := json.Marshal(Envelope{Type: channel, Channel: channel,
				Data: mustJSON(h.AppsSnapshot())}); err == nil {
				c.trySend(raw)
			}
		}
	case ChannelBackup:
		if h.BackupSnapshot != nil {
			if raw, err := json.Marshal(Envelope{Type: channel, Channel: channel,
				Data: mustJSON(h.BackupSnapshot())}); err == nil {
				c.trySend(raw)
			}
		}
	case ChannelResources:
		// Off the read pump, like the appstats case: this snapshot walks the process
		// table, and the pump is what carries the client's next subscribe.
		if h.ResourcesSnapshot != nil {
			go func() {
				if raw, err := json.Marshal(Envelope{Type: channel, Channel: channel,
					Data: mustJSON(h.ResourcesSnapshot())}); err == nil {
					c.trySend(raw)
				}
			}()
		}
	case ChannelAppStats:
		// Off the read pump: this snapshot blocks for about a second on the daemon,
		// and the pump is what carries the client's next subscribe.
		if h.AppStatsSnapshot != nil {
			go func() {
				if raw, err := json.Marshal(Envelope{Type: channel, Channel: channel,
					Data: mustJSON(h.AppStatsSnapshot())}); err == nil {
					c.trySend(raw)
				}
			}()
		}
	}
}

func (c *client) writePump(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-c.send:
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := c.conn.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("live: marshal: %v", err)
		return json.RawMessage("null")
	}
	return b
}
