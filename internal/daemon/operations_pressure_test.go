package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hostpressure"
	"github.com/randax/talos-box/internal/imagecache"
	"github.com/randax/talos-box/internal/vm"
)

func TestCreateClusterChecksHostPressureBeforeMutation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	service := &Server{hostPressure: extremeSwapPressure}
	raw, err := json.Marshal(createArgs{Name: "unsafe-create"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.createCluster(raw)
	if err == nil || !strings.Contains(err.Error(), "host swap is 90% used") {
		t.Fatalf("createCluster() error = %v, want host-pressure refusal", err)
	}
	if _, loadErr := cluster.Load("unsafe-create"); loadErr == nil {
		t.Fatal("createCluster() persisted state despite host-pressure refusal")
	}
}

func TestAddNodeChecksHostPressureBeforeMutation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("unsafe-add", 0, 0, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	service := &Server{
		vms:          make(map[string]map[string]*vm.VM),
		hostPressure: extremeSwapPressure,
	}
	raw, err := json.Marshal(nodeArgs{Cluster: item.Name, Name: "unsafe-worker", Role: cluster.RoleWorker})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.addNode(raw)
	if err == nil || !strings.Contains(err.Error(), "host swap is 90% used") {
		t.Fatalf("addNode() error = %v, want host-pressure refusal", err)
	}
	reloaded, err := cluster.Load(item.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Nodes) != 0 {
		t.Fatalf("addNode() persisted %d nodes despite host-pressure refusal", len(reloaded.Nodes))
	}
}

func TestAddNodeSurfacesHostPressureProbeFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	item, err := cluster.New("probe-failed-add", 0, 0, 0, cluster.NodeDefaults{DiskGiB: 1})
	if err != nil {
		t.Fatal(err)
	}
	item.Schematic = "test-schematic"
	item.TalosVersion = "v1.2.3"
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	cacheRoot := filepath.Join(home, "cache")
	cachedDisk := filepath.Join(cacheRoot, item.Schematic, item.TalosVersion, "disk.raw")
	if err := os.MkdirAll(filepath.Dir(cachedDisk), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachedDisk, []byte("test disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := &Server{
		cache: imagecache.New(cacheRoot),
		vms:   make(map[string]map[string]*vm.VM),
		hostPressure: func(string) (hostpressure.Snapshot, error) {
			return hostpressure.Snapshot{}, errors.New("statfs unavailable")
		},
	}
	raw, err := json.Marshal(nodeArgs{Cluster: item.Name, Name: "worker-1", Role: cluster.RoleWorker})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.addNode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Warning, "host-pressure probe failed: statfs unavailable") {
		t.Fatalf("NodeStatus.Warning = %q, want probe failure", result.Warning)
	}
}

func extremeSwapPressure(string) (hostpressure.Snapshot, error) {
	return hostpressure.Snapshot{
		Swap: hostpressure.Usage{TotalBytes: 10 << 30, AvailableBytes: 1 << 30},
	}, nil
}

func noHostPressure(string) (hostpressure.Snapshot, error) {
	return hostpressure.Snapshot{}, nil
}
