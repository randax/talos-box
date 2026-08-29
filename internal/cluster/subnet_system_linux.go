//go:build linux

package cluster

import (
	"errors"
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
)

func systemInterfaces() ([]HostInterface, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("dump netlink links: %w", err)
	}
	result := make([]HostInterface, 0, len(links))
	for _, link := range links {
		addresses, err := netlink.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			if _, lookupErr := netlink.LinkByIndex(link.Attrs().Index); lookupErr != nil {
				continue
			}
			return nil, fmt.Errorf("dump netlink addresses for %s: %w", link.Attrs().Name, err)
		}
		current := HostInterface{Name: link.Attrs().Name, Addrs: make([]net.Addr, 0, len(addresses))}
		for _, address := range addresses {
			if address.IPNet != nil {
				current.Addrs = append(current.Addrs, address.IPNet)
			}
		}
		result = append(result, current)
	}
	return result, nil
}

func systemRoute(destination net.IP) (HostRoute, error) {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return HostRoute{}, fmt.Errorf("dump netlink IPv4 routes: %w", err)
	}
	return selectLinuxSystemRoute(destination, routes, netlink.LinkByIndex)
}

func selectLinuxSystemRoute(
	destination net.IP,
	routes []netlink.Route,
	linkByIndex func(int) (netlink.Link, error),
) (HostRoute, error) {
	observed := make([]HostRoute, 0, len(routes))
	ignored := make(map[string]bool)
	for _, route := range routes {
		interfaceName := "netlink-route"
		looksLikeTunnel := false
		if route.LinkIndex != 0 {
			link, err := linkByIndex(route.LinkIndex)
			if err != nil {
				var notFound netlink.LinkNotFoundError
				if errors.As(err, &notFound) {
					continue
				}
				return HostRoute{}, fmt.Errorf("resolve route interface %d: %w", route.LinkIndex, err)
			}
			interfaceName = link.Attrs().Name
			looksLikeTunnel = linuxLinkLooksLikeTunnel(link)
			if ip := destination.To4(); ip != nil {
				index := int(ip[2])
				if interfaceName == LinuxBridgeName(index) && link.Attrs().Alias == LinuxBridgeAlias(index) {
					ignored[interfaceName] = true
				}
			}
		}
		network := route.Dst
		if network == nil {
			network = &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}
		}
		observed = append(observed, HostRoute{
			Interface:       interfaceName,
			Network:         network,
			Metric:          route.Priority,
			LooksLikeTunnel: looksLikeTunnel,
		})
	}
	selected := selectMostSpecificRoute(destination, observed, ignored)
	if selected.Interface == "" && selected.Network == nil {
		return HostRoute{}, nil
	}
	return selected, nil
}

func linuxLinkLooksLikeTunnel(link netlink.Link) bool {
	switch link.Type() {
	case "tuntap", "wireguard", "ppp", "ipip", "ip6tnl", "gre", "ip6gre", "gretap", "ip6gretap", "sit", "vti", "vti6", "xfrm":
		return true
	}

	switch link.Attrs().EncapType {
	case "ppp", "ipip", "tunnel6", "gre", "sit":
		return true
	default:
		return false
	}
}
