//go:build linux

package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/randax/talos-box/internal/daemon"
)

const resolverBypassMessage = "systemd-resolved route-only domain is missing or being bypassed"

func checkSystemDNS(clusters []daemon.ClusterSummary, command commandOutput) error {
	if _, err := command("resolvectl", "status"); err != nil {
		return optionalHostDNSError{detail: resolvedUnavailableDetail(err)}
	}
	var problems []string
	for _, item := range clusters {
		name := "doctor-probe." + item.EffectiveDomain()
		expected := net.ParseIP(fmt.Sprintf("172.30.%d.200", item.SubnetIndex))
		output, err := command("getent", "ahostsv4", name)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %s: lookup failed: %v", item.Name, resolverBypassMessage, err))
			continue
		}
		addresses := parseGetentAddresses(output)
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

func parseGetentAddresses(output []byte) []net.IP {
	var addresses []net.IP
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		if address := net.ParseIP(fields[0]); address != nil {
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
