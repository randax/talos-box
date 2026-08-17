package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/randax/talos-box/internal/balloon"
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

// countingReconcile records every reconcile the service runs, so a test can
// prove a run-state verb scheduled none.
func countingReconcile(service *Server) *int32 {
	var runs int32
	service.provisionReconcile = func(context.Context, provision.Request) (provision.Result, error) {
		atomic.AddInt32(&runs, 1)
		return provision.Result{StoragePhase: provision.StoragePhaseLive, StorageLive: true}, nil
	}
	return &runs
}

// TestNodeStopSchedulesNoReconcile pins the convergence rule: the reconcile's
// request still lists the node that was just powered off, so a forced run could
// only spin to the provision timeout and park storage at `provisioning` (#332).
func TestNodeStopSchedulesNoReconcile(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	runs := countingReconcile(service)
	service.storagePhases[item.Name] = StoragePhaseLive

	response := dispatchNodeRunState(t, service, "node.stop", item.Name, "demo-worker-2")

	if !response.OK {
		t.Fatalf("node.stop failed: %s", response.Error)
	}
	service.backgroundProvisions.Wait()
	if got := atomic.LoadInt32(runs); got != 0 {
		t.Fatalf("node.stop ran %d reconcile(s), want none", got)
	}
	if _, ok := service.provisions[item.Name]; ok {
		t.Fatal("node.stop left a provision active")
	}
	if phase := service.storagePhases[item.Name]; phase == StoragePhaseProvisioning {
		t.Fatalf("node.stop moved storage phase to %q", phase)
	}
}

// TestNodeStartOfAPartlyStoppedClusterSchedulesNoReconcile keeps the reconcile
// away from a topology it cannot converge: members are still powered off.
func TestNodeStartOfAPartlyStoppedClusterSchedulesNoReconcile(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	runs := countingReconcile(service)
	delete(service.vms[item.Name], "demo-worker-1")
	delete(service.vms[item.Name], "demo-worker-2")

	response := dispatchNodeRunState(t, service, "node.start", item.Name, "demo-worker-1")

	if !response.OK {
		t.Fatalf("node.start failed: %s", response.Error)
	}
	service.backgroundProvisions.Wait()
	if got := atomic.LoadInt32(runs); got != 0 {
		t.Fatalf("node.start over a partly stopped cluster ran %d reconcile(s), want none", got)
	}
}

// TestNodeStartOfTheLastStoppedMemberReconciles is the other half: the cluster
// is whole again, so the reconcile can actually converge.
func TestNodeStartOfTheLastStoppedMemberReconciles(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	reconciled := make(chan int, 1)
	service.provisionReconcile = func(_ context.Context, request provision.Request) (provision.Result, error) {
		reconciled <- len(request.Cluster.Nodes)
		return provision.Result{StoragePhase: provision.StoragePhaseLive, StorageLive: true}, nil
	}
	delete(service.vms[item.Name], "demo-worker-2")

	response := dispatchNodeRunState(t, service, "node.start", item.Name, "demo-worker-2")

	if !response.OK {
		t.Fatalf("node.start failed: %s", response.Error)
	}
	service.backgroundProvisions.Wait()
	select {
	case nodes := <-reconciled:
		if nodes != 3 {
			t.Fatalf("reconcile saw %d nodes, want 3", nodes)
		}
	default:
		t.Fatal("starting the last stopped member did not reconcile the whole cluster")
	}
}

// TestNodeStopOfAControlPlaneWarnsAboutQuorum pins the advisory: stopping a
// control-plane node never blocks, but the operator must learn what it costs
// etcd before the cluster stops answering.
func TestNodeStopOfAControlPlaneWarnsAboutQuorum(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 3, 1)
	countingReconcile(service)

	response := dispatchNodeRunState(t, service, "node.stop", item.Name, "demo-cp-2")

	if !response.OK {
		t.Fatalf("node.stop failed: %s", response.Error)
	}
	status := decodeNodeStatus(t, response)
	joined := strings.Join(status.Warnings, "\n")
	for _, want := range []string{"demo-cp-2", "2 of 3 control-plane nodes running", "quorum"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("node.stop warnings = %q, want them to mention %q", status.Warnings, want)
		}
	}
}

func TestNodeStopOfAWorkerCarriesNoQuorumWarning(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 3, 1)
	countingReconcile(service)

	response := dispatchNodeRunState(t, service, "node.stop", item.Name, "demo-worker-1")

	if !response.OK {
		t.Fatalf("node.stop failed: %s", response.Error)
	}
	if status := decodeNodeStatus(t, response); strings.Contains(strings.Join(status.Warnings, "\n"), "quorum") {
		t.Fatalf("stopping a worker warned about quorum: %q", status.Warnings)
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

// TestNodeAddOverAPartlyStoppedClusterSchedulesNoReconcile pins the gate that
// keeps `node add` off a topology the reconcile cannot converge: its request
// lists every member and its readiness gates poll nodes that are powered off,
// and node add's reconcile runs synchronously on the request path — so the
// caller would block for the whole provision timeout for nothing (#332).
func TestNodeAddOverAPartlyStoppedClusterSchedulesNoReconcile(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	runs := countingReconcile(service)
	delete(service.vms[item.Name], "demo-worker-1")

	raw, err := json.Marshal(nodeArgs{Cluster: item.Name, Name: "demo-worker-2", Role: cluster.RoleWorker})
	if err != nil {
		t.Fatal(err)
	}
	response := service.dispatch(Request{Op: "node.add", Args: raw})

	if !response.OK {
		t.Fatalf("node.add failed: %s", response.Error)
	}
	service.backgroundProvisions.Wait()
	if got := atomic.LoadInt32(runs); got != 0 {
		t.Fatalf("node.add over a partly stopped cluster ran %d reconcile(s), want none", got)
	}
	// The skipped reconcile is the whole story for the operator: the VM is up
	// but unconfigured, and only "added node ..." would otherwise be printed.
	status := decodeNodeStatus(t, response)
	joined := strings.Join(status.Warnings, "\n")
	for _, want := range []string{"cluster members are stopped", "demo-worker-2", "unconfigured"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("node.add warnings = %q, want them to mention %q", status.Warnings, want)
		}
	}
}

// A substrate-only cluster has no reconcile to defer, so warning about one
// would be a lie the operator cannot act on.
func TestNodeAddOverAPartlyStoppedSubstrateClusterCarriesNoReconcileWarning(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	item.ProvisioningIntent = cluster.ProvisioningIntent{}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	countingReconcile(service)
	delete(service.vms[item.Name], "demo-worker-1")

	raw, err := json.Marshal(nodeArgs{Cluster: item.Name, Name: "demo-worker-2", Role: cluster.RoleWorker})
	if err != nil {
		t.Fatal(err)
	}
	response := service.dispatch(Request{Op: "node.add", Args: raw})

	if !response.OK {
		t.Fatalf("node.add failed: %s", response.Error)
	}
	status := decodeNodeStatus(t, response)
	if joined := strings.Join(status.Warnings, "\n"); strings.Contains(joined, "cluster members are stopped") {
		t.Fatalf("node.add on a substrate-only cluster warned about a reconcile it never runs: %q", status.Warnings)
	}
}

func TestNodeRemoveOverAPartlyStoppedClusterSchedulesNoReconcile(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	runs := countingReconcile(service)
	service.nodeVolumeCount = func(context.Context, cluster.Cluster, string) (int, error) { return 0, nil }
	delete(service.vms[item.Name], "demo-worker-1")
	service.storagePhases[item.Name] = StoragePhaseLive

	response := dispatchNodeRemove(t, service, item.Name, "demo-worker-2", false)

	if !response.OK {
		t.Fatalf("node.remove failed: %s", response.Error)
	}
	service.backgroundProvisions.Wait()
	if got := atomic.LoadInt32(runs); got != 0 {
		t.Fatalf("node.remove over a partly stopped cluster ran %d reconcile(s), want none", got)
	}
	// A skipped reconcile is not a reason to keep a `live` memo standing:
	// membership just changed, and refreshStoragePhases short-circuits on it.
	if phase, ok := service.storagePhases[item.Name]; ok {
		t.Fatalf("node.remove left storage phase %q recorded; it must be re-probed after a membership change", phase)
	}
	status := decodeNodeStatus(t, response)
	joined := strings.Join(status.Warnings, "\n")
	for _, want := range []string{"cluster members are stopped", "not reconciled"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("node.remove warnings = %q, want them to mention %q", status.Warnings, want)
		}
	}
}

// TestNodeRemoveOverARunningClusterReconciles is the other half: the remaining
// membership is whole, so the topology change must still be pushed out.
func TestNodeRemoveOverARunningClusterReconciles(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	service.nodeVolumeCount = func(context.Context, cluster.Cluster, string) (int, error) { return 0, nil }
	reconciled := make(chan int, 1)
	service.provisionReconcile = func(_ context.Context, request provision.Request) (provision.Result, error) {
		reconciled <- len(request.Cluster.Nodes)
		return provision.Result{StoragePhase: provision.StoragePhaseLive, StorageLive: true}, nil
	}

	response := dispatchNodeRemove(t, service, item.Name, "demo-worker-2", false)

	if !response.OK {
		t.Fatalf("node.remove failed: %s", response.Error)
	}
	service.backgroundProvisions.Wait()
	select {
	case nodes := <-reconciled:
		if nodes != 2 {
			t.Fatalf("reconcile saw %d nodes, want 2", nodes)
		}
	default:
		t.Fatal("node.remove on a fully running cluster did not reconcile it")
	}
}

// TestNodeStartRefusesUnderHostPressureWithoutForce holds `node start` to the
// same host guards cluster start and node add enforce: powering a node on
// commits real host memory, so it cannot be the one verb that skips them.
func TestNodeStartRefusesUnderHostPressureWithoutForce(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	stubNodeMutationReconcile(service)
	delete(service.vms[item.Name], "demo-worker-1")
	service.hostPressure = extremeSwapPressure

	response := dispatchNodeRunState(t, service, "node.start", item.Name, "demo-worker-1")

	if response.OK {
		t.Fatal("node.start launched a VM under blocking host pressure")
	}
	if !strings.Contains(response.Error, "host swap is 90% used") {
		t.Fatalf("node.start error = %q, want the host-pressure refusal", response.Error)
	}
	if service.nodeRunning(item.Name, "demo-worker-1") {
		t.Fatal("node.start started the node despite refusing")
	}
}

// With --force the same finding is advisory: the node starts and the operator
// is told what they overrode.
func TestNodeStartUnderHostPressureWithForceWarns(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	stubNodeMutationReconcile(service)
	delete(service.vms[item.Name], "demo-worker-1")
	service.hostPressure = extremeSwapPressure

	raw, err := json.Marshal(nodeArgs{Cluster: item.Name, Name: "demo-worker-1", Force: true})
	if err != nil {
		t.Fatal(err)
	}
	response := service.dispatch(Request{Op: "node.start", Args: raw})

	if !response.OK {
		t.Fatalf("forced node.start failed: %s", response.Error)
	}
	if !service.nodeRunning(item.Name, "demo-worker-1") {
		t.Fatal("forced node.start did not launch the node")
	}
	status := decodeNodeStatus(t, response)
	if !strings.Contains(strings.Join(status.Warnings, "\n"), "host swap is 90% used") {
		t.Fatalf("forced node.start warnings = %q, want the host-pressure finding", status.Warnings)
	}
}

// TestNodeStartDoesNotDoubleCountItsOwnMemory pins the accounting: a running
// cluster's whole configured memory — the stopped member's share included — is
// already in the commitment checkOvercommit sums, so charging the started
// node's memory on top would refuse a member that costs the host nothing new.
// `cluster start` avoids it by skipping the check for a running cluster; the
// per-node verb must mirror that.
func TestNodeStartDoesNotDoubleCountItsOwnMemory(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	stubNodeMutationReconcile(service)
	delete(service.vms[item.Name], "demo-worker-2")
	// The host fits the cluster exactly: only a double-charged node tips it over.
	service.hostTotalMemory = func() (int, error) {
		return balloon.DefaultConfig().ReserveMiB + clusterMemoryMiB(item), nil
	}

	response := dispatchNodeRunState(t, service, "node.start", item.Name, "demo-worker-2")

	if !response.OK {
		t.Fatalf("node.start of a stopped member of a running cluster was refused: %s", response.Error)
	}
	if !service.nodeRunning(item.Name, "demo-worker-2") {
		t.Fatal("node.start did not launch the node")
	}
}

// The guard is still real where the memory is genuinely new: starting into a
// stopped cluster commits host memory nothing else has claimed.
func TestNodeStartOfAStoppedClusterStillRefusesOvercommit(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	stubNodeMutationReconcile(service)
	delete(service.vms, item.Name)
	service.hostTotalMemory = func() (int, error) { return balloon.DefaultConfig().ReserveMiB, nil }

	response := dispatchNodeRunState(t, service, "node.start", item.Name, "demo-cp-1")

	if response.OK {
		t.Fatal("node.start into a stopped cluster ignored the overcommit guard")
	}
	if !strings.Contains(response.Error, "exceeds host") {
		t.Fatalf("node.start error = %q, want the overcommit refusal", response.Error)
	}
	if service.nodeRunning(item.Name, "demo-cp-1") {
		t.Fatal("node.start launched the node despite refusing")
	}
}

// TestNodeStopInvalidatesTheStoragePhase pins the regression: a recorded `live`
// phase short-circuits refreshStoragePhases, so leaving it standing after a
// worker is powered off keeps reporting storage live over a cluster that just
// lost a node.
func TestNodeStopInvalidatesTheStoragePhase(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	countingReconcile(service)
	service.storagePhases[item.Name] = StoragePhaseLive

	response := dispatchNodeRunState(t, service, "node.stop", item.Name, "demo-worker-2")

	if !response.OK {
		t.Fatalf("node.stop failed: %s", response.Error)
	}
	if phase, ok := service.storagePhases[item.Name]; ok {
		t.Fatalf("node.stop left storage phase %q recorded; it must be re-probed", phase)
	}
	statuses, err := service.status(mustRawJSON(t, statusArgs{Cluster: item.Name}))
	if err != nil {
		t.Fatal(err)
	}
	if statuses[0].StoragePhase == StoragePhaseLive {
		t.Fatal("status still reports storage live after a node was stopped")
	}
}
