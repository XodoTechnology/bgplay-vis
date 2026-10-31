package bgp

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

// Handler receives session lifecycle events and decoded UPDATEs.
// Implemented by the RIB.
type Handler interface {
	SessionUp(PeerInfo)
	SessionDown(id string, reason string)
	SessionState(PeerInfo)
	Routes(id string, u *Update)
	EndOfRIB(id string, f Family)
}

// PeerInfo describes a session to the outside world.
type PeerInfo struct {
	ID       string   `json:"id"`
	Addr     string   `json:"addr"`
	ASN      uint32   `json:"asn"`
	Name     string   `json:"name,omitempty"`
	State    string   `json:"state"`
	Outbound bool     `json:"outbound"`
	Families []Family `json:"families,omitempty"`
	UpSince  int64    `json:"up_since,omitempty"`
}

type session struct {
	sp       *Speaker
	conn     net.Conn
	id       string
	outbound bool
	target   string // dial target for outbound sessions

	peer  PeerInfo
	wire  wireCaps
	state string
	sendQ chan []byte
	done  chan struct{}
	once  sync.Once
	hold  time.Duration
}

func (s *session) setState(st string) {
	if s.state == st {
		return
	}
	s.state = st
	s.peer.State = st
	s.sp.handler.SessionState(s.peer)
}

// notify queues a NOTIFICATION then terminates the session.
func (s *session) notify(code, sub uint8) {
	select {
	case s.sendQ <- notificationMsg(code, sub, nil):
	default:
	}
	s.close()
}

func (s *session) close() {
	s.once.Do(func() {
		close(s.done)
		s.conn.Close()
	})
}

// run is the session main loop: handshake then read UPDATEs forever.
func (s *session) run() {
	defer s.sp.unregister(s)

	if s.outbound {
		c, err := net.DialTimeout("tcp", s.target, 15*time.Second)
		if err != nil {
			s.setState("connect-failed")
			time.Sleep(2 * time.Second)
			return
		}
		s.conn = c
	}
	s.id = s.conn.RemoteAddr().(*net.TCPAddr).IP.String()
	s.peer.ID = s.id
	s.peer.Addr = s.conn.RemoteAddr().String()
	s.peer.Outbound = s.outbound
	s.sp.register(s)

	ka := keepaliveMsg()
	writeDone := make(chan error, 1)
	go s.writer(ka, writeDone)

	// Our OPEN goes out immediately (both inbound and outbound).
	openBody := encodeOpen(s.sp.cfg.LocalASN, uint16(s.sp.cfg.HoldTime), s.sp.cfg.RouterID)
	openBuf := make([]byte, HeaderLen+len(openBody))
	for i := 0; i < 16; i++ {
		openBuf[i] = 0xff
	}
	binary.BigEndian.PutUint16(openBuf[16:18], uint16(len(openBuf)))
	openBuf[18] = TypeOpen
	copy(openBuf[HeaderLen:], openBody)
	s.enqueue(openBuf)
	s.setState("open-sent")
	// Handshake: read their OPEN.
	s.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	typ, body, err := readMessage(s.conn)
	if err != nil || typ != TypeOpen {
		s.notify(5, 0) // FSM error
		s.drain(writeDone)
		return
	}
	open, pc, err := decodeOpen(body)
	if err != nil {
		s.notify(2, 0)
		s.drain(writeDone)
		return
	}
	s.peer.ASN = open.ASN
	s.peer.Name = pc.Name
	s.peer.Families = pc.familyList()
	s.wire.asn4 = pc.FourOctet
	s.wire.addPathRecv = map[Family]bool{}
	for f := range pc.AddPathSend {
		if f == FamilyV4 || f == FamilyV6 {
			s.wire.addPathRecv[f] = true
		}
	}
	// Negotiated holdtime.
	hold := int(open.HoldTime)
	if local := s.sp.cfg.HoldTime; local > 0 && (hold == 0 || local < hold) {
		hold = local
	}
	s.hold = time.Duration(hold) * time.Second

	s.enqueue(ka)
	s.setState("open-confirm")

	// Expect KEEPALIVE.
	typ, _, err = readMessage(s.conn)
	if err != nil || typ != TypeKeepalive {
		s.notify(5, 0)
		s.drain(writeDone)
		return
	}
	s.setState("established")
	s.peer.State = "established"
	s.peer.UpSince = time.Now().Unix()
	s.sp.handler.SessionUp(s.peer)
	log.Printf("bgp: session up peer=%s asn=AS%d families=%v", s.id, s.peer.ASN, s.peer.Families)

	reason := "closed"
	defer func() {
		s.sp.handler.SessionDown(s.id, reason)
		log.Printf("bgp: session down peer=%s asn=AS%d reason=%s", s.id, s.peer.ASN, reason)
	}()

	// Steady state.
	for {
		if s.hold > 0 {
			s.conn.SetReadDeadline(time.Now().Add(s.hold + s.hold/2))
		} else {
			s.conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
		}
		typ, body, err = readMessage(s.conn)
		if err != nil {
			reason = err.Error()
			s.drain(writeDone)
			return
		}
		switch typ {
		case TypeKeepalive:
		case TypeUpdate:
			u, err := decodeUpdate(body, s.wire)
			if err != nil {
				reason = err.Error()
				s.notify(3, 0)
				s.drain(writeDone)
				return
			}
			if len(u.EOR) > 0 {
				for _, f := range u.EOR {
					s.sp.handler.EndOfRIB(s.id, f)
				}
			}
			s.sp.handler.Routes(s.id, u)
		case TypeNotification:
			n := Notification{body[0], body[1], body[2:]}
			reason = n.Error()
			s.drain(writeDone)
			return
		case TypeRouteRefresh:
			// We're a listener; nothing to refresh back.
		default:
			s.notify(1, 3)
			s.drain(writeDone)
			return
		}
	}
}

func (s *session) drain(writeDone chan error) {
	s.close()
	<-writeDone
}

func (s *session) enqueue(b []byte) {
	select {
	case s.sendQ <- b:
	case <-s.done:
	}
}

// writer owns all writes to the conn: queued messages + keepalives.
// The keepalive interval is holdtime/3 (RFC 4271 §10), recomputed per cycle.
func (s *session) writer(ka []byte, done chan error) {
	defer func() { done <- nil }()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case b := <-s.sendQ:
			s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := s.conn.Write(b); err != nil {
				s.close()
				return
			}
		case <-timer.C:
			if s.hold > 0 && s.state == "established" {
				s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if _, err := s.conn.Write(ka); err != nil {
					s.close()
					return
				}
			}
			ivl := s.hold / 3
			if ivl < time.Second {
				ivl = time.Second
			}
			timer.Reset(ivl)
		case <-s.done:
			return
		}
	}
}

func (s *session) String() string {
	return fmt.Sprintf("%s(AS%d,%s)", s.id, s.peer.ASN, s.state)
}
