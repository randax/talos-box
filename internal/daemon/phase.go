package daemon

import (
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"github.com/randax/talos-box/internal/cluster"
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
	if len(status.Nodes) > 0 && len(stopped) == len(status.Nodes) {
		return []string{fmt.Sprintf("cluster is stopped — start it with: tbx cluster start %s", status.Name)}
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
		hints = append(hints,
			fmt.Sprintf("all nodes configured. If etcd is not yet bootstrapped: talosctl --nodes %s bootstrap, then talosctl kubeconfig .", cp.IP),
			fmt.Sprintf("node TUI (the Talos dashboard): talosctl dashboard --nodes %s", cp.IP),
		)
	}
	if len(unreachable) > 0 {
		hints = append(hints,
			fmt.Sprintf("%d node(s) not answering yet — boot takes ~1 minute; if it persists, run: tbx doctor", len(unreachable)),
		)
	}
	return hints
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
	return fmt.Sprintf("%s.%s.k8s.test", node.Name, status.Name)
}
