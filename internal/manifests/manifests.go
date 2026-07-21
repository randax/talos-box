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
	BGP         bool // cluster is in BGP mode: render BGP policy, not L2
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

// ClusterASN is the BGP ASN for the cluster at the given subnet index.
func ClusterASN(subnetIndex int) int { return clusterASNBase + subnetIndex }

func (f Facts) hostIP(host int) string {
	return fmt.Sprintf("172.30.%d.%d", f.SubnetIndex, host)
}

// LBPool renders the CiliumLoadBalancerIPPool covering the cluster's static
// range (.200–.239, with .200 the conventional ingress VIP).
func LBPool(f Facts) string {
	return fmt.Sprintf(`apiVersion: cilium.io/v2
kind: CiliumLoadBalancerIPPool
metadata:
  name: %s-pool
spec:
  blocks:
    - start: %s
      stop: %s
`, f.Cluster, f.hostIP(200), f.hostIP(239))
}

// L2Policy renders the CiliumL2AnnouncementPolicy that makes LB VIPs reachable
// by having a node ARP-reply for them — the default (non-BGP) mechanism.
func L2Policy(f Facts) string {
	return fmt.Sprintf(`apiVersion: cilium.io/v2alpha1
kind: CiliumL2AnnouncementPolicy
metadata:
  name: %s-l2
spec:
  loadBalancerIPs: true
  nodeSelector: {}
`, f.Cluster)
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
`, f.Cluster, ClusterASN(f.SubnetIndex), f.hostIP(1), HostASN)
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

// All renders every document with comments naming the consuming tool and the
// per-tool section that pipes cleanly into it.
func All(f Facts) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Apply with kubectl once Cilium is installed — this section alone:\n#   tbx manifests %s k8s | kubectl apply -f -\n", f.Cluster)
	b.WriteString(k8sSection(f))
	b.WriteString("---\n")
	fmt.Fprintf(&b, "# Apply with talosctl (machine config patches, e.g. talosctl patch mc -p @file) — this section alone:\n#   tbx manifests %s talos\n", f.Cluster)
	b.WriteString(join(RegistryMirrors(f), BalloonModule(f)))
	return b.String()
}

// sections is the single registry driving Render, its error text, and the CLI
// usage string. "k8s" and "talos" group the documents by consuming tool.
var sections = map[string]func(Facts) string{
	"all":     All,
	"lb-pool": LBPool,
	"bgp":     BGPPolicy,
	"l2":      L2Policy,
	"mirrors": RegistryMirrors,
	"balloon": BalloonModule,
	"k8s":     k8sSection,
	"talos":   func(f Facts) string { return join(RegistryMirrors(f), BalloonModule(f)) },
}

// k8sSection renders the LB pool plus exactly ONE announcement mechanism —
// BGP when the cluster is in BGP mode, L2 otherwise — because the two are
// mutually exclusive (SPEC §5: BGP "replaces" L2). Applying this section
// switches the cluster's LB reachability to the current mode.
func k8sSection(f Facts) string {
	announce := L2Policy(f)
	note := "# LB reachability via L2 announcements (default mode).\n"
	if f.BGP {
		announce = BGPPolicy(f)
		note = "# LB reachability via BGP (this cluster has `tbx bgp enable`d).\n"
	}
	return note + join(LBPool(f), announce)
}

// Sections lists the valid section names in stable display order.
func Sections() []string {
	return []string{"lb-pool", "bgp", "l2", "mirrors", "balloon", "k8s", "talos", "all"}
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
