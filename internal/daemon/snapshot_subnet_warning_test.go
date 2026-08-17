package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
)

// Snapshot create restarts the cluster, so it re-runs the host-subnet
// inspection. The finding belongs to the operator, not only to the daemon log.
func TestSnapshotCreateSurfacesTheRestartSubnetWarning(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	liveDisks(t, item, "running")
	service.subnetSources = gatewaySquatterSubnetSources(t)

	response := dispatchSnapshotCreateRequest(t, service, item.Name, "baseline")

	if !response.OK {
		t.Fatalf("snapshot.create failed: %s", response.Error)
	}
	status := decodeSnapshotStatus(t, response)
	if len(status.Snapshots) != 1 || status.Snapshots[0].Name != "baseline" {
		t.Fatalf("snapshots = %+v, want the baseline snapshot", status.Snapshots)
	}
	if !strings.Contains(status.Warning, "docker0") {
		t.Fatalf("SnapshotStatus.Warning = %q, want the restart's subnet finding", status.Warning)
	}
}

// Restore always ends powered on, so its restart raises the same finding, and
// it must ride alongside the volume gate's warning rather than replace it.
func TestSnapshotRestoreSurfacesTheRestartSubnetWarning(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	captureSnapshot(t, item, "before", "demo-cp-1")
	service.subnetSources = gatewaySquatterSubnetSources(t)
	service.nodeVolumeCount = func(context.Context, cluster.Cluster, string) (int, error) {
		return 1, nil
	}

	response := dispatchSnapshotRestoreRequest(t, service, item.Name, "before", true)

	if !response.OK {
		t.Fatalf("snapshot.restore failed: %s", response.Error)
	}
	status := decodeSnapshotStatus(t, response)
	if !strings.Contains(status.Warning, "docker0") {
		t.Fatalf("SnapshotStatus.Warning = %q, want the restart's subnet finding", status.Warning)
	}
	if !strings.Contains(status.Warning, "volume") {
		t.Fatalf("SnapshotStatus.Warning = %q, want the volume data-loss warning kept alongside it", status.Warning)
	}
}
