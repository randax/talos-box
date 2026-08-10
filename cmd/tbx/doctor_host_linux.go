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

func checkResolver() error {
	if _, err := execCombinedOutput("resolvectl", "status"); err != nil {
		return optionalHostDNSError{detail: resolvedUnavailableDetail(err)}
	}
	return nil
}

func checkPlatformDirectDNS() error {
	clusters, err := cluster.List()
	if err != nil {
		return fmt.Errorf("list clusters: %w", err)
	}
	var result error
	for _, item := range clusters {
		address := net.JoinHostPort(cluster.Gateway(item.SubnetIndex), "53")
		if err := tbxdns.Probe(address); err != nil {
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

func resolvedUnavailableDetail(cause error) string {
	clusters, err := cluster.List()
	if err != nil || len(clusters) == 0 {
		return fmt.Sprintf("systemd-resolved is unavailable (%v); host cluster names require resolved, while guests and by-IP access remain available", cause)
	}
	return fmt.Sprintf(
		"systemd-resolved is unavailable (%v); guests and by-IP access remain available; manual step: %s",
		cause, strings.Join(resolvedManualSteps(clusters), "; "),
	)
}

func resolvedManualSteps(clusters []cluster.Cluster) []string {
	steps := make([]string, 0, len(clusters))
	for _, item := range clusters {
		bridge := cluster.LinuxBridgeName(item.SubnetIndex)
		steps = append(steps, fmt.Sprintf(
			"sudo resolvectl dns %s %s && sudo resolvectl domain %s %q",
			bridge, cluster.Gateway(item.SubnetIndex), bridge, "~"+item.Name+".k8s.test",
		))
	}
	return steps
}
