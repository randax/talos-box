package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
	"github.com/randax/talos-box/internal/provision"
)

// releasableProvision builds a cluster whose reconcile blocks until the caller
// releases it — including past its own cancellation, which is what a drain has
// to survive: cancelling only asks the goroutine to stop.
func releasableProvision(t *testing.T) (*Server, cluster.Cluster, provisionTask, <-chan struct{}, chan struct{}) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, CSI: cluster.CSILocalPath}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	reconcile := func(context.Context, provision.Request) (provision.Result, error) {
		close(started)
		<-release
		return provision.Result{StorageLive: true}, nil
	}

	lifecycle, cancel := context.WithCancel(context.Background())
	service := &Server{
		vms:                make(map[string]map[string]hypervisor.Machine),
		provisions:         make(map[string]activeProvision),
		provisionReconcile: reconcile,
		lifecycleContext:   lifecycle,
		lifecycleCancel:    cancel,
	}
	service.opMu.Lock()
	tasks := service.beginProvisionTasksLocked([]cluster.Cluster{item})
	service.opMu.Unlock()
	if len(tasks) != 1 {
		t.Fatalf("provision tasks = %d, want 1", len(tasks))
	}
	return service, item, tasks[0], started, release
}

// A destroy that only cancels the reconcile races it: the goroutine can still be
// writing into the directory being removed, and its epilogue re-records a
// storage phase for a cluster that no longer exists — which a recreated cluster
// of the same name would then inherit.
func TestDestroyWaitsForTheBackgroundReconcile(t *testing.T) {
	service, item, task, started, release := releasableProvision(t)
	reconcileDone := make(chan struct{})
	go func() {
		defer close(reconcileDone)
		_ = service.runProvisionTasks(nil, []provisionTask{task}, nil)
	}()
	waitForProvisionStart(t, started)

	destroyed := make(chan Response, 1)
	go func() {
		destroyed <- service.dispatch(Request{
			Op:   "cluster.destroy",
			Args: json.RawMessage(`{"name":"demo","force":true}`),
		})
	}()
	select {
	case response := <-destroyed:
		close(release)
		t.Fatalf("destroy finished while the reconcile was still running: %+v", response)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	select {
	case response := <-destroyed:
		if !response.OK {
			t.Fatalf("cluster.destroy failed: %s", response.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("destroy did not finish after the reconcile was released")
	}
	select {
	case <-reconcileDone:
	case <-time.After(time.Second):
		t.Fatal("the reconcile outlived the destroy that drained it")
	}

	service.opMu.Lock()
	phase, recorded := service.storagePhases[item.Name]
	_, stillActive := service.provisions[item.Name]
	service.opMu.Unlock()
	if recorded {
		t.Fatalf("destroyed cluster kept storage phase %q; a recreated cluster of the same name would inherit it", phase)
	}
	if stillActive {
		t.Fatal("destroyed cluster kept an active provision entry")
	}
}

// A drain with nothing in flight must not block: every operation that destroys
// or stops a cluster runs through it.
func TestDrainProvisionReturnsWhenNothingIsRunning(t *testing.T) {
	service := &Server{provisions: make(map[string]activeProvision)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.drainProvision("demo")
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("drainProvision blocked on a cluster with no active reconcile")
	}
}

// A superseded pass finishes with a phase describing a cluster state nobody is
// in any more. Letting it write parks a stale `live` over the `provisioning` the
// newer pass just recorded — and an empty phase coerces to `provisioning`, so
// even a no-result pass can overwrite.
func TestSupersededProvisionDoesNotRecordItsStoragePhase(t *testing.T) {
	service := &Server{
		provisions:    map[string]activeProvision{"demo": {generation: 2, cancel: func() {}}},
		storagePhases: map[string]StoragePhase{"demo": StoragePhaseProvisioning},
	}

	service.recordStoragePhaseIfCurrentLocked("demo", 1, StoragePhaseLive)

	if got := service.storagePhases["demo"]; got != StoragePhaseProvisioning {
		t.Fatalf("storage phase = %q after a superseded write, want the newer pass's %q", got, StoragePhaseProvisioning)
	}

	// The pass that still owns the cluster does get to write.
	service.recordStoragePhaseIfCurrentLocked("demo", 2, StoragePhaseLive)
	if got := service.storagePhases["demo"]; got != StoragePhaseLive {
		t.Fatalf("storage phase = %q for the active pass, want %q", got, StoragePhaseLive)
	}

	// A cancelled pass — stop, suspend, destroy — has no entry at all, and must
	// not resurrect one.
	delete(service.provisions, "demo")
	delete(service.storagePhases, "demo")
	service.recordStoragePhaseIfCurrentLocked("demo", 2, StoragePhaseLive)
	if phase, ok := service.storagePhases["demo"]; ok {
		t.Fatalf("a cancelled pass resurrected storage phase %q", phase)
	}
}
