//go:build linux

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/randax/talos-box/internal/cluster"
	tbxdns "github.com/randax/talos-box/internal/dns"
)

type optionalHostDNSError struct{ detail string }

func (e optionalHostDNSError) Error() string { return e.detail }

func checkPlatformDirectDNS() error {
	return checkLinuxDirectDNS(cluster.List, os.ReadFile, tbxdns.Probe)
}

func checkLinuxDirectDNS(
	listClusters func() ([]cluster.Cluster, error),
	readFile func(string) ([]byte, error),
	probe func(string) error,
) error {
	clusters, err := listClusters()
	if err != nil {
		return fmt.Errorf("list clusters: %w", err)
	}
	if len(clusters) == 0 {
		// Nothing was probed, so there is nothing to pass: a PASS here reads as
		// "cluster names resolve", which is exactly the claim doctor must not
		// make without evidence (#447).
		return skippedDoctorCheck{detail: "no clusters are running"}
	}
	var result error
	for _, item := range clusters {
		bridge := cluster.LinuxBridgeName(item.SubnetIndex)
		if _, err := readFile("/sys/class/net/" + bridge + "/ifindex"); err != nil {
			result = errors.Join(result, fmt.Errorf("%s: inspect %s: %w", item.Name, bridge, err))
			continue
		}
		address := net.JoinHostPort(cluster.Gateway(item.SubnetIndex), "53")
		if err := probe(address); err != nil {
			result = errors.Join(result, fmt.Errorf("%s (%s): %w", item.Name, address, err))
		}
	}
	return result
}

func checkForwarding() error {
	content, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(content)) != "1" {
		return fmt.Errorf("net.ipv4.ip_forward is %q, want 1", strings.TrimSpace(string(content)))
	}
	return nil
}

func classifyResolverFailure(err error) (string, string) {
	var optional optionalHostDNSError
	if errors.As(err, &optional) {
		return "WARN", optional.Error()
	}
	return "FAIL", err.Error()
}

func classifySystemDNSFailure(err error) (string, string) {
	var optional optionalHostDNSError
	if errors.As(err, &optional) {
		return "WARN", optional.Error()
	}
	return "FAIL", err.Error()
}
