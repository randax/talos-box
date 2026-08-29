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

func TestLinuxLinkLooksLikeTunnel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		link netlink.Link
		want bool
	}{
		{name: "wireguard with arbitrary name", link: &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: "private0"}}, want: true},
		{name: "tuntap", link: &netlink.Tuntap{LinkAttrs: netlink.LinkAttrs{Name: "private1"}}, want: true},
		{name: "PPP link type", link: &netlink.GenericLink{LinkAttrs: netlink.LinkAttrs{Name: "private2"}, LinkType: "ppp"}, want: true},
		{name: "PPP encapsulation", link: &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "private3", EncapType: "ppp"}}, want: true},
		{name: "GRE metadata", link: &netlink.Gretun{LinkAttrs: netlink.LinkAttrs{Name: "private4"}, Local: net.ParseIP("192.0.2.1")}, want: true},
		{name: "VTI metadata", link: &netlink.Vti{LinkAttrs: netlink.LinkAttrs{Name: "private5"}, Local: net.ParseIP("192.0.2.1")}, want: true},
		{name: "WSL2 NAT ethernet", link: &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "eth0", EncapType: "ether"}}, want: false},
		{name: "bridge", link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "bridge0"}}, want: false},
		{name: "veth", link: &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "veth0"}}, want: false},
		{name: "dummy", link: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0"}}, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := linuxLinkLooksLikeTunnel(test.link); got != test.want {
				t.Errorf("linuxLinkLooksLikeTunnel(%s, %s) = %t, want %t", test.link.Type(), test.link.Attrs().EncapType, got, test.want)
			}
		})
	}
}

func TestSelectLinuxSystemRouteCarriesTunnelSignal(t *testing.T) {
	t.Parallel()

	got, err := selectLinuxSystemRoute(
		net.ParseIP("172.30.9.2"),
		[]netlink.Route{{LinkIndex: 7}},
		func(int) (netlink.Link, error) {
			return &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: "private0", Index: 7}}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LooksLikeTunnel {
		t.Fatalf("selected route = %+v, want tunnel signal", got)
	}
}

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
