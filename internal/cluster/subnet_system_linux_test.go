//go:build linux

package cluster

import (
	"errors"
	"math"
	"net"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestSelectLinuxSystemRouteSkipsVanishedLink(t *testing.T) {
	t.Parallel()

	destination := net.ParseIP("172.30.9.2")
	routes := []netlink.Route{
		{LinkIndex: 99, Dst: mustIPNet(t, "172.30.9.0/24")},
		{LinkIndex: 1},
	}
	got, err := selectLinuxSystemRoute(destination, routes, func(index int) (netlink.Link, error) {
		if index == 99 {
			return nil, linuxLinkNotFoundError(t)
		}
		return &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0", Index: index}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Interface != "eth0" || got.Network.String() != "0.0.0.0/0" {
		t.Fatalf("selected route = %+v, want eth0 default route", got)
	}
}

func TestSelectLinuxSystemRouteKeepsUnexpectedLookupErrors(t *testing.T) {
	t.Parallel()

	want := errors.New("netlink permission denied")
	_, err := selectLinuxSystemRoute(net.ParseIP("172.30.9.2"), []netlink.Route{{LinkIndex: 9}}, func(int) (netlink.Link, error) {
		return nil, want
	})
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "resolve route interface 9") {
		t.Fatalf("selectLinuxSystemRoute() error = %v, want wrapped lookup error", err)
	}
}

func linuxLinkNotFoundError(t *testing.T) error {
	t.Helper()
	_, err := netlink.LinkByIndex(math.MaxInt32)
	var notFound netlink.LinkNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("LinkByIndex(max int) error = %v, want LinkNotFoundError", err)
	}
	return err
}

func mustIPNet(t *testing.T, cidr string) *net.IPNet {
	t.Helper()
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatal(err)
	}
	return network
}
