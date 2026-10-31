// Package hub batches RIB events and fans them out to websocket clients.
package hub

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/XodoTechnology/bgplay-vis/internal/rib"
)

const (
	ringSize   = 500 // events replayed to newly connected clients
	flushEvery = 75 * time.Millisecond
	clientBuf  = 64 // batches buffered per client before we drop it
)

// Client is one websocket subscriber.
type Client struct {
	Ch chan []byte
}

type batchMsg struct {
	Type  string      `json:"type"` // "events"
	Items []rib.Event `json:"items"`
}

// Hub collects events and broadcasts JSON batches.
type Hub struct {
	mu      sync.Mutex
	pending []rib.Event
	ring    []rib.Event
	pos     int
	clients map[*Client]struct{}
}

func New() *Hub {
	return &Hub{clients: map[*Client]struct{}{}}
}

// Publish is called by the RIB for every event. Cheap: just appends.
func (h *Hub) Publish(e rib.Event) {
	h.mu.Lock()
	h.pending = append(h.pending, e)
	if len(h.ring) < ringSize {
		h.ring = append(h.ring, e)
	} else {
		h.ring[h.pos%ringSize] = e
		h.pos++
	}
	h.mu.Unlock()
}

// Recent returns up to n most recent events for replay.
func (h *Hub) Recent(n int) []rib.Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n > len(h.ring) {
		n = len(h.ring)
	}
	out := make([]rib.Event, 0, n)
	for i := len(h.ring) - n; i < len(h.ring); i++ {
		out = append(out, h.ring[i])
	}
	return out
}

// Broadcast marshals v and sends it to all clients immediately
// (used for low-rate messages like the stats ticker).
func (h *Hub) Broadcast(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		h.send(c, b)
	}
}

// Subscribe registers a client; caller must Unsubscribe.
func (h *Hub) Subscribe() *Client {
	c := &Client{Ch: make(chan []byte, clientBuf)}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	return c
}

func (h *Hub) Unsubscribe(c *Client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

func (h *Hub) send(c *Client, b []byte) {
	select {
	case c.Ch <- b:
	default:
		// Slow client — drop it entirely; it will reconnect and resnapshot.
		delete(h.clients, c)
		close(c.Ch)
	}
}

// Run is the batching flush loop.
func (h *Hub) Run(stop <-chan struct{}) {
	t := time.NewTicker(flushEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		h.mu.Lock()
		if len(h.pending) == 0 {
			h.mu.Unlock()
			continue
		}
		items := h.pending
		h.pending = nil
		msg, err := json.Marshal(batchMsg{Type: "events", Items: items})
		if err == nil {
			for c := range h.clients {
				h.send(c, msg)
			}
		}
		h.mu.Unlock()
	}
}
