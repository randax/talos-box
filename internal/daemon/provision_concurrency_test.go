package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
	"github.com/randax/talos-box/internal/provision"
)

func TestStatusRemainsResponsiveDuringProvisioning(t *testing.T) {
	service, item, task, started := blockedProvision(t)
	done := make(chan error, 1)
	go func() {
		done <- service.runProvisionTasks(&ClusterSummary{Name: item.Name}, []provisionTask{task})
	}()
	waitForProvisionStart(t, started)

	response := make(chan Response, 1)
	go func() {
		response <- service.dispatch(Request{Op: "status", Args: json.RawMessage(`{"cluster":"demo"}`)})
	}()
	select {
	case got := <-response:
		if !got.OK {
			t.Fatalf("status failed during provisioning: %s", got.Error)
		}
	case <-time.After(time.Second):
		t.Fatal("status blocked behind long-running provisioning")
	}

	service.opMu.Lock()
	service.cancelProvisionLocked(item.Name)
	service.opMu.Unlock()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("provisioning error = %v, want cancellation", err)
	}
}

func TestStopCancelsActiveProvisioning(t *testing.T) {
	service, item, task, started := blockedProvision(t)
	service.storagePhases = map[string]StoragePhase{item.Name: StoragePhaseLive}
	done := make(chan error, 1)
	go func() {
		done <- service.runProvisionTasks(&ClusterSummary{Name: item.Name}, []provisionTask{task})
	}()
	waitForProvisionStart(t, started)

	response := service.dispatch(Request{Op: "cluster.stop", Args: json.RawMessage(`{"name":"demo"}`)})
	if !response.OK {
		t.Fatalf("stop failed: %s", response.Error)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("provisioning error = %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel active provisioning")
	}
	if _, ok := service.storagePhases[item.Name]; ok {
		t.Fatal("stop retained stale storage-live observation")
	}
}

func TestShutdownCancelsActiveProvisioning(t *testing.T) {
	service, item, task, started := blockedProvision(t)
	done := make(chan error, 1)
	go func() {
		done <- service.runProvisionTasks(&ClusterSummary{Name: item.Name}, []provisionTask{task})
	}()
	waitForProvisionStart(t, started)

	if err := service.Shutdown(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("provisioning error = %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel active provisioning")
	}
}

func TestProvisionObserverDoesNotHoldOperationLockWhileProbing(t *testing.T) {
	service, item, _, _ := blockedProvision(t)
	service.vms[item.Name] = map[string]hypervisor.Machine{
		item.Nodes[0].Name: &fakeMachine{active: true},
	}
	started := make(chan struct{})
	release := make(chan struct{})
	service.nodeIPLookup = func(string, int) string { return "172.30.0.2" }
	service.nodeProbe = func(string) ProbeResult {
		close(started)
		<-release
		return ProbeResult{Dialed: true, TLS: true}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.observeProvisionNodes(item)
	}()
	waitForProvisionStart(t, started)

	lockAcquired := make(chan struct{})
	go func() {
		service.opMu.Lock()
		close(lockAcquired)
		service.opMu.Unlock()
	}()
	select {
	case <-lockAcquired:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("node probe held the daemon operation lock")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("node observation did not finish after probe release")
	}
}

func TestStatusDoesNotHoldOperationLockWhileProbing(t *testing.T) {
	service, item, _, _ := blockedProvision(t)
	service.vms[item.Name] = map[string]hypervisor.Machine{
		item.Nodes[0].Name: &fakeMachine{active: true},
	}
	started := make(chan struct{})
	release := make(chan struct{})
	service.nodeIPLookup = func(string, int) string { return "172.30.0.2" }
	service.nodeProbe = func(string) ProbeResult {
		close(started)
		<-release
		return ProbeResult{Dialed: true, TLS: true}
	}
	done := make(chan Response, 1)
	go func() {
		done <- service.dispatchStatus(Request{Op: "status", Args: json.RawMessage(`{"cluster":"demo"}`)})
	}()
	waitForProvisionStart(t, started)

	lockAcquired := make(chan struct{})
	go func() {
		service.opMu.Lock()
		close(lockAcquired)
		service.opMu.Unlock()
	}()
	select {
	case <-lockAcquired:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("status node probe held the daemon operation lock")
	}
	close(release)
	select {
	case response := <-done:
		if !response.OK {
			t.Fatalf("status failed: %s", response.Error)
		}
	case <-time.After(time.Second):
		t.Fatal("status did not finish after probe release")
	}
}

func blockedProvision(t *testing.T) (*Server, cluster.Cluster, provisionTask, <-chan struct{}) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	reconcile := func(ctx context.Context, _ provision.Request) (provision.Result, error) {
		close(started)
		<-ctx.Done()
		return provision.Result{}, ctx.Err()
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
	return service, item, tasks[0], started
}

func waitForProvisionStart(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provisioning did not start")
	}
}
