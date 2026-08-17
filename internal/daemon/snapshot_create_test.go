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

// liveDisks writes a placeholder disk image per node, the shape CreateSnapshot
// clones, and returns the cluster directory holding them.
func liveDisks(t *testing.T, item cluster.Cluster, contents string) string {
	t.Helper()
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range item.Nodes {
		if err := os.WriteFile(filepath.Join(dir, node.Name+".img"), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func dispatchSnapshotCreateRequest(t *testing.T, service *Server, clusterName, snapshot string) Response {
	t.Helper()
	raw, err := json.Marshal(snapshotArgs{Cluster: clusterName, Name: snapshot})
	if err != nil {
		t.Fatal(err)
	}
	return service.dispatch(Request{Op: "snapshot.create", Args: raw})
}

func TestSnapshotCreateStopsRunningClusterBeforeCloningAndRestartsAfter(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	dir := liveDisks(t, item, "running")
	// each node rewrites its disk as it powers off, so a clone taken while the
	// guests were live is distinguishable from a crash-consistent one
	running := make(map[string]*fakeMachine, len(service.vms[item.Name]))
	for name, machine := range service.vms[item.Name] {
		node, fake := name, machine.(*fakeMachine)
		fake.onClose = func() {
			if err := os.WriteFile(filepath.Join(dir, node+".img"), []byte("stopped"), 0o600); err != nil {
				t.Errorf("write %s disk on stop: %v", node, err)
			}
		}
		running[name] = fake
	}

	response := dispatchSnapshotCreateRequest(t, service, item.Name, "baseline")

	if !response.OK {
		t.Fatalf("snapshot.create failed: %s", response.Error)
	}
	for name, machine := range running {
		if len(machine.calls) == 0 || machine.calls[0] != "stop" {
			t.Fatalf("node %s calls = %v, want the VM stopped before the clone", name, machine.calls)
		}
	}
	for _, node := range item.Nodes {
		clone, err := os.ReadFile(filepath.Join(dir, "snapshots", "baseline", node.Name+".img"))
		if err != nil {
			t.Fatalf("read snapshot disk for %s: %v", node.Name, err)
		}
		if string(clone) != "stopped" {
			t.Fatalf("snapshot disk for %s = %q, want the post-stop contents: cloning a live disk is not crash-consistent", node.Name, clone)
		}
	}
	backend := service.hypervisor.(*fakeHypervisor)
	if len(backend.specs) != len(item.Nodes) {
		t.Fatalf("relaunched %d nodes, want the running cluster restarted with %d", len(backend.specs), len(item.Nodes))
	}
	if !service.clusterRunning(item.Name) {
		t.Fatal("cluster is not running after the snapshot")
	}
}

func TestSnapshotCreateLeavesAStoppedClusterStopped(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	liveDisks(t, item, "cold")
	delete(service.vms, item.Name)

	response := dispatchSnapshotCreateRequest(t, service, item.Name, "baseline")

	if !response.OK {
		t.Fatalf("snapshot.create on a stopped cluster failed: %s", response.Error)
	}
	if backend := service.hypervisor.(*fakeHypervisor); len(backend.specs) != 0 {
		t.Fatalf("snapshot of a stopped cluster launched %d nodes, want none", len(backend.specs))
	}
	if service.clusterRunning(item.Name) {
		t.Fatal("snapshot of a stopped cluster left it running")
	}
	snapshots := decodeSnapshotStatus(t, response).Snapshots
	if len(snapshots) != 1 || snapshots[0].Name != "baseline" {
		t.Fatalf("snapshots = %+v, want the baseline snapshot", snapshots)
	}
}

func TestSnapshotCreateReportsARestartFailureAsASucceededSnapshot(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	dir := liveDisks(t, item, "running")
	service.hypervisor = &fakeHypervisor{
		architecture: hypervisor.ArchitectureARM64,
		launch: func(context.Context, hypervisor.Spec) (hypervisor.Machine, error) {
			return nil, errors.New("no hypervisor")
		},
	}

	response := dispatchSnapshotCreateRequest(t, service, item.Name, "baseline")

	if response.OK {
		t.Fatal("snapshot.create reported success despite a failed restart")
	}
	for _, want := range []string{`snapshot "baseline" was created`, "restart cluster", "no hypervisor", "tbx cluster start demo"} {
		if !strings.Contains(response.Error, want) {
			t.Fatalf("restart-failure error %q does not mention %q", response.Error, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "snapshots", "baseline", "cluster.json")); err != nil {
		t.Fatalf("stat snapshot state after the failed restart = %v, want the snapshot kept", err)
	}
}
