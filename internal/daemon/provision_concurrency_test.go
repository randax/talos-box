package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/config"
	"github.com/randax/talos-box/internal/hypervisor"
	"github.com/randax/talos-box/internal/provision"
)

func TestStatusRemainsResponsiveDuringProvisioning(t *testing.T) {
	service, item, task, started := blockedProvision(t)
	done := make(chan error, 1)
	go func() {
		done <- service.runProvisionTasks(&ClusterSummary{Name: item.Name}, []provisionTask{task}, nil)
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
		done <- service.runProvisionTasks(&ClusterSummary{Name: item.Name}, []provisionTask{task}, nil)
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
		done <- service.runProvisionTasks(&ClusterSummary{Name: item.Name}, []provisionTask{task}, nil)
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

func TestUpMaintenanceGateDoesNotHoldOperationLockWhileProbing(t *testing.T) {
	service, item, task, _ := blockedProvision(t)
	service.opMu.Lock()
	service.cancelProvisionLocked(task.item.Name)
	service.opMu.Unlock()
	item.ProvisioningIntent = cluster.ProvisioningIntent{}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	service.vms[item.Name] = map[string]hypervisor.Machine{
		item.Nodes[0].Name: &fakeMachine{active: true},
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var firstProbe sync.Once
	service.nodeIPLookup = func(string, int) string { return "172.30.0.2" }
	service.nodeProbe = func(string) ProbeResult {
		wait := false
		firstProbe.Do(func() {
			wait = true
			close(started)
		})
		if wait {
			<-release
		}
		return ProbeResult{Dialed: true, TLS: true, MaintenanceCert: true}
	}
	service.provisionReconcile = func(context.Context, provision.Request) (provision.Result, error) {
		return provision.Result{}, nil
	}
	args, err := json.Marshal(upArgs{Clusters: []config.ClusterSpec{{
		Name:               item.Name,
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan Response, 1)
	go func() {
		done <- service.dispatchProvisioning(Request{Op: "up", Args: args}, nil)
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
		t.Fatal("up maintenance probe held the daemon operation lock")
	}
	close(release)
	select {
	case response := <-done:
		if !response.OK {
			t.Fatalf("up failed: %s", response.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("up did not finish after maintenance probe release")
	}
}

func TestUpMaintenanceObservationDoesNotHoldOperationLockWhileLoadingCluster(t *testing.T) {
	service, item, task, _ := blockedProvision(t)
	service.opMu.Lock()
	service.cancelProvisionLocked(task.item.Name)
	service.opMu.Unlock()
	item.ProvisioningIntent = cluster.ProvisioningIntent{}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	service.maintenanceLoad = func(name string) (cluster.Cluster, error) {
		close(started)
		<-release
		return cluster.Load(name)
	}
	args, err := json.Marshal(upArgs{Clusters: []config.ClusterSpec{{
		Name:               item.Name,
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := service.observeUpMaintenance(args)
		done <- err
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
		<-done
		t.Fatal("cluster.Load held the daemon operation lock")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUpRejectsMaintenanceEvidenceThatChangesBeforeCommit(t *testing.T) {
	service, item, task, _ := blockedProvision(t)
	service.opMu.Lock()
	service.cancelProvisionLocked(task.item.Name)
	service.opMu.Unlock()
	item.ProvisioningIntent = cluster.ProvisioningIntent{}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	service.vms[item.Name] = map[string]hypervisor.Machine{
		item.Nodes[0].Name: &fakeMachine{active: true},
	}
	service.nodeIPLookup = func(string, int) string { return "172.30.0.2" }
	probes := 0
	service.nodeProbe = func(string) ProbeResult {
		probes++
		return ProbeResult{Dialed: true, TLS: true, MaintenanceCert: probes == 1}
	}
	args, err := json.Marshal(upArgs{Clusters: []config.ClusterSpec{{
		Name:               item.Name,
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	response := service.dispatchProvisioning(Request{Op: "up", Args: args}, nil)
	if response.OK || !strings.Contains(response.Error, "all nodes are in maintenance") {
		t.Fatalf("up response = %+v, want stale-maintenance rejection", response)
	}
	stored, err := cluster.Load(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CNI != "" {
		t.Fatalf("stored CNI = %q after rejected adoption, want empty", stored.CNI)
	}
}

func TestUpRejectsVMStateThatChangesAfterMaintenanceConfirmation(t *testing.T) {
	service, item, task, _ := blockedProvision(t)
	service.opMu.Lock()
	service.cancelProvisionLocked(task.item.Name)
	service.opMu.Unlock()
	item.ProvisioningIntent = cluster.ProvisioningIntent{}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	service.vms[item.Name] = map[string]hypervisor.Machine{
		item.Nodes[0].Name: &fakeMachine{active: true},
	}
	service.nodeIPLookup = func(string, int) string { return "172.30.0.2" }
	confirmed := make(chan struct{})
	release := make(chan struct{})
	probes := 0
	service.nodeProbe = func(string) ProbeResult {
		probes++
		if probes == 2 {
			close(confirmed)
			<-release
		}
		return ProbeResult{Dialed: true, TLS: true, MaintenanceCert: true}
	}
	args, err := json.Marshal(upArgs{Clusters: []config.ClusterSpec{{
		Name:               item.Name,
		ProvisioningIntent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan Response, 1)
	go func() {
		done <- service.dispatchProvisioning(Request{Op: "up", Args: args}, nil)
	}()
	waitForProvisionStart(t, confirmed)

	service.opMu.Lock()
	service.vms[item.Name][item.Nodes[0].Name] = &fakeMachine{active: false}
	service.opMu.Unlock()
	close(release)
	response := <-done
	if response.OK || !strings.Contains(response.Error, "all nodes are in maintenance") {
		t.Fatalf("up response = %+v, want changed-VM-state rejection", response)
	}
	stored, err := cluster.Load(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CNI != "" {
		t.Fatalf("stored CNI = %q after changed VM state, want empty", stored.CNI)
	}
}

func TestMaintenanceObservationRejectsNodeSetChanges(t *testing.T) {
	observation := maintenanceObservation{
		running: map[string]bool{"demo-cp-1": true},
		phases:  map[string]provision.Phase{"demo-cp-1": provision.PhaseMaintenance},
	}
	item := cluster.Cluster{Nodes: []cluster.Node{
		{Name: "demo-cp-1"},
		{Name: "demo-worker-1"},
	}}
	if observation.allNodesMaintenance(item, func(string) bool { return true }) {
		t.Fatal("maintenance observation accepted a node that was never probed")
	}
}

func TestMaintenanceObservationRequiresFreshMaintenancePhase(t *testing.T) {
	item := cluster.Cluster{Nodes: []cluster.Node{{Name: "demo-cp-1"}}}
	observation := maintenanceObservation{
		running: map[string]bool{"demo-cp-1": true},
		phases:  map[string]provision.Phase{"demo-cp-1": provision.PhaseConfigured},
	}
	if observation.allNodesMaintenance(item, func(string) bool { return true }) {
		t.Fatal("maintenance decision accepted a freshly observed configured node")
	}
}

func TestMaintenanceObservationRequiresMatchingVMState(t *testing.T) {
	item := cluster.Cluster{Nodes: []cluster.Node{{Name: "demo-cp-1"}}}
	observation := maintenanceObservation{
		running: map[string]bool{"demo-cp-1": true},
		phases:  map[string]provision.Phase{"demo-cp-1": provision.PhaseMaintenance},
	}
	if observation.allNodesMaintenance(item, func(string) bool { return false }) {
		t.Fatal("maintenance decision accepted VM state changed after confirmation")
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
