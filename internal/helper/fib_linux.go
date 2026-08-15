//go:build linux

package helper

import (
	"errors"
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

type linuxRouteOps interface {
	Replace(*netlink.Route) error
	Delete(*netlink.Route) error
}

type realLinuxRouteOps struct{}

var linuxRoutes linuxRouteOps = realLinuxRouteOps{}

func (realLinuxRouteOps) Replace(route *netlink.Route) error { return netlink.RouteReplace(route) }
func (realLinuxRouteOps) Delete(route *netlink.Route) error  { return netlink.RouteDel(route) }

var (
	errRouteNotIPv4Host    = errors.New("route is not an IPv4 host route")
	errRouteNextHopNotIPv4 = errors.New("route next hop is not an IPv4 address")
)

// RouteFIBError reports a failed host-FIB operation with enough structured
// context for callers and tests to match it without parsing strings.
type RouteFIBError struct {
	Operation string
	Prefix    string
	Nexthop   string
	Err       error
}

func (e *RouteFIBError) Error() string {
	operation := e.Operation
	switch operation {
	case "parse-prefix":
		operation = "parse route prefix"
	case "parse-nexthop":
		operation = "parse route next hop"
	default:
		operation += " route"
	}
	if e.Nexthop == "" {
		return fmt.Sprintf("%s %s: %v", operation, e.Prefix, e.Err)
	}
	return fmt.Sprintf("%s %s via %s: %v", operation, e.Prefix, e.Nexthop, e.Err)
}

func (e *RouteFIBError) Unwrap() error { return e.Err }

type routeFIB struct{}

func (routeFIB) AddHostRoute(prefix, nexthop string) error {
	route, err := linuxRoute(prefix, nexthop)
	if err != nil {
		return err
	}
	if err := linuxRoutes.Replace(route); err != nil {
		return &RouteFIBError{Operation: "replace", Prefix: prefix, Nexthop: nexthop, Err: err}
	}
	return nil
}

func (routeFIB) DeleteHostRoute(prefix string) error {
	route, err := linuxRoute(prefix, "")
	if err != nil {
		return err
	}
	if err := linuxRoutes.Delete(route); err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ESRCH) {
			return nil
		}
		return &RouteFIBError{Operation: "delete", Prefix: prefix, Err: err}
	}
	return nil
}

func linuxRoute(prefix, nexthop string) (*netlink.Route, error) {
	_, network, err := net.ParseCIDR(prefix)
	if err != nil {
		return nil, &RouteFIBError{Operation: "parse-prefix", Prefix: prefix, Nexthop: nexthop, Err: err}
	}
	if bits, size := network.Mask.Size(); size != net.IPv4len*8 || bits != size {
		return nil, &RouteFIBError{Operation: "parse-prefix", Prefix: prefix, Nexthop: nexthop, Err: errRouteNotIPv4Host}
	}
	route := &netlink.Route{Dst: network}
	if nexthop == "" {
		return route, nil
	}
	gateway := net.ParseIP(nexthop).To4()
	if gateway == nil {
		return nil, &RouteFIBError{Operation: "parse-nexthop", Prefix: prefix, Nexthop: nexthop, Err: errRouteNextHopNotIPv4}
	}
	route.Gw = gateway
	return route, nil
}
