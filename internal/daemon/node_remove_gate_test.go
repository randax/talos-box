package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/provision"
)

func stubNodeMutationReconcile(service *Server) {
	service.provisionReconcile = func(context.Context, provision.Request) (provision.Result, error) {
		return provision.Result{StoragePhase: provision.StoragePhaseLive, StorageLive: true}, nil
	}
}

func decodeNodeStatus(t *testing.T, response Response) NodeStatus {
	t.Helper()
	var status NodeStatus
	if err := json.Unmarshal(response.Data, &status); err != nil {
		t.Fatalf("decode node.remove NodeStatus: %v", err)
	}
	return status
}

func dispatchNodeRemove(t *testing.T, service *Server, clusterName, nodeName string, force bool) Response {
	t.Helper()
	raw, err := json.Marshal(nodeArgs{Cluster: clusterName, Name: nodeName, Force: force})
	if err != nil {
		t.Fatal(err)
	}
	return service.dispatch(Request{Op: "node.remove", Args: raw})
}

func TestNodeRemoveRefusesWhenNodeHoldsTheOnlyVolumeCopy(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	var observedNode string
	service.nodeVolumeCount = func(_ context.Context, _ cluster.Cluster, node string) (int, error) {
		observedNode = node
		return 2, nil
	}

	response := dispatchNodeRemove(t, service, item.Name, "demo-worker-2", false)

	if response.OK {
		t.Fatal("node.remove succeeded, want refusal while the node holds volume data")
	}
	if observedNode != "demo-worker-2" {
		t.Fatalf("volume observation ran for node %q, want demo-worker-2", observedNode)
	}
	for _, want := range []string{"demo-worker-2", "2", "longhorn", "--force"} {
		if !strings.Contains(response.Error, want) {
			t.Fatalf("refusal %q does not mention %q", response.Error, want)
		}
	}
	reloaded, err := cluster.Load(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Nodes) != 3 {
		t.Fatalf("cluster node count after refused remove = %d, want 3", len(reloaded.Nodes))
	}
}

func TestNodeRemoveForceDeletesAndWarnsAboutVolumeData(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	stubNodeMutationReconcile(service)
	service.nodeVolumeCount = func(context.Context, cluster.Cluster, string) (int, error) {
		return 1, nil
	}

	response := dispatchNodeRemove(t, service, item.Name, "demo-worker-2", true)

	if !response.OK {
		t.Fatalf("forced node.remove failed: %s", response.Error)
	}
	status := decodeNodeStatus(t, response)
	for _, want := range []string{"demo-worker-2", "1", "longhorn"} {
		if !strings.Contains(status.Warning, want) {
			t.Fatalf("forced-remove warning %q does not mention %q", status.Warning, want)
		}
	}
	reloaded, err := cluster.Load(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Nodes) != 2 {
		t.Fatalf("cluster node count after forced remove = %d, want 2", len(reloaded.Nodes))
	}
}

func TestNodeRemoveKeepsDataLossWarningWhenReconcileFails(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	service.provisionReconcile = func(context.Context, provision.Request) (provision.Result, error) {
		return provision.Result{}, errors.New("api server unreachable")
	}
	service.nodeVolumeCount = func(context.Context, cluster.Cluster, string) (int, error) {
		return 1, nil
	}

	response := dispatchNodeRemove(t, service, item.Name, "demo-worker-2", true)

	if response.OK {
		t.Fatal("node.remove reported success despite a failed reconcile")
	}
	if !strings.Contains(response.Error, "permanently deletes the only copy of 1 longhorn volume") {
		t.Fatalf("reconcile-failure error %q lost the data-loss warning", response.Error)
	}
}

func TestNodeRemoveSerializesObservationWithRemoval(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	stubNodeMutationReconcile(service)
	firstObserving := make(chan struct{})
	releaseFirst := make(chan struct{})
	var secondSawNodes int
	service.nodeVolumeCount = func(_ context.Context, _ cluster.Cluster, node string) (int, error) {
		if node == "demo-worker-1" {
			close(firstObserving)
			<-releaseFirst
			return 0, nil
		}
		reloaded, err := cluster.Load(item.Name)
		if err != nil {
			t.Errorf("load cluster during second observation: %v", err)
			return 0, err
		}
		secondSawNodes = len(reloaded.Nodes)
		return 0, nil
	}

	firstDone := make(chan Response, 1)
	go func() { firstDone <- dispatchNodeRemove(t, service, item.Name, "demo-worker-1", false) }()
	<-firstObserving
	secondDone := make(chan Response, 1)
	go func() { secondDone <- dispatchNodeRemove(t, service, item.Name, "demo-worker-2", false) }()
	time.Sleep(50 * time.Millisecond)
	close(releaseFirst)

	if response := <-firstDone; !response.OK {
		t.Fatalf("first node.remove failed: %s", response.Error)
	}
	if response := <-secondDone; !response.OK {
		t.Fatalf("second node.remove failed: %s", response.Error)
	}
	// The second observation must run against the post-first-removal cluster:
	// an unserialized gate would have observed 3 nodes while the first
	// removal was still pending.
	if secondSawNodes != 2 {
		t.Fatalf("second observation saw %d nodes, want 2 (after the first removal completed)", secondSawNodes)
	}
}

func TestNodeRemoveProceedsWithWarningWhenVolumesAreUnverifiable(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	stubNodeMutationReconcile(service)
	service.nodeVolumeCount = func(context.Context, cluster.Cluster, string) (int, error) {
		return 0, errors.New("connection refused")
	}

	response := dispatchNodeRemove(t, service, item.Name, "demo-worker-2", false)

	if !response.OK {
		t.Fatalf("node.remove with unverifiable volumes failed: %s", response.Error)
	}
	status := decodeNodeStatus(t, response)
	if !strings.Contains(status.Warning, "demo-worker-2") || !strings.Contains(status.Warning, "could not verify") {
		t.Fatalf("unverifiable-remove warning %q does not state the possible data loss", status.Warning)
	}
}

func TestNodeRemoveSkipsVolumeObservationOnStoppedCluster(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	stubNodeMutationReconcile(service)
	delete(service.vms, item.Name)
	service.nodeVolumeCount = func(context.Context, cluster.Cluster, string) (int, error) {
		t.Fatal("volume observation ran against a stopped cluster")
		return 0, nil
	}

	response := dispatchNodeRemove(t, service, item.Name, "demo-worker-2", false)

	if !response.OK {
		t.Fatalf("node.remove on stopped cluster failed: %s", response.Error)
	}
	status := decodeNodeStatus(t, response)
	if !strings.Contains(status.Warning, "could not verify") {
		t.Fatalf("stopped-cluster remove warning %q does not state the possible data loss", status.Warning)
	}
}

func TestNodeRemoveObservationTimeoutDegradesToWarning(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	stubNodeMutationReconcile(service)
	lifecycleContext, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	service.lifecycleContext = lifecycleContext
	service.nodeVolumeCount = func(ctx context.Context, _ cluster.Cluster, _ string) (int, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}

	start := time.Now()
	response := dispatchNodeRemove(t, service, item.Name, "demo-worker-2", false)

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("node.remove blocked %v on a hanging observation", elapsed)
	}
	if !response.OK {
		t.Fatalf("node.remove with a hanging observation failed: %s", response.Error)
	}
	if status := decodeNodeStatus(t, response); !strings.Contains(status.Warning, "could not verify") {
		t.Fatalf("timeout-remove warning %q does not state the possible data loss", status.Warning)
	}
}

func TestNodeRemoveLegacyArgsWithoutForceStillGate(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	service.nodeVolumeCount = func(context.Context, cluster.Cluster, string) (int, error) {
		return 1, nil
	}

	// a pre-gate CLI sends no force field at all; the zero value must gate
	raw := json.RawMessage(`{"cluster":"` + item.Name + `","name":"demo-worker-2"}`)
	response := service.dispatch(Request{Op: "node.remove", Args: raw})

	if response.OK {
		t.Fatal("legacy node.remove args bypassed the volume gate")
	}
	if !strings.Contains(response.Error, "--force") {
		t.Fatalf("legacy-args refusal %q does not name the way forward", response.Error)
	}
}

func TestNodeRemoveSkipsVolumeObservationForUnknownNode(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	service.nodeVolumeCount = func(context.Context, cluster.Cluster, string) (int, error) {
		t.Fatal("volume observation ran for a node that is not a cluster member")
		return 0, nil
	}

	response := dispatchNodeRemove(t, service, item.Name, "demo-worker-9", false)

	if response.OK {
		t.Fatal("node.remove of an unknown node succeeded")
	}
	if strings.Contains(response.Error, "--force") {
		t.Fatalf("unknown-node error %q gated on volumes instead of naming the missing node", response.Error)
	}
}

func TestNodeRemoveSkipsVolumeObservationWithoutCSI(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	stubNodeMutationReconcile(service)
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	service.nodeVolumeCount = func(context.Context, cluster.Cluster, string) (int, error) {
		t.Fatal("volume observation ran for a cluster without csi")
		return 0, nil
	}

	response := dispatchNodeRemove(t, service, item.Name, "demo-worker-2", false)

	if !response.OK {
		t.Fatalf("node.remove without csi failed: %s", response.Error)
	}
	status := decodeNodeStatus(t, response)
	if status.Warning != "" {
		t.Fatalf("node.remove without csi warned %q, want no warning", status.Warning)
	}
}
