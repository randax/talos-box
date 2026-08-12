// Package manifests renders curated CNI values, ready-to-apply resources, and
// Talos machine-config patches that match a cluster's networking.
package manifests

import (
	"fmt"
	"strings"
)

// Facts are the cluster values the documents are rendered against.
type Facts struct {
	Cluster     string
	SubnetIndex int
	CNI         string
	LB          bool
	BGP         bool // cluster is in BGP mode: configure BGP, not L2 announcements
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

// CatchAllPort serves the Talos "*" mirror entry.
const CatchAllPort = 5059

const (
	// HostASN is the "top of rack" ASN the host's BGP speaker uses (SPEC §5).
	HostASN = 64512
	// clusterASNBase + subnet index = the cluster's ASN.
	clusterASNBase = 64600
	// The LB pool has 40 addresses and Cilium renews L2 leases every 5 seconds:
	// 40 * (1 / 5s) = 8 QPS. Do not undercut Cilium 1.19.6's 10/20 defaults.
	ciliumClientQPS   = 10
	ciliumClientBurst = 20
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

// CiliumValues renders the curated, Talos-safe Cilium value surface. The
// ingress controller deliberately remains attendee-owned: talosbox proves the
// LoadBalancer path with its own probe instead of claiming the workshop VIP.
func CiliumValues(f Facts) string {
	// The manual Cilium values surface retains its historic L2 default. The
	// provisioner names Cilium explicitly and turns L2 off with lb: false.
	l2Enabled := !f.BGP && (f.CNI != "cilium" || f.LB)
	return fmt.Sprintf(`ipam:
  mode: kubernetes
kubeProxyReplacement: true
k8sServiceHost: localhost
k8sServicePort: 7445
securityContext:
  capabilities:
    ciliumAgent:
      - CHOWN
      - KILL
      - NET_ADMIN
      - NET_RAW
      - IPC_LOCK
      - SYS_ADMIN
      - SYS_RESOURCE
      - DAC_OVERRIDE
      - FOWNER
      - SETGID
      - SETUID
    cleanCiliumState:
      - NET_ADMIN
      - SYS_ADMIN
      - SYS_RESOURCE
cgroup:
  autoMount:
    enabled: false
  hostRoot: /sys/fs/cgroup
bpf:
  hostLegacyRouting: true
l2announcements:
  enabled: %t
bgpControlPlane:
  enabled: %t
k8sClientRateLimit:
  qps: %d
  burst: %d
ingressController:
  enabled: false
hubble:
  enabled: false
  relay:
    enabled: false
  ui:
    enabled: false
`, l2Enabled, f.BGP, ciliumClientQPS, ciliumClientBurst)
}

// BGPPolicy renders Cilium's BGP v2 resources for "host as ToR": every node
// peers eBGP with the host gateway and advertises LoadBalancer Service IPs.
func BGPPolicy(f Facts) string {
	return fmt.Sprintf(`apiVersion: cilium.io/v2
kind: CiliumBGPClusterConfig
metadata:
  name: %s-bgp
spec:
  nodeSelector: {}
  bgpInstances:
    - name: %s-bgp
      localASN: %d
      peers:
        - name: host-gateway
          peerASN: %d
          peerAddress: %s
          peerConfigRef:
            name: %s-bgp-peer
---
apiVersion: cilium.io/v2
kind: CiliumBGPPeerConfig
metadata:
  name: %s-bgp-peer
spec:
  families:
    - afi: ipv4
      safi: unicast
      advertisements:
        matchLabels:
          talosbox.dev/advertisement: service-load-balancer
---
apiVersion: cilium.io/v2
kind: CiliumBGPAdvertisement
metadata:
  name: %s-bgp-advertisement
  labels:
    talosbox.dev/advertisement: service-load-balancer
spec:
  advertisements:
    - advertisementType: Service
      service:
        addresses:
          - LoadBalancerIP
      selector:
        matchExpressions:
          - key: talosbox.dev/never-used
            operator: NotIn
            values:
              - never
`, f.Cluster, f.Cluster, ClusterASN(f.SubnetIndex), HostASN, f.hostIP(1), f.Cluster, f.Cluster, f.Cluster)
}

// RegistryMirrors renders the Talos machine-config patch pointing every
// upstream registry at the host-side pull-through mirrors.
func RegistryMirrors(f Facts) string {
	return fmt.Sprintf(`machine:
  registries:
    mirrors:
      "*":
        endpoints:
          - http://%s:%d
        skipFallback: true
`, f.hostIP(1), CatchAllPort)
}

// MetalLBValues disables every BGP backend. The flannel path is deliberately
// L2-only: speakers still use the existing catch-all Talos registry mirror for
// their quay.io images, while the chart itself is rendered by tbx on the host.
func MetalLBValues(Facts) string {
	return `speaker:
  frr:
    enabled: false
frrk8s:
  enabled: false
`
}

// MetalLBExtras renders the L2-only address pool for a flannel cluster. The
// explicit selector prevents a future attendee-created pool from being
// accidentally announced by talosbox's L2Advertisement.
func MetalLBExtras(f Facts) string {
	return fmt.Sprintf(`apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: %s-pool
  namespace: metallb-system
  labels:
    talosbox.dev/managed: "true"
spec:
  addresses:
    - %s-%s
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: %s-l2
  namespace: metallb-system
  labels:
    talosbox.dev/managed: "true"
spec:
  ipAddressPools:
    - %s-pool
`, f.Cluster, f.hostIP(200), f.hostIP(239), f.Cluster, f.Cluster)
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
	fmt.Fprintf(&b, "# Pass to Helm when installing Cilium — save this section alone:\n#   tbx manifests %s cilium-values > cilium-values.yaml\n", f.Cluster)
	b.WriteString(CiliumValues(f))
	b.WriteString("---\n")
	fmt.Fprintf(&b, "# Apply with talosctl (machine config patches, e.g. talosctl patch mc -p @file) — this section alone:\n#   tbx manifests %s talos\n", f.Cluster)
	b.WriteString(join(RegistryMirrors(f), BalloonModule(f)))
	return b.String()
}

// sections is the single registry driving Render, its error text, and the CLI
// usage string. Grouped sections keep output consumable by one tool.
var sections = map[string]func(Facts) string{
	"all":            All,
	"lb-pool":        LBPool,
	"bgp":            BGPPolicy,
	"l2":             L2Policy,
	"cilium-values":  CiliumValues,
	"mirrors":        RegistryMirrors,
	"metallb-values": MetalLBValues,
	"metallb-extras": MetalLBExtras,
	"balloon":        BalloonModule,
	"k8s":            k8sSection,
	"talos":          func(f Facts) string { return join(RegistryMirrors(f), BalloonModule(f)) },
}

// k8sSection renders the LB pool plus exactly ONE announcement mechanism —
// BGP when the cluster is in BGP mode, L2 otherwise — because the two are
// mutually exclusive (SPEC §5: BGP "replaces" L2). Callers switching a live
// cluster must remove the previously applied policy; kubectl apply does not
// prune objects omitted from this output.
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
	return []string{"lb-pool", "bgp", "l2", "cilium-values", "mirrors", "balloon", "metallb-values", "metallb-extras", "k8s", "talos", "all"}
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
