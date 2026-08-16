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
	"github.com/randax/talos-box/internal/hypervisor"
	"github.com/randax/talos-box/internal/imagecache"
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

func TestCheckHostPressureAllowsStickySwapWhileMemoryPressureIsNormal(t *testing.T) {
	// checkHostPressure is the shared guard behind both createCluster call
	// sites and startCluster. macOS keeps swap allocated after pressure
	// clears; the guard must not block on the swap percentage alone.
	service := &Server{hostPressure: func(string) (hostpressure.Snapshot, error) {
		return hostpressure.Snapshot{
			Swap:           hostpressure.Usage{TotalBytes: 10 << 30, AvailableBytes: 1 << 30},
			MemoryPressure: hostpressure.MemoryPressureNormal,
		}, nil
	}}
	warning, err := service.checkHostPressure(t.TempDir(), false)
	if err != nil {
		t.Fatalf("checkHostPressure() = %v, want no refusal for sticky swap with normal pressure", err)
	}
	if warning != "" {
		t.Fatalf("checkHostPressure() warning = %q, want none", warning)
	}
}

func TestCreateClusterRejectsInvalidProvisioningIntentBeforeMutation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	service := &Server{}
	falseValue := false
	trueValue := true
	raw, err := json.Marshal(createArgs{
		Name: "invalid-intent",
		ProvisioningIntentInput: cluster.ProvisioningIntentInput{
			CNI: string(cluster.CNIFlannel), LB: &falseValue, BGP: &trueValue,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.createCluster(raw)
	if err == nil || !strings.Contains(err.Error(), "bgp requires lb: true") {
		t.Fatalf("createCluster() error = %v, want pre-mutation provisioning validation", err)
	}
	if _, loadErr := cluster.Load("invalid-intent"); loadErr == nil {
		t.Fatal("createCluster() persisted state despite invalid provisioning intent")
	}
}

func TestCreateClusterRejectsInvalidCSIIntentBeforeMutation(t *testing.T) {
	tests := []struct {
		name    string
		input   cluster.ProvisioningIntentInput
		wantErr string
	}{
		{
			name:    "csi without cni",
			input:   cluster.ProvisioningIntentInput{CSI: string(cluster.CSILonghorn)},
			wantErr: "csi requires cni",
		},
		{
			name: "unknown csi",
			input: cluster.ProvisioningIntentInput{
				CNI: string(cluster.CNICilium), CSI: "rook",
			},
			wantErr: "csi must be one of longhorn | local-path",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			service := &Server{}
			raw, err := json.Marshal(createArgs{Name: "invalid-csi", ProvisioningIntentInput: tt.input})
			if err != nil {
				t.Fatal(err)
			}
			_, err = service.createCluster(raw)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("createCluster() error = %v, want containing %q", err, tt.wantErr)
			}
			if _, loadErr := cluster.Load("invalid-csi"); loadErr == nil {
				t.Fatal("createCluster() persisted state despite invalid CSI intent")
			}
		})
	}
}
func TestAddNodeChecksHostPressureBeforeMutation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	item, err := cluster.New("unsafe-add", 0, 0, 0, cluster.NodeDefaults{MemoryMiB: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	service := &Server{
		vms:          make(map[string]map[string]hypervisor.Machine),
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
	item, err := cluster.New("probe-failed-add", 0, 0, 0, cluster.NodeDefaults{MemoryMiB: 1, DiskGiB: 1})
	if err != nil {
		t.Fatal(err)
	}
	item.Schematic = "test-schematic"
	item.TalosVersion = "v1.12.1"
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	cacheRoot := filepath.Join(home, "cache")
	cachedDisk := filepath.Join(cacheRoot, item.Schematic, item.TalosVersion, "arm64", "disk.raw")
	if err := os.MkdirAll(filepath.Dir(cachedDisk), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachedDisk, []byte("test disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := &Server{
		cache:      imagecache.New(cacheRoot),
		hypervisor: &fakeHypervisor{architecture: hypervisor.ArchitectureARM64},
		vms:        make(map[string]map[string]hypervisor.Machine),
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
