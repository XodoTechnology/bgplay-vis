package rib

import "net/netip"

// trie is a unibit prefix index over route keys.
type trieNode struct {
	child [2]*trieNode
	keys  map[string]struct{} // route keys anchored at this exact prefix
}

type trie struct {
	root *trieNode
	bits int
}

func newTrie(bits int) *trie {
	return &trie{root: &trieNode{}, bits: bits}
}

func addrBytes(p netip.Prefix, bits int) []byte {
	a := p.Addr()
	if bits == 32 {
		b := a.As4()
		return b[:]
	}
	b := a.As16()
	return b[:]
}

func bitAt(b []byte, i int) int {
	return int(b[i/8]>>(7-i%8)) & 1
}

func (t *trie) add(p netip.Prefix, key string) {
	b := addrBytes(p, t.bits)
	n := t.root
	for i := 0; i < p.Bits(); i++ {
		bit := bitAt(b, i)
		if n.child[bit] == nil {
			n.child[bit] = &trieNode{}
		}
		n = n.child[bit]
	}
	if n.keys == nil {
		n.keys = map[string]struct{}{}
	}
	n.keys[key] = struct{}{}
}

func (t *trie) remove(p netip.Prefix, key string) {
	b := addrBytes(p, t.bits)
	path := make([]*trieNode, 0, p.Bits()+1)
	n := t.root
	path = append(path, n)
	for i := 0; i < p.Bits(); i++ {
		n = n.child[bitAt(b, i)]
		if n == nil {
			return
		}
		path = append(path, n)
	}
	delete(n.keys, key)
	// prune empty tail
	for i := len(path) - 1; i > 0; i-- {
		cur := path[i]
		if len(cur.keys) > 0 || cur.child[0] != nil || cur.child[1] != nil {
			break
		}
		bit := bitAt(b, i-1)
		path[i-1].child[bit] = nil
	}
}

// exactKeys returns route keys anchored exactly at p.
func (t *trie) exactKeys(p netip.Prefix) map[string]struct{} {
	b := addrBytes(p, t.bits)
	n := t.root
	for i := 0; i < p.Bits(); i++ {
		n = n.child[bitAt(b, i)]
		if n == nil {
			return nil
		}
	}
	return n.keys
}

// subtreeKeys collects route keys under prefix p (inclusive), capped.
func (t *trie) subtreeKeys(p netip.Prefix, cap int) []string {
	b := addrBytes(p, t.bits)
	n := t.root
	for i := 0; i < p.Bits(); i++ {
		n = n.child[bitAt(b, i)]
		if n == nil {
			return nil
		}
	}
	var out []string
	var walk func(*trieNode)
	walk = func(x *trieNode) {
		if x == nil || len(out) >= cap {
			return
		}
		for k := range x.keys {
			out = append(out, k)
		}
		walk(x.child[0])
		walk(x.child[1])
	}
	walk(n)
	return out
}

// coveringKeys returns route keys for every prefix on the LPM chain of addr,
// longest-match first.
func (t *trie) coveringKeys(addr netip.Addr, cap int) []string {
	var b []byte
	if t.bits == 32 {
		a := addr.As4()
		b = a[:]
	} else {
		a := addr.As16()
		b = a[:]
	}
	var out []string
	n := t.root
	for i := 0; i < t.bits && n != nil && len(out) < cap; i++ {
		for k := range n.keys {
			out = append(out, k)
		}
		n = n.child[bitAt(b, i)]
	}
	if n != nil {
		for k := range n.keys {
			out = append(out, k)
		}
	}
	// out is shortest→longest; reverse for longest first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func (r *RIB) index(rt *Route) {
	t := r.v4
	if !rt.Prefix.Addr().Is4() {
		t = r.v6
	}
	t.add(rt.Prefix, rt.Key)
}

func (r *RIB) removeIndex(rt *Route) {
	t := r.v4
	if !rt.Prefix.Addr().Is4() {
		t = r.v6
	}
	t.remove(rt.Prefix, rt.Key)
}
