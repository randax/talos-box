package bgp

import (
	"context"
	"fmt"
	"strings"
	"time"

	api "github.com/osrg/gobgp/v3/api"
	"github.com/osrg/gobgp/v3/pkg/server"
	"google.golang.org/protobuf/types/known/anypb"
)

// Speaker is the host's "top of rack" BGP router for one cluster: it listens
// on the gateway, accepts the cluster's nodes as dynamic neighbors, and drives
// a Reconciler from the paths they advertise. All of it needs root (port 179 +
// FIB), so a Speaker is created inside the helper.
type Speaker struct {
	server     *server.BgpServer
	reconciler *Reconciler
	cancel     context.CancelFunc
}

// bgpPort is BGP's well-known port; nodes' Cilium connects here.
const bgpPort = 179

// StartSpeaker brings up GoBGP with local ASN localASN on gatewayIP:179,
// accepting peers from peerCIDR as ASN peerASN, injecting learned /32s into fib.
func StartSpeaker(localASN, peerASN uint32, gatewayIP, peerCIDR string, fib FIB) (*Speaker, error) {
	return startSpeakerOnPort(localASN, peerASN, gatewayIP, peerCIDR, fib, bgpPort)
}

func startSpeakerOnPort(localASN, peerASN uint32, gatewayIP, peerCIDR string, fib FIB, port int32) (*Speaker, error) {
	bgpServer := server.NewBgpServer()
	go bgpServer.Serve()

	ctx := context.Background()
	if err := bgpServer.StartBgp(ctx, &api.StartBgpRequest{
		Global: &api.Global{
			Asn:             localASN,
			RouterId:        gatewayIP,
			ListenPort:      port,
			ListenAddresses: []string{gatewayIP},
		},
	}); err != nil {
		bgpServer.Stop()
		return nil, fmt.Errorf("start bgp: %w", err)
	}

	// accept any node in the cluster subnet as a passive dynamic neighbor
	if err := bgpServer.AddPeerGroup(ctx, &api.AddPeerGroupRequest{
		PeerGroup: &api.PeerGroup{
			Conf:      &api.PeerGroupConf{PeerGroupName: "nodes", PeerAsn: peerASN},
			Transport: &api.Transport{PassiveMode: true},
		},
	}); err != nil {
		bgpServer.Stop()
		return nil, fmt.Errorf("add peer group: %w", err)
	}
	if err := bgpServer.AddDynamicNeighbor(ctx, &api.AddDynamicNeighborRequest{
		DynamicNeighbor: &api.DynamicNeighbor{Prefix: peerCIDR, PeerGroup: "nodes"},
	}); err != nil {
		bgpServer.Stop()
		return nil, fmt.Errorf("add dynamic neighbor: %w", err)
	}

	speaker := &Speaker{server: bgpServer, reconciler: NewReconciler(fib)}
	watchCtx, cancel := context.WithCancel(ctx)
	speaker.cancel = cancel
	go speaker.watch(watchCtx)
	return speaker, nil
}

// watch re-reconciles the FIB from the full RIB whenever a path changes.
func (s *Speaker) watch(ctx context.Context) {
	reconcileFromRIB := func() {
		routes := s.currentRoutes(ctx)
		_ = s.reconciler.Reconcile(routes)
	}
	_ = s.server.WatchEvent(ctx, &api.WatchEventRequest{
		Table: &api.WatchEventRequest_Table{
			Filters: []*api.WatchEventRequest_Table_Filter{
				{Type: api.WatchEventRequest_Table_Filter_BEST},
			},
		},
	}, func(*api.WatchEventResponse) {
		reconcileFromRIB()
	})
	// periodic backstop in case an event is missed
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcileFromRIB()
		}
	}
}

// currentRoutes reads the global RIB and returns the LB /32 paths.
func (s *Speaker) currentRoutes(ctx context.Context) []Route {
	var routes []Route
	_ = s.server.ListPath(ctx, &api.ListPathRequest{
		TableType: api.TableType_GLOBAL,
		Family:    &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST},
	}, func(d *api.Destination) {
		// only inject host routes for LB VIP /32s; anything else would be
		// passed verbatim to `route -host` and error every cycle
		if !strings.HasSuffix(d.Prefix, "/32") {
			return
		}
		nexthop := bestNexthop(d)
		if nexthop == "" {
			return
		}
		routes = append(routes, Route{Prefix: d.Prefix, Nexthop: nexthop})
	})
	return routes
}

func bestNexthop(d *api.Destination) string {
	for _, path := range d.Paths {
		if path.IsWithdraw {
			continue
		}
		for _, attr := range path.Pattrs {
			if nh := nexthopFromAttr(attr); nh != "" {
				return nh
			}
		}
	}
	return ""
}

func nexthopFromAttr(attr *anypb.Any) string {
	var nh api.NextHopAttribute
	if err := attr.UnmarshalTo(&nh); err == nil {
		return nh.NextHop
	}
	var mp api.MpReachNLRIAttribute
	if err := attr.UnmarshalTo(&mp); err == nil && len(mp.NextHops) > 0 {
		return mp.NextHops[0]
	}
	return ""
}

// Stop shuts the speaker down FIB-safely: stop GoBGP first so no further RIB
// events can fire a Reconcile, cancel the watch loop, then withdraw every
// injected route. Reversing this order would let a late callback re-inject a
// route after WithdrawAll cleared it.
func (s *Speaker) Stop() {
	s.server.Stop()
	if s.cancel != nil {
		s.cancel()
	}
	s.reconciler.WithdrawAll()
}
