package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
)

// writeSnapshotDir plants a snapshot on disk the way a real one lands: a
// directory under the cluster's snapshots/. The listing reads directories, so
// the node disks a real CreateSnapshot clones are beside the point here.
func writeSnapshotDir(t *testing.T, clusterName, name string) {
	t.Helper()
	dir, err := cluster.Dir(clusterName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "snapshots", name), 0o755); err != nil {
		t.Fatal(err)
	}
}

// snapshot.list without a cluster answers the cleanup question every runbook's
// C5 asks — "are there any snapshots left anywhere?" — which has no cluster
// name to name once the clusters are gone (#417).
func TestSnapshotListWithoutAClusterListsEveryCluster(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 0)
	other, err := cluster.New("other", 1, 1, 0, cluster.NodeDefaults{MemoryMiB: 1, DiskGiB: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(other); err != nil {
		t.Fatal(err)
	}
	writeSnapshotDir(t, item.Name, "baseline")
	writeSnapshotDir(t, other.Name, "before")

	snapshots, err := service.snapshotList(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("snapshot.list without a cluster failed: %v", err)
	}
	if len(snapshots) != 2 {
		t.Fatalf("snapshot.list = %+v, want both clusters' snapshots", snapshots)
	}
	byCluster := map[string]string{}
	for _, snapshot := range snapshots {
		byCluster[snapshot.Cluster] = snapshot.Name
	}
	if byCluster["demo"] != "baseline" || byCluster["other"] != "before" {
		t.Fatalf("snapshot.list = %+v, want each snapshot tagged with its cluster", snapshots)
	}
}

// With no clusters at all the answer is an empty set, not a failure: that is
// exactly the post-destroy residue check (#417).
func TestSnapshotListWithoutAClusterIsEmptyWhenNothingExists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	service := &Server{}

	snapshots, err := service.snapshotList(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("snapshot.list with no clusters failed: %v", err)
	}
	if len(snapshots) != 0 {
		t.Fatalf("snapshot.list = %+v, want an empty set", snapshots)
	}
	if snapshots == nil {
		t.Fatal("snapshot.list returned nil, which marshals as null rather than []")
	}
}

// The filtered form stays what it was: one cluster's snapshots, untagged.
func TestSnapshotListForOneClusterStaysFiltered(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 0)
	writeSnapshotDir(t, item.Name, "baseline")

	raw, err := json.Marshal(snapshotArgs{Cluster: item.Name})
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := service.snapshotList(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].Name != "baseline" || snapshots[0].Cluster != "" {
		t.Fatalf("snapshot.list demo = %+v, want just baseline", snapshots)
	}
}
