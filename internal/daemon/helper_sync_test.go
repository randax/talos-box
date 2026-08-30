package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
)

type fakeSyncClient struct {
	synced [][]cluster.Cluster
	closed int
	err    error
	onSync func()
}

func (c *fakeSyncClient) Sync(clusters []cluster.Cluster) error {
	c.synced = append(c.synced, clusters)
	if c.onSync != nil {
		c.onSync()
	}
	return c.err
}

func (c *fakeSyncClient) Close() error {
	c.closed++
	return nil
}

func stubHelperSync(t *testing.T, client *fakeSyncClient, connectErr error, clusters []cluster.Cluster) {
	t.Helper()
	originalConnect, originalList := connectSyncHelper, listClustersForSync
	connectSyncHelper = func() (helperSyncClient, error) {
		if connectErr != nil {
			return nil, connectErr
		}
		return client, nil
	}
	listClustersForSync = func() ([]cluster.Cluster, error) { return clusters, nil }
	t.Cleanup(func() {
		connectSyncHelper, listClustersForSync = originalConnect, originalList
	})
}

func TestSyncHelperStatePushesEveryCluster(t *testing.T) {
	item, err := cluster.New("sync-demo", 0, 1, 0, cluster.NodeDefaults{CPUs: 1, MemoryMiB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeSyncClient{}
	stubHelperSync(t, client, nil, []cluster.Cluster{item})

	if err := SyncHelperState(); err != nil {
		t.Fatal(err)
	}
	if len(client.synced) != 1 || len(client.synced[0]) != 1 || client.synced[0][0].Name != "sync-demo" {
		t.Fatalf("synced clusters = %+v, want the listed set", client.synced)
	}
	if client.closed != 1 {
		t.Fatalf("client closes = %d, want 1", client.closed)
	}
}

func TestSyncHelperStateWrapsConnectFailure(t *testing.T) {
	stubHelperSync(t, nil, errors.New("helper unavailable"), nil)

	err := SyncHelperState()
	if err == nil || !strings.Contains(err.Error(), "sync helper state") || !strings.Contains(err.Error(), "helper unavailable") {
		t.Fatalf("SyncHelperState() error = %v, want a wrapped connect failure", err)
	}
}

func TestStartSyncsReservationsBeforeLaunchingNodes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("sync-before-attach", 0, 1, 0, cluster.NodeDefaults{CPUs: 1, MemoryMiB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	backend := &fakeHypervisor{}
	client := &fakeSyncClient{onSync: func() {
		if len(backend.specs) != 0 {
			t.Errorf("helper was synced after %d node(s) launched", len(backend.specs))
		}
	}}
	stubHelperSync(t, client, nil, []cluster.Cluster{item})

	service := &Server{
		hypervisors:   singleFakeRegistry(backend),
		vms:           make(map[string]map[string]hypervisor.Machine),
		subnetSources: emptySubnetSources(),
	}
	if _, err := service.start(item); err != nil {
		t.Fatal(err)
	}
	if len(client.synced) != 1 {
		t.Fatalf("helper syncs = %d, want 1", len(client.synced))
	}
	if len(backend.specs) != 1 {
		t.Fatalf("launches = %d, want 1", len(backend.specs))
	}
}

func TestStartRefusesToLaunchWhenTheHelperCannotBeSynced(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("sync-failure", 0, 1, 0, cluster.NodeDefaults{CPUs: 1, MemoryMiB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	backend := &fakeHypervisor{}
	stubHelperSync(t, nil, errors.New("helper unavailable"), []cluster.Cluster{item})

	service := &Server{
		hypervisors:   singleFakeRegistry(backend),
		vms:           make(map[string]map[string]hypervisor.Machine),
		subnetSources: emptySubnetSources(),
	}
	if _, err := service.start(item); err == nil {
		t.Fatal("start() succeeded without a synced helper")
	}
	if len(backend.specs) != 0 {
		t.Fatalf("launches = %d, want none", len(backend.specs))
	}
}

// The helper must hold the new node's reservation before the record commits:
// a sync that failed after the save left a node on disk with no lease and no
// rollback. Both the running and the stopped-cluster add paths go through it.
func TestAddNodeDoesNotCommitWhenTheHelperRejectsTheReservation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running bool
	}{{name: "running cluster", running: true}, {name: "stopped cluster", running: false}} {
		t.Run(tc.name, func(t *testing.T) {
			service, item := balloonableFixture(t, 1024)
			stubNodeMutationReconcile(service)
			if !tc.running {
				delete(service.vms, item.Name)
			}
			client := &fakeSyncClient{err: errors.New("helper refused the reservation")}
			stubHelperSync(t, client, nil, []cluster.Cluster{item})
			before := len(item.Nodes)
			dir, err := cluster.Dir(item.Name)
			if err != nil {
				t.Fatal(err)
			}
			filesBefore := dirNames(t, dir)

			raw, err := json.Marshal(nodeArgs{Cluster: item.Name, Role: cluster.RoleWorker})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := service.addNodeLocked(raw, nil); err == nil {
				t.Fatal("addNodeLocked() succeeded although the helper refused the sync")
			}
			if len(client.synced) != 1 || len(client.synced[0][0].Nodes) != before+1 {
				t.Fatalf("helper was offered %d syncs, want one carrying the proposed node", len(client.synced))
			}
			saved, err := cluster.Load(item.Name)
			if err != nil {
				t.Fatal(err)
			}
			if len(saved.Nodes) != before {
				t.Fatalf("cluster on disk has %d nodes after a refused sync, want %d (no commit)", len(saved.Nodes), before)
			}
			// ProvisionDisks also materialises the existing nodes' disks (it is
			// idempotent for them), so only the proposed node's files must be gone.
			for _, name := range dirNames(t, dir) {
				if strings.Contains(name, fmt.Sprintf("worker-%d", before)) {
					t.Fatalf("proposed node file %s left behind after a refused sync (dir before: %v)", name, filesBefore)
				}
			}
		})
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
