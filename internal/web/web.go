// Package web serves the embedded UI, REST API and websocket event stream.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"time"

	"github.com/XodoTechnology/bgplay-vis/internal/hub"
	"github.com/XodoTechnology/bgplay-vis/internal/rib"
)

//go:embed static
var staticFS embed.FS

type Server struct {
	r   *rib.RIB
	h   *hub.Hub
	mux *http.ServeMux
}

type snapshotMsg struct {
	Type   string      `json:"type"` // "snapshot"
	Stats  rib.Stats   `json:"stats"`
	Peers  []rib.Peer  `json:"peers"`
	Recent []rib.Event `json:"recent"`
}

type statsMsg struct {
	Type  string     `json:"type"` // "stats"
	Stats rib.Stats  `json:"stats"`
	Peers []rib.Peer `json:"peers"`
}

func New(r *rib.RIB, h *hub.Hub) *Server {
	s := &Server{r: r, h: h, mux: http.NewServeMux()}
	static, _ := fs.Sub(staticFS, "static")
	s.mux.Handle("/", http.FileServer(http.FS(static)))
	s.mux.HandleFunc("/api/stats", s.apiStats)
	s.mux.HandleFunc("/api/peers", s.apiPeers)
	s.mux.HandleFunc("/api/search", s.apiSearch)
	s.mux.HandleFunc("/ws", s.ws)
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

// RunStatsTicker pushes stats+peers to all clients every second.
func (s *Server) RunStatsTicker(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			stats, peers := s.r.Snapshot()
			s.h.Broadcast(statsMsg{Type: "stats", Stats: stats, Peers: peers})
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) apiStats(w http.ResponseWriter, r *http.Request) {
	stats, peers := s.r.Snapshot()
	writeJSON(w, map[string]any{"stats": stats, "peers": peers})
}

func (s *Server) apiPeers(w http.ResponseWriter, r *http.Request) {
	_, peers := s.r.Snapshot()
	writeJSON(w, peers)
}

func (s *Server) apiSearch(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.r.Search(r.URL.Query().Get("q")))
}

func (s *Server) ws(w http.ResponseWriter, r *http.Request) {
	c, err := wsUpgrade(w, r)
	if err != nil {
		http.Error(w, "bad websocket request", 400)
		return
	}
	cl := s.h.Subscribe()

	stats, peers := s.r.Snapshot()
	snap, _ := json.Marshal(snapshotMsg{
		Type: "snapshot", Stats: stats, Peers: peers,
		Recent: s.h.Recent(300),
	})
	if err := c.WriteText(snap); err != nil {
		c.conn.Close()
		s.h.Unsubscribe(cl)
		return
	}
	log.Printf("web: ws client connected %s", r.RemoteAddr)

	done := make(chan struct{})
	go c.readLoop(func() { close(done) })

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case b, ok := <-cl.Ch:
			if !ok {
				c.conn.Close()
				return
			}
			if err := c.WriteText(b); err != nil {
				s.h.Unsubscribe(cl)
				c.conn.Close()
				return
			}
		case <-ping.C:
			if err := c.Ping(); err != nil {
				s.h.Unsubscribe(cl)
				c.conn.Close()
				return
			}
		case <-done:
			s.h.Unsubscribe(cl)
			c.conn.Close()
			return
		}
	}
}
