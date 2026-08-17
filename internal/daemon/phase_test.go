package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
)

func TestClassifyPhase(t *testing.T) {
	tests := []struct {
		name    string
		running bool
		probe   ProbeResult
		want    Phase
	}{
		{"vm stopped", false, ProbeResult{}, PhaseStopped},
		{"running, no answer on apid", true, ProbeResult{Dialed: false}, PhaseUnreachable},
		{"running, apid not speaking TLS", true, ProbeResult{Dialed: true, TLS: false}, PhaseUnreachable},
		{"running, cluster-CA cert", true, ProbeResult{Dialed: true, TLS: true}, PhaseConfigured},
		{"running, maintenance cert", true, ProbeResult{Dialed: true, TLS: true, MaintenanceCert: true}, PhaseMaintenance},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyPhase(tt.running, tt.probe); got != tt.want {
				t.Errorf("ClassifyPhase(%v, %+v) = %q, want %q", tt.running, tt.probe, got, tt.want)
			}
		})
	}
}

func TestHints(t *testing.T) {
	base := ClusterStatus{Name: "demo", Subnet: "172.30.0.0/24"}
	node := func(name string, phase Phase) NodeStatus {
		return NodeStatus{Name: name, Phase: phase, IP: "172.30.0.2"}
	}
	tests := []struct {
		name  string
		nodes []NodeStatus
		want  []string // substrings that must each appear in exactly the hint list
	}{
		{
			name:  "maintenance node suggests config workflow",
			nodes: []NodeStatus{node("demo-cp-1", PhaseMaintenance)},
			want:  []string{"talosctl gen config", "apply-config --insecure"},
		},
		{
			name:  "all configured suggests bootstrap and the dashboard",
			nodes: []NodeStatus{node("demo-cp-1", PhaseConfigured)},
			want: []string{
				"talosctl bootstrap --talosconfig ./talosconfig --nodes 172.30.0.2 --endpoints 172.30.0.2",
				"talosctl kubeconfig . --talosconfig ./talosconfig --nodes 172.30.0.2 --endpoints 172.30.0.2",
				"talosctl dashboard --talosconfig ./talosconfig --nodes 172.30.0.2 --endpoints 172.30.0.2",
			},
		},
		{
			name:  "stopped cluster suggests start",
			nodes: []NodeStatus{node("demo-cp-1", PhaseStopped)},
			want:  []string{"tbx cluster start demo"},
		},
		{
			name:  "unreachable suggests patience then doctor",
			nodes: []NodeStatus{node("demo-cp-1", PhaseUnreachable)},
			want:  []string{"tbx doctor"},
		},
		{
			name: "mixed phases yield maintenance hint, not bootstrap",
			nodes: []NodeStatus{
				node("demo-cp-1", PhaseConfigured),
				node("demo-worker-1", PhaseMaintenance),
			},
			want: []string{"apply-config --insecure"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := base
			status.Nodes = tt.nodes
			hints := Hints(status)
			for _, substr := range tt.want {
				found := false
				for _, h := range hints {
					if strings.Contains(h, substr) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("hints %q missing substring %q", hints, substr)
				}
			}
		})
	}
	// the gen-config endpoint must name a control plane, not the maintenance worker
	status2 := base
	status2.Nodes = []NodeStatus{
		{Name: "demo-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2"},
		{Name: "demo-worker-1", Role: cluster.RoleWorker, Phase: PhaseMaintenance, IP: "172.30.0.3"},
	}
	genHintFound := false
	for _, h := range Hints(status2) {
		if strings.Contains(h, "gen config") {
			genHintFound = true
			if !strings.Contains(h, "demo-cp-1.demo.k8s.test") {
				t.Errorf("gen config hint should use the control-plane endpoint, got %q", h)
			}
		}
	}
	if !genHintFound {
		t.Error("expected a gen config hint for the maintenance worker")
	}

	// bootstrap hint must NOT appear while any node is in maintenance
	status := base
	status.Nodes = []NodeStatus{node("a", PhaseConfigured), node("b", PhaseMaintenance)}
	for _, h := range Hints(status) {
		if strings.Contains(h, "bootstrap") {
			t.Errorf("bootstrap hint offered while a node is still in maintenance: %q", h)
		}
	}
}

func TestHintsDescribeFlannelReadyWithoutLoadBalancer(t *testing.T) {
	status := ClusterStatus{
		Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel}, KubernetesReady: true,
		Nodes: []NodeStatus{{Name: "demo-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2"}},
	}
	hints := Hints(status)
	joined := strings.Join(hints, "\n")
	for _, wanted := range []string{"Ready", "lb: false", "disabled", "TALOSCONFIG=~/.talosbox/clusters/demo/talosconfig", "KUBECONFIG=~/.talosbox/clusters/demo/kubeconfig"} {
		if !strings.Contains(joined, wanted) {
			t.Fatalf("hints missing %q:\n%s", wanted, joined)
		}
	}
}

func TestHintsAccumulateStorageProvisioningAndFlannelReadyWithoutLoadBalancer(t *testing.T) {
	status := ClusterStatus{
		Name:               "demo",
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILocalPath},
		KubernetesReady:    true,
		StoragePhase:       StoragePhaseProvisioning,
		Nodes: []NodeStatus{{
			Name: "demo-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2",
		}},
	}

	hints := Hints(status)
	joined := strings.Join(hints, "\n")
	for _, wanted := range []string{
		"storage provisioning",
		"waiting for the CSI readiness probe to pass",
		"Kubernetes is Ready with Talos-managed flannel",
	} {
		if !strings.Contains(joined, wanted) {
			t.Fatalf("hints missing %q:\n%s", wanted, joined)
		}
	}
}

func TestHintsDescribeLiveFlannelMetalLBVIPAndPolicyLimit(t *testing.T) {
	status := ClusterStatus{
		Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true}, KubernetesReady: true,
		VIP: "172.30.4.200", VIPLive: true,
		Nodes: []NodeStatus{{Role: cluster.RoleControlPlane, Phase: PhaseConfigured}},
	}
	hints := Hints(status)
	if len(hints) != 1 || !strings.Contains(hints[0], "http://172.30.4.200/") || !strings.Contains(hints[0], "does not enforce NetworkPolicies") {
		t.Fatalf("hints = %v", hints)
	}
}

func TestHintsDescribeStorageLive(t *testing.T) {
	status := ClusterStatus{
		Name:               "demo",
		ProvisioningIntent: cluster.ProvisioningIntent{CSI: cluster.CSILocalPath},
		StoragePhase:       StoragePhaseLive,
	}

	hints := Hints(status)
	if len(hints) != 1 || !strings.Contains(hints[0], "storage live") || !strings.Contains(hints[0], "CSI readiness probe passed") {
		t.Fatalf("hints = %v", hints)
	}
}

func TestHintsKeepSingleNodeLonghornWarningAlongsideNetworkingHints(t *testing.T) {
	status := ClusterStatus{
		Name:               "demo",
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILonghorn, LB: true},
		Running:            true,
		KubernetesReady:    true,
		StoragePhase:       StoragePhaseLive,
		VIP:                "172.30.0.200",
		VIPLive:            true,
		Nodes: []NodeStatus{{
			Name: "demo-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2",
		}},
	}

	joined := strings.Join(Hints(status), "\n")
	for _, wanted := range []string{
		"storage live",
		"no redundancy",
		"http://172.30.0.200/",
	} {
		if !strings.Contains(joined, wanted) {
			t.Fatalf("hints missing %q:\n%s", wanted, joined)
		}
	}
}

func TestHintsReportLonghornRedundancyByStorageNodeCount(t *testing.T) {
	controlPlane := NodeStatus{Name: "demo-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2"}
	worker1 := NodeStatus{Name: "demo-worker-1", Role: cluster.RoleWorker, Phase: PhaseConfigured, IP: "172.30.0.3"}
	worker2 := NodeStatus{Name: "demo-worker-2", Role: cluster.RoleWorker, Phase: PhaseConfigured, IP: "172.30.0.4"}
	tests := []struct {
		name  string
		nodes []NodeStatus
		want  bool
	}{
		{name: "one worker holds the only replica", nodes: []NodeStatus{controlPlane, worker1}, want: true},
		{name: "two workers replicate", nodes: []NodeStatus{controlPlane, worker1, worker2}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := ClusterStatus{
				Name:               "demo",
				ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILonghorn},
				Running:            true,
				KubernetesReady:    true,
				StoragePhase:       StoragePhaseLive,
				Nodes:              tt.nodes,
			}
			got := strings.Contains(strings.Join(Hints(status), "\n"), "no redundancy")
			if got != tt.want {
				t.Fatalf("no-redundancy hint present = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHintsSuppressSingleNodeLonghornWarningUntilStorageIsLive(t *testing.T) {
	node := NodeStatus{
		Name: "demo-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2",
	}
	tests := []struct {
		name   string
		status ClusterStatus
	}{
		{
			name: "stopped cluster",
			status: ClusterStatus{
				Name:               "demo",
				ProvisioningIntent: cluster.ProvisioningIntent{CSI: cluster.CSILonghorn},
				StoragePhase:       StoragePhaseLive,
				Nodes:              []NodeStatus{node},
			},
		},
		{
			name: "storage still provisioning",
			status: ClusterStatus{
				Name:               "demo",
				ProvisioningIntent: cluster.ProvisioningIntent{CSI: cluster.CSILonghorn},
				Running:            true,
				StoragePhase:       StoragePhaseProvisioning,
				Nodes:              []NodeStatus{node},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, hint := range Hints(tt.status) {
				if strings.Contains(hint, "no redundancy") {
					t.Fatalf("Hints() unexpectedly reported live single-node Longhorn warning: %q", hint)
				}
			}
		})
	}
}

func TestHintsReportCiliumStorageProvisioning(t *testing.T) {
	status := ClusterStatus{
		Name:               "demo",
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, CSI: cluster.CSILonghorn},
		StoragePhase:       StoragePhaseProvisioning,
	}

	joined := strings.Join(Hints(status), "\n")
	if !strings.Contains(joined, "storage provisioning") || strings.Contains(joined, "not implemented") {
		t.Fatalf("hints = %q, want active Cilium storage provisioning", joined)
	}
}

func TestHintsDescribeCiliumReadyWithAndWithoutLoadBalancer(t *testing.T) {
	for _, test := range []struct {
		name   string
		intent cluster.ProvisioningIntent
		vip    string
		live   bool
		wants  []string
	}{
		{
			name:   "live VIP",
			intent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true},
			vip:    "172.30.4.200",
			live:   true,
			wants:  []string{"Cilium LB-IPAM", "http://172.30.4.200/"},
		},
		{
			name:   "load balancer disabled",
			intent: cluster.ProvisioningIntent{CNI: cluster.CNICilium},
			wants:  []string{"Ready", "lb: false", "no VIP"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			status := ClusterStatus{
				Name:               "demo",
				ProvisioningIntent: test.intent,
				KubernetesReady:    true,
				VIP:                test.vip,
				VIPLive:            test.live,
				Nodes:              []NodeStatus{{Role: cluster.RoleControlPlane, Phase: PhaseConfigured}},
			}
			joined := strings.Join(Hints(status), "\n")
			for _, wanted := range test.wants {
				if !strings.Contains(joined, wanted) {
					t.Fatalf("hints missing %q:\n%s", wanted, joined)
				}
			}
		})
	}
}

func TestHintsDoNotInferFlannelKubernetesReadiness(t *testing.T) {
	status := ClusterStatus{
		Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel},
		Nodes: []NodeStatus{{Name: "demo-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2"}},
	}
	for _, hint := range Hints(status) {
		if strings.Contains(hint, "Kubernetes is Ready") {
			t.Fatalf("Hints() inferred Kubernetes readiness from Talos state: %q", hint)
		}
	}
}

func TestHintsDescribeProvisioningInProgressAndExports(t *testing.T) {
	status := ClusterStatus{
		Name: "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true},
		Nodes: []NodeStatus{{Name: "demo-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured}},
	}
	joined := strings.Join(Hints(status), "\n")
	for _, want := range []string{"provisioning is in progress", "tbx up; export TALOSCONFIG=~/.talosbox/clusters/demo/talosconfig", "KUBECONFIG=~/.talosbox/clusters/demo/kubeconfig"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("provisioning hint missing %q:\n%s", want, joined)
		}
	}
}

func TestCredentialExportsQuoteClusterName(t *testing.T) {
	got := credentialExports("demo; echo owned")
	for _, want := range []string{
		"TALOSCONFIG=~/.talosbox/clusters/'demo; echo owned'/talosconfig",
		"KUBECONFIG=~/.talosbox/clusters/'demo; echo owned'/kubeconfig",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("credentialExports() missing %q: %s", want, got)
		}
	}
}

// The calm unreachable hint states the boot budget in prose; formatting it
// from the constant keeps the promise and the clock from desyncing.
func TestFormatBootWindowRendersTheConstant(t *testing.T) {
	cases := map[time.Duration]string{
		time.Minute:      "1 minute",
		2 * time.Minute:  "2 minutes",
		90 * time.Second: "90 seconds",
	}
	for window, want := range cases {
		if got := formatBootWindow(window); got != want {
			t.Fatalf("formatBootWindow(%v) = %q, want %q", window, got, want)
		}
	}
	if got := formatBootWindow(nodeBootWindow); got != "1 minute" {
		t.Fatalf("formatBootWindow(nodeBootWindow) = %q, want the documented promise", got)
	}
}
