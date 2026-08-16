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

func TestPrintStatusRendersStorageProvisioningAlongsideNetworkingHints(t *testing.T) {
	t.Parallel()

	status := daemon.ClusterStatus{
		Name:               "demo",
		Subnet:             "172.30.4.0/24",
		Domain:             "demo.k8s.test",
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILocalPath},
		KubernetesReady:    true,
		StoragePhase:       daemon.StoragePhaseProvisioning,
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
	for _, wanted := range []string{
		"storage provisioning",
		"waiting for the CSI readiness probe to pass",
		"Kubernetes is Ready with Talos-managed flannel",
		"hint [demo]:",
	} {
		if !strings.Contains(rendered, wanted) {
			t.Fatalf("status output missing %q:\n%s", wanted, rendered)
		}
	}
}

func TestPrintStatusRendersStorageLiveHint(t *testing.T) {
	t.Parallel()

	status := daemon.ClusterStatus{
		Name:               "demo",
		Subnet:             "172.30.4.0/24",
		Domain:             "demo.k8s.test",
		ProvisioningIntent: cluster.ProvisioningIntent{CSI: cluster.CSILocalPath},
		StoragePhase:       daemon.StoragePhaseLive,
		Nodes: []daemon.NodeStatus{{
			Name:  "demo-cp-1",
			Role:  cluster.RoleControlPlane,
			MAC:   "52:54:00:00:00:01",
			Phase: daemon.PhaseConfigured,
		}},
	}
	status.Hints = daemon.Hints(status)

	var output bytes.Buffer
	if err := printStatus(&output, []daemon.ClusterStatus{status}, false); err != nil {
		t.Fatal(err)
	}
	rendered := output.String()
	for _, wanted := range []string{"storage live", "CSI readiness probe passed", "hint [demo]:"} {
		if !strings.Contains(rendered, wanted) {
			t.Fatalf("status output missing %q:\n%s", wanted, rendered)
		}
	}
}

func TestPrintStatusShowsTalosVersionAndSchematic(t *testing.T) {
	t.Parallel()

	statuses := []daemon.ClusterStatus{
		{
			Name: "stable", Subnet: "172.30.3.0/24", Domain: "stable.k8s.test",
			TalosVersion: "v1.13.6", Schematic: "aaa111",
			Nodes: []daemon.NodeStatus{{Name: "stable-cp-1", Role: cluster.RoleControlPlane, MAC: "52:54:00:00:00:01", Phase: daemon.PhaseConfigured}},
		},
		{
			Name: "canary", Subnet: "172.30.4.0/24", Domain: "canary.k8s.test",
			TalosVersion: "v1.14.0", Schematic: "bbb222",
			Nodes: []daemon.NodeStatus{{Name: "canary-cp-1", Role: cluster.RoleControlPlane, MAC: "52:54:00:00:00:02", Phase: daemon.PhaseConfigured}},
		},
	}

	var output bytes.Buffer
	if err := printStatus(&output, statuses, false); err != nil {
		t.Fatal(err)
	}
	rendered := output.String()
	for _, wanted := range []string{
		"TALOS",
		"v1.13.6",
		"v1.14.0",
		"cluster stable: schematic aaa111",
		"cluster canary: schematic bbb222",
	} {
		if !strings.Contains(rendered, wanted) {
			t.Fatalf("status output missing %q:\n%s", wanted, rendered)
		}
	}
}

func TestPrintStatusQuietSuppressesSchematicLines(t *testing.T) {
	t.Parallel()

	status := daemon.ClusterStatus{
		Name: "demo", Subnet: "172.30.3.0/24", Domain: "demo.k8s.test",
		TalosVersion: "v1.13.6", Schematic: "aaa111",
		Nodes: []daemon.NodeStatus{{Name: "demo-cp-1", Role: cluster.RoleControlPlane, MAC: "52:54:00:00:00:01", Phase: daemon.PhaseConfigured}},
	}

	var output bytes.Buffer
	if err := printStatus(&output, []daemon.ClusterStatus{status}, true); err != nil {
		t.Fatal(err)
	}
	if rendered := output.String(); strings.Contains(rendered, "schematic") {
		t.Fatalf("quiet status unexpectedly included the schematic line:\n%s", rendered)
	}
	if rendered := output.String(); !strings.Contains(rendered, "v1.13.6") {
		t.Fatalf("quiet status lost the TALOS column:\n%s", rendered)
	}
}
