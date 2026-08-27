package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
)

type commandOutput func(name string, args ...string) ([]byte, error)

// commandProbeTimeout bounds each diagnostic subprocess; system utilities can
// stall behind stuck directory services or security agents.
const commandProbeTimeout = 10 * time.Second

func execCombinedOutput(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandProbeTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if ctx.Err() != nil {
		return output, fmt.Errorf("%s timed out after %s", name, commandProbeTimeout)
	}
	return output, err
}

// routeProbe is the per-GOOS half of the route check: how this host names the
// interface an address routes through, which interface names belong to
// talosbox, and what the host calls its loopback. Doctor must never exec
// another platform's tools, so the probe is built by platformRouteProbe (#468).
type routeProbe struct {
	iface        func(ip string) (string, error)
	clusterIface func(string) bool
	loopback     string
}

// checkClusterRoutes verifies routes only for running clusters: a stopped
// cluster has no bridge interface, so its subnet legitimately resolves via the
// default route and would read as a false VPN/ZTNA capture.
func checkClusterRoutes(clusters []daemon.ClusterSummary, statuses []daemon.ClusterStatus, probe routeProbe) error {
	firstNodeIP := make(map[string]string, len(statuses))
	for _, status := range statuses {
		if !status.Running {
			continue
		}
		for _, node := range status.Nodes {
			// a stopped node's IP is a stale DHCP lease, not a live route target
			if node.IP != "" && !node.Phase.Stopped() {
				firstNodeIP[status.Name] = node.IP
				break
			}
		}
	}

	type routeTarget struct {
		ip      string
		localOK bool // the gateway is a host-local address; the host routes it via loopback
	}
	var problems []string
	for _, item := range clusters {
		if !item.Running {
			continue
		}
		targets := []routeTarget{{ip: cluster.Gateway(item.SubnetIndex), localOK: true}}
		if nodeIP := firstNodeIP[item.Name]; nodeIP != "" && nodeIP != targets[0].ip {
			targets = append(targets, routeTarget{ip: nodeIP})
		}
		for _, target := range targets {
			iface, err := probe.iface(target.ip)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s route to %s: %v", item.Name, target.ip, err))
				continue
			}
			if probe.clusterIface(iface) || (target.localOK && iface == probe.loopback) {
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"%s route to %s exits via %s; a VPN/ZTNA client has captured the cluster subnet",
				item.Name, target.ip, iface))
		}
	}
	if len(problems) != 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}
