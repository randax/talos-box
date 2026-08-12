package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
)

func TestPrintStatusRendersFlannelLiveVIPHint(t *testing.T) {
	t.Parallel()

	status := daemon.ClusterStatus{
		Name:               "demo",
		Subnet:             "172.30.4.0/24",
		Domain:             "demo.k8s.test",
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
		KubernetesReady:    true,
		VIP:                "172.30.4.200",
		VIPLive:            true,
		Nodes: []daemon.NodeStatus{{
			Name:  "demo-cp-1",
			Role:  cluster.RoleControlPlane,
			MAC:   "52:54:00:00:00:01",
			IP:    "172.30.4.2",
			Phase: daemon.PhaseConfigured,
		}},
	}
	status.Hints = daemon.Hints(status)

	var output bytes.Buffer
	if err := printStatus(&output, []daemon.ClusterStatus{status}, false); err != nil {
		t.Fatal(err)
	}
	rendered := output.String()
	for _, wanted := range []string{"http://172.30.4.200/", "does not enforce NetworkPolicies", "hint [demo]:"} {
		if !strings.Contains(rendered, wanted) {
			t.Fatalf("status output missing %q:\n%s", wanted, rendered)
		}
	}
}

func TestPrintStatusQuietSuppressesFlannelLiveVIPHint(t *testing.T) {
	t.Parallel()

	status := daemon.ClusterStatus{
		Name:               "demo",
		Subnet:             "172.30.4.0/24",
		Domain:             "demo.k8s.test",
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true},
		KubernetesReady:    true,
		VIP:                "172.30.4.200",
		VIPLive:            true,
		Nodes: []daemon.NodeStatus{{
			Name:  "demo-cp-1",
			Role:  cluster.RoleControlPlane,
			MAC:   "52:54:00:00:00:01",
			IP:    "172.30.4.2",
			Phase: daemon.PhaseConfigured,
		}},
	}
	status.Hints = daemon.Hints(status)

	var output bytes.Buffer
	if err := printStatus(&output, []daemon.ClusterStatus{status}, true); err != nil {
		t.Fatal(err)
	}
	rendered := output.String()
	for _, unwanted := range []string{"http://172.30.4.200/", "does not enforce NetworkPolicies", "hint [demo]:"} {
		if strings.Contains(rendered, unwanted) {
			t.Fatalf("quiet status unexpectedly included %q:\n%s", unwanted, rendered)
		}
	}
}
