package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

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
	service.observeProvisionGate("demo")(provision.GateLonghornScheduling, errors.New(`longhorn node "demo-cp-1" allowScheduling = true, want false`))

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
		vms:                 make(map[string]map[string]hypervisor.Machine),
		provisions:          make(map[string]activeProvision),
		storagePhases:       make(map[string]StoragePhase),
		storageStatusProbes: make(map[string]activeStorageProbe),
		lifecycleContext:    lifecycle,
		lifecycleCancel:     cancel,
		provisionReconcile: func(context.Context, provision.Request) (provision.Result, error) {
			return provision.Result{}, errors.New("reconcile longhorn storage: longhorn manager is not Ready")
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
