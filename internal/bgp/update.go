package bgp

import (
	"encoding/binary"
	"fmt"
	"net/netip"
)

// Path attribute codes we decode.
const (
	attrOrigin         = 1
	attrASPath         = 2
	attrNextHop        = 3
	attrMED            = 4
	attrLocalPref      = 5
	attrAtomicAgg      = 6
	attrAggregator     = 7
	attrCommunity      = 8
	attrOriginatorID   = 9
	attrClusterList    = 10
	attrMPReach        = 14
	attrMPUnreach      = 15
	attrExtCommunity   = 16
	attrAS4Path        = 17
	attrAS4Aggregator  = 18
	attrLargeCommunity = 32
	attrOTC            = 35
)

// NLRI is one decoded prefix reachability item.
type NLRI struct {
	Prefix  netip.Prefix `json:"prefix"`
	PathID  uint32       `json:"path_id,omitempty"`
	Family  Family       `json:"family"`
	NextHop netip.Addr   `json:"nexthop,omitempty"`
}

// Attrs are the path attributes shared by the announced NLRI in an UPDATE.
type Attrs struct {
	Origin           uint8
	ASPath           []uint32
	NextHop          netip.Addr
	MED              uint32
	HasMED           bool
	LocalPref        uint32
	HasLocalPref     bool
	Atomic           bool
	Aggregator       string
	OriginatorID     netip.Addr
	ClusterList      []netip.Addr
	Communities      []string
	LargeCommunities []string
	ExtCommunities   []string
	OTC              uint32
	HasOTC           bool
}

// Update is a fully decoded UPDATE message.
type Update struct {
	Withdrawn []NLRI
	Announced []NLRI
	Attrs     *Attrs
	EOR       []Family // families which signalled End-of-RIB
}

// wireCaps are the per-session decode rules negotiated in OPEN.
type wireCaps struct {
	asn4        bool
	addPathRecv map[Family]bool // peer transmits path-ids for these families
}

func (w wireCaps) addPath(f Family) bool {
	return w.addPathRecv[f]
}

// decodeUpdate parses an UPDATE body.
func decodeUpdate(body []byte, caps wireCaps) (*Update, error) {
	u := &Update{}
	if len(body) < 4 {
		return nil, fmt.Errorf("bgp: short UPDATE")
	}
	wLen := int(binary.BigEndian.Uint16(body[:2]))
	if len(body) < 2+wLen+2 {
		return nil, fmt.Errorf("bgp: truncated UPDATE")
	}
	wd := body[2 : 2+wLen]
	rest := body[2+wLen:]
	aLen := int(binary.BigEndian.Uint16(rest[:2]))
	if len(rest) < 2+aLen {
		return nil, fmt.Errorf("bgp: truncated UPDATE attrs")
	}
	abuf := rest[2 : 2+aLen]
	nlri := rest[2+aLen:]

	// Withdrawn prefixes (always IPv4 unicast in the fixed field).
	for len(wd) > 0 {
		p, pid, n, err := decodeNLRI(wd, 32, caps.addPath(FamilyV4))
		if err != nil {
			return nil, err
		}
		u.Withdrawn = append(u.Withdrawn, NLRI{Prefix: p, PathID: pid, Family: FamilyV4})
		wd = wd[n:]
	}

	attrs := &Attrs{}
	var mpReach, mpUnreach []mpBody
	var as4path []uint32
	asn4Size := 2
	if caps.asn4 {
		asn4Size = 4
	}

	for len(abuf) >= 3 {
		flags, code := abuf[0], abuf[1]
		var l int
		hdr := 3
		if flags&0x10 != 0 {
			if len(abuf) < 4 {
				return nil, fmt.Errorf("bgp: truncated attr header")
			}
			l = int(binary.BigEndian.Uint16(abuf[2:4]))
			hdr = 4
		} else {
			l = int(abuf[2])
		}
		if len(abuf) < hdr+l {
			return nil, fmt.Errorf("bgp: truncated attr %d", code)
		}
		v := abuf[hdr : hdr+l]
		abuf = abuf[hdr+l:]

		switch code {
		case attrOrigin:
			if len(v) >= 1 {
				attrs.Origin = v[0]
			}
		case attrASPath:
			p, err := decodeASPath(v, asn4Size)
			if err != nil {
				return nil, err
			}
			attrs.ASPath = p
		case attrAS4Path:
			p, err := decodeASPath(v, 4)
			if err == nil {
				as4path = p
			}
		case attrNextHop:
			if len(v) >= 4 {
				attrs.NextHop = netip.AddrFrom4([4]byte(v[:4]))
			}
		case attrMED:
			if len(v) >= 4 {
				attrs.MED = binary.BigEndian.Uint32(v[:4])
				attrs.HasMED = true
			}
		case attrLocalPref:
			if len(v) >= 4 {
				attrs.LocalPref = binary.BigEndian.Uint32(v[:4])
				attrs.HasLocalPref = true
			}
		case attrAtomicAgg:
			attrs.Atomic = true
		case attrAggregator:
			attrs.Aggregator = decodeAggregator(v, caps.asn4)
		case attrAS4Aggregator:
			attrs.Aggregator = decodeAggregator(v, true)
		case attrCommunity:
			for len(v) >= 4 {
				attrs.Communities = append(attrs.Communities, communityString(binary.BigEndian.Uint32(v[:4])))
				v = v[4:]
			}
		case attrLargeCommunity:
			for len(v) >= 12 {
				attrs.LargeCommunities = append(attrs.LargeCommunities, fmt.Sprintf("%d:%d:%d",
					binary.BigEndian.Uint32(v[:4]),
					binary.BigEndian.Uint32(v[4:8]),
					binary.BigEndian.Uint32(v[8:12])))
				v = v[12:]
			}
		case attrExtCommunity:
			for len(v) >= 8 {
				attrs.ExtCommunities = append(attrs.ExtCommunities, fmt.Sprintf("0x%016x", binary.BigEndian.Uint64(v[:8])))
				v = v[8:]
			}
		case attrOriginatorID:
			if len(v) >= 4 {
				attrs.OriginatorID = netip.AddrFrom4([4]byte(v[:4]))
			}
		case attrClusterList:
			for len(v) >= 4 {
				attrs.ClusterList = append(attrs.ClusterList, netip.AddrFrom4([4]byte(v[:4])))
				v = v[4:]
			}
		case attrOTC:
			if len(v) >= 4 {
				attrs.OTC = binary.BigEndian.Uint32(v[:4])
				attrs.HasOTC = true
			}
		case attrMPReach:
			m, err := decodeMP(v, true)
			if err != nil {
				return nil, err
			}
			mpReach = append(mpReach, m)
		case attrMPUnreach:
			m, err := decodeMP(v, false)
			if err != nil {
				return nil, err
			}
			mpUnreach = append(mpUnreach, m)
		}
	}

	// Merge AS4_PATH into AS_PATH for 2-byte peers.
	if !caps.asn4 && len(as4path) > 0 {
		if len(as4path) <= len(attrs.ASPath) {
			merged := make([]uint32, len(attrs.ASPath))
			copy(merged, attrs.ASPath[:len(attrs.ASPath)-len(as4path)])
			copy(merged[len(attrs.ASPath)-len(as4path):], as4path)
			attrs.ASPath = merged
		} else {
			attrs.ASPath = as4path
		}
	}
	u.Attrs = attrs

	// MP_REACH announced NLRI.
	for _, m := range mpReach {
		bits := 128
		if m.family.AFI == 1 {
			bits = 32
		}
		if !isUnicast(m.family) {
			continue // unknown family NLRI formats — can't decode
		}
		rest := m.nlri
		if len(rest) == 0 {
			u.EOR = append(u.EOR, m.family)
			continue
		}
		for len(rest) > 0 {
			p, pid, n, err := decodeNLRI(rest, bits, caps.addPath(m.family))
			if err != nil {
				return nil, err
			}
			u.Announced = append(u.Announced, NLRI{Prefix: p, PathID: pid, Family: m.family, NextHop: m.nexthop})
			rest = rest[n:]
		}
	}

	// MP_UNREACH withdrawn NLRI.
	for _, m := range mpUnreach {
		if len(m.nlri) == 0 {
			u.EOR = append(u.EOR, m.family)
			continue
		}
		if !isUnicast(m.family) {
			continue
		}
		bits := 128
		if m.family.AFI == 1 {
			bits = 32
		}
		rest := m.nlri
		for len(rest) > 0 {
			p, pid, n, err := decodeNLRI(rest, bits, caps.addPath(m.family))
			if err != nil {
				return nil, err
			}
			u.Withdrawn = append(u.Withdrawn, NLRI{Prefix: p, PathID: pid, Family: m.family})
			rest = rest[n:]
		}
	}

	// Plain IPv4 unicast NLRI tail.
	emptyV4 := wLen == 0 && aLen == 0 && len(nlri) == 0
	for len(nlri) > 0 {
		p, pid, n, err := decodeNLRI(nlri, 32, caps.addPath(FamilyV4))
		if err != nil {
			return nil, err
		}
		u.Announced = append(u.Announced, NLRI{Prefix: p, PathID: pid, Family: FamilyV4, NextHop: attrs.NextHop})
		nlri = nlri[n:]
	}

	// A completely empty UPDATE is End-of-RIB for IPv4 unicast.
	if emptyV4 {
		u.EOR = append(u.EOR, FamilyV4)
	}
	return u, nil
}

func isUnicast(f Family) bool {
	return (f.AFI == 1 || f.AFI == 2) && (f.SAFI == 1 || f.SAFI == 4)
}

type mpBody struct {
	family  Family
	nexthop netip.Addr
	nlri    []byte
}

func decodeMP(v []byte, reach bool) (mpBody, error) {
	var m mpBody
	if len(v) < 3 {
		return m, fmt.Errorf("bgp: short MP attr")
	}
	m.family = Family{binary.BigEndian.Uint16(v[:2]), v[2]}
	if !reach {
		m.nlri = v[3:]
		return m, nil
	}
	if len(v) < 4 {
		return m, fmt.Errorf("bgp: short MP_REACH")
	}
	nhLen := int(v[3])
	if len(v) < 4+nhLen+1 {
		return m, fmt.Errorf("bgp: truncated MP_REACH nexthop")
	}
	nh := v[4 : 4+nhLen]
	switch {
	case nhLen >= 16:
		m.nexthop = netip.AddrFrom16([16]byte(nh[:16]))
	case nhLen == 4:
		m.nexthop = netip.AddrFrom4([4]byte(nh[:4]))
	}
	rest := v[4+nhLen:]
	snpaLen := int(rest[0])
	rest = rest[1:]
	if len(rest) < snpaLen {
		return m, fmt.Errorf("bgp: truncated MP_REACH SNPA")
	}
	m.nlri = rest[snpaLen:]
	return m, nil
}

// decodeNLRI reads one prefix (optionally preceded by a 4-byte path-id).
// bits is 32 or 128. Returns the masked prefix, path-id and bytes consumed.
func decodeNLRI(buf []byte, bits int, addPath bool) (netip.Prefix, uint32, int, error) {
	n := 0
	var pid uint32
	if addPath {
		if len(buf) < 4 {
			return netip.Prefix{}, 0, 0, fmt.Errorf("bgp: truncated path-id")
		}
		pid = binary.BigEndian.Uint32(buf[:4])
		buf = buf[4:]
		n += 4
	}
	if len(buf) < 1 {
		return netip.Prefix{}, 0, 0, fmt.Errorf("bgp: truncated NLRI")
	}
	plen := int(buf[0])
	buf = buf[1:]
	n++
	if plen > bits {
		return netip.Prefix{}, 0, 0, fmt.Errorf("bgp: NLRI plen %d > %d", plen, bits)
	}
	nbytes := (plen + 7) / 8
	if len(buf) < nbytes {
		return netip.Prefix{}, 0, 0, fmt.Errorf("bgp: truncated NLRI bytes")
	}
	var raw [16]byte
	copy(raw[:], buf[:nbytes])
	if rem := plen % 8; rem != 0 && nbytes > 0 {
		raw[nbytes-1] &= 0xff << (8 - rem)
	}
	n += nbytes
	var addr netip.Addr
	if bits == 32 {
		addr = netip.AddrFrom4([4]byte(raw[:4]))
	} else {
		addr = netip.AddrFrom16(raw)
	}
	return netip.PrefixFrom(addr, plen).Masked(), pid, n, nil
}

func decodeASPath(v []byte, asnSize int) ([]uint32, error) {
	var out []uint32
	for len(v) >= 2 {
		segType, segLen := v[0], int(v[1])
		v = v[2:]
		need := segLen * asnSize
		if len(v) < need {
			return nil, fmt.Errorf("bgp: truncated AS_PATH")
		}
		for i := 0; i < segLen; i++ {
			var asn uint32
			if asnSize == 4 {
				asn = binary.BigEndian.Uint32(v[i*4:])
			} else {
				asn = uint32(binary.BigEndian.Uint16(v[i*2:]))
			}
			out = append(out, asn)
		}
		_ = segType // sets/sequences flattened for display purposes
		v = v[need:]
	}
	return out, nil
}

func decodeAggregator(v []byte, asn4 bool) string {
	if asn4 {
		if len(v) < 8 {
			return ""
		}
		return fmt.Sprintf("AS%d %s", binary.BigEndian.Uint32(v[:4]),
			netip.AddrFrom4([4]byte(v[4:8])))
	}
	if len(v) < 6 {
		return ""
	}
	return fmt.Sprintf("AS%d %s", binary.BigEndian.Uint16(v[:2]),
		netip.AddrFrom4([4]byte(v[2:6])))
}

func communityString(c uint32) string {
	switch c {
	case 0xFFFFFF01:
		return "NO_EXPORT"
	case 0xFFFFFF02:
		return "NO_ADVERTISE"
	case 0xFFFFFF03:
		return "NO_EXPORT_SUBCONFED"
	case 0xFFFFFF04:
		return "NOPEER"
	case 0xFFFF029A:
		return "BLACKHOLE"
	}
	return fmt.Sprintf("%d:%d", c>>16, c&0xffff)
}
