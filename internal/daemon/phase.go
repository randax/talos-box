package daemon

import (
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/shellquote"
)

// Phase is a node's observed lifecycle state, derived without Talos credentials:
// both apid modes speak TLS, but maintenance mode presents the well-known
// "maintenance-service.talos.dev" certificate (verified empirically; a
// configured node presents a cluster-CA cert with the node's identity and
// additionally demands a client certificate).
type Phase string

const (
	PhaseStopped     Phase = "stopped"
	PhaseUnreachable Phase = "unreachable"
	PhaseMaintenance Phase = "maintenance"
	PhaseConfigured  Phase = "configured"
)

// ProbeResult is what one apid probe observed.
type ProbeResult struct {
	Dialed          bool // TCP connection to :50000 succeeded
	TLS             bool // TLS handshake completed (server presented a certificate)
	MaintenanceCert bool // the presented certificate is the maintenance-service identity
}

// maintenanceCN is the CommonName Talos maintenance mode presents.
const maintenanceCN = "maintenance-service.talos.dev"

// ClassifyPhase turns VM state plus a probe observation into a Phase.
func ClassifyPhase(vmRunning bool, probe ProbeResult) Phase {
	switch {
	case !vmRunning:
		return PhaseStopped
	case !probe.Dialed, !probe.TLS:
		return PhaseUnreachable
	case probe.MaintenanceCert:
		return PhaseMaintenance
	default:
		return PhaseConfigured
	}
}

// apidPort is Talos's machine API port.
const apidPort = "50000"

// probeAPID observes a node's apid: reachable? speaking TLS?
func probeAPID(ip string) ProbeResult {
	return probeHostPort(net.JoinHostPort(ip, apidPort))
}

func probeHostPort(address string) ProbeResult {
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		return ProbeResult{}
	}
	_ = conn.Close()
	tlsConn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: time.Second},
		"tcp", address,
		&tls.Config{InsecureSkipVerify: true}, //nolint:gosec // probing our own local VM
	)
	if err != nil {
		return ProbeResult{Dialed: true, TLS: false}
	}
	defer func() { _ = tlsConn.Close() }()
	certs := tlsConn.ConnectionState().PeerCertificates
	maintenance := len(certs) > 0 && certs[0].Subject.CommonName == maintenanceCN
	return ProbeResult{Dialed: true, TLS: true, MaintenanceCert: maintenance}
}

// Hints returns copy-pasteable next steps for a cluster, keyed on its nodes'
// phases. Hints describe; they never execute (SPEC §10).
func Hints(status ClusterStatus) []string {
	var stopped, unreachable, maintenance, configured []NodeStatus
	for _, node := range status.Nodes {
		switch node.Phase {
		case PhaseStopped:
			stopped = append(stopped, node)
		case PhaseUnreachable:
			unreachable = append(unreachable, node)
		case PhaseMaintenance:
			maintenance = append(maintenance, node)
		case PhaseConfigured:
			configured = append(configured, node)
		}
	}

	var hints []string
	// Capability gates hold whatever the cluster is doing: the config is
	// accepted and the extension baked, but this host cannot honour it.
	for _, capability := range status.Capabilities {
		if !capability.Supported {
			hints = append(hints, fmt.Sprintf("%s is unavailable on this host: %s", capability.Name, capability.Reason))
		}
	}
	if len(status.Nodes) > 0 && len(stopped) == len(status.Nodes) {
		return append(hints, fmt.Sprintf("cluster is stopped — start it with: tbx cluster start %s", status.Name))
	}
	if hint := storageHint(status); hint != "" {
		hints = append(hints, hint)
	}
	if hint := longhornSingleNodeHint(status); hint != "" {
		hints = append(hints, hint)
	}
	if status.CNI != "" && !status.KubernetesReady {
		hints = append(hints, fmt.Sprintf("%s provisioning is in progress; tbx will apply machine config, bootstrap, and reconcile the CNI. Rerun: tbx up;%s", status.CNI, credentialExports(status.Name)))
	}
	if len(maintenance) > 0 {
		first := maintenance[0]
		endpoint := status.controlPlaneOr(first)
		hints = append(hints,
			fmt.Sprintf("%d node(s) await machine config. Generate one: talosctl gen config %s https://%s:6443 --output-dir .",
				len(maintenance), status.Name, nodeHost(status, endpoint)),
			fmt.Sprintf("then apply it: talosctl apply-config --insecure --nodes %s --file controlplane.yaml (workers get worker.yaml)",
				first.IP),
		)
	}
	if len(configured) == len(status.Nodes) && len(status.Nodes) > 0 {
		cp := status.controlPlaneOr(status.Nodes[0])
		if status.CNI == cluster.CNIFlannel && status.LB && status.KubernetesReady {
			if status.VIPLive {
				hints = append(hints,
					fmt.Sprintf("Kubernetes is Ready; MetalLB L2 VIP is live at http://%s/. Flannel does not enforce NetworkPolicies; use cilium to exercise policies.%s", status.VIP, credentialExports(status.Name)),
				)
			} else {
				hints = append(hints,
					"Kubernetes is Ready; waiting for the MetalLB L2 LoadBalancer VIP probe to respond. Flannel does not enforce NetworkPolicies; use cilium to exercise policies."+credentialExports(status.Name),
				)
			}
		}
		if status.CNI == cluster.CNIFlannel && !status.LB && status.KubernetesReady {
			hints = append(hints,
				"Kubernetes is Ready with Talos-managed flannel; LoadBalancer support is disabled by lb: false, so no VIP is provisioned."+credentialExports(status.Name),
			)
		}
		if status.CNI == cluster.CNICilium && status.LB && status.KubernetesReady {
			if status.VIPLive {
				hints = append(hints, fmt.Sprintf("Kubernetes is Ready; Cilium LB-IPAM VIP is live at http://%s/.%s", status.VIP, credentialExports(status.Name)))
			} else {
				hints = append(hints, "Kubernetes is Ready; waiting for the Cilium LoadBalancer VIP probe to respond."+credentialExports(status.Name))
			}
		}
		if status.CNI == cluster.CNICilium && !status.LB && status.KubernetesReady {
			hints = append(hints, "Kubernetes is Ready with Cilium; LoadBalancer support is disabled by lb: false, so no VIP is provisioned."+credentialExports(status.Name))
		}
		if !status.KubernetesReady {
			hints = append(hints,
				fmt.Sprintf("all nodes configured. If etcd is not yet bootstrapped: talosctl bootstrap --talosconfig ./talosconfig --nodes %[1]s --endpoints %[1]s, then talosctl kubeconfig . --talosconfig ./talosconfig --nodes %[1]s --endpoints %[1]s", cp.IP),
				fmt.Sprintf("node TUI (the Talos dashboard): talosctl dashboard --talosconfig ./talosconfig --nodes %[1]s --endpoints %[1]s", cp.IP),
			)
		}
	}
	if len(unreachable) > 0 {
		hints = append(hints,
			fmt.Sprintf("%d node(s) not answering yet — boot takes ~1 minute; if it persists, run: tbx doctor", len(unreachable)),
		)
	}
	return hints
}

func credentialExports(name string) string {
	name = shellquote.Quote(name)
	return fmt.Sprintf(" export TALOSCONFIG=~/.talosbox/clusters/%s/talosconfig; export KUBECONFIG=~/.talosbox/clusters/%s/kubeconfig", name, name)
}

func storageHint(status ClusterStatus) string {
	if status.CSI == "" {
		return ""
	}
	switch status.StoragePhase {
	case StoragePhaseProvisioning:
		if status.StorageError != "" {
			return fmt.Sprintf("storage provisioning: CSI readiness probe failed: %s; retrying after backoff.", status.StorageError)
		}
		return "storage provisioning: waiting for the CSI readiness probe to pass."
	case StoragePhaseLive:
		return "storage live: the CSI readiness probe passed."
	default:
		return ""
	}
}

func longhornSingleNodeHint(status ClusterStatus) string {
	if status.CSI == cluster.CSILonghorn &&
		storageNodeCount(status) == 1 &&
		status.Running &&
		status.StoragePhase == StoragePhaseLive {
		return "Longhorn is running with a single replica on one node, so volumes have no redundancy."
	}
	return ""
}

// storageNodeCount mirrors the provisioning replica policy: replicas live on
// workers, or on the control planes of a worker-less cluster.
func storageNodeCount(status ClusterStatus) int {
	workers := 0
	for _, node := range status.Nodes {
		if node.Role == cluster.RoleWorker {
			workers++
		}
	}
	if workers == 0 {
		return len(status.Nodes)
	}
	return workers
}

// controlPlaneOr returns the cluster's first control-plane node, or fallback.
func (c ClusterStatus) controlPlaneOr(fallback NodeStatus) NodeStatus {
	for _, node := range c.Nodes {
		if node.Role == cluster.RoleControlPlane {
			return node
		}
	}
	return fallback
}

// nodeHost prefers the DNS name talosbox serves for a node.
func nodeHost(status ClusterStatus, node NodeStatus) string {
	domain := status.Domain
	if domain == "" {
		domain = status.Name + "." + cluster.DefaultDomainSuffix
	}
	return node.Name + "." + domain
}
