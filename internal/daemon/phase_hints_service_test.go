package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
)

// TestHintsSuppressManualBootstrapWhileTbxProvisions pins #366: the
// provisioning hint promises tbx will bootstrap, so the manual talosctl
// bootstrap line must not appear beside it — following it would race tbx.
func TestHintsSuppressManualBootstrapWhileTbxProvisions(t *testing.T) {
	t.Parallel()

	status := ClusterStatus{
		Name:               "qa-fla",
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
		ConfigOrigin:       cluster.OriginManaged,
		Nodes:              []NodeStatus{{Name: "qa-fla-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.50"}},
	}
	joined := strings.Join(Hints(status), "\n")
	if !strings.Contains(joined, "provisioning is in progress") {
		t.Fatalf("provisioning hint missing:\n%s", joined)
	}
	if strings.Contains(joined, "talosctl bootstrap") {
		t.Fatalf("manual bootstrap hint offered mid-provision:\n%s", joined)
	}
	// The dashboard is an inspection, not a competing plan, so it stays.
	if !strings.Contains(joined, "talosctl dashboard") {
		t.Fatalf("dashboard hint lost with the bootstrap hint:\n%s", joined)
	}
}

func TestHintsNameStalledServicesAndQuoteRecoveryCommands(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	since := now.Add(-4 * time.Minute)
	status := ClusterStatus{
		Name: "qa core",
		Nodes: []NodeStatus{{
			Name: "my node", Phase: PhaseConfigured, IP: "172.30.0.2",
			StalledServices: []StalledService{{Service: "kubelet", State: "Preparing", Since: since}},
		}},
	}
	joined := strings.Join(hintsAt(status, now), "\n")
	for _, want := range []string{
		"my node: kubelet has remained Preparing for 4m0s",
		"image pull may be stalled",
		"tbx console 'qa core' 'my node'",
		"tbx node stop 'qa core' 'my node' && tbx node start 'qa core' 'my node'",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("service stall hint missing %q:\n%s", want, joined)
		}
	}
}

func TestHintsExplainMissingTalosCredentialsOnly(t *testing.T) {
	t.Parallel()
	missing := ServiceProbe{Status: ServiceProbeMissingCredentials}
	failed := ServiceProbe{Status: ServiceProbeFailed, Error: "authentication failed"}
	status := ClusterStatus{Name: "demo", Nodes: []NodeStatus{
		{Name: "demo-cp-1", Phase: PhaseConfigured, ServiceProbe: &missing},
		{Name: "demo-worker-1", Phase: PhaseConfigured, ServiceProbe: &missing},
	}}
	joined := strings.Join(Hints(status), "\n")
	for _, want := range []string{`no talosconfig context "demo" was found`, "talosctl config merge", `lists exactly "demo"`} {
		if !strings.Contains(joined, want) {
			t.Fatalf("credential hint missing %q:\n%s", want, joined)
		}
	}
	if strings.Count(joined, "talosctl config merge") != 1 {
		t.Fatalf("credential hint repeated per node:\n%s", joined)
	}

	status.Nodes = []NodeStatus{{Name: "demo-cp-1", Phase: PhaseConfigured, ServiceProbe: &failed}}
	if joined = strings.Join(Hints(status), "\n"); strings.Contains(joined, "talosctl config merge") {
		t.Fatalf("probe failure was mislabeled as missing credentials:\n%s", joined)
	}
}

func TestHintsIgnoreFreshServiceStartup(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	status := ClusterStatus{Name: "demo", Nodes: []NodeStatus{{
		Name: "demo-cp-1", Phase: PhaseConfigured,
		Services: []NodeService{{Name: "kubelet", State: "Preparing", Since: timePointer(now.Add(-time.Minute))}},
	}}}
	joined := strings.Join(hintsAt(status, now), "\n")
	if strings.Contains(joined, "image pull may be stalled") || strings.Contains(joined, "node stop") {
		t.Fatalf("fresh startup raised a stall hint:\n%s", joined)
	}
}

// TestHintsKeepManualBootstrapWithoutAServiceReading keeps the hand-bootstrap
// path when a substrate-only cluster has no positive kubelet observation.
func TestHintsKeepManualBootstrapWithoutAServiceReading(t *testing.T) {
	t.Parallel()

	status := ClusterStatus{
		Name:  "bare",
		Nodes: []NodeStatus{{Name: "bare-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2"}},
	}
	joined := strings.Join(Hints(status), "\n")
	if !strings.Contains(joined, "talosctl bootstrap") {
		t.Fatalf("hand-bootstrap path lost its hint:\n%s", joined)
	}
}

func TestHintsSuppressManualBootstrapAfterHealthyKubelet(t *testing.T) {
	t.Parallel()

	status := ClusterStatus{
		Name: "bare",
		Nodes: []NodeStatus{{
			Name: "bare-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2",
			Kubelet: &NodeService{Name: kubeletService, Health: ServiceHealthHealthy},
		}},
	}
	joined := strings.Join(Hints(status), "\n")
	if strings.Contains(joined, "talosctl bootstrap") {
		t.Fatalf("manual bootstrap hint offered after kubelet became healthy:\n%s", joined)
	}
}

func TestHintsSuppressManualBootstrapWhenAnyNodeHasHealthyKubelet(t *testing.T) {
	t.Parallel()

	status := ClusterStatus{
		Name: "bare",
		Nodes: []NodeStatus{
			{
				Name: "bare-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2",
				Kubelet: &NodeService{Name: kubeletService, Health: ServiceHealthUnknown},
			},
			{
				Name: "bare-worker-1", Role: cluster.RoleWorker, Phase: PhaseConfigured, IP: "172.30.0.3",
				Kubelet: &NodeService{Name: kubeletService, Health: ServiceHealthHealthy},
			},
		},
	}
	joined := strings.Join(Hints(status), "\n")
	if strings.Contains(joined, "talosctl bootstrap") {
		t.Fatalf("manual bootstrap hint offered when a worker kubelet is healthy:\n%s", joined)
	}
}

func TestHintsKeepManualBootstrapWithoutHealthyKubelet(t *testing.T) {
	t.Parallel()

	missing := ServiceProbe{Status: ServiceProbeMissingCredentials}
	failed := ServiceProbe{Status: ServiceProbeFailed, Error: "probe failed"}
	tests := []struct {
		name    string
		kubelet *NodeService
		probe   *ServiceProbe
	}{
		{name: "starting", kubelet: &NodeService{Name: kubeletService, Health: ServiceHealthStarting}},
		{name: "unknown", kubelet: &NodeService{Name: kubeletService, Health: ServiceHealthUnknown}},
		{name: "unhealthy", kubelet: &NodeService{Name: kubeletService, Health: ServiceHealthUnhealthy}},
		{name: "crashlooping", kubelet: &NodeService{Name: kubeletService, Health: ServiceHealthCrashLooping}},
		{name: "probe missing credentials", probe: &missing},
		{name: "probe failure", probe: &failed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			status := ClusterStatus{
				Name: "bare",
				Nodes: []NodeStatus{{
					Name: "bare-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2",
					Kubelet: tt.kubelet, ServiceProbe: tt.probe,
				}},
			}
			joined := strings.Join(Hints(status), "\n")
			if !strings.Contains(joined, "talosctl bootstrap") {
				t.Fatalf("manual bootstrap hint missing without a healthy kubelet:\n%s", joined)
			}
		})
	}
}

func TestHealthyKubeletSuppressesOnlyBootstrapHint(t *testing.T) {
	t.Parallel()

	missing := ServiceProbe{Status: ServiceProbeMissingCredentials}
	status := ClusterStatus{
		Name: "bare",
		Nodes: []NodeStatus{
			{
				Name: "bare-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2",
				ServiceProbe: &missing,
			},
			{
				Name: "bare-worker-1", Role: cluster.RoleWorker, Phase: PhaseConfigured, IP: "172.30.0.3",
				Kubelet: &NodeService{Name: kubeletService, Health: ServiceHealthHealthy},
			},
		},
	}
	joined := strings.Join(Hints(status), "\n")
	if strings.Contains(joined, "talosctl bootstrap") {
		t.Fatalf("manual bootstrap hint offered after kubelet became healthy:\n%s", joined)
	}
	for _, want := range []string{"talosctl dashboard", "Talos service state unavailable", "talosctl config merge"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("healthy kubelet suppression removed independent hint %q:\n%s", want, joined)
		}
	}
}

func TestHintsSuppressManualBootstrapWhenKubernetesIsReady(t *testing.T) {
	t.Parallel()

	status := ClusterStatus{
		Name:            "bare",
		KubernetesReady: true,
		Nodes:           []NodeStatus{{Name: "bare-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2"}},
	}
	joined := strings.Join(Hints(status), "\n")
	if strings.Contains(joined, "talosctl bootstrap") {
		t.Fatalf("manual bootstrap hint offered after Kubernetes became Ready:\n%s", joined)
	}
}

// TestHintsNameACrashloopingKubeletInsteadOfRerunningUp pins #357: `tbx up`
// cannot fix a node whose kubelet cannot exec, so the recovery names the node,
// the crash loop and the two moves that can change the outcome.
func TestHintsNameACrashloopingKubeletInsteadOfRerunningUp(t *testing.T) {
	t.Parallel()

	kubelet := classifyService(kubeletService, ServiceObservation{
		State:    "Failed",
		Message:  "exec /usr/local/bin/kubelet: input/output error",
		Failures: 7,
	})
	status := ClusterStatus{
		Name:               "qa-core",
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, LB: true},
		ConfigOrigin:       cluster.OriginManaged,
		Nodes: []NodeStatus{
			{Name: "qa-core-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2"},
			{Name: "qa-core-worker-1", Role: cluster.RoleWorker, Phase: PhaseConfigured, IP: "172.30.0.3", Kubelet: &kubelet},
		},
	}
	joined := strings.Join(Hints(status), "\n")
	for _, want := range []string{
		"qa-core-worker-1's kubelet is crashlooping",
		"exec /usr/local/bin/kubelet: input/output error",
		"tbx console qa-core qa-core-worker-1",
		"tbx node remove qa-core qa-core-worker-1",
		"tbx node add qa-core qa-core-worker-1 --role worker",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("crashloop hint missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "Rerun: tbx up") {
		t.Fatalf("hint still offers a rerun that cannot fix a crashlooping kubelet:\n%s", joined)
	}
}

// TestHintsQuoteACrashloopingNodeName pins #357: the crash-loop hint prints
// commands the operator is told to paste, and `tbx node add <cluster> <node>`
// accepts any name that is not a path separator — spaces included. An unquoted
// node name splits into extra positionals and the pasted `tbx node remove`
// answers with a usage error instead of removing the node, so the name needs
// the same quoting the cluster name already gets. The prose subject keeps the
// bare name; only the commands are quoted.
func TestHintsQuoteACrashloopingNodeName(t *testing.T) {
	t.Parallel()

	kubelet := classifyService(kubeletService, ServiceObservation{State: "Failed", Failures: 5})
	status := ClusterStatus{
		Name:               "qa core",
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium},
		ConfigOrigin:       cluster.OriginManaged,
		Nodes: []NodeStatus{
			{Name: "my node", Role: cluster.RoleWorker, Phase: PhaseConfigured, IP: "172.30.0.3", Kubelet: &kubelet},
		},
	}
	joined := strings.Join(Hints(status), "\n")
	for _, want := range []string{
		"my node's kubelet is crashlooping",
		"tbx console 'qa core' 'my node'",
		"tbx node remove 'qa core' 'my node'",
		"tbx node add 'qa core' 'my node' --role worker",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("crashloop hint missing %q:\n%s", want, joined)
		}
	}
	for _, unwanted := range []string{
		"tbx console 'qa core' my node",
		"tbx node remove 'qa core' my node",
		"tbx node add 'qa core' my node",
	} {
		if strings.Contains(joined, unwanted) {
			t.Fatalf("crashloop hint pastes an unquoted node name %q:\n%s", unwanted, joined)
		}
	}
}

// TestHintsNameEveryCrashloopingNode keeps a multi-node failure from reading as
// a single bad node.
func TestHintsNameEveryCrashloopingNode(t *testing.T) {
	t.Parallel()

	kubelet := classifyService(kubeletService, ServiceObservation{State: "Failed", Failures: 3})
	status := ClusterStatus{
		Name:               "qa-core",
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium},
		ConfigOrigin:       cluster.OriginManaged,
		Nodes: []NodeStatus{
			{Name: "qa-core-worker-1", Role: cluster.RoleWorker, Phase: PhaseConfigured, IP: "172.30.0.3", Kubelet: &kubelet},
			{Name: "qa-core-worker-2", Role: cluster.RoleWorker, Phase: PhaseConfigured, IP: "172.30.0.4", Kubelet: &kubelet},
		},
	}
	joined := strings.Join(Hints(status), "\n")
	for _, want := range []string{"2 node(s)", "qa-core-worker-1, qa-core-worker-2"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("crashloop hint missing %q:\n%s", want, joined)
		}
	}
}

// TestHintsIgnoreAHealthyKubelet keeps the ordinary recovery wording for a
// cluster whose nodes are merely still converging.
func TestHintsIgnoreAHealthyKubelet(t *testing.T) {
	t.Parallel()

	kubelet := classifyService(kubeletService, ServiceObservation{State: "Running", Healthy: true})
	status := ClusterStatus{
		Name:               "demo",
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel},
		ConfigOrigin:       cluster.OriginManaged,
		Nodes:              []NodeStatus{{Name: "demo-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2", Kubelet: &kubelet}},
	}
	joined := strings.Join(Hints(status), "\n")
	if !strings.Contains(joined, "Rerun: tbx up") {
		t.Fatalf("healthy cluster lost its ordinary recovery hint:\n%s", joined)
	}
	if strings.Contains(joined, "crashlooping") {
		t.Fatalf("healthy kubelet reported as a crash loop:\n%s", joined)
	}
}
