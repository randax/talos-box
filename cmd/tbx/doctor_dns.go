//go:build darwin

package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/resolverset"
)

const resolverBypassMessage = "scoped resolver is being bypassed (DNS filtering agent or browser/system DoH)"

func checkSystemDNS(clusters []daemon.ClusterSummary, command commandOutput) error {
	problems := customDomainResolverProblems(clusters)
	for _, item := range clusters {
		name := "doctor-probe." + item.EffectiveDomain()
		expected := net.ParseIP(fmt.Sprintf("172.30.%d.200", item.SubnetIndex))

		// dscacheutil goes through macOS SystemConfiguration and therefore exercises
		// scoped /etc/resolver domains directly. That is more reliable here than
		// depending on whether this Go build selects cgo getaddrinfo at runtime.
		output, err := command("/usr/bin/dscacheutil", "-q", "host", "-a", "name", name)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %s: lookup failed: %v", item.Name, resolverBypassMessage, err))
			continue
		}
		addresses := parseDSCacheAddresses(output)
		matched := false
		for _, address := range addresses {
			if address.Equal(expected) {
				matched = true
				break
			}
		}
		if !matched {
			problems = append(problems, fmt.Sprintf("%s: %s: %s resolved to %s, want %s",
				item.Name, resolverBypassMessage, name, formatAddresses(addresses), expected))
		}
	}
	if len(problems) != 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// customDomainResolverProblems diagnoses the per-domain resolver files:
// a missing or drifted file for a custom-domain cluster, or an unmanaged
// (unmarked) file squatting on a cluster's domain, which talosbox reports
// but never modifies.
func customDomainResolverProblems(clusters []daemon.ClusterSummary) []string {
	var problems []string
	for _, item := range clusters {
		if item.Domain == "" {
			continue
		}
		path := filepath.Join(filepath.Dir(resolverPath), item.Domain)
		info, err := os.Lstat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			problems = append(problems, fmt.Sprintf("%s: resolver file %s is missing; tbxd re-creates it within a minute if the helper is healthy", item.Name, path))
			continue
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s: resolver file %s is unreadable: %v", item.Name, path, err))
			continue
		case !info.Mode().IsRegular():
			// Reading a FIFO would hang doctor; a symlink would hide what the
			// helper (correctly) refuses to touch.
			problems = append(problems, fmt.Sprintf("%s: resolver path %s is not a regular file; talosbox will not touch it — remove it manually", item.Name, path))
			continue
		}
		content, err := os.ReadFile(path)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s: resolver file %s is unreadable: %v", item.Name, path, err))
		case !resolverset.Managed(content):
			problems = append(problems, fmt.Sprintf("%s: resolver file %s exists but is not managed by talosbox; it will not be touched — remove or fix it manually", item.Name, path))
		}
	}
	return problems
}

func parseDSCacheAddresses(output []byte) []net.IP {
	var addresses []net.IP
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(scanner.Text()), ":")
		if !ok || (key != "ip_address" && key != "ipv6_address") {
			continue
		}
		if address := net.ParseIP(strings.TrimSpace(value)); address != nil {
			addresses = append(addresses, address)
		}
	}
	return addresses
}

func formatAddresses(addresses []net.IP) string {
	if len(addresses) == 0 {
		return "no addresses"
	}
	values := make([]string, 0, len(addresses))
	for _, address := range addresses {
		values = append(values, address.String())
	}
	return strings.Join(values, ", ")
}
