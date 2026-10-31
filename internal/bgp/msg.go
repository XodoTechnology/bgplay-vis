// Package bgp implements a minimal passive BGP speaker: it accepts (or
// initiates) sessions, speaks OPEN/KEEPALIVE/UPDATE/NOTIFICATION and decodes
// UPDATEs into prefix events. It never originates routes of its own.
package bgp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
)

const (
	HeaderLen = 19
	MaxMsgLen = 4096

	TypeOpen         = 1
	TypeUpdate       = 2
	TypeNotification = 3
	TypeKeepalive    = 4
	TypeRouteRefresh = 5
)

// Capability codes.
const (
	CapMultiprotocol = 1
	CapRouteRefresh  = 2
	CapExtNexthop    = 5
	CapGraceful      = 64
	CapFourOctetASN  = 65
	CapAddPath       = 69
	CapEnhancedRR    = 70
	CapFQDN          = 73
)

// Add-path direction modes (RFC 7911), as advertised.
const (
	AddPathReceive = 1
	AddPathSend    = 2
	AddPathBoth    = 3
)

var errMarker = errors.New("bgp: bad message marker")

// Family identifies an AFI/SAFI pair.
type Family struct {
	AFI  uint16 `json:"afi"`
	SAFI uint8  `json:"safi"`
}

func (f Family) String() string {
	switch f {
	case Family{1, 1}:
		return "ipv4-unicast"
	case Family{2, 1}:
		return "ipv6-unicast"
	default:
		return fmt.Sprintf("afi%d/safi%d", f.AFI, f.SAFI)
	}
}

// FamilyV4 / FamilyV6 are the only families we negotiate.
var (
	FamilyV4 = Family{AFI: 1, SAFI: 1}
	FamilyV6 = Family{AFI: 2, SAFI: 1}
)

// Open is a decoded (or to-be-encoded) OPEN message.
type Open struct {
	ASN      uint32 // effective ASN (4-octet aware)
	HoldTime uint16
	RouterID netip.Addr
	Caps     []Capability
}

type Capability struct {
	Code uint8
	Data []byte
}

// PeerCaps summarises the capabilities that matter to us.
type PeerCaps struct {
	Families     map[Family]bool
	FourOctet    bool
	AddPathSend  map[Family]bool // peer may transmit path-ids to us
	Name         string
	Graceful     bool
	RouteRefresh bool
}

func (c PeerCaps) familyList() []Family {
	out := make([]Family, 0, len(c.Families))
	for f := range c.Families {
		out = append(out, f)
	}
	return out
}

// Notification is a decoded NOTIFICATION message.
type Notification struct {
	Code    uint8
	Subcode uint8
	Data    []byte
}

func (n Notification) Error() string {
	return fmt.Sprintf("bgp notification %d/%d data=%x", n.Code, n.Subcode, n.Data)
}

func readMessage(r io.Reader) (typ uint8, body []byte, err error) {
	var hdr [HeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	for i := 0; i < 16; i++ {
		if hdr[i] != 0xff {
			return 0, nil, errMarker
		}
	}
	l := int(binary.BigEndian.Uint16(hdr[16:18]))
	if l < HeaderLen || l > MaxMsgLen {
		return 0, nil, fmt.Errorf("bgp: bad message length %d", l)
	}
	body = make([]byte, l-HeaderLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return hdr[18], body, nil
}

func writeMessage(w io.Writer, typ uint8, body []byte) error {
	buf := make([]byte, HeaderLen+len(body))
	for i := 0; i < 16; i++ {
		buf[i] = 0xff
	}
	binary.BigEndian.PutUint16(buf[16:18], uint16(len(buf)))
	buf[18] = typ
	copy(buf[HeaderLen:], body)
	_, err := w.Write(buf)
	return err
}

func keepaliveMsg() []byte {
	buf := make([]byte, HeaderLen)
	for i := 0; i < 16; i++ {
		buf[i] = 0xff
	}
	binary.BigEndian.PutUint16(buf[16:18], HeaderLen)
	buf[18] = TypeKeepalive
	return buf
}

func notificationMsg(code, subcode uint8, data []byte) []byte {
	body := make([]byte, 2+len(data))
	body[0] = code
	body[1] = subcode
	copy(body[2:], data)
	buf := make([]byte, HeaderLen+len(body))
	for i := 0; i < 16; i++ {
		buf[i] = 0xff
	}
	binary.BigEndian.PutUint16(buf[16:18], uint16(len(buf)))
	buf[18] = TypeNotification
	copy(buf[HeaderLen:], body)
	return buf
}

// encodeOpen builds an OPEN message body for our local speaker.
func encodeOpen(localASN uint32, hold uint16, routerID netip.Addr) []byte {
	var caps []byte
	addCap := func(code uint8, data []byte) {
		caps = append(caps, code, uint8(len(data)))
		caps = append(caps, data...)
	}
	// MP-BGP: IPv4 + IPv6 unicast.
	for _, fam := range []Family{FamilyV4, FamilyV6} {
		d := []byte{byte(fam.AFI >> 8), byte(fam.AFI), 0, fam.SAFI}
		addCap(CapMultiprotocol, d)
	}
	addCap(CapRouteRefresh, nil)
	addCap(CapEnhancedRR, nil)
	// Graceful restart: advertise restart-time only, no forwarding state.
	addCap(CapGraceful, []byte{0x00, 120})
	addCap(CapFourOctetASN, []byte{byte(localASN >> 24), byte(localASN >> 16), byte(localASN >> 8), byte(localASN)})
	// Add-path: receive-only for both families — lets route servers /
	// collectors send us multiple paths per prefix.
	addCap(CapAddPath, []byte{
		0, 1, 1, AddPathReceive,
		0, 2, 1, AddPathReceive,
	})
	// Extended nexthop: allow v6 nexthops for v4 NLRI (RFC 8950).
	// Triplets are AFI(2) SAFI(2) NexthopAFI(2) — SAFI is 2 bytes here.
	addCap(CapExtNexthop, []byte{0, 1, 0, 1, 0, 2})

	params := []byte{2, uint8(len(caps))}
	params = append(params, caps...)

	asn2 := localASN
	if asn2 > 65535 {
		asn2 = 23456 // AS_TRANS
	}
	body := make([]byte, 0, 64)
	body = append(body, 4)
	body = binary.BigEndian.AppendUint16(body, uint16(asn2))
	body = binary.BigEndian.AppendUint16(body, hold)
	rid := routerID.As4()
	body = append(body, rid[:]...)
	body = append(body, uint8(len(params)))
	body = append(body, params...)
	return body
}

// decodeOpen parses an OPEN body, returning the effective peer ASN and caps.
func decodeOpen(body []byte) (*Open, PeerCaps, error) {
	o := &Open{}
	pc := PeerCaps{
		Families:    map[Family]bool{},
		AddPathSend: map[Family]bool{},
	}
	if len(body) < 10 {
		return nil, pc, fmt.Errorf("bgp: short OPEN")
	}
	if body[0] != 4 {
		return nil, pc, fmt.Errorf("bgp: unsupported version %d", body[0])
	}
	o.ASN = uint32(binary.BigEndian.Uint16(body[1:3]))
	o.HoldTime = binary.BigEndian.Uint16(body[3:5])
	o.RouterID = netip.AddrFrom4([4]byte{body[5], body[6], body[7], body[8]})
	optLen := int(body[9])
	if len(body) < 10+optLen {
		return nil, pc, fmt.Errorf("bgp: truncated OPEN params")
	}
	params := body[10 : 10+optLen]
	for len(params) >= 2 {
		ptype, plen := params[0], int(params[1])
		params = params[2:]
		if len(params) < plen {
			return nil, pc, fmt.Errorf("bgp: truncated OPEN param")
		}
		val := params[:plen]
		params = params[plen:]
		if ptype != 2 { // only capability params interest us
			continue
		}
		for len(val) >= 2 {
			code, clen := val[0], int(val[1])
			val = val[2:]
			if len(val) < clen {
				return nil, pc, fmt.Errorf("bgp: truncated capability")
			}
			data := val[:clen]
			val = val[clen:]
			o.Caps = append(o.Caps, Capability{Code: code, Data: data})
			switch code {
			case CapMultiprotocol:
				if len(data) >= 4 {
					pc.Families[Family{binary.BigEndian.Uint16(data[:2]), data[3]}] = true
				}
			case CapFourOctetASN:
				if len(data) >= 4 {
					pc.FourOctet = true
					o.ASN = binary.BigEndian.Uint32(data[:4])
				}
			case CapAddPath:
				for len(data) >= 4 {
					fam := Family{binary.BigEndian.Uint16(data[:2]), data[2]}
					mode := data[3]
					if mode == AddPathSend || mode == AddPathBoth {
						pc.AddPathSend[fam] = true
					}
					data = data[4:]
				}
			case CapGraceful:
				pc.Graceful = true
			case CapRouteRefresh:
				pc.RouteRefresh = true
			case CapFQDN:
				if len(data) > 0 {
					n := int(data[0])
					if n > len(data)-1 {
						n = len(data) - 1
					}
					pc.Name = string(data[1 : 1+n])
				}
			}
		}
	}
	return o, pc, nil
}
