package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/config"
	"github.com/randax/talos-box/internal/hypervisor"
	"github.com/randax/talos-box/internal/provision"
)

// A sweep over a large cluster is not free: every silent node costs a dial
// timeout, and a cluster with enough nodes can spend more on one pass than the
// whole budget allows. The wait must therefore observe its context between
// nodes, not only after the complete pass — otherwise a shutdown waits for the
// sweep it interrupted.
func TestBootWaitObservesItsContextBetweenNodes(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	if len(item.Nodes) != 3 {
		t.Fatalf("cluster has %d nodes, want 3 for a multi-node sweep", len(item.Nodes))
	}
	restoreInterval(t, time.Millisecond, time.Minute)
	service.nodeIPLookup = func(string, int) string { return "10.0.0.2" }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var probes int
	service.nodeProbe = func(string) ProbeResult {
		probes++
		// the daemon goes down while the first node is being probed
		cancel()
		return ProbeResult{}
	}

	warning := service.waitForNodesBootedContext(ctx, item.Name, nil)
	if probes != 1 {
		t.Fatalf("probed %d nodes after the context was cancelled, want the sweep to stop at the first", probes)
	}
	// The nodes the sweep never reached are unanswered as far as the operator
	// is concerned, and the advisory has to name all of them.
	if !strings.Contains(warning, "3 of 3 node(s) had not answered") {
		t.Fatalf("warning = %q, want every unprobed node reported as unanswered", warning)
	}
	for _, node := range item.Nodes {
		if !strings.Contains(warning, node.Name) {
			t.Fatalf("warning = %q, want node %s named", warning, node.Name)
		}
	}
}

// The wait's own probe has to answer to the deadline too: bounding the loop
// while each probe blocks on its dial timeouts only moves the overrun.
func TestProbeHonoursACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A blackhole address the dial would otherwise sit on for its full timeout;
	// the cancelled context must beat it. No packet leaves the host.
	if probe := probeAPIDContext(ctx, "127.0.0.1"); probe.Dialed {
		t.Fatalf("probe = %+v against a cancelled context, want no dial", probe)
	}
}

// stoppedClusterForStart is a cluster whose VMs are all stopped — what
// `cluster start` boots. It declares no CNI, so nothing reconciles behind the
// launches and the boot wait is the only thing that can hold the request.
func stoppedClusterForStart(t *testing.T) (*Server, cluster.Cluster) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 1, cluster.NodeDefaults{MemoryMiB: 1, CPUs: 1, DiskGiB: 1})
	if err != nil {
		t.Fatal(err)
	}
	item.ImageArchitecture = string(hypervisor.ArchitectureARM64)
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	return &Server{
		hypervisor:      &fakeHypervisor{architecture: hypervisor.ArchitectureARM64},
		vms:             make(map[string]map[string]hypervisor.Machine),
		hostPressure:    noHostPressure,
		hostTotalMemory: plentifulHostMemory,
		hostFreeMemory:  plentifulHostMemory,
		nodeIPLookup:    func(string, int) string { return "10.0.0.2" },
		// the host's real routing table is not this test's subject
		subnetSources: emptySubnetSources(),
	}, item
}

func startRequest(t *testing.T, name string) Request {
	t.Helper()
	raw, err := json.Marshal(startArgs{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return Request{Op: "cluster.start", Args: raw}
}

// A start used to hand its reconcile a cluster whose VMs had just been
// launched, so the pass ran against nodes that could not answer yet and the
// operator watched a silent socket for the whole boot (#364). The start now
// waits for the same boot create waits for, and narrates it node by node.
func TestClusterStartDispatchHoldsItsAnswerForTheBootWait(t *testing.T) {
	service, item := stoppedClusterForStart(t)
	restoreInterval(t, time.Millisecond, 30*time.Second)
	var probes int
	service.nodeProbe = func(string) ProbeResult {
		probes++
		// every node stays silent for the first pass, exactly as a real one does
		if probes <= len(item.Nodes) {
			return ProbeResult{}
		}
		return ProbeResult{Dialed: true, TLS: true, MaintenanceCert: true}
	}
	progress, stages := recordStages()

	response := service.dispatchProvisioning(startRequest(t, item.Name), progress)
	if !response.OK {
		t.Fatalf("cluster.start failed: %s", response.Error)
	}
	if probes <= len(item.Nodes) {
		t.Fatalf("probed %d times, want the start to have re-probed the silent nodes", probes)
	}
	if indexOfStage(*stages, "waiting for 2 node(s) to boot") < 0 {
		t.Fatalf("narration = %q, want the boot wait announced", *stages)
	}
	if indexOfStage(*stages, "reached configured") < 0 && indexOfStage(*stages, "reached maintenance") < 0 {
		t.Fatalf("narration = %q, want each node reported as it comes up", *stages)
	}
	if indexOfStage(*stages, "all 2 node(s) booted") < 0 {
		t.Fatalf("narration = %q, want the whole cluster reported booted", *stages)
	}
	var summary ClusterSummary
	if err := json.Unmarshal(response.Data, &summary); err != nil {
		t.Fatal(err)
	}
	if len(summary.Warnings) != 0 {
		t.Fatalf("summary warnings = %q, want none for nodes that booted", summary.Warnings)
	}
}

// A node that never answers must not fail the start — the VMs are up — but the
// advisory has to reach the summary the CLI prints, as it does for create.
func TestClusterStartDispatchFoldsTheUnansweredNodeWarningIntoTheSummary(t *testing.T) {
	service, item := stoppedClusterForStart(t)
	restoreInterval(t, time.Millisecond, 20*time.Millisecond)
	service.nodeProbe = func(string) ProbeResult { return ProbeResult{} }

	response := service.dispatchProvisioning(startRequest(t, item.Name), nil)
	if !response.OK {
		t.Fatalf("cluster.start failed: %s", response.Error)
	}
	var summary ClusterSummary
	if err := json.Unmarshal(response.Data, &summary); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary.Warning, "2 of 2 node(s) had not answered") {
		t.Fatalf("summary warning = %q, want the unanswered nodes reported", summary.Warning)
	}
	if indexOfWarning(summary.Warnings, "tbx status demo") < 0 {
		t.Fatalf("summary warnings = %q, want the advisory as its own finding", summary.Warnings)
	}
}

// stoppedProvisionedClusterForStart is stoppedClusterForStart with a CNI, so a
// start schedules the reconcile the boot wait exists to protect.
func stoppedProvisionedClusterForStart(t *testing.T) (*Server, cluster.Cluster) {
	t.Helper()
	service, item := stoppedClusterForStart(t)
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	service.provisions = make(map[string]activeProvision)
	service.storagePhases = make(map[string]StoragePhase)
	service.storageStatusProbes = make(map[string]activeStorageProbe)
	service.storageProbeFailures = make(map[string]storageProbeFailure)
	return service, item
}

// bootWaitProbe answers silence for the first sweep and a booted node after it,
// exactly as a real boot does, and reports how many probes have been served.
func bootWaitProbe(service *Server, nodes int) *int {
	probes := 0
	service.nodeProbe = func(string) ProbeResult {
		probes++
		if probes <= nodes {
			return ProbeResult{}
		}
		return ProbeResult{Dialed: true, TLS: true, MaintenanceCert: true}
	}
	return &probes
}

// The boot wait is only worth anything if it runs *before* the reconcile: a
// pass that judges nodes launched microseconds earlier loses its fast no-op
// check and re-applies every chart the cluster already had (#364). The reconcile
// therefore has to observe nodes that already answer.
func TestClusterStartWaitsForTheBootBeforeItReconciles(t *testing.T) {
	service, item := stoppedProvisionedClusterForStart(t)
	restoreInterval(t, time.Millisecond, 30*time.Second)
	probes := bootWaitProbe(service, len(item.Nodes))
	probesAtReconcile := -1
	var observed []provision.Node
	service.provisionReconcile = func(context.Context, provision.Request) (provision.Result, error) {
		probesAtReconcile = *probes
		observed = service.observeProvisionNodes(item)
		return provision.Result{}, nil
	}

	response := service.dispatchProvisioning(startRequest(t, item.Name), nil)
	if !response.OK {
		t.Fatalf("cluster.start failed: %s", response.Error)
	}

	assertReconcileSawBootedNodes(t, item, probesAtReconcile, observed)
}

// `tbx up` is the file-driven way to reach the same start, and it used to skip
// the wait entirely: the declarative path is the common one, so the fix has to
// cover it (#364).
func TestUpWaitsForTheBootOfTheClustersItStartsBeforeReconciling(t *testing.T) {
	service, item := stoppedProvisionedClusterForStart(t)
	restoreInterval(t, time.Millisecond, 30*time.Second)
	probes := bootWaitProbe(service, len(item.Nodes))
	probesAtReconcile := -1
	var observed []provision.Node
	service.provisionReconcile = func(context.Context, provision.Request) (provision.Result, error) {
		probesAtReconcile = *probes
		observed = service.observeProvisionNodes(item)
		return provision.Result{}, nil
	}
	progress, stages := recordStages()

	response := service.dispatchProvisioning(upStartRequest(t, item.Name), progress)
	if !response.OK {
		t.Fatalf("up failed: %s", response.Error)
	}

	var actions []Action
	if err := json.Unmarshal(response.Data, &actions); err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Kind != ActionStart {
		t.Fatalf("up actions = %+v, want a single start", actions)
	}
	if indexOfStage(*stages, "waiting for 2 node(s) to boot") < 0 {
		t.Fatalf("narration = %q, want the boot wait announced", *stages)
	}
	assertReconcileSawBootedNodes(t, item, probesAtReconcile, observed)
}

// A node that never answers must not fail the up — the VMs are up — but the
// advisory has to reach the action the CLI prints, as it does for a start.
func TestUpFoldsTheUnansweredNodeWarningIntoItsAction(t *testing.T) {
	service, item := stoppedProvisionedClusterForStart(t)
	restoreInterval(t, time.Millisecond, 20*time.Millisecond)
	service.nodeProbe = func(string) ProbeResult { return ProbeResult{} }
	service.provisionReconcile = func(context.Context, provision.Request) (provision.Result, error) {
		return provision.Result{}, nil
	}

	response := service.dispatchProvisioning(upStartRequest(t, item.Name), nil)
	if !response.OK {
		t.Fatalf("up failed: %s", response.Error)
	}

	var actions []Action
	if err := json.Unmarshal(response.Data, &actions); err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 {
		t.Fatalf("up actions = %+v, want one", actions)
	}
	if !strings.Contains(actions[0].Warning, "2 of 2 node(s) had not answered") {
		t.Fatalf("action warning = %q, want the unanswered nodes reported", actions[0].Warning)
	}
	if indexOfWarning(actions[0].Warnings, "tbx status demo") < 0 {
		t.Fatalf("action warnings = %q, want the advisory as its own finding", actions[0].Warnings)
	}
}

// upStartRequest is the declarative request for a cluster that already exists
// and is stopped, which `up` plans as a start.
func upStartRequest(t *testing.T, name string) Request {
	t.Helper()
	raw, err := json.Marshal(upArgs{Clusters: []config.ClusterSpec{{
		Name:               name,
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return Request{Op: "up", Args: raw}
}

func assertReconcileSawBootedNodes(t *testing.T, item cluster.Cluster, probesAtReconcile int, observed []provision.Node) {
	t.Helper()
	if probesAtReconcile <= len(item.Nodes) {
		t.Fatalf("reconcile started after %d probes, want it to follow a completed boot wait", probesAtReconcile)
	}
	if len(observed) != len(item.Nodes) {
		t.Fatalf("reconcile observed %d nodes, want %d", len(observed), len(item.Nodes))
	}
	for _, node := range observed {
		if node.Phase != provision.Phase(PhaseMaintenance) && node.Phase != provision.Phase(PhaseConfigured) {
			t.Fatalf("reconcile observed node %s in phase %q, want a node that had already booted", node.Name, node.Phase)
		}
	}
}

// indexOfWarning reports where the first warning containing needle appears.
func indexOfWarning(warnings []string, needle string) int {
	for i, warning := range warnings {
		if strings.Contains(warning, needle) {
			return i
		}
	}
	return -1
}

// The boot wait must add its advisory to what the start already reported, not
// replace it: an old-style action carrying only the joined string keeps it.
func TestActionAddWarningsKeepsWhatTheActionAlreadyCarried(t *testing.T) {
	action := Action{Cluster: "demo", Kind: ActionStart, Warning: "host memory is tight"}

	action.addWarnings("2 of 2 node(s) had not answered", "")

	if len(action.Warnings) != 2 {
		t.Fatalf("warnings = %q, want both findings", action.Warnings)
	}
	if !strings.Contains(action.Warning, "host memory is tight") || !strings.Contains(action.Warning, "had not answered") {
		t.Fatalf("joined warning = %q, want both findings", action.Warning)
	}
}

// restoreKubernetesReadyWait pins the second half of the started-cluster wait so
// tests do not sleep through a real control-plane bringup.
func restoreKubernetesReadyWait(t *testing.T, interval, timeout time.Duration) {
	t.Helper()
	previousInterval, previousTimeout := kubernetesReadyPollInterval, kubernetesReadyWaitTimeout
	kubernetesReadyPollInterval, kubernetesReadyWaitTimeout = interval, timeout
	t.Cleanup(func() {
		kubernetesReadyPollInterval, kubernetesReadyWaitTimeout = previousInterval, previousTimeout
	})
}

// lateKubernetesReadiness stubs the readiness probe the fast no-op check leads
// with so it answers only from the successAfter'th call on — a control plane
// that comes up some polls after apid did, which is every stop/start.
func lateKubernetesReadiness(t *testing.T, successAfter int) *int {
	t.Helper()
	calls := 0
	previous := kubernetesReadyProbe
	t.Cleanup(func() { kubernetesReadyProbe = previous })
	kubernetesReadyProbe = func(context.Context, []byte, []string) error {
		calls++
		if calls < successAfter {
			return errors.New("kube-apiserver is not answering yet")
		}
		return nil
	}
	return &calls
}

// Ending the wait at apid was not enough: the fast no-op it protects asks
// whether provisioning is complete, and that starts with a Kubernetes readiness
// probe. apid answers seconds before kube-apiserver, etcd quorum and Node Ready
// do, so the fast check still lost and every chart was re-applied — #364's
// behaviour, one phase later. The wait now covers the Kubernetes half too.
func TestStartWaitsForKubernetesSoTheFastNoopCanFire(t *testing.T) {
	service, item := stoppedProvisionedClusterForStart(t)
	restoreInterval(t, time.Millisecond, 30*time.Second)
	restoreKubernetesReadyWait(t, time.Millisecond, 30*time.Second)
	bootWaitProbe(service, len(item.Nodes))
	stubConvergedProvisioning(t, item)
	readiness := lateKubernetesReadiness(t, 4)
	reconciled := false
	service.provisionReconcile = func(context.Context, provision.Request) (provision.Result, error) {
		reconciled = true
		return provision.Result{}, nil
	}

	response := service.dispatchProvisioning(startRequest(t, item.Name), nil)
	if !response.OK {
		t.Fatalf("cluster.start failed: %s", response.Error)
	}
	if *readiness < 4 {
		t.Fatalf("readiness probed %d times, want the wait to have retried until Kubernetes answered", *readiness)
	}
	if reconciled {
		t.Fatal("full pass ran even though Kubernetes came ready inside the wait; the fast no-op is still being lost")
	}
}

// The window is a wait, not a promise: a control plane that never answers must
// not hold the request open, and the full pass that follows is the right answer
// for a cluster that did not converge.
func TestKubernetesReadyWaitIsBoundedWhenReadinessNeverComes(t *testing.T) {
	service, item := stoppedProvisionedClusterForStart(t)
	restoreInterval(t, time.Millisecond, 30*time.Second)
	restoreKubernetesReadyWait(t, time.Millisecond, 50*time.Millisecond)
	bootWaitProbe(service, len(item.Nodes))
	stubConvergedProvisioning(t, item)
	lateKubernetesReadiness(t, math.MaxInt)
	reconciled := false
	service.provisionReconcile = func(context.Context, provision.Request) (provision.Result, error) {
		reconciled = true
		return provision.Result{}, nil
	}

	started := time.Now()
	response := service.dispatchProvisioning(startRequest(t, item.Name), nil)
	elapsed := time.Since(started)
	if !response.OK {
		t.Fatalf("cluster.start failed: %s", response.Error)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("start took %s, want the readiness wait bounded by its own window", elapsed)
	}
	if !reconciled {
		t.Fatal("full pass was skipped after the readiness window expired, want the pass a non-converged cluster needs")
	}
}

// A cluster that was never provisioned has no fast no-op to protect: waiting
// for a control plane that does not exist yet would add the whole window to
// every first start after a failed create.
func TestKubernetesReadyWaitIsSkippedWithoutCredentials(t *testing.T) {
	service, item := stoppedProvisionedClusterForStart(t)
	restoreKubernetesReadyWait(t, time.Millisecond, 30*time.Second)
	readiness := lateKubernetesReadiness(t, math.MaxInt)

	service.waitForKubernetesReady(item.Name, nil, nil)

	if *readiness != 0 {
		t.Fatalf("readiness probed %d times for a cluster with no credentials, want none", *readiness)
	}
}

// Registration is also replacement: a node or BGP mutation that supersedes the
// provisioning task cancels the task's context, and the readiness wait must
// end with it — otherwise the original start holds its request through the
// whole window only to discard the task afterwards.
func TestSupersededTaskEndsTheKubernetesReadyWait(t *testing.T) {
	service, item := stoppedProvisionedClusterForStart(t)
	stubConvergedProvisioning(t, item)
	// A window far past any test budget: only the supersession can end it.
	restoreKubernetesReadyWait(t, time.Millisecond, time.Hour)
	readiness := lateKubernetesReadiness(t, math.MaxInt)

	superseded, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		service.waitForKubernetesReady(item.Name, superseded, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("readiness wait outlived the provisioning task it protects")
	}
	if *readiness > 1 {
		t.Fatalf("readiness probed %d times after supersession, want at most the in-flight one", *readiness)
	}
}

// The boot half of the wait answers to the same supersession, and its advisory
// warning names that end apart from a blown budget.
func TestSupersededTaskEndsTheBootWait(t *testing.T) {
	service, item := stoppedProvisionedClusterForStart(t)
	superseded, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan string, 1)
	go func() { done <- service.waitForNodesBootedUnless(item.Name, superseded, nil) }()
	select {
	case warning := <-done:
		if !strings.Contains(warning, "superseded") {
			t.Fatalf("boot wait warning = %q, want the supersession named", warning)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("boot wait outlived the provisioning task it protects")
	}
}
