//go:build darwin

package main

import (
	"fmt"
	"strings"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hostport"
)

// doctorBGPPort is the port the host BGP speaker binds on each cluster gateway.
const doctorBGPPort = 179

func platformDoctorDependencies(deps *doctorDependencies) {
	if deps == nil {
		return
	}
	probe := deps.checkDirectDNS
	listClusters := deps.listClusters
	deps.checkDirectDNS = func() error {
		return directDNSDaemonSkip(probe, listClusters)
	}
	deps.platform = func() []doctorFinding {
		return darwinPlatformDoctorFindings(*deps)
	}
}

func darwinPlatformDoctorFindings(deps doctorDependencies) []doctorFinding {
	return []doctorFinding{darwinBGPPortFinding(doctorBGPPort, deps.listConfig, deps.command)}
}

// darwinBGPPortFinding reports a foreign listener sitting on the BGP port. It
// is macOS's counterpart to the Linux port-179 check, and it reads netstat
// rather than lsof deliberately: an unprivileged `lsof -iTCP:179` cannot see a
// root-owned socket, which is exactly what a squatter on this port tends to be
// (#359).
//
// Only an any-address listener is a finding. The speaker binds one cluster
// gateway each, so a gateway-bound listener is tbx's own; a wildcard one — the
// stray `nc -l 179` this check was filed for — sits in front of every gateway
// at once.
func darwinBGPPortFinding(port int, listConfig func() ([]cluster.Cluster, error), command commandOutput) doctorFinding {
	check := fmt.Sprintf("port-%d", port)
	if listConfig == nil || command == nil {
		return doctorFinding{level: "FAIL", check: check, detail: "port preflight is unavailable"}
	}
	clusters, err := listConfig()
	if err != nil {
		return doctorFinding{level: "FAIL", check: check, detail: fmt.Sprintf("list clusters: %v", err)}
	}
	if len(clusters) == 0 {
		return doctorFinding{level: "SKIP", check: check, detail: "no clusters exist"}
	}
	output, err := command("netstat", "-an", "-p", "tcp")
	if err != nil {
		return doctorFinding{level: "FAIL", check: check, detail: fmt.Sprintf("inspect listening sockets: %v", err)}
	}
	var squatters []string
	for _, listener := range hostport.ParseNetstatListeners(output, port) {
		if hostport.Wildcard(listener.Address) {
			squatters = append(squatters, listener.Line)
		}
	}
	if len(squatters) == 0 {
		return doctorFinding{level: "PASS", check: check}
	}
	return doctorFinding{
		level: "WARN",
		check: check,
		detail: fmt.Sprintf(
			"another process is listening on every address at port %d, ahead of the host BGP speaker's %s bind (%s); identify it with `%s` and stop it",
			port,
			strings.Join(darwinClusterGateways(clusters), ", "),
			strings.Join(squatters, "; "),
			darwinPortOwnerCommand(port),
		),
	}
}

func darwinClusterGateways(clusters []cluster.Cluster) []string {
	gateways := make([]string, 0, len(clusters))
	seen := make(map[string]bool, len(clusters))
	for _, item := range clusters {
		gateway := cluster.Gateway(item.SubnetIndex)
		if seen[gateway] {
			continue
		}
		seen[gateway] = true
		gateways = append(gateways, gateway)
	}
	return gateways
}

func darwinPortOwnerCommand(port int) string {
	return fmt.Sprintf("sudo lsof -nP -iTCP:%d -sTCP:LISTEN", port)
}
