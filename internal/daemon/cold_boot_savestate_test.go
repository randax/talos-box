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

// TestStartClusterKeepsSavedStateWhenLaunchFails pins the ordering: the launch
// never reads the save, so discarding before it would destroy the memory that a
// rolled-back start still needs `cluster resume` to be able to use.
func TestStartClusterKeepsSavedStateWhenLaunchFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item := savedClusterForSubnetTest(t, "failed-start-save")
	writeSavedState(t, item)

	service := &Server{
		hypervisor: &fakeHypervisor{launch: func(context.Context, hypervisor.Spec) (hypervisor.Machine, error) {
			return nil, errors.New("no hypervisor today")
		}},
		vms:           make(map[string]map[string]hypervisor.Machine),
		hostPressure:  noHostPressure,
		subnetSources: emptySubnetSources(),
	}
	raw, err := json.Marshal(startArgs{Name: item.Name})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.startCluster(raw); err == nil {
		t.Fatal("startCluster() succeeded despite a failing launch")
	}
	for _, node := range item.Nodes {
		if !savedStateExists(t, item.Name, node.Name) {
			t.Fatalf("a failed start discarded %s's saved state; resume can no longer recover it", node.Name)
		}
	}
	if !clusterHasSavedState(item.Name) {
		t.Fatal("the cluster stopped reporting saved state after a start that never launched anything")
	}
}

// TestNodeStartKeepsSavedStateWhenLaunchFails is the per-node half.
func TestNodeStartKeepsSavedStateWhenLaunchFails(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	stubNodeMutationReconcile(service)
	writeSavedState(t, item)
	delete(service.vms[item.Name], "demo-worker-1")
	service.hypervisor = &fakeHypervisor{launch: func(context.Context, hypervisor.Spec) (hypervisor.Machine, error) {
		return nil, errors.New("no hypervisor today")
	}}

	response := dispatchNodeRunState(t, service, "node.start", item.Name, "demo-worker-1")

	if response.OK {
		t.Fatal("node.start succeeded despite a failing launch")
	}
	if !savedStateExists(t, item.Name, "demo-worker-1") {
		t.Fatal("a failed node.start discarded the node's saved state")
	}
}

// TestNodeStartWarnsWhenSavedStateSurvives covers the other failure: the cold
// boot happened but the save could not be removed, so status will keep calling
// the node suspended and the hint will keep offering a resume onto memory the
// running VM no longer matches. Silence there is what makes it dangerous.
func TestNodeStartWarnsWhenSavedStateSurvives(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 1)
	stubNodeMutationReconcile(service)
	delete(service.vms[item.Name], "demo-worker-1")
	dir, err := cluster.Dir(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	// a non-empty directory stands in for the save file: os.Remove refuses it
	undeletable := saveStatePath(dir, "demo-worker-1")
	if err := os.MkdirAll(filepath.Join(undeletable, "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}

	response := dispatchNodeRunState(t, service, "node.start", item.Name, "demo-worker-1")

	if !response.OK {
		t.Fatalf("node.start failed: %s", response.Error)
	}
	status := decodeNodeStatus(t, response)
	joined := strings.Join(status.Warnings, "\n")
	for _, want := range []string{"could not discard suspended memory state for demo-worker-1", "do not resume"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("node.start warnings = %q, want them to mention %q", status.Warnings, want)
		}
	}
}
