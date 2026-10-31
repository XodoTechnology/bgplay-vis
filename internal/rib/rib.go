// Package rib maintains per-peer adj-rib-in tables, diffs updates into
// announce/withdraw/change events, and indexes prefixes for search.
package rib

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/XodoTechnology/bgplay-vis/internal/bgp"
)

// Event is one JSON-broadcastable thing that happened.
type Event struct {
	Seq     uint64   `json:"seq"`
	TS      int64    `json:"ts"`
	Type    string   `json:"type"` // announce|withdraw|change|peer_up|peer_down|peer_state|eor|flush|stats
	Peer    string   `json:"peer,omitempty"`
	PeerASN uint32   `json:"pa,omitempty"`
	Prefix  string   `json:"pfx,omitempty"`
	Path    []uint32 `json:"path,omitempty"`
	Old     []uint32 `json:"old,omitempty"`
	NH      string   `json:"nh,omitempty"`
	Family  string   `json:"fam,omitempty"`
	N       int      `json:"n,omitempty"`
	State   string   `json:"state,omitempty"`
	Reason  string   `json:"reason,omitempty"`
}

// Route is one stored route in adj-rib-in.
type Route struct {
	Key              string       `json:"-"`
	Prefix           netip.Prefix `json:"prefix"`
	PathID           uint32       `json:"path_id,omitempty"`
	PeerID           string       `json:"peer"`
	ASPath           []uint32     `json:"path"`
	NextHop          string       `json:"nexthop,omitempty"`
	Origin           string       `json:"origin"`
	MED              uint32       `json:"med,omitempty"`
	HasMED           bool         `json:"-"`
	LocalPref        uint32       `json:"local_pref,omitempty"`
	HasLP            bool         `json:"-"`
	Communities      []string     `json:"communities,omitempty"`
	LargeCommunities []string     `json:"large_communities,omitempty"`
	Atomic           bool         `json:"atomic,omitempty"`
	Aggregator       string       `json:"aggregator,omitempty"`
	Stale            bool         `json:"stale,omitempty"`
	Since            int64        `json:"since"`
	Fingerprint      string       `json:"-"`
}

// Peer is a tracked BGP session.
type Peer struct {
	Info        bgp.PeerInfo    `json:"info"`
	Prefixes    int             `json:"prefixes"`
	Established bool            `json:"established"`
	EOR         map[string]bool `json:"eor"`
	Events      uint64          `json:"events"`
	Since       int64           `json:"since"`
}

type Stats struct {
	Started    int64  `json:"started"`
	LocalASN   uint32 `json:"local_asn"`
	Prefixes   int    `json:"prefixes"`        // total stored routes
	UniquePfx  int    `json:"unique_prefixes"` // distinct prefixes
	Peers      int    `json:"peers"`
	PeersUp    int    `json:"peers_up"`
	Events     uint64 `json:"events"`
	Announces  uint64 `json:"announces"`
	Withdraws  uint64 `json:"withdraws"`
	Changes    uint64 `json:"changes"`
	Rate       int    `json:"rate"` // events in the last second
	RateSeries []int  `json:"rate_series,omitempty"`
}

// RIB is the central store. All public methods are safe for concurrent use.
type RIB struct {
	mu       sync.Mutex
	peers    map[string]*Peer
	routes   map[string]*Route
	refcount map[string]int // distinct prefix -> number of stored routes
	v4       *trie
	v6       *trie

	seq                           uint64
	sink                          func(Event)
	started                       time.Time
	localASN                      uint32
	announces, withdraws, changes uint64
	rateWin                       [120]int // per-second ring
	ratePos                       int
	lastTick                      time.Time
	pendingN                      int
}

func New(sink func(Event)) *RIB {
	return &RIB{
		peers:    map[string]*Peer{},
		routes:   map[string]*Route{},
		refcount: map[string]int{},
		v4:       newTrie(32),
		v6:       newTrie(128),
		sink:     sink,
		started:  time.Now(),
		lastTick: time.Now(),
	}
}

func (r *RIB) emit(e Event) {
	r.seq++
	e.Seq = r.seq
	e.TS = time.Now().UnixMilli()
	r.sink(e)
}

// SetLocalASN records our own ASN for the UI's center node.
func (r *RIB) SetLocalASN(asn uint32) { r.localASN = asn }

func (r *RIB) tickRate() {
	now := time.Now()
	for now.Sub(r.lastTick) >= time.Second {
		r.ratePos = (r.ratePos + 1) % len(r.rateWin)
		r.rateWin[r.ratePos] = 0
		r.lastTick = r.lastTick.Add(time.Second)
	}
	r.rateWin[r.ratePos]++
}

// RegisterPeer inserts a peer without a live session (used by demo mode).
func (r *RIB) RegisterPeer(pi bgp.PeerInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.peers[pi.ID] == nil {
		r.peers[pi.ID] = &Peer{Info: pi, EOR: map[string]bool{}}
	}
}

// SessionState implements bgp.Handler.
func (r *RIB) SessionState(pi bgp.PeerInfo) {
	r.mu.Lock()
	p := r.peers[pi.ID]
	if p == nil {
		p = &Peer{EOR: map[string]bool{}}
		r.peers[pi.ID] = p
	}
	p.Info = pi
	p.Since = time.Now().Unix()
	st := pi.State
	r.mu.Unlock()
	r.emit(Event{Type: "peer_state", Peer: pi.ID, PeerASN: pi.ASN, State: st})
}

// SessionUp implements bgp.Handler.
func (r *RIB) SessionUp(pi bgp.PeerInfo) {
	r.mu.Lock()
	p := r.peers[pi.ID]
	if p == nil {
		p = &Peer{EOR: map[string]bool{}}
	}
	p.Info = pi
	p.Established = true
	p.EOR = map[string]bool{}
	p.Since = time.Now().Unix()
	r.peers[pi.ID] = p
	r.mu.Unlock()
	r.emit(Event{Type: "peer_up", Peer: pi.ID, PeerASN: pi.ASN, State: "established"})
}

// SessionDown implements bgp.Handler. Routes are kept but marked stale,
// so a graceful-restart/resync can refresh them.
func (r *RIB) SessionDown(id string, reason string) {
	r.mu.Lock()
	p := r.peers[id]
	var asn uint32
	if p != nil {
		asn = p.Info.ASN
		p.Established = false
		p.Info.State = "down"
		p.Since = time.Now().Unix()
	}
	n := 0
	for _, rt := range r.routes {
		if rt.PeerID == id && !rt.Stale {
			rt.Stale = true
			n++
		}
	}
	r.mu.Unlock()
	r.emit(Event{Type: "peer_down", Peer: id, PeerASN: asn, Reason: reason, N: n})
}

// Routes implements bgp.Handler: applies a decoded UPDATE to adj-rib-in.
func (r *RIB) Routes(id string, u *bgp.Update) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tickRate()
	p := r.peers[id]
	if p == nil {
		p = &Peer{Info: bgp.PeerInfo{ID: id}, EOR: map[string]bool{}}
		r.peers[id] = p
	}
	p.Events++

	for _, w := range u.Withdrawn {
		key := routeKey(id, w.Prefix, w.PathID)
		old := r.routes[key]
		if old == nil {
			continue
		}
		r.deleteLocked(old)
		r.withdraws++
		r.emitLocked(Event{Type: "withdraw", Peer: id, PeerASN: p.Info.ASN,
			Prefix: w.Prefix.String(), Path: old.ASPath, Family: famName(w.Prefix)})
	}

	for _, a := range u.Announced {
		key := routeKey(id, a.Prefix, a.PathID)
		rt := &Route{
			Key: key, Prefix: a.Prefix, PathID: a.PathID, PeerID: id,
			ASPath: u.Attrs.ASPath, NextHop: a.NextHop.String(),
			Origin: originString(u.Attrs.Origin),
			MED:    u.Attrs.MED, HasMED: u.Attrs.HasMED,
			LocalPref: u.Attrs.LocalPref, HasLP: u.Attrs.HasLocalPref,
			Communities:      u.Attrs.Communities,
			LargeCommunities: u.Attrs.LargeCommunities,
			Atomic:           u.Attrs.Atomic, Aggregator: u.Attrs.Aggregator,
			Since:       time.Now().Unix(),
			Fingerprint: fingerprint(u.Attrs, a.NextHop),
		}
		old := r.routes[key]
		if old != nil && old.Fingerprint == rt.Fingerprint {
			old.Stale = false // refresh
			continue
		}
		typ := "announce"
		var oldPath []uint32
		if old != nil {
			typ = "change"
			oldPath = old.ASPath
			r.removeIndex(old)
		} else {
			r.refcount[a.Prefix.String()]++
			p.Prefixes++
		}
		r.routes[key] = rt
		r.index(rt)
		if typ == "announce" {
			r.announces++
		} else {
			r.changes++
		}
		r.emitLocked(Event{Type: typ, Peer: id, PeerASN: p.Info.ASN,
			Prefix: a.Prefix.String(), Path: rt.ASPath, Old: oldPath,
			NH: rt.NextHop, Family: famName(a.Prefix)})
	}
}

// EndOfRIB implements bgp.Handler: marks the family synced and flushes any
// stale routes the peer did not refresh.
func (r *RIB) EndOfRIB(id string, f bgp.Family) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.peers[id]
	if p == nil {
		return
	}
	p.EOR[f.String()] = true
	flushed := 0
	for key, rt := range r.routes {
		if rt.PeerID == id && rt.Stale && famOf(rt.Prefix) == f {
			r.deleteLocked(rt)
			delete(r.routes, key)
			flushed++
			r.withdraws++
		}
	}
	var asn uint32 = p.Info.ASN
	r.seq++
	r.sink(Event{Seq: r.seq, TS: time.Now().UnixMilli(), Type: "eor",
		Peer: id, PeerASN: asn, Family: f.String(), N: flushed})
	if flushed > 0 {
		r.seq++
		r.sink(Event{Seq: r.seq, TS: time.Now().UnixMilli(), Type: "flush",
			Peer: id, PeerASN: asn, Family: f.String(), N: flushed})
	}
}

func (r *RIB) emitLocked(e Event) {
	r.seq++
	e.Seq = r.seq
	e.TS = time.Now().UnixMilli()
	r.sink(e)
}

func (r *RIB) deleteLocked(rt *Route) {
	if !rt.Stale {
		// still counted under peer + refcount
	}
	if p := r.peers[rt.PeerID]; p != nil {
		p.Prefixes--
	}
	r.refcount[rt.Prefix.String()]--
	if r.refcount[rt.Prefix.String()] <= 0 {
		delete(r.refcount, rt.Prefix.String())
	}
	r.removeIndex(rt)
	delete(r.routes, rt.Key)
}

// Snapshot returns stats + peer table for the API/websocket snapshot.
func (r *RIB) Snapshot() (Stats, []Peer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// drain any seconds elapsed since last event
	now := time.Now()
	for now.Sub(r.lastTick) >= time.Second {
		r.ratePos = (r.ratePos + 1) % len(r.rateWin)
		r.rateWin[r.ratePos] = 0
		r.lastTick = r.lastTick.Add(time.Second)
	}
	series := make([]int, 0, 60)
	for i := 1; i <= 60; i++ {
		series = append(series, r.rateWin[(r.ratePos+i)%len(r.rateWin)])
	}
	up := 0
	peers := make([]Peer, 0, len(r.peers))
	for _, p := range r.peers {
		if p.Established {
			up++
		}
		peers = append(peers, *p)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].Info.ASN < peers[j].Info.ASN })
	return Stats{
		Started: r.started.Unix(), LocalASN: r.localASN, Prefixes: len(r.routes),
		UniquePfx: len(r.refcount), Peers: len(r.peers), PeersUp: up,
		Events: r.seq, Announces: r.announces, Withdraws: r.withdraws,
		Changes: r.changes, Rate: r.rateWin[r.ratePos], RateSeries: series,
	}, peers
}

// ---- helpers ----

func routeKey(peer string, p netip.Prefix, pid uint32) string {
	return fmt.Sprintf("%s|%s|%d", peer, p, pid)
}

func famOf(p netip.Prefix) bgp.Family {
	if p.Addr().Is4() {
		return bgp.FamilyV4
	}
	return bgp.FamilyV6
}

func famName(p netip.Prefix) string { return famOf(p).String() }

func originString(o uint8) string {
	switch o {
	case 0:
		return "IGP"
	case 1:
		return "EGP"
	default:
		return "INCOMPLETE"
	}
}

func fingerprint(a *bgp.Attrs, nh netip.Addr) string {
	var b strings.Builder
	for _, asn := range a.ASPath {
		fmt.Fprintf(&b, "%d.", asn)
	}
	fmt.Fprintf(&b, "|%s|%d|%d|%d|%v|%v|%s|%v", nh, a.Origin, a.MED, a.LocalPref,
		a.Communities, a.LargeCommunities, a.Aggregator, a.Atomic)
	return b.String()
}
