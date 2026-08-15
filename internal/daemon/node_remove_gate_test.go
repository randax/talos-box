package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

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
	if !strings.Contains(status.Warning, "demo-worker-2") || !strings.Contains(status.Warning, "may permanently delete") {
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
	if !strings.Contains(status.Warning, "may permanently delete") {
		t.Fatalf("stopped-cluster remove warning %q does not state the possible data loss", status.Warning)
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
