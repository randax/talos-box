package helper

import "github.com/randax/talos-box/internal/bgp"

// bgpSpeaker is the platform-neutral handle the server keeps per cluster; the
// concrete type is the darwin/linux bgp.Speaker.
type bgpSpeaker interface {
	Stop()
}

// BGPRoute is one path the host speaker learned from a cluster's nodes and
// installed in the host FIB.
type BGPRoute struct {
	Prefix  string `json:"prefix"`
	Nexthop string `json:"nexthop"`
}

// BGPState is what bgp.status reports for one cluster: whether the helper owns
// a running speaker for it, and the routes that speaker announces to the host.
type BGPState struct {
	Active bool       `json:"active"`
	Routes []BGPRoute `json:"routes,omitempty"`
}

// bgpRouteReporter is the optional speaker capability behind BGPState.Routes.
type bgpRouteReporter interface {
	Routes() []bgp.Route
}

// The real speaker must keep satisfying bgpRouteReporter: the map only demands
// Stop(), so without this guard a rename or receiver change on Routes would
// silently downgrade `tbx bgp status` to reporting no announced routes.
var _ bgpRouteReporter = (*bgp.Speaker)(nil)

// bgpState reads one speaker's reportable state. A speaker the server does not
// own reports as stopped, and one that cannot report its routes reports none.
func bgpState(speaker bgpSpeaker, active bool) BGPState {
	state := BGPState{Active: active}
	reporter, reports := speaker.(bgpRouteReporter)
	if !active || !reports {
		return state
	}
	for _, route := range reporter.Routes() {
		state.Routes = append(state.Routes, BGPRoute{Prefix: route.Prefix, Nexthop: route.Nexthop})
	}
	return state
}
