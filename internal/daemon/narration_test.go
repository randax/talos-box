package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/provision"
)

// recordStages collects the narration one request emitted, in order.
func recordStages() (stageFunc, *[]string) {
	var stages []string
	return func(stage string) { stages = append(stages, stage) }, &stages
}

// indexOfStage reports where the first stage containing needle appears, or -1.
func indexOfStage(stages []string, needle string) int {
	for i, stage := range stages {
		if strings.Contains(stage, needle) {
			return i
		}
	}
	return -1
}

// SPEC §7 orders the snapshot: stop, clone as one crash-consistent set,
// restart. The ordering test on the disks proves it happened (#244); this one
// proves the operator can see it happen (#273), and pins the order so a future
// live-clone fast path cannot slip in silently.
func TestSnapshotCreateNarratesStopBeforeCloneBeforeRestart(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	liveDisks(t, item, "running")
	progress, stages := recordStages()

	raw, err := json.Marshal(snapshotArgs{Cluster: item.Name, Name: "baseline"})
	if err != nil {
		t.Fatal(err)
	}
	response := service.dispatchWithProgress(Request{Op: "snapshot.create", Args: raw}, progress)
	if !response.OK {
		t.Fatalf("snapshot.create failed: %s", response.Error)
	}

	stop := indexOfStage(*stages, "stopping cluster demo")
	clone := indexOfStage(*stages, "cloning 2 node disk(s) as one crash-consistent set")
	restart := indexOfStage(*stages, "restarting cluster demo")
	hint := indexOfStage(*stages, "tbx status demo")
	if stop < 0 || clone < 0 || restart < 0 || hint < 0 {
		t.Fatalf("snapshot.create narration = %q, want stop, clone, restart and a convergence hint", *stages)
	}
	if !(stop < clone && clone < restart && restart < hint) {
		t.Fatalf("snapshot.create narration = %q, want the stop before the clone before the restart", *stages)
	}
}

// A snapshot of a stopped cluster clones nothing under a live guest, so it
// narrates the clone alone: no stop to announce, and no restart to promise.
func TestSnapshotCreateOnAStoppedClusterNarratesOnlyTheClone(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	liveDisks(t, item, "cold")
	delete(service.vms, item.Name)
	progress, stages := recordStages()

	raw, err := json.Marshal(snapshotArgs{Cluster: item.Name, Name: "baseline"})
	if err != nil {
		t.Fatal(err)
	}
	response := service.dispatchWithProgress(Request{Op: "snapshot.create", Args: raw}, progress)
	if !response.OK {
		t.Fatalf("snapshot.create failed: %s", response.Error)
	}
	if indexOfStage(*stages, "cloning") < 0 {
		t.Fatalf("narration = %q, want the clone announced", *stages)
	}
	if indexOfStage(*stages, "stopping") >= 0 || indexOfStage(*stages, "restarting") >= 0 {
		t.Fatalf("narration = %q, want no stop or restart for an already stopped cluster", *stages)
	}
}

func TestSnapshotRestoreNarratesStopRestoreAndStart(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	liveDisks(t, item, "running")
	if err := cluster.CreateSnapshot(item, "before"); err != nil {
		t.Fatal(err)
	}
	progress, stages := recordStages()

	raw, err := json.Marshal(snapshotArgs{Cluster: item.Name, Name: "before"})
	if err != nil {
		t.Fatal(err)
	}
	response := service.dispatchWithProgress(Request{Op: "snapshot.restore", Args: raw}, progress)
	if !response.OK {
		t.Fatalf("snapshot.restore failed: %s", response.Error)
	}

	stop := indexOfStage(*stages, "stopping cluster demo")
	restore := indexOfStage(*stages, "restoring 2 node disk(s) from snapshot before")
	start := indexOfStage(*stages, "starting cluster demo")
	if stop < 0 || restore < 0 || start < 0 {
		t.Fatalf("snapshot.restore narration = %q, want stop, restore and start", *stages)
	}
	if !(stop < restore && restore < start) {
		t.Fatalf("snapshot.restore narration = %q, want the stop before the restore before the start", *stages)
	}
}

// A request that did not ask for progress must not be narrated: the sink is
// nil, and every stage call has to survive that.
func TestSnapshotCreateWithoutProgressStaysSilent(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	liveDisks(t, item, "running")

	response := dispatchSnapshotCreateRequest(t, service, item.Name, "baseline")
	if !response.OK {
		t.Fatalf("snapshot.create failed: %s", response.Error)
	}
	if response.IsProgress() {
		t.Fatalf("final response carries a stage: %+v", response)
	}
}

func TestNodeAddNarratesTheDiskCloneAndTheLaunch(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	convergedClusterForFastNoop(t, item.Name)
	service.hostTotalMemory = func() (int, error) { return 1 << 20, nil }
	service.hostFreeMemory = func() (int, error) { return 1 << 20, nil }
	service.nodeIPLookup = func(string, int) string { return "" }
	service.provisionReconcile = func(context.Context, provision.Request) (provision.Result, error) {
		return provision.Result{StoragePhase: provision.StoragePhaseLive, StorageLive: true}, nil
	}
	progress, stages := recordStages()

	raw, err := json.Marshal(nodeArgs{Cluster: item.Name, Role: cluster.RoleWorker})
	if err != nil {
		t.Fatal(err)
	}
	response := service.dispatchWithProgress(Request{Op: "node.add", Args: raw}, progress)
	if !response.OK {
		t.Fatalf("node.add failed: %s", response.Error)
	}
	clone := indexOfStage(*stages, "cloning the disk for node")
	launch := indexOfStage(*stages, "starting node")
	if clone < 0 || launch < 0 || clone > launch {
		t.Fatalf("node.add narration = %q, want the disk clone before the launch", *stages)
	}
}

func TestNodeRemoveNarratesTheStopAndTheDiskDeletion(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	convergedClusterForFastNoop(t, item.Name)
	service.nodeIPLookup = func(string, int) string { return "" }
	service.nodeVolumeCount = func(context.Context, cluster.Cluster, string) (int, error) { return 0, nil }
	service.provisionReconcile = func(context.Context, provision.Request) (provision.Result, error) {
		return provision.Result{StoragePhase: provision.StoragePhaseLive, StorageLive: true}, nil
	}
	progress, stages := recordStages()

	raw, err := json.Marshal(nodeArgs{Cluster: item.Name, Name: item.Nodes[len(item.Nodes)-1].Name})
	if err != nil {
		t.Fatal(err)
	}
	response := service.dispatchWithProgress(Request{Op: "node.remove", Args: raw}, progress)
	if !response.OK {
		t.Fatalf("node.remove failed: %s", response.Error)
	}
	stop := indexOfStage(*stages, "stopping node")
	deletion := indexOfStage(*stages, "deleting the disk for node")
	if stop < 0 || deletion < 0 || stop > deletion {
		t.Fatalf("node.remove narration = %q, want the stop before the disk deletion", *stages)
	}
}

// The create's answer must outlast the boot it started: a node that is not
// answering yet holds the wait, and the success the CLI prints from it is only
// true once every node has (#263).
func TestWaitForNodesBootedHoldsUntilEveryNodeAnswers(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	restoreInterval(t, time.Millisecond)
	service.nodeIPLookup = func(string, int) string { return "10.0.0.2" }
	var probes int
	service.nodeProbe = func(string) ProbeResult {
		probes++
		// every node stays silent for the first pass over the cluster
		if probes <= len(item.Nodes) {
			return ProbeResult{}
		}
		return ProbeResult{Dialed: true, TLS: true, MaintenanceCert: true}
	}
	progress, stages := recordStages()

	if warning := service.waitForNodesBooted(item.Name, progress); warning != "" {
		t.Fatalf("wait warned about nodes that booted: %s", warning)
	}
	if probes <= len(item.Nodes) {
		t.Fatalf("probed %d times, want the wait to have re-probed the silent nodes", probes)
	}
	if indexOfStage(*stages, "waiting for 2 node(s) to boot") != 0 {
		t.Fatalf("narration = %q, want the wait announced first", *stages)
	}
	if indexOfStage(*stages, "reached maintenance") < 0 || indexOfStage(*stages, "all 2 node(s) booted") < 0 {
		t.Fatalf("narration = %q, want each node and the whole cluster reported booted", *stages)
	}
}

// A node that never answers must not hold the request forever: the cluster
// exists, so the wait gives up with an advisory finding instead of failing.
func TestWaitForNodesBootedWarnsWhenTheBudgetRunsOut(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 0)
	restoreInterval(t, time.Millisecond)
	service.nodeIPLookup = func(string, int) string { return "10.0.0.2" }
	service.nodeProbe = func(string) ProbeResult { return ProbeResult{} }

	warning := service.waitForNodesBooted(item.Name, nil)
	if !strings.Contains(warning, "1 of 1 node(s) had not answered") || !strings.Contains(warning, "tbx status demo") {
		t.Fatalf("warning = %q, want the unanswered nodes named with the status command", warning)
	}
}

// The wait runs on the request goroutine Shutdown waits for, so a cancelled
// lifecycle must end it at once: holding it for the rest of the boot budget
// would keep the daemon from closing its VMs before a supervisor kills it.
func TestWaitForNodesBootedGivesUpWhenTheLifecycleIsCancelled(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 0)
	// a real boot budget: only the cancellation may end this wait
	service.nodeIPLookup = func(string, int) string { return "10.0.0.2" }
	lifecycle, cancel := context.WithCancel(context.Background())
	service.lifecycleContext, service.lifecycleCancel = lifecycle, cancel
	service.nodeProbe = func(string) ProbeResult {
		service.cancelLifecycle()
		return ProbeResult{}
	}

	done := make(chan string, 1)
	go func() { done <- service.waitForNodesBooted(item.Name, nil) }()

	select {
	case warning := <-done:
		if !strings.Contains(warning, "before the daemon stopped waiting") {
			t.Fatalf("warning = %q, want the cancelled wait reported as such", warning)
		}
		if !strings.Contains(warning, "tbx status demo") {
			t.Fatalf("warning = %q, want the unanswered nodes named with the status command", warning)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("waitForNodesBooted did not return after the lifecycle was cancelled")
	}
}

// restoreInterval shrinks the boot wait so a test does not sleep through a
// real Talos boot, and restores both knobs afterward.
func restoreInterval(t *testing.T, interval time.Duration) {
	t.Helper()
	previousInterval, previousTimeout := nodeBootPollInterval, nodeBootTimeout
	nodeBootPollInterval, nodeBootTimeout = interval, 20*interval
	t.Cleanup(func() { nodeBootPollInterval, nodeBootTimeout = previousInterval, previousTimeout })
}
