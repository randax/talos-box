package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
	"github.com/randax/talos-box/internal/provision"
)

func storageStatus(name string) ClusterStatus {
	return ClusterStatus{
		Name: name, Running: true, KubernetesReady: true,
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, CSI: cluster.CSILonghorn},
	}
}

// The storage hint used to blame the CSI readiness probe during a create where
// the probe had not run at all — the real blocker was the Longhorn scheduling
// gate. Status has to name the gate the pass is actually held at (#391).
func TestStorageStatusNamesTheGateThatIsActuallyBlocking(t *testing.T) {
	service := &Server{
		provisions:        map[string]activeProvision{"demo": {generation: 1, cancel: func() {}}},
		storagePhases:     map[string]StoragePhase{"demo": StoragePhaseProvisioning},
		provisionBlockers: map[string]provisionBlocker{},
	}
	service.observeProvisionGate("demo", 1)(provision.GateLonghornScheduling, errors.New(`longhorn node "demo-cp-1" allowScheduling = true, want false`))

	statuses := []ClusterStatus{storageStatus("demo")}
	service.refreshStoragePhases(statuses)

	if statuses[0].StoragePhase != StoragePhaseProvisioning {
		t.Fatalf("storage phase = %q, want %q", statuses[0].StoragePhase, StoragePhaseProvisioning)
	}
	if statuses[0].StorageGate != string(provision.GateLonghornScheduling) {
		t.Fatalf("storage gate = %q, want the blocking gate named", statuses[0].StorageGate)
	}
	if !strings.Contains(statuses[0].StorageError, "allowScheduling") {
		t.Fatalf("storage error = %q, want the gate's own observation, not an empty field", statuses[0].StorageError)
	}
	hint := storageHint(statuses[0])
	if !strings.Contains(hint, string(provision.GateLonghornScheduling)) {
		t.Fatalf("storage hint = %q, want it to name the blocking gate", hint)
	}
	if strings.Contains(hint, "readiness probe") {
		t.Fatalf("storage hint = %q, want no claim about a probe that never ran", hint)
	}
}

// With no gate reported yet the pass has nothing specific to say, and the hint
// stays the generic one rather than inventing a blocker.
func TestStorageStatusWithoutAGateKeepsTheGenericWait(t *testing.T) {
	service := &Server{
		provisions:    map[string]activeProvision{"demo": {generation: 1, cancel: func() {}}},
		storagePhases: map[string]StoragePhase{"demo": StoragePhaseProvisioning},
	}
	statuses := []ClusterStatus{storageStatus("demo")}
	service.refreshStoragePhases(statuses)
	if statuses[0].StorageGate != "" || statuses[0].StorageError != "" {
		t.Fatalf("gate = %q, error = %q, want both unset", statuses[0].StorageGate, statuses[0].StorageError)
	}
	if got := storageHint(statuses[0]); !strings.Contains(got, "waiting for the CSI readiness probe") {
		t.Fatalf("storage hint = %q, want the generic wait", got)
	}
}

// After an aborted create the storage phase used to sit at `provisioning`
// forever, so status kept reporting activity against a process that had exited
// — and the backoff loop was free to keep probing it (#395).
func TestAbortedProvisionSettlesStorageIntoATerminalFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNICilium, CSI: cluster.CSILonghorn}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	lifecycle, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelledProbes := 0
	service := &Server{
		// The cluster has to look running, or beginStorageStatusProbe returns
		// on its "not running" guard and the terminal-failure guard below is
		// never exercised.
		vms:                 map[string]map[string]hypervisor.Machine{"demo": {"demo-cp-1": &fakeMachine{active: true}}},
		provisions:          make(map[string]activeProvision),
		storagePhases:       make(map[string]StoragePhase),
		storageStatusProbes: make(map[string]activeStorageProbe),
		lifecycleContext:    lifecycle,
		lifecycleCancel:     cancel,
		provisionReconcile: func(context.Context, provision.Request) (provision.Result, error) {
			return provision.Result{}, provision.StorageStageError(errors.New("reconcile longhorn storage: longhorn manager is not Ready"))
		},
	}
	service.opMu.Lock()
	tasks := service.beginProvisionTasksLocked([]cluster.Cluster{item})
	// A probe left over from the abandoned pass keeps looping unless the
	// failure takes it down with it.
	service.storageStatusProbes["demo"] = activeStorageProbe{generation: 1, cancel: func() { cancelledProbes++ }}
	service.opMu.Unlock()

	if err := service.runProvisionTasks(nil, tasks, nil); err == nil {
		t.Fatal("runProvisionTasks() error = nil, want the reconcile failure")
	}
	if got := service.storagePhases["demo"]; got != StoragePhaseFailed {
		t.Fatalf("storage phase after an aborted pass = %q, want %q", got, StoragePhaseFailed)
	}
	if cancelledProbes != 1 {
		t.Fatalf("cancelled probes = %d, want the abandoned probe stopped", cancelledProbes)
	}
	if _, running := service.storageStatusProbes["demo"]; running {
		t.Fatal("a storage probe survived the aborted provision")
	}

	statuses := []ClusterStatus{storageStatus("demo")}
	service.refreshStoragePhases(statuses)
	if statuses[0].StoragePhase != StoragePhaseFailed {
		t.Fatalf("reported storage phase = %q, want %q", statuses[0].StoragePhase, StoragePhaseFailed)
	}
	if !strings.Contains(statuses[0].StorageError, "longhorn manager is not Ready") {
		t.Fatalf("storage error = %q, want the failure that ended the pass", statuses[0].StorageError)
	}
	hint := storageHint(statuses[0])
	if !strings.Contains(hint, "failed") || !strings.Contains(hint, "Nothing is retrying") {
		t.Fatalf("storage hint = %q, want a terminal failure, not provisioning in progress", hint)
	}
	// The recovery for a failed storage stage is the pass that re-drives it,
	// not a destroy: the cluster itself is fine.
	if !strings.Contains(hint, "tbx cluster start demo") {
		t.Fatalf("storage hint = %q, want the stop/start reset that re-runs the storage pass", hint)
	}
	if strings.Contains(hint, "tbx cluster destroy") {
		t.Fatalf("storage hint = %q, want no destroy advice for a healthy cluster", hint)
	}
	// Nothing re-probes a terminal failure: the pass that owned it has ended.
	service.beginStorageStatusProbe("demo")
	if _, running := service.storageStatusProbes["demo"]; running {
		t.Fatal("a terminal storage failure started another probe")
	}
	// A fresh provisioning pass clears it: the phase describes the pass, not
	// the cluster forever.
	service.opMu.Lock()
	service.beginProvisionTasksLocked([]cluster.Cluster{item})
	phase, message := service.storagePhases["demo"], service.storageFailures["demo"]
	service.opMu.Unlock()
	if phase != StoragePhaseProvisioning || message != "" {
		t.Fatalf("phase = %q, failure = %q after a new pass, want a clean provisioning state", phase, message)
	}
}

// A failure outside the storage stage says nothing about storage. A full-scope
// forced reconcile (a node add, or a BGP change inheriting a full pass) that
// dies at the Cilium VIP gate used to settle a live storage stack into the
// terminal failed phase — with the CNI error as the storage cause — which no
// status probe was then allowed to re-evaluate (#395).
func TestCNIStageFailureLeavesStorageProvisioning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNICilium, CSI: cluster.CSILonghorn}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	lifecycle, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := &Server{
		vms:                 map[string]map[string]hypervisor.Machine{"demo": {"demo-cp-1": &fakeMachine{active: true}}},
		provisions:          make(map[string]activeProvision),
		storagePhases:       make(map[string]StoragePhase),
		storageStatusProbes: make(map[string]activeStorageProbe),
		lifecycleContext:    lifecycle,
		lifecycleCancel:     cancel,
		provisionReconcile: func(context.Context, provision.Request) (provision.Result, error) {
			return provision.Result{}, errors.New("reconcile Cilium: LoadBalancer VIP never answered")
		},
	}
	service.opMu.Lock()
	tasks := service.beginProvisionTasksLocked([]cluster.Cluster{item})
	service.opMu.Unlock()

	if err := service.runProvisionTasks(nil, tasks, nil); err == nil {
		t.Fatal("runProvisionTasks() error = nil, want the reconcile failure")
	}
	if got := service.storagePhases["demo"]; got != StoragePhaseProvisioning {
		t.Fatalf("storage phase after a CNI-stage failure = %q, want %q", got, StoragePhaseProvisioning)
	}
	if got := service.storageFailures["demo"]; got != "" {
		t.Fatalf("storage failure = %q, want a CNI failure not recorded as a storage cause", got)
	}

	statuses := []ClusterStatus{storageStatus("demo")}
	service.refreshStoragePhases(statuses)
	if statuses[0].StoragePhase != StoragePhaseProvisioning {
		t.Fatalf("reported storage phase = %q, want %q", statuses[0].StoragePhase, StoragePhaseProvisioning)
	}
	if strings.Contains(statuses[0].StorageError, "Cilium") {
		t.Fatalf("storage error = %q, want no CNI failure attributed to storage", statuses[0].StorageError)
	}
	// The status probe is still allowed to re-establish `live` on its own:
	// either it is still registered, or it already ran and recorded its own
	// outcome. A terminal phase would have let neither happen.
	service.opMu.Lock()
	_, probing := service.storageStatusProbes["demo"]
	_, probed := service.storageProbeFailures["demo"]
	probedLive := service.storagePhases["demo"] == StoragePhaseLive
	service.opMu.Unlock()
	if !probing && !probed && !probedLive {
		t.Fatal("refreshStoragePhases started no status probe: nothing would re-establish a live storage stack")
	}
}

// A pass that has been superseded or cancelled can still land one last gate
// observation: its in-flight check returns `context canceled` and the observer
// then blocks on opMu until the operation that displaced it releases the lock.
// Recording it would report a dead pass's wait as the live cluster's blocker.
func TestSupersededPassDoesNotOverwriteTheLiveBlocker(t *testing.T) {
	service := &Server{
		provisions:        map[string]activeProvision{"demo": {generation: 2, cancel: func() {}}},
		storagePhases:     map[string]StoragePhase{"demo": StoragePhaseProvisioning},
		provisionBlockers: map[string]provisionBlocker{},
	}
	service.observeProvisionGate("demo", 2)(provision.GateLonghornScheduling, errors.New(`longhorn node "demo-cp-1" allowScheduling = true, want false`))
	// The generation-1 pass, already replaced, reports its cancellation.
	service.observeProvisionGate("demo", 1)(provision.GateLonghorn, context.Canceled)

	statuses := []ClusterStatus{storageStatus("demo")}
	service.refreshStoragePhases(statuses)

	if statuses[0].StorageGate != string(provision.GateLonghornScheduling) {
		t.Fatalf("storage gate = %q, want the live pass's gate", statuses[0].StorageGate)
	}
	if strings.Contains(statuses[0].StorageError, "canceled") {
		t.Fatalf("storage error = %q, want the live pass's observation, not a cancelled pass's", statuses[0].StorageError)
	}
}

// After the cancelling operation retired the pass there is no entry left to
// speak for: a late observation must not resurrect one, or `tbx status` reports
// a gate for a pass that ended before the suspend.
func TestCancelledPassDoesNotResurrectItsBlocker(t *testing.T) {
	service := &Server{
		provisions:        map[string]activeProvision{},
		storagePhases:     map[string]StoragePhase{"demo": StoragePhaseProvisioning},
		provisionBlockers: map[string]provisionBlocker{},
	}
	service.observeProvisionGate("demo", 1)(provision.GateLonghorn, context.Canceled)
	if len(service.provisionBlockers) != 0 {
		t.Fatalf("provisionBlockers = %v, want no entry for a retired pass", service.provisionBlockers)
	}
}

// With no pass running the storage arm is reached on !KubernetesReady alone, so
// a blocker left over from an earlier pass has to age out rather than be quoted
// as the current wait.
func TestStaleBlockerIsNotReportedWithoutARunningPass(t *testing.T) {
	service := &Server{
		storagePhases: map[string]StoragePhase{"demo": StoragePhaseProvisioning},
		provisionBlockers: map[string]provisionBlocker{"demo": {
			gate:    provision.GateLonghorn,
			message: "context canceled",
			at:      time.Now().Add(-2 * provisionBlockerFreshness),
		}},
		storageProbeFailures: map[string]storageProbeFailure{},
	}
	status := storageStatus("demo")
	status.KubernetesReady = false
	statuses := []ClusterStatus{status}
	service.refreshStoragePhases(statuses)

	if statuses[0].StorageGate != "" || statuses[0].StorageError != "" {
		t.Fatalf("gate = %q, error = %q, want a stale blocker ignored", statuses[0].StorageGate, statuses[0].StorageError)
	}
}

// A running pass has un-gated stretches — the control plane spends minutes
// bringing up etcd and kube-apiserver — during which the last recorded gate is
// simply out of date. Quoting it would tell an operator the cluster is waiting
// on a gate the pass moved past, which is the mis-steering #391 removed.
func TestStaleBlockerIsNotReportedWhileAPassRuns(t *testing.T) {
	service := &Server{
		provisions:    map[string]activeProvision{"demo": {generation: 1, cancel: func() {}}},
		storagePhases: map[string]StoragePhase{"demo": StoragePhaseProvisioning},
		provisionBlockers: map[string]provisionBlocker{"demo": {
			gate:    provision.GateMachineConfig,
			message: "nodes not configured yet: demo-cp-1",
			at:      time.Now().Add(-2 * provisionBlockerFreshness),
		}},
		storageProbeFailures: map[string]storageProbeFailure{},
	}
	statuses := []ClusterStatus{storageStatus("demo")}
	service.refreshStoragePhases(statuses)

	if statuses[0].StoragePhase != StoragePhaseProvisioning {
		t.Fatalf("phase = %q, want provisioning while the pass runs", statuses[0].StoragePhase)
	}
	if statuses[0].StorageGate != "" || statuses[0].StorageError != "" {
		t.Fatalf("gate = %q, error = %q, want a stale gate dropped", statuses[0].StorageGate, statuses[0].StorageError)
	}
}
