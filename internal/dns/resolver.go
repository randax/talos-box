package dns

import (
	"net"
	"strings"

	"github.com/randax/talos-box/internal/cluster"
)

// Resolve returns a node address or the ingress wildcard for a live cluster.
// Domains may nest across clusters; the cluster with the longest matching
// domain suffix owns the name outright, so a name under the nested cluster's
// domain never falls through to the enclosing one — not even to its node
// records. The domain apex itself has no record.
func Resolve(name string, clusters []cluster.Cluster, lease func(mac string, subnetIndex int) string) net.IP {
	name = strings.ToLower(strings.TrimSuffix(name, "."))

	var owner *cluster.Cluster
	ownerDomain := ""
	for i, item := range clusters {
		domain := strings.ToLower(item.EffectiveDomain())
		if len(domain) <= len(ownerDomain) {
			continue
		}
		if name == domain || strings.HasSuffix(name, "."+domain) {
			owner, ownerDomain = &clusters[i], domain
		}
	}
	if owner == nil || name == ownerDomain {
		return nil
	}
	if owner.SubnetIndex < 0 || owner.SubnetIndex > cluster.MaxSubnetIndex {
		return nil
	}

	for _, node := range owner.Nodes {
		if name == strings.ToLower(node.Name)+"."+ownerDomain {
			return net.ParseIP(lease(node.MAC, owner.SubnetIndex)).To4()
		}
	}
	return net.IPv4(172, 30, byte(owner.SubnetIndex), 200)
}
