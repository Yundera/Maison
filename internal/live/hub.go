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
)

const sampleInterval = 2 * time.Second

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
}

// NewHub creates a hub sampling utilization via the given collector.
func NewHub(collector *system.Collector) *Hub {
	h := &Hub{
		collector: collector,
		clients:   make(map[*client]struct{}),
	}
	go h.sampleLoop()
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
