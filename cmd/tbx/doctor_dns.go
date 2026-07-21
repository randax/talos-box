package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/randax/talos-box/internal/daemon"
)

const resolverBypassMessage = "scoped resolver is being bypassed (DNS filtering agent or browser/system DoH)"

func checkSystemDNS(clusters []daemon.ClusterSummary, command commandOutput) error {
	var problems []string
	for _, item := range clusters {
		name := fmt.Sprintf("doctor-probe.%s.k8s.test", item.Name)
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
