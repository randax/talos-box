package main

import (
	"bufio"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
)

type commandOutput func(name string, args ...string) ([]byte, error)

func execCombinedOutput(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

func checkClusterRoutes(clusters []daemon.ClusterSummary, statuses []daemon.ClusterStatus, command commandOutput) error {
	firstNodeIP := make(map[string]string, len(statuses))
	for _, status := range statuses {
		for _, node := range status.Nodes {
			if node.IP != "" {
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
