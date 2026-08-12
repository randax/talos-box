package daemon

import (
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
)

func TestRefreshKubernetesReadinessDoesNotClaimReadyWithoutCredentials(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	statuses := []ClusterStatus{{
		Running: true,
		Name:    "demo", ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel},
		Nodes: []NodeStatus{{Name: "demo-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseConfigured, IP: "172.30.0.2"}},
	}}
	refreshKubernetesReadiness(statuses)
	if statuses[0].KubernetesReady {
		t.Fatal("status claimed Kubernetes Ready without a valid kubeconfig")
	}
	for _, hint := range statuses[0].Hints {
		if strings.Contains(hint, "Kubernetes is Ready") {
			t.Fatalf("status emitted an unverified Ready hint: %q", hint)
		}
	}
}

func TestRefreshKubernetesReadinessSkipsStoppedFlannelClusters(t *testing.T) {
	statuses := []ClusterStatus{{
		Name:               "demo",
		Running:            false,
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
		KubernetesReady:    true,
		VIP:                "172.30.0.200",
		VIPLive:            true,
		Nodes:              []NodeStatus{{Name: "demo-cp-1", Role: cluster.RoleControlPlane, Phase: PhaseStopped, IP: "172.30.0.2"}},
	}}

	refreshKubernetesReadiness(statuses)

	if statuses[0].KubernetesReady {
		t.Fatal("stopped cluster reported Kubernetes ready")
	}
	if statuses[0].VIP != "" || statuses[0].VIPLive {
		t.Fatalf("stopped cluster retained stale VIP state: %+v", statuses[0])
	}
}
