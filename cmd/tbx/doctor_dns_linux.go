//go:build linux

package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
)

const resolverBypassMessage = "systemd-resolved route-only domain is missing or being bypassed"

func checkSystemDNS(clusters []daemon.ClusterSummary, command commandOutput) error {
	// An unavailable resolved is not by itself the finding: the verdict is
	// whether the host resolves cluster names. So probe resolved for the
	// *reason*, then always run the lookups, and let their result decide
	// PASS versus WARN (#447).
	_, resolvedErr := command("resolvectl", "status")
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
	if len(problems) == 0 {
		// The names resolve, whichever resolver did it — a genuine PASS even
		// when resolved is absent.
		return nil
	}
	if resolvedErr != nil {
		// The manual step and fallback are phrased from the clusters this check
		// just probed, so the detail follows the probe rather than a second,
		// independent listing of cluster state.
		return optionalHostDNSError{detail: hostDNSUnavailableDetail(resolvedErr, probedClusters(clusters))}
	}
	return errors.New(strings.Join(problems, "; "))
}

// probedClusters restates the daemon's cluster summaries as the cluster records
// the resolved advice is phrased from. EffectiveDomain is already resolved on
// the summary, so carrying it as the explicit domain keeps the two identical.
func probedClusters(clusters []daemon.ClusterSummary) []cluster.Cluster {
	result := make([]cluster.Cluster, 0, len(clusters))
	for _, item := range clusters {
		result = append(result, cluster.Cluster{
			Name: item.Name, Domain: item.EffectiveDomain(), SubnetIndex: item.SubnetIndex,
		})
	}
	return result
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
