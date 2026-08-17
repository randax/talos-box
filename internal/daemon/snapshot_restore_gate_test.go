package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
)

// captureSnapshot writes a snapshot that captured only the named nodes, the
// shape RestoreSnapshot reads: one disk per captured node plus the cluster
// state that pins the restored node set.
func captureSnapshot(t *testing.T, item cluster.Cluster, snapshot string, nodeNames ...string) {
	t.Helper()
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	captured := item
	captured.Nodes = nil
	for _, node := range item.Nodes {
		for _, name := range nodeNames {
			if node.Name == name {
				captured.Nodes = append(captured.Nodes, node)
			}
		}
	}
	snapshotDir := filepath.Join(dir, "snapshots", snapshot)
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, node := range captured.Nodes {
		if err := os.WriteFile(filepath.Join(snapshotDir, node.Name+".img"), []byte("snapshot-"+node.Name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	state, err := json.Marshal(captured)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshotDir, "cluster.json"), state, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, node := range item.Nodes {
		if err := os.WriteFile(filepath.Join(dir, node.Name+".img"), []byte("live-"+node.Name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func dispatchSnapshotRestoreRequest(t *testing.T, service *Server, clusterName, snapshot string, force bool) Response {
	t.Helper()
	raw, err := json.Marshal(snapshotArgs{Cluster: clusterName, Name: snapshot, Force: force})
	if err != nil {
		t.Fatal(err)
	}
	return service.dispatch(Request{Op: "snapshot.restore", Args: raw})
}

func decodeSnapshotStatus(t *testing.T, response Response) SnapshotStatus {
	t.Helper()
	var status SnapshotStatus
	if err := json.Unmarshal(response.Data, &status); err != nil {
		t.Fatalf("decode snapshot.restore status: %v", err)
	}
	return status
}

func TestSnapshotRestoreRefusesWhenVanishingNodeHoldsTheOnlyVolumeCopy(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	captureSnapshot(t, item, "before", "demo-cp-1", "demo-worker-1")
	var observed cluster.Cluster
	var observedNode string
	service.nodeVolumeCount = func(_ context.Context, observation cluster.Cluster, node string) (int, error) {
		observed, observedNode = observation, node
		return 2, nil
	}

	response := dispatchSnapshotRestoreRequest(t, service, item.Name, "before", false)

	if response.OK {
		t.Fatal("snapshot.restore succeeded, want refusal while a vanishing node holds volume data")
	}
	if observedNode != "demo-worker-2" {
		t.Fatalf("volume observation ran for node %q, want demo-worker-2", observedNode)
	}
	// no node remains to vouch for a copy: the restore reverts the surviving
	// nodes' disks too, so the observation must weigh the vanishing node alone
	if len(observed.Nodes) != 1 || observed.Nodes[0].Name != "demo-worker-2" {
		t.Fatalf("observation node set = %v, want demo-worker-2 alone", nodeNames(observed.Nodes))
	}
	for _, want := range []string{"demo", "before", "demo-worker-2 (2 volumes)", "longhorn", "--force"} {
		if !strings.Contains(response.Error, want) {
			t.Fatalf("refusal %q does not mention %q", response.Error, want)
		}
	}
	reloaded, err := cluster.Load(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Nodes) != 3 {
		t.Fatalf("cluster node count after refused restore = %d, want 3", len(reloaded.Nodes))
	}
}

func TestSnapshotRestoreForceRestoresAndWarnsAboutVolumeData(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	captureSnapshot(t, item, "before", "demo-cp-1", "demo-worker-1")
	service.nodeVolumeCount = func(context.Context, cluster.Cluster, string) (int, error) {
		return 1, nil
	}

	response := dispatchSnapshotRestoreRequest(t, service, item.Name, "before", true)

	if !response.OK {
		t.Fatalf("forced snapshot.restore failed: %s", response.Error)
	}
	status := decodeSnapshotStatus(t, response)
	for _, want := range []string{"demo-worker-2 (1 volume)", "longhorn"} {
		if !strings.Contains(status.Warning, want) {
			t.Fatalf("forced-restore warning %q does not mention %q", status.Warning, want)
		}
	}
	if len(status.Snapshots) != 1 || status.Snapshots[0].Name != "before" {
		t.Fatalf("restore snapshots = %+v, want the before snapshot", status.Snapshots)
	}
	reloaded, err := cluster.Load(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Nodes) != 2 {
		t.Fatalf("cluster node count after forced restore = %d, want 2", len(reloaded.Nodes))
	}
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "demo-worker-2.img")); !os.IsNotExist(err) {
		t.Fatalf("stat demo-worker-2.img after forced restore = %v, want the disk deleted", err)
	}
}

func TestSnapshotRestoreCountsEachVanishingNodeSeparately(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 3)
	captureSnapshot(t, item, "before", "demo-cp-1", "demo-worker-1")
	counts := map[string]int{"demo-worker-2": 2, "demo-worker-3": 1}
	service.nodeVolumeCount = func(_ context.Context, _ cluster.Cluster, node string) (int, error) {
		return counts[node], nil
	}

	response := dispatchSnapshotRestoreRequest(t, service, item.Name, "before", false)

	if response.OK {
		t.Fatal("snapshot.restore succeeded, want refusal while two vanishing nodes hold volume data")
	}
	// per-node counts keep the report honest: a volume replicated across both
	// vanishing nodes must not read as two lost volumes
	if !strings.Contains(response.Error, "demo-worker-2 (2 volumes), demo-worker-3 (1 volume)") {
		t.Fatalf("refusal %q does not report each vanishing node's volume count", response.Error)
	}
}

func TestSnapshotRestoreNamesOnlyVanishingNodesHoldingVolumes(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 3)
	captureSnapshot(t, item, "before", "demo-cp-1", "demo-worker-1")
	service.nodeVolumeCount = func(_ context.Context, _ cluster.Cluster, node string) (int, error) {
		if node == "demo-worker-2" {
			return 2, nil
		}
		return 0, nil
	}

	response := dispatchSnapshotRestoreRequest(t, service, item.Name, "before", false)

	if response.OK {
		t.Fatal("snapshot.restore succeeded, want refusal while a vanishing node holds volume data")
	}
	if strings.Contains(response.Error, "demo-worker-3") {
		t.Fatalf("refusal %q names a vanishing node that holds no volume data", response.Error)
	}
}

func TestSnapshotRestoreRefusesVerifiedVolumesDespiteAnUnverifiableNode(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 3)
	captureSnapshot(t, item, "before", "demo-cp-1", "demo-worker-1")
	service.nodeVolumeCount = func(_ context.Context, _ cluster.Cluster, node string) (int, error) {
		if node == "demo-worker-3" {
			return 0, errors.New("connection refused")
		}
		return 2, nil
	}

	response := dispatchSnapshotRestoreRequest(t, service, item.Name, "before", false)

	if response.OK {
		t.Fatal("an unverifiable node discarded another node's verified volume count")
	}
	if !strings.Contains(response.Error, "demo-worker-2 (2 volumes)") {
		t.Fatalf("refusal %q lost the verified count", response.Error)
	}
	if !strings.Contains(response.Error, "demo-worker-3 could not be verified") {
		t.Fatalf("refusal %q does not report the unverifiable node", response.Error)
	}
}

func TestSnapshotRestoreOfSnapshotWithoutStateLeavesTheClusterIntact(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	captureSnapshot(t, item, "before", "demo-cp-1", "demo-worker-1")
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "snapshots", "before", "cluster.json")); err != nil {
		t.Fatal(err)
	}
	service.nodeVolumeCount = func(context.Context, cluster.Cluster, string) (int, error) {
		t.Fatal("volume observation ran for a snapshot without readable state")
		return 0, nil
	}

	response := dispatchSnapshotRestoreRequest(t, service, item.Name, "before", false)

	if response.OK {
		t.Fatal("snapshot.restore of a snapshot without readable state succeeded")
	}
	// the gate skips an unreadable snapshot; the restore must refuse it before
	// deleting anything, or the ungated nodes are gone
	reloaded, err := cluster.Load(item.Name)
	if err != nil {
		t.Fatalf("live cluster state destroyed by a failed restore: %v", err)
	}
	if len(reloaded.Nodes) != 3 {
		t.Fatalf("cluster node count after the failed restore = %d, want 3", len(reloaded.Nodes))
	}
	if _, err := os.Stat(filepath.Join(dir, "demo-worker-2.img")); err != nil {
		t.Fatalf("stat demo-worker-2.img after the failed restore = %v, want the disk intact", err)
	}
}

func TestSnapshotRestoreKeepsDataLossWarningWhenRestoreFails(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	captureSnapshot(t, item, "before", "demo-cp-1", "demo-worker-1")
	service.hypervisor = &fakeHypervisor{
		architecture: hypervisor.ArchitectureARM64,
		launch: func(context.Context, hypervisor.Spec) (hypervisor.Machine, error) {
			return nil, errors.New("no hypervisor")
		},
	}
	service.nodeVolumeCount = func(context.Context, cluster.Cluster, string) (int, error) {
		return 1, nil
	}

	response := dispatchSnapshotRestoreRequest(t, service, item.Name, "before", true)

	if response.OK {
		t.Fatal("snapshot.restore reported success despite a failed cold boot")
	}
	if !strings.Contains(response.Error, "permanently deletes longhorn volume data on demo-worker-2 (1 volume)") {
		t.Fatalf("restore-failure error %q lost the data-loss warning", response.Error)
	}
}

func TestSnapshotRestoreProceedsWithWarningWhenVolumesAreUnverifiable(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	captureSnapshot(t, item, "before", "demo-cp-1", "demo-worker-1")
	service.nodeVolumeCount = func(context.Context, cluster.Cluster, string) (int, error) {
		return 0, errors.New("connection refused")
	}

	response := dispatchSnapshotRestoreRequest(t, service, item.Name, "before", false)

	if !response.OK {
		t.Fatalf("snapshot.restore with unverifiable volumes failed: %s", response.Error)
	}
	status := decodeSnapshotStatus(t, response)
	if !strings.Contains(status.Warning, "demo-worker-2") || !strings.Contains(status.Warning, "could not verify") {
		t.Fatalf("unverifiable-restore warning %q does not state the possible data loss", status.Warning)
	}
}

func TestSnapshotRestoreSkipsVolumeObservationOnStoppedCluster(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	captureSnapshot(t, item, "before", "demo-cp-1", "demo-worker-1")
	delete(service.vms, item.Name)
	service.nodeVolumeCount = func(context.Context, cluster.Cluster, string) (int, error) {
		t.Fatal("volume observation ran against a stopped cluster")
		return 0, nil
	}

	response := dispatchSnapshotRestoreRequest(t, service, item.Name, "before", false)

	if !response.OK {
		t.Fatalf("snapshot.restore on stopped cluster failed: %s", response.Error)
	}
	status := decodeSnapshotStatus(t, response)
	if !strings.Contains(status.Warning, "could not verify") {
		t.Fatalf("stopped-cluster restore warning %q does not state the possible data loss", status.Warning)
	}
}

func TestSnapshotRestoreSkipsVolumeObservationWhenNoNodeDisappears(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	captureSnapshot(t, item, "before", "demo-cp-1", "demo-worker-1", "demo-worker-2")
	service.nodeVolumeCount = func(context.Context, cluster.Cluster, string) (int, error) {
		t.Fatal("volume observation ran for a restore that deletes no node")
		return 0, nil
	}

	response := dispatchSnapshotRestoreRequest(t, service, item.Name, "before", false)

	if !response.OK {
		t.Fatalf("snapshot.restore of the same node set failed: %s", response.Error)
	}
	if status := decodeSnapshotStatus(t, response); status.Warning != "" {
		t.Fatalf("same-node-set restore warned %q, want no warning", status.Warning)
	}
}

func TestSnapshotRestoreSkipsVolumeObservationWithoutCSI(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	item.ProvisioningIntent = cluster.ProvisioningIntent{CNI: cluster.CNIFlannel}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	captureSnapshot(t, item, "before", "demo-cp-1", "demo-worker-1")
	service.nodeVolumeCount = func(context.Context, cluster.Cluster, string) (int, error) {
		t.Fatal("volume observation ran for a cluster without csi")
		return 0, nil
	}

	response := dispatchSnapshotRestoreRequest(t, service, item.Name, "before", false)

	if !response.OK {
		t.Fatalf("snapshot.restore without csi failed: %s", response.Error)
	}
	if status := decodeSnapshotStatus(t, response); status.Warning != "" {
		t.Fatalf("snapshot.restore without csi warned %q, want no warning", status.Warning)
	}
}

func TestSnapshotRestoreReportsMissingSnapshotWithoutGating(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	service.nodeVolumeCount = func(context.Context, cluster.Cluster, string) (int, error) {
		t.Fatal("volume observation ran for a snapshot that does not exist")
		return 0, nil
	}

	response := dispatchSnapshotRestoreRequest(t, service, item.Name, "nope", false)

	if response.OK {
		t.Fatal("snapshot.restore of a missing snapshot succeeded")
	}
	if !strings.Contains(response.Error, "does not exist") {
		t.Fatalf("missing-snapshot error = %q, want a does-not-exist error", response.Error)
	}
}

func TestSnapshotRestoreHasNoUngatedLockedPath(t *testing.T) {
	service, _ := runningLonghornClusterForNodeMutation(t, 1, 2)

	_, err := service.handle(Request{Op: "snapshot.restore"})

	if err == nil || !strings.Contains(err.Error(), "snapshot.restore") {
		t.Fatalf("locked handle of snapshot.restore = %v, want a dispatch refusal", err)
	}
}

func TestSnapshotRestoreForceWarnsAboutVerifiedAndUnverifiableNodesTogether(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 3)
	captureSnapshot(t, item, "before", "demo-cp-1", "demo-worker-1")
	service.nodeVolumeCount = func(_ context.Context, _ cluster.Cluster, node string) (int, error) {
		if node == "demo-worker-3" {
			return 0, errors.New("connection refused")
		}
		return 2, nil
	}

	response := dispatchSnapshotRestoreRequest(t, service, item.Name, "before", true)

	if !response.OK {
		t.Fatalf("forced snapshot.restore failed: %s", response.Error)
	}
	status := decodeSnapshotStatus(t, response)
	if !strings.Contains(status.Warning, "demo-worker-2 (2 volumes)") {
		t.Fatalf("forced-restore warning %q lost the verified count", status.Warning)
	}
	if !strings.Contains(status.Warning, "demo-worker-3 could not be verified") {
		t.Fatalf("forced-restore warning %q does not report the unverifiable node", status.Warning)
	}
}
