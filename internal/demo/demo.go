// Package demo generates a synthetic BGP feed through the real RIB
// pipeline, so the UI can be exercised without a router.
package demo

import (
	"context"
	"math/rand"
	"net/netip"
	"time"

	"github.com/XodoTechnology/bgplay-vis/internal/bgp"
	"github.com/XodoTechnology/bgplay-vis/internal/rib"
)

const peerID = "demo-feed"
const peerASN = 64512

var transits = []uint32{174, 701, 1299, 2914, 3257, 3356, 3491, 5511, 6453, 6461, 6762, 6830, 7018, 6939}

type pfxEntry struct {
	pfx  netip.Prefix
	path []uint32
}

// Run drives the fake peer until ctx is cancelled.
func Run(ctx context.Context, r *rib.RIB) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	r.RegisterPeer(bgp.PeerInfo{ID: peerID, Addr: "127.0.0.1:4444", ASN: peerASN, Name: "demo-feed.internal"})
	r.SessionUp(bgp.PeerInfo{ID: peerID, Addr: "127.0.0.1:4444", ASN: peerASN, Name: "demo-feed.internal", State: "established", UpSince: time.Now().Unix()})

	live := map[string]*pfxEntry{}

	mkPath := func() []uint32 {
		p := []uint32{peerASN}
		for i := 0; i < 1+rng.Intn(3); i++ {
			p = append(p, transits[rng.Intn(len(transits))])
		}
		p = append(p, uint32(64513+rng.Intn(4000)))
		return p
	}
	mkPfx := func() netip.Prefix {
		if rng.Intn(8) == 0 {
			var b [16]byte
			b[0] = 0x20
			b[1] = 0x01
			for i := 2; i < 6; i++ {
				b[i] = byte(rng.Intn(256))
			}
			return netip.PrefixFrom(netip.AddrFrom16(b), 48)
		}
		return netip.PrefixFrom(netip.AddrFrom4([4]byte{
			byte([]uint8{8, 23, 31, 45, 91, 103, 139, 185, 203}[rng.Intn(9)]),
			byte(rng.Intn(256)), byte(rng.Intn(256)), 0}), 24)
	}
	announce := func(e *pfxEntry) {
		u := &bgp.Update{Attrs: &bgp.Attrs{Origin: 0, ASPath: e.path,
			NextHop: netip.MustParseAddr("10.0.0.1"), HasLocalPref: true, LocalPref: 100,
			Communities: []string{"64512:100"}},
			Announced: []bgp.NLRI{{Prefix: e.pfx, Family: fam(e.pfx), NextHop: netip.MustParseAddr("10.0.0.1")}}}
		r.Routes(peerID, u)
	}
	withdraw := func(e *pfxEntry) {
		u := &bgp.Update{Attrs: &bgp.Attrs{},
			Withdrawn: []bgp.NLRI{{Prefix: e.pfx, Family: fam(e.pfx)}}}
		r.Routes(peerID, u)
	}

	// Initial "table dump" — a burst so the graph fills up.
	for i := 0; i < 400; i++ {
		e := &pfxEntry{pfx: mkPfx(), path: mkPath()}
		if _, ok := live[e.pfx.String()]; ok {
			continue
		}
		live[e.pfx.String()] = e
		announce(e)
		if i%60 == 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(60 * time.Millisecond):
			}
		}
	}
	r.EndOfRIB(peerID, bgp.FamilyV4)
	r.EndOfRIB(peerID, bgp.FamilyV6)

	// Steady churn.
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(120+rng.Intn(900)) * time.Millisecond):
		}
		roll := rng.Intn(100)
		switch {
		case roll < 45 || len(live) < 30: // announce new
			e := &pfxEntry{pfx: mkPfx(), path: mkPath()}
			live[e.pfx.String()] = e
			announce(e)
		case roll < 70: // withdraw
			for k, e := range live {
				delete(live, k)
				withdraw(e)
				break
			}
		default: // path change
			i := 0
			for _, e := range live {
				if i == rng.Intn(len(live)) {
					e.path = mkPath()
					announce(e)
					break
				}
				i++
			}
		}
	}
}

func fam(p netip.Prefix) bgp.Family {
	if p.Addr().Is4() {
		return bgp.FamilyV4
	}
	return bgp.FamilyV6
}
