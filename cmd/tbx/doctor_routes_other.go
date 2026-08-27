//go:build !darwin && !linux

package main

import (
	"fmt"
	"runtime"
)

// platformRouteProbe has no host tool to ask on this platform. It reports that
// honestly instead of execing another platform's binary (#468).
func platformRouteProbe(_ commandOutput) routeProbe {
	return routeProbe{
		iface: func(string) (string, error) {
			return "", fmt.Errorf("route probing is not supported on %s", runtime.GOOS)
		},
		clusterIface: func(string) bool { return false },
	}
}
