//go:build darwin

package cluster

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
)

func systemInterfaces() ([]HostInterface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	result := make([]HostInterface, 0, len(interfaces))
	for _, current := range interfaces {
		addresses, err := current.Addrs()
		if err != nil {
			// An interface that vanished during VPN churn cannot collide now.
			continue
		}
		result = append(result, HostInterface{Name: current.Name, Addrs: addresses})
	}
	return result, nil
}

func systemRoute(destination net.IP) (HostRoute, error) {
	output, err := exec.Command("/sbin/route", "-n", "get", destination.String()).CombinedOutput()
	if err != nil {
		if routeNotFound(output) {
			return HostRoute{}, nil
		}
		return HostRoute{}, fmt.Errorf("run /sbin/route: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return parseDarwinHostRoute(output, destination)
}

func parseDarwinHostRoute(output []byte, destination net.IP) (HostRoute, error) {
	route, err := parseHostRoute(output, destination)
	if err != nil {
		return HostRoute{}, err
	}
	route.LooksLikeTunnel = darwinRouteLooksLikeTunnel(route.Interface)
	return route, nil
}

func darwinRouteLooksLikeTunnel(interfaceName string) bool {
	return strings.HasPrefix(interfaceName, "utun")
}
