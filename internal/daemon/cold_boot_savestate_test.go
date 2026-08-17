package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
)

// savedStateExists reports whether a node still holds suspended memory.
func savedStateExists(t *testing.T, clusterName, nodeName string) bool {
	t.Helper()
	dir, err := cluster.Dir(clusterName)
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Stat(filepath.Join(dir, nodeName+saveStateSuffix))
	return err == nil
}

// TestStartClusterDiscardsStaleSavedState pins the cold-boot rule: a save is
// only consumed by a successful resume, so a start over one left the cluster
// reporting Suspended forever and the hint recommending a restore onto memory
// that no longer matched the running VMs.
func TestStartClusterDiscardsStaleSavedState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item := savedClusterForSubnetTest(t, "stale-save-start")
	writeSavedState(t, item)

	service := &Server{
		hypervisor:    &fakeHypervisor{},
		vms:           make(map[string]map[string]hypervisor.Machine),
		hostPressure:  noHostPressure,
		subnetSources: emptySubnetSources(),
	}
	raw, err := json.Marshal(startArgs{Name: item.Name})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.startCluster(raw)
	if err != nil {
		t.Fatalf("startCluster() error = %v", err)
	}
	for _, node := range item.Nodes {
		if savedStateExists(t, item.Name, node.Name) {
			t.Fatalf("start left %s's saved state behind", node.Name)
		}
	}
	if !strings.Contains(result.Warning, "discarded suspended memory state") {
		t.Fatalf("ClusterSummary.Warning = %q, want the discard warning", result.Warning)
	}
	if clusterHasSavedState(item.Name) {
		t.Fatal("the cluster still reports saved state after a cold boot")
	}
	statuses, err := service.status(mustRawJSON(t, statusArgs{Cluster: item.Name}))
	if err != nil {
		t.Fatal(err)
	}
	if statuses[0].Suspended {
		t.Fatal("status still reports the cold-booted cluster as suspended")
	}
}

// TestNodeStartDiscardsStaleSavedState is the per-node half of the same rule.
func TestNodeStartDiscardsStaleSavedState(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	stubNodeMutationReconcile(service)
	writeSavedState(t, item)
	delete(service.vms[item.Name], "demo-worker-1")

	response := dispatchNodeRunState(t, service, "node.start", item.Name, "demo-worker-1")

	if !response.OK {
		t.Fatalf("node.start failed: %s", response.Error)
	}
	if savedStateExists(t, item.Name, "demo-worker-1") {
		t.Fatal("node.start left the started node's saved state behind")
	}
	if !savedStateExists(t, item.Name, "demo-cp-1") {
		t.Fatal("node.start discarded a saved state for a node it was not asked about")
	}
	status := decodeNodeStatus(t, response)
	joined := strings.Join(status.Warnings, "\n")
	if !strings.Contains(joined, "discarded suspended memory state") {
		t.Fatalf("node.start warnings = %q, want the discard warning", status.Warnings)
	}
}
