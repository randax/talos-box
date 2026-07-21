package main

import (
	"bufio"
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

// checkClusterRoutes verifies routes only for running clusters: a stopped
// cluster has no bridge/vmnet interface, so its subnet legitimately resolves
// via the default route and would read as a false VPN/ZTNA capture.
func checkClusterRoutes(clusters []daemon.ClusterSummary, statuses []daemon.ClusterStatus, command commandOutput) error {
	firstNodeIP := make(map[string]string, len(statuses))
	for _, status := range statuses {
		if !status.Running {
			continue
		}
		for _, node := range status.Nodes {
			// a stopped node's IP is a stale DHCP lease, not a live route target
			if node.IP != "" && node.Phase != daemon.PhaseStopped {
				firstNodeIP[status.Name] = node.IP
				break
			}
		}
	}

	type routeTarget struct {
		ip      string
		localOK bool // the gateway is a host-local address; macOS routes it via lo0
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
			output, err := command("/sbin/route", "-n", "get", target.ip)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s route to %s: %v", item.Name, target.ip, err))
				continue
			}
			iface, err := parseRouteInterface(output)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s route to %s: %v", item.Name, target.ip, err))
				continue
			}
			if isClusterInterface(iface) || (target.localOK && iface == "lo0") {
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
