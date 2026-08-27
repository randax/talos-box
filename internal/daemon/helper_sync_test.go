package daemon

import (
	"errors"
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
		hypervisor:    backend,
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
		hypervisor:    backend,
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
