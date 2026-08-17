package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/daemon"
)

func suspendedStatus() daemon.ClusterStatus {
	status := daemon.ClusterStatus{
		Name:      "napping",
		Subnet:    "172.30.4.0/24",
		Domain:    "napping.k8s.test",
		Suspended: true,
		Nodes: []daemon.NodeStatus{{
			Name:      "napping-cp-1",
			Role:      cluster.RoleControlPlane,
			MAC:       "52:54:00:00:00:01",
			IP:        "172.30.4.2",
			Phase:     daemon.PhaseStopped,
			Suspended: true,
		}},
	}
	status.Hints = daemon.Hints(status)
	return status
}

// TestPrintStatusMarksSuspendedNodes pins #272: a suspended cluster must not
// read as plain "stopped" in the table, quiet or not.
func TestPrintStatusMarksSuspendedNodes(t *testing.T) {
	t.Parallel()

	for _, quiet := range []bool{false, true} {
		var output bytes.Buffer
		if err := printStatus(&output, []daemon.ClusterStatus{suspendedStatus()}, quiet); err != nil {
			t.Fatal(err)
		}
		rendered := output.String()
		if !strings.Contains(rendered, "suspended") {
			t.Fatalf("quiet=%v status output does not mark the cluster suspended:\n%s", quiet, rendered)
		}
		for _, line := range strings.Split(rendered, "\n") {
			if strings.Contains(line, "napping-cp-1") && strings.Contains(line, "stopped") {
				t.Fatalf("quiet=%v node line still reads stopped: %q", quiet, line)
			}
		}
	}
}

// TestPrintStatusSuspendedHintNamesResume keeps the copy-pasteable way out in
// the rendered hints.
func TestPrintStatusSuspendedHintNamesResume(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	if err := printStatus(&output, []daemon.ClusterStatus{suspendedStatus()}, false); err != nil {
		t.Fatal(err)
	}
	rendered := output.String()
	if !strings.Contains(rendered, "tbx cluster resume napping") {
		t.Fatalf("status output missing the resume hint:\n%s", rendered)
	}
	if strings.Contains(rendered, "start it with") {
		t.Fatalf("status output still hints start for a suspended cluster:\n%s", rendered)
	}
}

// TestPrintStatusKeepsUnsuspendedMembersStopped pins the per-node accuracy:
// suspend only saves the memory of nodes that were running, so a member that
// was already stopped must not inherit the cluster's Suspended flag — a resume
// cold-boots it, it does not pick up where it left off.
func TestPrintStatusKeepsUnsuspendedMembersStopped(t *testing.T) {
	t.Parallel()

	status := suspendedStatus()
	status.Nodes = append(status.Nodes, daemon.NodeStatus{
		Name:  "napping-worker-1",
		Role:  cluster.RoleWorker,
		MAC:   "52:54:00:00:00:02",
		Phase: daemon.PhaseStopped,
	})

	var output bytes.Buffer
	if err := printStatus(&output, []daemon.ClusterStatus{status}, true); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(output.String(), "\n") {
		switch {
		case strings.Contains(line, "napping-worker-1") && !strings.Contains(line, "stopped"):
			t.Fatalf("a member without saved memory does not read stopped: %q", line)
		case strings.Contains(line, "napping-cp-1") && !strings.Contains(line, "suspended"):
			t.Fatalf("the member holding saved memory does not read suspended: %q", line)
		}
	}
}

// TestPrintClustersMarksSuspendedState keeps tbx list consistent with status.
func TestPrintClustersMarksSuspendedState(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	if err := printClusters(&output, []daemon.ClusterSummary{{Name: "napping", Suspended: true}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "suspended") {
		t.Fatalf("list output does not mark the suspended cluster:\n%s", output.String())
	}
}
