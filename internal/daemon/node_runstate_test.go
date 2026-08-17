package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
	"github.com/randax/talos-box/internal/provision"
)

func dispatchNodeRunState(t *testing.T, service *Server, op, clusterName, nodeName string) Response {
	t.Helper()
	raw, err := json.Marshal(nodeArgs{Cluster: clusterName, Name: nodeName})
	if err != nil {
		t.Fatal(err)
	}
	return service.dispatch(Request{Op: op, Args: raw})
}

func TestNodeStopClosesOnlyTheNamedNode(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	stubNodeMutationReconcile(service)

	response := dispatchNodeRunState(t, service, "node.stop", item.Name, "demo-worker-2")

	if !response.OK {
		t.Fatalf("node.stop failed: %s", response.Error)
	}
	if status := decodeNodeStatus(t, response); status.Name != "demo-worker-2" || status.Phase != PhaseStopped {
		t.Fatalf("node.stop status = %+v, want a stopped demo-worker-2", status)
	}
	if _, running := service.vms[item.Name]["demo-worker-2"]; running {
		t.Fatal("node.stop left the stopped node's VM registered")
	}
	if !service.nodeRunning(item.Name, "demo-worker-1") {
		t.Fatal("node.stop closed a node it was not asked about")
	}
	reloaded, err := cluster.Load(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Nodes) != 3 {
		t.Fatalf("cluster node count after node.stop = %d, want 3: stopping a node must not change membership", len(reloaded.Nodes))
	}
}

func TestNodeStopOfLastRunningNodeStopsTheCluster(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 0)
	service.storagePhases[item.Name] = StoragePhaseLive
	service.provisions[item.Name] = activeProvision{generation: 1, cancel: func() {}}

	response := dispatchNodeRunState(t, service, "node.stop", item.Name, "demo-cp-1")

	if !response.OK {
		t.Fatalf("node.stop failed: %s", response.Error)
	}
	if service.clusterRunning(item.Name) {
		t.Fatal("cluster still reports running after its last node stopped")
	}
	if _, ok := service.provisions[item.Name]; ok {
		t.Fatal("node.stop of the last node left provisioning active")
	}
	if phase, ok := service.storagePhases[item.Name]; ok {
		t.Fatalf("node.stop of the last node left storage phase %q", phase)
	}
}

func TestNodeStopIsIdempotentForAStoppedNode(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	delete(service.vms[item.Name], "demo-worker-1")

	response := dispatchNodeRunState(t, service, "node.stop", item.Name, "demo-worker-1")

	if !response.OK {
		t.Fatalf("node.stop of an already-stopped node failed: %s", response.Error)
	}
	if status := decodeNodeStatus(t, response); status.Phase != PhaseStopped {
		t.Fatalf("node.stop status phase = %q, want %q", status.Phase, PhaseStopped)
	}
}

func TestNodeStartLaunchesOnlyTheNamedNode(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	stubNodeMutationReconcile(service)
	delete(service.vms[item.Name], "demo-worker-1")

	response := dispatchNodeRunState(t, service, "node.start", item.Name, "demo-worker-1")

	if !response.OK {
		t.Fatalf("node.start failed: %s", response.Error)
	}
	if !service.nodeRunning(item.Name, "demo-worker-1") {
		t.Fatal("node.start did not launch the node's VM")
	}
	if status := decodeNodeStatus(t, response); status.Name != "demo-worker-1" {
		t.Fatalf("node.start status = %+v, want demo-worker-1", status)
	}
}

func TestNodeStartOfFirstNodeReconcilesTheStoppedCluster(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 0)
	delete(service.vms, item.Name)
	reconciled := make(chan int, 1)
	service.provisionReconcile = func(_ context.Context, request provision.Request) (provision.Result, error) {
		reconciled <- len(request.Cluster.Nodes)
		return provision.Result{StoragePhase: provision.StoragePhaseLive, StorageLive: true}, nil
	}

	response := dispatchNodeRunState(t, service, "node.start", item.Name, "demo-cp-1")

	if !response.OK {
		t.Fatalf("node.start failed: %s", response.Error)
	}
	service.backgroundProvisions.Wait()
	select {
	case nodes := <-reconciled:
		if nodes != 1 {
			t.Fatalf("reconcile saw %d nodes, want 1", nodes)
		}
	default:
		t.Fatal("starting the first node of a provisioned cluster did not reconcile it")
	}
}

func TestNodeStartIsIdempotentForARunningNode(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	service.hypervisor = &fakeHypervisor{launch: func(context.Context, hypervisor.Spec) (hypervisor.Machine, error) {
		t.Error("node.start launched a VM for a node that is already running")
		return &fakeMachine{active: true}, nil
	}}

	response := dispatchNodeRunState(t, service, "node.start", item.Name, "demo-worker-1")

	if !response.OK {
		t.Fatalf("node.start of a running node failed: %s", response.Error)
	}
}

func TestNodeRunStateRefusesUnknownClusterAndNode(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)

	for _, test := range []struct{ op, cluster, node, want string }{
		{"node.start", "missing", "demo-worker-1", "missing"},
		{"node.stop", "missing", "demo-worker-1", "missing"},
		{"node.start", item.Name, "demo-worker-9", "demo-worker-9"},
		{"node.stop", item.Name, "demo-worker-9", "demo-worker-9"},
	} {
		response := dispatchNodeRunState(t, service, test.op, test.cluster, test.node)
		if response.OK {
			t.Fatalf("%s %s/%s succeeded, want a refusal", test.op, test.cluster, test.node)
		}
		if !strings.Contains(response.Error, test.want) {
			t.Fatalf("%s error %q does not name %q", test.op, response.Error, test.want)
		}
	}
}
