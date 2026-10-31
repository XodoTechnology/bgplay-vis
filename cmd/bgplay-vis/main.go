// bgplay-vis — a live BGP route collector + visualizer.
//
// Routers peer with it (or it peers with them), it maintains per-peer
// adj-rib-in tables, and streams announce/withdraw/change events to an
// animated web UI.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/XodoTechnology/bgplay-vis/internal/bgp"
	"github.com/XodoTechnology/bgplay-vis/internal/demo"
	"github.com/XodoTechnology/bgplay-vis/internal/hub"
	"github.com/XodoTechnology/bgplay-vis/internal/rib"
	"github.com/XodoTechnology/bgplay-vis/internal/web"
)

type strList []string

func (s *strList) String() string { return strings.Join(*s, ",") }
func (s *strList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	var (
		asn      = flag.Uint("asn", 65000, "local ASN")
		rid      = flag.String("rid", "192.0.2.1", "BGP router-id")
		bgpAddr  = flag.String("bgp", ":179", "BGP listen address (empty disables)")
		webAddr  = flag.String("web", ":8080", "web UI listen address")
		hold     = flag.Int("hold", 90, "BGP holdtime seconds (0 = none)")
		demoMode = flag.Bool("demo", false, "generate a synthetic feed for a lively UI")
		peers    strList
	)
	flag.Var(&peers, "peer", "outbound BGP peer (host or host:port); repeatable")
	flag.Parse()

	routerID, err := netip.ParseAddr(*rid)
	if err != nil {
		log.Fatalf("bad -rid: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	h := hub.New()
	r := rib.New(h.Publish)
	r.SetLocalASN(uint32(*asn))
	go h.Run(ctx.Done())

	if *bgpAddr != "" || len(peers) > 0 {
		sp := bgp.NewSpeaker(bgp.Config{
			LocalASN: uint32(*asn),
			RouterID: routerID,
			HoldTime: *hold,
			Listen:   *bgpAddr,
			Outbound: peers,
			Handler:  r,
		})
		if err := sp.Start(ctx); err != nil {
			log.Fatalf("bgp: %v", err)
		}
		defer sp.Close()
	} else {
		log.Printf("bgp: listener disabled")
	}

	if *demoMode {
		go demo.Run(ctx, r)
	}

	ws := web.New(r, h)
	go ws.RunStatsTicker(ctx)

	srv := &http.Server{
		Addr:              *webAddr,
		Handler:           ws.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shCtx)
	}()
	fmt.Printf("bgplay-vis: web UI on http://%s  (AS%d, bgp=%s)\n", *webAddr, *asn, *bgpAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("web: %v", err)
	}
}
