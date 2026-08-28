package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
)

func kubeletStatus(health daemon.ServiceHealth, message string) *daemon.NodeService {
	return &daemon.NodeService{Name: "kubelet", State: "Failed", Health: health, Message: message}
}

func TestPrintStatusShowsStalledServiceButQuietSuppressesRecovery(t *testing.T) {
	t.Parallel()
	since := time.Now().Add(-4 * time.Minute)
	status := daemon.ClusterStatus{
		Name: "demo", Subnet: "172.30.0.0/24", Domain: "demo.k8s.test", Running: true,
		Nodes: []daemon.NodeStatus{{
			Name: "demo-worker-1", Role: cluster.RoleWorker, MAC: "52:54:00:00:00:02", IP: "172.30.0.3", Phase: daemon.PhaseConfigured,
			Services:        []daemon.NodeService{{Name: "kubelet", State: "Preparing", Health: daemon.ServiceHealthStarting, Since: &since}},
			StalledServices: []daemon.StalledService{{Service: "kubelet", State: "Preparing", Since: since}},
		}},
		Hints: []string{"demo-worker-1: kubelet has remained Preparing — image pull may be stalled; cold-restart with: tbx node stop demo demo-worker-1 && tbx node start demo demo-worker-1"},
	}
	for _, quiet := range []bool{false, true} {
		var output bytes.Buffer
		if err := printStatus(&output, []daemon.ClusterStatus{status}, quiet); err != nil {
			t.Fatal(err)
		}
		rendered := output.String()
		if !strings.Contains(rendered, "node demo-worker-1 service kubelet Preparing") {
			t.Fatalf("quiet=%v status missing factual observation:\n%s", quiet, rendered)
		}
		if quiet && (strings.Contains(rendered, "image pull may be stalled") || strings.Contains(rendered, "tbx node stop")) {
			t.Fatalf("quiet status retained advisory hint:\n%s", rendered)
		}
		if !quiet && (!strings.Contains(rendered, "image pull may be stalled") || !strings.Contains(rendered, "tbx node stop")) {
			t.Fatalf("status lost recovery hint:\n%s", rendered)
		}
	}
}

func TestStatusJSONPreservesTalosServicesAndStalls(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 8, 28, 9, 38, 0, 0, time.UTC)
	statuses := []daemon.ClusterStatus{{Name: "demo", Nodes: []daemon.NodeStatus{{
		Name:            "demo-worker-1",
		Services:        []daemon.NodeService{{Name: "kubelet", State: "Preparing", Health: daemon.ServiceHealthStarting, Since: &since}},
		StalledServices: []daemon.StalledService{{Service: "kubelet", State: "Preparing", Since: since}},
	}}}}
	var output bytes.Buffer
	if err := encodeJSON(&output, statuses); err != nil {
		t.Fatal(err)
	}
	var decoded []daemon.ClusterStatus
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded[0].Nodes[0].Services) != 1 || len(decoded[0].Nodes[0].StalledServices) != 1 {
		t.Fatalf("service fields lost from JSON: %s", output.String())
	}
}

// TestPrintStatusShowsKubeletHealth pins #357: a node answering apid while its
// kubelet crashloops must not read as a plain healthy `configured` node, and
// the message that says why survives --quiet — it is an observation, not advice.
func TestPrintStatusShowsKubeletHealth(t *testing.T) {
	t.Parallel()

	status := daemon.ClusterStatus{
		Name: "qa-core", Subnet: "172.30.0.0/24", Domain: "qa-core.k8s.test", Running: true,
		Nodes: []daemon.NodeStatus{{
			Name: "qa-core-worker-1", Role: cluster.RoleWorker, MAC: "52:54:00:00:00:02", IP: "172.30.0.3",
			Phase:   daemon.PhaseConfigured,
			Kubelet: kubeletStatus(daemon.ServiceHealthCrashLooping, "exec /usr/local/bin/kubelet: input/output error"),
		}},
	}
	for _, quiet := range []bool{false, true} {
		var output bytes.Buffer
		if err := printStatus(&output, []daemon.ClusterStatus{status}, quiet); err != nil {
			t.Fatal(err)
		}
		rendered := output.String()
		for _, want := range []string{"KUBELET", "crashlooping", "exec /usr/local/bin/kubelet: input/output error"} {
			if !strings.Contains(rendered, want) {
				t.Fatalf("quiet=%v status output missing %q:\n%s", quiet, want, rendered)
			}
		}
	}
}

// TestPrintStatusOmitsTheKubeletColumnWithoutReadings keeps the table as it was
// for a stopped cluster or an older daemon: an unasked node is not a healthy
// one, and a column of dashes would claim a measurement nobody took.
func TestPrintStatusOmitsTheKubeletColumnWithoutReadings(t *testing.T) {
	t.Parallel()

	status := daemon.ClusterStatus{
		Name: "demo", Subnet: "172.30.0.0/24", Domain: "demo.k8s.test",
		Nodes: []daemon.NodeStatus{{Name: "demo-cp-1", Role: cluster.RoleControlPlane, MAC: "52:54:00:00:00:01", Phase: daemon.PhaseStopped}},
	}
	var output bytes.Buffer
	if err := printStatus(&output, []daemon.ClusterStatus{status}, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "KUBELET") {
		t.Fatalf("kubelet column printed without any reading:\n%s", output.String())
	}
}

// TestPrintStatusKeepsAHealthyKubeletQuiet keeps the per-node note for the
// nodes that are actually degraded.
func TestPrintStatusKeepsAHealthyKubeletQuiet(t *testing.T) {
	t.Parallel()

	status := daemon.ClusterStatus{
		Name: "demo", Subnet: "172.30.0.0/24", Domain: "demo.k8s.test", Running: true,
		Nodes: []daemon.NodeStatus{{
			Name: "demo-cp-1", Role: cluster.RoleControlPlane, MAC: "52:54:00:00:00:01", IP: "172.30.0.2",
			Phase:   daemon.PhaseConfigured,
			Kubelet: &daemon.NodeService{Name: "kubelet", State: "Running", Health: daemon.ServiceHealthHealthy},
		}},
	}
	var output bytes.Buffer
	if err := printStatus(&output, []daemon.ClusterStatus{status}, true); err != nil {
		t.Fatal(err)
	}
	rendered := output.String()
	if !strings.Contains(rendered, "healthy") {
		t.Fatalf("healthy kubelet not shown in the table:\n%s", rendered)
	}
	if strings.Contains(rendered, "node demo-cp-1 kubelet") {
		t.Fatalf("healthy kubelet raised a note:\n%s", rendered)
	}
}
