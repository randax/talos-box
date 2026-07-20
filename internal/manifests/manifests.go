// Package manifests renders the ready-to-apply Cilium resources and Talos
// machine-config patches that match a cluster's networking (SPEC §5, §10).
// talosbox prints these; applying them is the attendees' work.
package manifests

import (
	"fmt"
	"strings"
)

// Facts are the cluster values the documents are rendered against.
type Facts struct {
	Cluster     string
	SubnetIndex int
}

// MirrorPorts fixes the upstream registry → host mirror port mapping.
// The mirror implementation (slice #34) must serve exactly this layout.
var MirrorPorts = []struct {
	Upstream string
	Port     int
}{
	{"docker.io", 5055},
	{"ghcr.io", 5056},
	{"quay.io", 5057},
	{"registry.k8s.io", 5058},
}

const (
	// HostASN is the "top of rack" ASN the host's BGP speaker uses (SPEC §5).
	HostASN = 64512
	// clusterASNBase + subnet index = the cluster's ASN.
	clusterASNBase = 64600
)

func (f Facts) hostIP(host int) string {
	return fmt.Sprintf("172.30.%d.%d", f.SubnetIndex, host)
}

// LBPool renders the CiliumLoadBalancerIPPool covering the cluster's static
// range (.200–.239, with .200 the conventional ingress VIP).
func LBPool(f Facts) string {
	return fmt.Sprintf(`apiVersion: cilium.io/v2alpha1
kind: CiliumLoadBalancerIPPool
metadata:
  name: %s-pool
spec:
  blocks:
    - start: %s
      stop: %s
`, f.Cluster, f.hostIP(200), f.hostIP(239))
}

// BGPPolicy renders the CiliumBGPPeeringPolicy for "host as ToR": every node
// peers eBGP with the host gateway. (The BGP milestone validates this against
// a live Cilium and may bump the API version.)
func BGPPolicy(f Facts) string {
	return fmt.Sprintf(`apiVersion: cilium.io/v2alpha1
kind: CiliumBGPPeeringPolicy
metadata:
  name: %s-bgp
spec:
  nodeSelector: {}
  virtualRouters:
    - localASN: %d
      exportPodCIDR: false
      serviceSelector: {}
      neighbors:
        - peerAddress: %s/32
          peerASN: %d
`, f.Cluster, clusterASNBase+f.SubnetIndex, f.hostIP(1), HostASN)
}

// RegistryMirrors renders the Talos machine-config patch pointing every
// upstream registry at the host-side pull-through mirrors.
func RegistryMirrors(f Facts) string {
	var b strings.Builder
	b.WriteString("machine:\n  registries:\n    mirrors:\n")
	for _, m := range MirrorPorts {
		fmt.Fprintf(&b, "      %s:\n        endpoints:\n          - http://%s:%d\n", m.Upstream, f.hostIP(1), m.Port)
	}
	return b.String()
}

// BalloonModule renders the Talos machine-config patch loading virtio_balloon,
// required for tbxd's memory ballooning (SPEC §8).
func BalloonModule(Facts) string {
	return `machine:
  kernel:
    modules:
      - name: virtio_balloon
`
}

// All renders every document with comments naming the consuming tool.
func All(f Facts) string {
	var b strings.Builder
	b.WriteString("# Apply with kubectl (once Cilium is installed):\n")
	b.WriteString(join(LBPool(f), BGPPolicy(f)))
	b.WriteString("---\n")
	b.WriteString("# Apply with talosctl (machine config patches, e.g. talosctl patch mc -p @file):\n")
	b.WriteString(join(RegistryMirrors(f), BalloonModule(f)))
	return b.String()
}

// sections is the single registry driving Render, its error text, and the CLI
// usage string. "k8s" and "talos" group the documents by consuming tool.
var sections = map[string]func(Facts) string{
	"all":     All,
	"lb-pool": LBPool,
	"bgp":     BGPPolicy,
	"mirrors": RegistryMirrors,
	"balloon": BalloonModule,
	"k8s":     func(f Facts) string { return join(LBPool(f), BGPPolicy(f)) },
	"talos":   func(f Facts) string { return join(RegistryMirrors(f), BalloonModule(f)) },
}

// Sections lists the valid section names in stable display order.
func Sections() []string {
	return []string{"lb-pool", "bgp", "mirrors", "balloon", "k8s", "talos", "all"}
}

func join(docs ...string) string {
	return strings.Join(docs, "---\n")
}

// Render returns one named section, or an error naming the valid ones.
func Render(f Facts, section string) (string, error) {
	if section == "" {
		section = "all"
	}
	render, ok := sections[section]
	if !ok {
		return "", fmt.Errorf("unknown section %q (use %s)", section, strings.Join(Sections(), ", "))
	}
	return render(f), nil
}
