package rib

import (
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

const searchCap = 150

// SearchResult is what /api/search returns.
type SearchResult struct {
	Query     string   `json:"query"`
	Kind      string   `json:"kind"` // prefix | ip | asn | peer
	Routes    []*Route `json:"routes"`
	Truncated bool     `json:"truncated,omitempty"`
	Total     int      `json:"total"`
}

// Search resolves q as (in order): prefix, IP address, AS number, peer name.
func (r *RIB) Search(q string) *SearchResult {
	res := &SearchResult{Query: q, Routes: []*Route{}}
	q = strings.TrimSpace(q)
	if q == "" {
		return res
	}
	lq := strings.ToLower(q)

	r.mu.Lock()
	defer r.mu.Unlock()

	collect := func(keys []string) {
		for _, k := range keys {
			if len(res.Routes) >= searchCap {
				res.Truncated = true
				return
			}
			if rt := r.routes[k]; rt != nil {
				res.Routes = append(res.Routes, rt)
			}
		}
	}

	// 1. Prefix literal ("203.0.113.0/24", "2001:db8::/32")
	if p, err := netip.ParsePrefix(lq); err == nil {
		res.Kind = "prefix"
		p = p.Masked()
		t := r.trieFor(p.Addr())
		seen := map[string]struct{}{}
		// exact
		for k := range t.exactKeys(p) {
			seen[k] = struct{}{}
		}
		// covering (LPM chain, longest first)
		cov := t.coveringKeys(p.Addr(), 30)
		for _, k := range cov {
			seen[k] = struct{}{}
		}
		var ordered []string
		for k := range seen {
			ordered = append(ordered, k)
		}
		collect(ordered)
		// more-specifics
		var sub []string
		for _, k := range t.subtreeKeys(p, 400) {
			if _, ok := seen[k]; !ok {
				sub = append(sub, k)
			}
		}
		collect(sub)
		res.Total = len(res.Routes)
		r.sortRoutes(res.Routes)
		return res
	}

	// 2. Bare IP → LPM chain
	if a, err := netip.ParseAddr(lq); err == nil {
		res.Kind = "ip"
		collect(r.trieFor(a).coveringKeys(a, searchCap))
		res.Total = len(res.Routes)
		r.sortRoutes(res.Routes)
		return res
	}

	// 3. ASN — "as1299", "1299", "AS 6939"
	asnStr := strings.TrimPrefix(lq, "as")
	asnStr = strings.TrimSpace(asnStr)
	if asn, err := strconv.ParseUint(asnStr, 10, 32); err == nil && asn > 0 {
		res.Kind = "asn"
		a := uint32(asn)
		for _, rt := range r.routes {
			if len(res.Routes) >= searchCap {
				res.Truncated = true
				break
			}
			if pathHas(rt.ASPath, a) {
				res.Routes = append(res.Routes, rt)
			}
		}
		res.Total = len(res.Routes)
		r.sortRoutes(res.Routes)
		return res
	}

	// 4. Peer name/addr substring
	res.Kind = "peer"
	for id, p := range r.peers {
		name := strings.ToLower(p.Info.Name + " " + id + " " + p.Info.Addr)
		if strings.Contains(name, lq) {
			for _, rt := range r.routes {
				if len(res.Routes) >= searchCap {
					res.Truncated = true
					break
				}
				if rt.PeerID == id {
					res.Routes = append(res.Routes, rt)
				}
			}
		}
	}
	res.Total = len(res.Routes)
	r.sortRoutes(res.Routes)
	return res
}

func (r *RIB) trieFor(a netip.Addr) *trie {
	if a.Is4() || a.Is4In6() {
		return r.v4
	}
	return r.v6
}

func (r *RIB) sortRoutes(rts []*Route) {
	sort.Slice(rts, func(i, j int) bool {
		if rts[i].Prefix != rts[j].Prefix {
			return rts[i].Prefix.String() < rts[j].Prefix.String()
		}
		return rts[i].PeerID < rts[j].PeerID
	})
}

func pathHas(path []uint32, asn uint32) bool {
	for _, a := range path {
		if a == asn {
			return true
		}
	}
	return false
}
