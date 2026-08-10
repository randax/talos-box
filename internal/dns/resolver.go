package dns

import (
	"net"
	"strings"

	"github.com/randax/talos-box/internal/cluster"
)

// Resolve returns a node address or the ingress wildcard for a live cluster.
// Domains may nest across clusters; the longest matching suffix wins, so a
// name under the nested cluster's domain never falls through to the enclosing
// one.
func Resolve(name string, clusters []cluster.Cluster, lease func(mac string, subnetIndex int) string) net.IP {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	for _, item := range clusters {
		domain := strings.ToLower(item.EffectiveDomain())
		for _, node := range item.Nodes {
			if name == strings.ToLower(node.Name)+"."+domain {
				return net.ParseIP(lease(node.MAC, item.SubnetIndex)).To4()
			}
		}
	}

	bestIndex, bestSuffix := -1, ""
	for _, item := range clusters {
		suffix := "." + strings.ToLower(item.EffectiveDomain())
		if len(suffix) > len(bestSuffix) && strings.HasSuffix(name, suffix) {
			bestIndex, bestSuffix = item.SubnetIndex, suffix
		}
	}
	if bestIndex < 0 || bestIndex > cluster.MaxSubnetIndex {
		return nil
	}
	return net.IPv4(172, 30, byte(bestIndex), 200)
}
