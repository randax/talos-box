package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hostpressure"
	"github.com/randax/talos-box/internal/hypervisor"
	"github.com/randax/talos-box/internal/imagecache"
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
	if stop >= clone || clone >= restart || restart >= hint {
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
	if stop >= restore || restore >= start {
		t.Fatalf("snapshot.restore narration = %q, want the stop before the restore before the start", *stages)
	}
}

// The restore narrates the snapshot's node set, not the live one: a cluster
// grown since the snapshot restores fewer disks than it has nodes, and the
// nodes the snapshot never captured are deleted rather than restored (#273).
func TestSnapshotRestoreNarratesTheCapturedNodesAndTheOnesItDeletes(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	liveDisks(t, item, "running")
	if err := cluster.CreateSnapshot(item, "before"); err != nil {
		t.Fatal(err)
	}
	// the cluster grew after the snapshot, exactly as `tbx node add` grows it
	grown := item
	grown.Workers++
	grown.Nodes = append(append([]cluster.Node{}, item.Nodes...), cluster.Node{Name: "demo-worker-2", Role: cluster.RoleWorker})
	if err := cluster.Save(grown); err != nil {
		t.Fatal(err)
	}
	liveDisks(t, grown, "running")
	service.nodeVolumeCount = func(context.Context, cluster.Cluster, string) (int, error) { return 0, nil }
	progress, stages := recordStages()

	raw, err := json.Marshal(snapshotArgs{Cluster: grown.Name, Name: "before"})
	if err != nil {
		t.Fatal(err)
	}
	response := service.dispatchWithProgress(Request{Op: "snapshot.restore", Args: raw}, progress)
	if !response.OK {
		t.Fatalf("snapshot.restore failed: %s", response.Error)
	}
	restore := indexOfStage(*stages, "restoring 2 node disk(s) from snapshot before")
	if restore < 0 {
		t.Fatalf("snapshot.restore narration = %q, want the 2 captured disks reported, not the 3 live nodes", *stages)
	}
	if !strings.Contains((*stages)[restore], "deleting 1 node(s) it did not capture (demo-worker-2)") {
		t.Fatalf("restore stage = %q, want the node the restore deletes named", (*stages)[restore])
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
	// node.add keeps its reconcile on the request path, and that reconcile is
	// the longest stretch of the verb: unannounced it is a silent 25 minutes
	// after the last stage (#273).
	reconcile := indexOfStage(*stages, "reconciling flannel on cluster demo")
	if reconcile < 0 || reconcile < launch {
		t.Fatalf("node.add narration = %q, want the reconcile announced after the launch", *stages)
	}
	// The budget is named as a phase budget, so it cannot be confused with the
	// request-wide deadline the CLI heartbeat states (#423).
	if !strings.Contains((*stages)[reconcile], "CNI+storage budget 25m") {
		t.Fatalf("reconcile stage = %q, want the named storage budget the daemon holds it to", (*stages)[reconcile])
	}
	// The convergence hint closes a verb that left nodes booting. This one
	// blocks on its own reconcile and returns with the node configured, so
	// sending the operator to `tbx status` would point them away from a call
	// that still owns their terminal (#273).
	if indexOfStage(*stages, "tbx status") >= 0 {
		t.Fatalf("node.add narration = %q, want no convergence hint from a verb that waits for the reconcile", *stages)
	}
}

// A substrate-only cluster reconciles nothing, so the reconcile must not be
// narrated: the stage would promise work that never happens.
func TestNodeAddOnASubstrateOnlyClusterDoesNotNarrateAReconcile(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	item.ProvisioningIntent = cluster.ProvisioningIntent{}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	service.hostTotalMemory = func() (int, error) { return 1 << 20, nil }
	service.hostFreeMemory = func() (int, error) { return 1 << 20, nil }
	service.nodeIPLookup = func(string, int) string { return "" }
	progress, stages := recordStages()

	raw, err := json.Marshal(nodeArgs{Cluster: item.Name, Role: cluster.RoleWorker})
	if err != nil {
		t.Fatal(err)
	}
	response := service.dispatchWithProgress(Request{Op: "node.add", Args: raw}, progress)
	if !response.OK {
		t.Fatalf("node.add failed: %s", response.Error)
	}
	if indexOfStage(*stages, "reconciling") >= 0 {
		t.Fatalf("node.add narration = %q, want no reconcile announced for a substrate-only cluster", *stages)
	}
	// Nothing follows the launch here, so the hint is genuinely the closing
	// line: the verb is done and the node it started is still booting.
	hint := indexOfStage(*stages, "tbx status demo")
	if hint < 0 || hint != len(*stages)-1 {
		t.Fatalf("node.add narration = %q, want the convergence hint as the closing stage", *stages)
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
	restoreInterval(t, time.Millisecond, 30*time.Second)
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
	restoreInterval(t, time.Millisecond, 20*time.Millisecond)
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

// newBootWaitCreateServer is a server a cluster.create request can be
// dispatched against end to end: a cached disk for the default image, a fake
// hypervisor, and a stubbed host so the overcommit check cannot refuse on a
// small runner.
func newBootWaitCreateServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	path := filepath.Join(root, "aaa", DefaultTalosVersion, "arm64", "disk.raw")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Server{
		cache:           imagecache.New(root),
		hypervisor:      &fakeHypervisor{architecture: hypervisor.ArchitectureARM64},
		vms:             make(map[string]map[string]hypervisor.Machine),
		helperCheck:     func() error { return nil },
		hostPressure:    func(string) (hostpressure.Snapshot, error) { return hostpressure.Snapshot{}, nil },
		hostTotalMemory: func() (int, error) { return 1 << 20, nil },
		hostFreeMemory:  func() (int, error) { return 1 << 20, nil },
		nodeIPLookup:    func(string, int) string { return "10.0.0.2" },
		// the host's real routing table is not this test's subject, and a VPN
		// route on the developer's machine would otherwise add a warning
		subnetSources: emptySubnetSources(),
	}
}

// bootWaitCreateRequest is one substrate-only create of a single node: nothing
// to reconcile afterwards, so the boot wait is the only thing that can hold it.
func bootWaitCreateRequest(t *testing.T) Request {
	t.Helper()
	raw, err := json.Marshal(createArgs{
		Name: "booted", Schematic: "aaa",
		ControlPlanes: intPointer(1), Workers: intPointer(0),
		Node: cluster.NodeDefaults{MemoryMiB: 1, CPUs: 1, DiskGiB: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return Request{Op: "cluster.create", Args: raw}
}

// The whole point of #263 is that the create request itself does not answer
// until its nodes have booted. The wait has its own tests; this one pins that
// cluster.create is actually wired to it, so a refactor of the dispatch cannot
// drop the wait and stay green.
func TestClusterCreateDispatchHoldsItsAnswerForTheBootWait(t *testing.T) {
	service := newBootWaitCreateServer(t)
	restoreInterval(t, time.Millisecond, 30*time.Second)
	var probes int
	service.nodeProbe = func(string) ProbeResult {
		probes++
		// the node stays silent for the first pass, exactly as a real one does
		if probes == 1 {
			return ProbeResult{}
		}
		return ProbeResult{Dialed: true, TLS: true, MaintenanceCert: true}
	}
	progress, stages := recordStages()

	response := service.dispatchProvisioning(bootWaitCreateRequest(t), progress)
	if !response.OK {
		t.Fatalf("cluster.create failed: %s", response.Error)
	}
	if probes < 2 {
		t.Fatalf("probed %d times, want the create to have waited for the silent node", probes)
	}
	launch := indexOfStage(*stages, "starting 1 node(s)")
	wait := indexOfStage(*stages, "waiting for 1 node(s) to boot")
	booted := indexOfStage(*stages, "all 1 node(s) booted")
	if launch < 0 || wait < 0 || booted < 0 || launch >= wait || wait >= booted {
		t.Fatalf("cluster.create narration = %q, want the boot wait between the launch and the answer", *stages)
	}
	var summary ClusterSummary
	if err := json.Unmarshal(response.Data, &summary); err != nil {
		t.Fatal(err)
	}
	if len(summary.Warnings) != 0 {
		t.Fatalf("summary warnings = %q, want none for nodes that booted", summary.Warnings)
	}
}

// A node that never answers must not fail the create — the cluster exists —
// but the advisory has to reach the summary the CLI prints (#263).
func TestClusterCreateDispatchFoldsTheUnansweredNodeWarningIntoTheSummary(t *testing.T) {
	service := newBootWaitCreateServer(t)
	restoreInterval(t, time.Millisecond, 20*time.Millisecond)
	service.nodeProbe = func(string) ProbeResult { return ProbeResult{} }

	response := service.dispatchProvisioning(bootWaitCreateRequest(t), nil)
	if !response.OK {
		t.Fatalf("cluster.create failed: %s", response.Error)
	}
	var summary ClusterSummary
	if err := json.Unmarshal(response.Data, &summary); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary.Warning, "1 of 1 node(s) had not answered") {
		t.Fatalf("summary warning = %q, want the unanswered node reported", summary.Warning)
	}
	if len(summary.Warnings) != 1 || !strings.Contains(summary.Warnings[0], "tbx status booted") {
		t.Fatalf("summary warnings = %q, want the advisory as its own finding", summary.Warnings)
	}
}

// restoreInterval shrinks the boot wait so a test does not sleep through a
// real Talos boot, and restores both knobs afterward. The budget is the
// caller's own choice: a test that must see the deadline expire passes a tiny
// one, while a success-path test passes a generous one so a scheduling stall on
// a loaded runner cannot expire the budget it is not testing.
func restoreInterval(t *testing.T, interval, timeout time.Duration) {
	t.Helper()
	previousInterval, previousTimeout := nodeBootPollInterval, nodeBootTimeout
	nodeBootPollInterval, nodeBootTimeout = interval, timeout
	t.Cleanup(func() { nodeBootPollInterval, nodeBootTimeout = previousInterval, previousTimeout })
}
