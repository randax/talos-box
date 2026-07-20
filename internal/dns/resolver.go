package dns

import (
	"net"
	"strings"

	"github.com/randax/talos-box/internal/cluster"
)

// Resolve returns a node address or the ingress wildcard for a live cluster.
func Resolve(name string, clusters []cluster.Cluster, lease func(mac string, subnetIndex int) string) net.IP {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	for _, item := range clusters {
		for _, node := range item.Nodes {
			fqdn := strings.ToLower(node.Name + "." + item.Name + ".k8s.test")
			if name == fqdn {
				return net.ParseIP(lease(node.MAC, item.SubnetIndex)).To4()
			}
		}
	}

	bestIndex, bestSuffix := -1, ""
	for _, item := range clusters {
		suffix := "." + strings.ToLower(item.Name) + ".k8s.test"
		if len(suffix) > len(bestSuffix) && strings.HasSuffix(name, suffix) && len(name) > len(suffix) {
			bestIndex, bestSuffix = item.SubnetIndex, suffix
		}
	}
	if bestIndex < 0 || bestIndex > cluster.MaxSubnetIndex {
		return nil
	}
	return net.IPv4(172, 30, byte(bestIndex), 200)
}
