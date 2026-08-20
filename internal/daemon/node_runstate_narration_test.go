package daemon

import (
	"encoding/json"
	"testing"
)

// `node stop` and `node start` used to answer with a single line and nothing
// else, while their siblings narrated every phase and pointed at status for
// what happens after the verb returns (#414).
func TestNodeStopNarratesThePhasesAndPointsAtStatus(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	stubNodeMutationReconcile(service)
	progress, stages := recordStages()

	raw, err := json.Marshal(nodeArgs{Cluster: item.Name, Name: "demo-worker-2"})
	if err != nil {
		t.Fatal(err)
	}
	response := service.dispatchWithProgress(Request{Op: "node.stop", Args: raw}, progress)
	if !response.OK {
		t.Fatalf("node.stop failed: %s", response.Error)
	}

	stopping := indexOfStage(*stages, "stopping node demo-worker-2")
	hint := indexOfStage(*stages, "tbx status demo")
	if stopping < 0 || hint < 0 {
		t.Fatalf("node.stop narration = %q, want the stop phase and a status hint", *stages)
	}
	if stopping >= hint {
		t.Fatalf("node.stop narration = %q, want the stop phase before the hint", *stages)
	}
}

// Stopping the last running node stops the cluster, which is a consequence the
// operator did not ask for by name and must be told about.
func TestNodeStopOfTheLastRunningNodeNarratesTheStoppedCluster(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 0)
	stubNodeMutationReconcile(service)
	progress, stages := recordStages()

	raw, err := json.Marshal(nodeArgs{Cluster: item.Name, Name: "demo-cp-1"})
	if err != nil {
		t.Fatal(err)
	}
	response := service.dispatchWithProgress(Request{Op: "node.stop", Args: raw}, progress)
	if !response.OK {
		t.Fatalf("node.stop failed: %s", response.Error)
	}

	if indexOfStage(*stages, "cluster demo is now stopped") < 0 {
		t.Fatalf("node.stop narration = %q, want the stopped cluster narrated", *stages)
	}
	// nothing is converging behind a cluster with no running node
	if indexOfStage(*stages, "tbx status demo") >= 0 {
		t.Fatalf("node.stop narration = %q, want no convergence hint for a stopped cluster", *stages)
	}
}

func TestNodeStartNarratesTheBootAndPointsAtStatus(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	stubNodeMutationReconcile(service)
	delete(service.vms[item.Name], "demo-worker-1")
	progress, stages := recordStages()

	raw, err := json.Marshal(nodeArgs{Cluster: item.Name, Name: "demo-worker-1"})
	if err != nil {
		t.Fatal(err)
	}
	response := service.dispatchWithProgress(Request{Op: "node.start", Args: raw}, progress)
	if !response.OK {
		t.Fatalf("node.start failed: %s", response.Error)
	}

	starting := indexOfStage(*stages, "starting node demo-worker-1")
	hint := indexOfStage(*stages, "tbx status demo")
	if starting < 0 || hint < 0 {
		t.Fatalf("node.start narration = %q, want the start phase and a convergence hint", *stages)
	}
	if starting >= hint {
		t.Fatalf("node.start narration = %q, want the start phase before the hint", *stages)
	}
}

// A verb that changed nothing narrates nothing: the no-op line is the whole
// story, and phases it never ran would be fiction (#362).
func TestNodeStartOfARunningNodeNarratesNothing(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	stubNodeMutationReconcile(service)
	progress, stages := recordStages()

	raw, err := json.Marshal(nodeArgs{Cluster: item.Name, Name: "demo-worker-1"})
	if err != nil {
		t.Fatal(err)
	}
	response := service.dispatchWithProgress(Request{Op: "node.start", Args: raw}, progress)
	if !response.OK {
		t.Fatalf("node.start failed: %s", response.Error)
	}
	if len(*stages) != 0 {
		t.Fatalf("no-op node.start narration = %q, want none", *stages)
	}
}
