//go:build darwin

package main

import (
	"bufio"
	"errors"
	"fmt"
	"strings"
)

// platformRouteProbe reads routes with `route -n get`; vmnet hands a cluster
// either a `bridge*` (shared/bridged mode) or a `vmnet*` interface, and a
// host-local gateway resolves via `lo0`.
func platformRouteProbe(command commandOutput) routeProbe {
	return routeProbe{
		iface: func(ip string) (string, error) {
			output, err := command("/sbin/route", "-n", "get", ip)
			if err != nil {
				return "", err
			}
			return parseRouteInterface(output)
		},
		clusterIface: isClusterInterface,
		loopback:     "lo0",
	}
}

func parseRouteInterface(output []byte) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(scanner.Text()), ":")
		if ok && key == "interface" {
			fields := strings.Fields(value)
			if len(fields) != 0 {
				return fields[0], nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("parse route output: %w", err)
	}
	return "", errors.New("route output has no interface")
}

func isClusterInterface(iface string) bool {
	return strings.HasPrefix(iface, "bridge") || strings.HasPrefix(iface, "vmnet")
}
