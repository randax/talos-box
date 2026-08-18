package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/provision"
)

// convergedClusterForFastNoop arranges the on-disk credentials and probe stubs
// that make provisioningComplete report a fully converged cluster.
func convergedClusterForFastNoop(t *testing.T, name string) {
	t.Helper()
	dir, err := cluster.Dir(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"secrets.yaml", "talosconfig", "kubeconfig"} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	originalReady := kubernetesReadyProbe
	originalStorage := storageConvergenceProbe
	originalScheduling := controlPlaneSchedulingProbe
	t.Cleanup(func() {
		kubernetesReadyProbe = originalReady
		storageConvergenceProbe = originalStorage
		controlPlaneSchedulingProbe = originalScheduling
	})
	kubernetesReadyProbe = func(context.Context, []byte, []string) error { return nil }
	storageConvergenceProbe = func(context.Context, cluster.Cluster, []byte) error { return nil }
	controlPlaneSchedulingProbe = func(context.Context, []byte, cluster.Cluster) error { return nil }
}

// A topology change alters the machine config that already-configured nodes
// need (a worker-less cluster schedules on its control planes), so it must run
// a full reconcile pass even though every observed outcome is still healthy.
func TestNodeRemoveForcesFullReconcileDespiteConvergedCluster(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	convergedClusterForFastNoop(t, item.Name)
	service.nodeVolumeCount = func(context.Context, cluster.Cluster, string) (int, error) {
		return 0, nil
	}
	if !service.provisioningComplete(item) {
		t.Fatal("test arrangement does not reach the fast no-op path")
	}

	reconciledNodes := -1
	service.provisionReconcile = func(_ context.Context, request provision.Request) (provision.Result, error) {
		reconciledNodes = len(request.Cluster.Nodes)
		return provision.Result{StoragePhase: provision.StoragePhaseLive, StorageLive: true}, nil
	}

	response := dispatchNodeRemove(t, service, item.Name, "demo-worker-1", false)

	if !response.OK {
		t.Fatalf("node.remove failed: %s", response.Error)
	}
	service.backgroundProvisions.Wait()
	if reconciledNodes != 1 {
		t.Fatalf("node.remove reconciled %d nodes, want 1: the worker-less crossing must bypass the fast no-op", reconciledNodes)
	}
}

func TestNodeAddForcesFullReconcileDespiteConvergedCluster(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 0)
	convergedClusterForFastNoop(t, item.Name)
	if !service.provisioningComplete(item) {
		t.Fatal("test arrangement does not reach the fast no-op path")
	}

	reconciledNodes := -1
	service.provisionReconcile = func(_ context.Context, request provision.Request) (provision.Result, error) {
		reconciledNodes = len(request.Cluster.Nodes)
		return provision.Result{StoragePhase: provision.StoragePhaseLive, StorageLive: true}, nil
	}

	raw, err := json.Marshal(nodeArgs{Cluster: item.Name, Name: "demo-worker-1", Role: cluster.RoleWorker})
	if err != nil {
		t.Fatal(err)
	}
	response := service.dispatch(Request{Op: "node.add", Args: raw})

	if !response.OK {
		t.Fatalf("node.add failed: %s", response.Error)
	}
	if reconciledNodes != 2 {
		t.Fatalf("node.add reconciled %d nodes, want 2: the first-worker crossing must bypass the fast no-op", reconciledNodes)
	}
}

// The forced pass is scoped to topology mutations: a rerun of an untouched
// converged cluster stays a fast no-op.
func TestProvisionCNIKeepsFastNoopWithoutForce(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	convergedClusterForFastNoop(t, item.Name)
	service.provisionReconcile = func(context.Context, provision.Request) (provision.Result, error) {
		t.Fatal("converged cluster ran a full reconcile pass without force")
		return provision.Result{}, nil
	}

	_, _, phase, err := service.provisionCNI(context.Background(), item, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if phase != StoragePhaseLive {
		t.Fatalf("fast no-op storage phase = %q, want %q", phase, StoragePhaseLive)
	}
}

// A mutation pass that failed or was superseded before it patched the control
// planes leaves drift behind while every health probe still passes. Recovery
// must not depend on the in-memory force flag: rerunning tbx up observes the
// scheduling posture and runs a full pass.
func TestProvisionCNIRecoversDriftedControlPlaneSchedulingWithoutForce(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	convergedClusterForFastNoop(t, item.Name)
	controlPlaneSchedulingProbe = func(context.Context, []byte, cluster.Cluster) error {
		return errors.New("control plane is still schedulable")
	}
	if service.provisioningComplete(item) {
		t.Fatal("drifted control plane scheduling was reported as converged")
	}

	reconciled := false
	service.provisionReconcile = func(context.Context, provision.Request) (provision.Result, error) {
		reconciled = true
		return provision.Result{StoragePhase: provision.StoragePhaseLive, StorageLive: true}, nil
	}

	if _, _, _, err := service.provisionCNI(context.Background(), item, false, false); err != nil {
		t.Fatal(err)
	}
	if !reconciled {
		t.Fatal("drifted control plane scheduling took the fast no-op path")
	}
}
