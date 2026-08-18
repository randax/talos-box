package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
)

// A restore swaps the node disks out from under any suspended memory, so a
// surviving save describes a machine state that no longer matches its disk:
// status would keep calling the cluster suspended and the hint would invite a
// resume that corrupts the restored disks. Every save in the directory goes,
// including one belonging to a node the snapshot does not contain.
func TestSnapshotRestoreDiscardsSavedState(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	captureSnapshot(t, item, "before", "demo-cp-1")
	service.nodeVolumeCount = func(context.Context, cluster.Cluster, string) (int, error) {
		return 0, nil
	}
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	saves := []string{
		saveStatePath(dir, "demo-cp-1"),
		saveStatePath(dir, "demo-worker-1"),
		// a node the snapshot never captured still has an invalid save
		saveStatePath(dir, "demo-worker-7"),
	}
	for _, path := range saves {
		if err := os.WriteFile(path, []byte("memory"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	response := dispatchSnapshotRestoreRequest(t, service, item.Name, "before", true)

	if !response.OK {
		t.Fatalf("snapshot.restore failed: %s", response.Error)
	}
	for _, path := range saves {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("saved state %s survived the restore (stat err = %v)", filepath.Base(path), err)
		}
	}
	if clusterHasSavedState(item.Name) {
		t.Fatal("the cluster still reports suspended memory after a restore")
	}
	status := decodeSnapshotStatus(t, response)
	if !strings.Contains(status.Warning, "discarded suspended memory state") {
		t.Fatalf("SnapshotStatus.Warning = %q, want the discard finding", status.Warning)
	}
}
