//go:build linux

package main

import (
	"errors"
	"strings"

	"github.com/randax/talos-box/internal/cluster"
)

// linuxClusterInterfacePrefix is what every talosbox bridge is named after;
// cluster.LinuxBridgeName appends the subnet index to it.
var linuxClusterInterfacePrefix = strings.TrimSuffix(cluster.LinuxBridgeName(0), "0")

// platformRouteProbe reads routes with `ip -o route get`: `/sbin/route` is a
// macOS binary and execing it here was the whole of #468.1. Cluster traffic
// leaves through `br-tbx<n>`, and a host-local gateway resolves via `lo`.
func platformRouteProbe(command commandOutput) routeProbe {
	return routeProbe{
		iface: func(ip string) (string, error) {
			output, err := command("ip", "-o", "route", "get", ip)
			if err != nil {
				return "", err
			}
			return parseIPRouteInterface(output)
		},
		clusterIface: func(iface string) bool {
			return strings.HasPrefix(iface, linuxClusterInterfacePrefix)
		},
		loopback: "lo",
	}
}

// parseIPRouteInterface pulls the outgoing device out of an `ip -o route get`
// line: "172.30.3.2 dev br-tbx3 src ..." and "local 172.30.3.1 dev lo src ..."
// both name it after the `dev` token.
func parseIPRouteInterface(output []byte) (string, error) {
	fields := strings.Fields(string(output))
	for i, field := range fields {
		if field == "dev" && i+1 < len(fields) {
			return fields[i+1], nil
		}
	}
	return "", errors.New("ip route output has no dev")
}
