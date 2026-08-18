package daemon

import (
	"strings"
	"testing"

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

// TestHintsKeepManualBootstrapWithoutAProvisioningIntent keeps the hand-bootstrap
// path — a substrate-only cluster tbx is not driving — pointed at talosctl.
func TestHintsKeepManualBootstrapWithoutAProvisioningIntent(t *testing.T) {
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
