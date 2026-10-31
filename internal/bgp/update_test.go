package bgp

import (
	"encoding/binary"
	"testing"
)

// build a mixed UPDATE: withdraws a v4 /24, announces two v4 /24s,
// MP_REACH one v6 /32, empty MP_UNREACH (EOR) for v6.
func testUpdate() []byte {
	wd := []byte{24, 192, 0, 2}

	var attrs []byte
	attr := func(flags, code byte, v []byte) {
		attrs = append(attrs, flags, code, byte(len(v)))
		attrs = append(attrs, v...)
	}
	attr(0x40, attrOrigin, []byte{0})
	attr(0x40, attrASPath, []byte{2, 3, 0xFC, 0x00, 0x00, 0xAE, 0xFB, 0xF0})
	attr(0x40, attrNextHop, []byte{10, 0, 0, 1})
	attr(0xC0, attrCommunity, []byte{0xFC, 0x00, 0x00, 0x64})

	v6nh := [16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
	mpReach := append([]byte{0, 2, 1, 16}, v6nh[:]...)
	mpReach = append(mpReach, 0)
	mpReach = append(mpReach, 32, 0x20, 0x01, 0x0d, 0xb8)
	attr(0x80, attrMPReach, mpReach)
	attr(0x80, attrMPUnreach, []byte{0, 2, 1})

	nlri := []byte{24, 203, 0, 113, 24, 198, 51, 100}

	body := make([]byte, 0, 128)
	body = binary.BigEndian.AppendUint16(body, uint16(len(wd)))
	body = append(body, wd...)
	body = binary.BigEndian.AppendUint16(body, uint16(len(attrs)))
	body = append(body, attrs...)
	body = append(body, nlri...)
	return body
}

func TestDecodeUpdate(t *testing.T) {
	u, err := decodeUpdate(testUpdate(), wireCaps{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(u.Withdrawn) != 1 || u.Withdrawn[0].Prefix.String() != "192.0.2.0/24" {
		t.Fatalf("withdrawn: %+v", u.Withdrawn)
	}
	want := map[string]string{
		"203.0.113.0/24":  "10.0.0.1",
		"198.51.100.0/24": "10.0.0.1",
		"2001:db8::/32":   "2001:db8::1",
	}
	if len(u.Announced) != 3 {
		t.Fatalf("announced: %+v", u.Announced)
	}
	for _, a := range u.Announced {
		nh, ok := want[a.Prefix.String()]
		if !ok {
			t.Fatalf("unexpected prefix %s", a.Prefix)
		}
		if a.NextHop.String() != nh {
			t.Fatalf("%s nexthop=%s want %s", a.Prefix, a.NextHop, nh)
		}
	}
	got := u.Attrs.ASPath
	if len(got) != 3 || got[0] != 64512 || got[1] != 174 || got[2] != 64496 {
		t.Fatalf("aspath %v", got)
	}
	if len(u.Attrs.Communities) != 1 || u.Attrs.Communities[0] != "64512:100" {
		t.Fatalf("communities %v", u.Attrs.Communities)
	}
	if len(u.EOR) != 1 || u.EOR[0] != FamilyV6 {
		t.Fatalf("eor %v", u.EOR)
	}
}

func TestEmptyUpdateIsEORv4(t *testing.T) {
	u, err := decodeUpdate([]byte{0, 0, 0, 0}, wireCaps{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(u.EOR) != 1 || u.EOR[0] != FamilyV4 {
		t.Fatalf("eor %v", u.EOR)
	}
}

// 2-byte peer: AS_PATH has AS_TRANS + 2-byte ASNs, AS4_PATH carries the real tail.
func TestAS4Merge(t *testing.T) {
	var attrs []byte
	attr := func(flags, code byte, v []byte) {
		attrs = append(attrs, flags, code, byte(len(v)))
		attrs = append(attrs, v...)
	}
	attr(0x40, attrOrigin, []byte{0})
	// seq [23456, 64512] in 2-byte form
	attr(0x40, attrASPath, []byte{2, 2, 0x5B, 0xA0, 0xFC, 0x00})
	// AS4_PATH seq [196615, 64512]
	attr(0xC0, attrAS4Path, []byte{2, 2, 0x00, 0x03, 0x00, 0x07, 0x00, 0x00, 0xFC, 0x00})
	attr(0x40, attrNextHop, []byte{10, 0, 0, 1})

	body := make([]byte, 0, 64)
	body = binary.BigEndian.AppendUint16(body, 0)
	body = binary.BigEndian.AppendUint16(body, uint16(len(attrs)))
	body = append(body, attrs...)
	body = append(body, 24, 203, 0, 113)

	u, err := decodeUpdate(body, wireCaps{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := u.Attrs.ASPath
	// AS4 tail (len 2) replaces last 2 of AS_PATH → [196615, 64512]
	if len(got) != 2 || got[0] != 196615 || got[1] != 64512 {
		t.Fatalf("merged aspath %v", got)
	}
}

// add-path: path-id precedes each NLRI when negotiated.
func TestAddPathNLRI(t *testing.T) {
	caps := wireCaps{addPathRecv: map[Family]bool{FamilyV4: true}}
	var wd []byte
	wd = binary.BigEndian.AppendUint32(wd, 77) // path-id
	wd = append(wd, 24, 192, 0, 2)
	body := make([]byte, 0, 16)
	body = binary.BigEndian.AppendUint16(body, uint16(len(wd)))
	body = append(body, wd...)
	body = binary.BigEndian.AppendUint16(body, 0)
	u, err := decodeUpdate(body, caps)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(u.Withdrawn) != 1 || u.Withdrawn[0].PathID != 77 {
		t.Fatalf("withdrawn %+v", u.Withdrawn)
	}
}
