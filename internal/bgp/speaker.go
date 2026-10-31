package bgp

import (
	"context"
	"log"
	"net"
	"net/netip"
	"sync"
	"time"
)

// Config controls the speaker.
type Config struct {
	LocalASN uint32
	RouterID netip.Addr
	HoldTime int      // seconds; 0 disables keepalive requirement
	Listen   string   // e.g. ":179"; empty disables inbound listener
	Outbound []string // dial targets, host or host:port (default port 179)
	Handler  Handler
}

// Speaker owns the listener, outbound dialers and the session registry.
type Speaker struct {
	cfg      Config
	handler  Handler
	mu       sync.Mutex
	sessions map[string]*session // keyed by remote IP / target
	ln       net.Listener
}

func NewSpeaker(cfg Config) *Speaker {
	return &Speaker{cfg: cfg, handler: cfg.Handler, sessions: map[string]*session{}}
}

func (s *Speaker) Start(ctx context.Context) error {
	if s.cfg.Listen != "" {
		ln, err := net.Listen("tcp", s.cfg.Listen)
		if err != nil {
			return err
		}
		s.ln = ln
		log.Printf("bgp: listening on %s (AS%d)", s.cfg.Listen, s.cfg.LocalASN)
		go s.acceptLoop(ctx)
	}
	for _, t := range s.cfg.Outbound {
		go s.dialLoop(ctx, normalizeTarget(t))
	}
	return nil
}

func (s *Speaker) Close() {
	if s.ln != nil {
		s.ln.Close()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		sess.close()
	}
}

func normalizeTarget(t string) string {
	if _, _, err := net.SplitHostPort(t); err != nil {
		return net.JoinHostPort(t, "179")
	}
	return t
}

func (s *Speaker) acceptLoop(ctx context.Context) {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.newInbound(c).run()
	}
}

// dialLoop keeps trying an outbound session forever with backoff.
func (s *Speaker) dialLoop(ctx context.Context, target string) {
	for {
		if ctx.Err() != nil {
			return
		}
		sess := &session{
			sp:       s,
			outbound: true,
			target:   target,
			id:       target,
			sendQ:    make(chan []byte, 16),
			done:     make(chan struct{}),
		}
		sess.run()
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Second):
		}
	}
}

func (s *Speaker) newInbound(c net.Conn) *session {
	return &session{
		sp:    s,
		conn:  c,
		sendQ: make(chan []byte, 16),
		done:  make(chan struct{}),
	}
}

// register adds the session, closing any pre-existing session with the
// same remote peer IP (session collision — newest wins).
func (s *Speaker) register(sess *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.sessions[sess.id]; old != nil && old != sess {
		old.close()
	}
	s.sessions[sess.id] = sess
}

func (s *Speaker) unregister(sess *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[sess.id] == sess {
		delete(s.sessions, sess.id)
	}
}

// Sessions returns live session infos (for the API).
func (s *Speaker) Sessions() []PeerInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PeerInfo, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, sess.peer)
	}
	return out
}
