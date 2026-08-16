package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hostpressure"
	"github.com/randax/talos-box/internal/hypervisor"
	"github.com/randax/talos-box/internal/imagecache"
)

// TestCreateClusterWithCachedCompositionStaysOffline pins the offline
// contract: a recorded composition plus a cached composed image is everything
// create needs, so it neither validates nor talks to the Image Factory. The
// cache here points at the real Factory URL — any request would leave the
// machine, and the composed id would not survive it.
func TestCreateClusterWithCachedCompositionStaysOffline(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const composed = "composed-schematic-id"
	root := t.TempDir()
	cache := imagecache.New(root)
	if err := cache.RecordComposition("", DefaultTalosVersion, []string{"gvisor"}, composed); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, composed, DefaultTalosVersion, "arm64", "disk.raw")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := &Server{
		cache:        cache,
		hypervisor:   &fakeHypervisor{architecture: hypervisor.ArchitectureARM64},
		vms:          make(map[string]map[string]hypervisor.Machine),
		helperCheck:  func() error { return nil },
		hostPressure: func(string) (hostpressure.Snapshot, error) { return hostpressure.Snapshot{}, nil },
	}
	raw, err := json.Marshal(createArgs{
		Name: "sandboxed", Extensions: []string{"gvisor"},
		Node: cluster.NodeDefaults{MemoryMiB: 1, CPUs: 1, DiskGiB: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.createCluster(raw); err != nil {
		t.Fatalf("createCluster() error = %v", err)
	}

	item, err := cluster.Load("sandboxed")
	if err != nil {
		t.Fatal(err)
	}
	if item.Schematic != composed {
		t.Fatalf("persisted schematic = %q, want %q", item.Schematic, composed)
	}
	if want := []string{"gvisor"}; !reflect.DeepEqual(item.TalosExtensions, want) {
		t.Fatalf("persisted extensions = %v, want %v", item.TalosExtensions, want)
	}

	statuses, err := service.status(json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 {
		t.Fatalf("status() returned %d clusters, want 1", len(statuses))
	}
	if want := []string{"gvisor"}; !reflect.DeepEqual(statuses[0].TalosExtensions, want) {
		t.Fatalf("status extensions = %v, want %v", statuses[0].TalosExtensions, want)
	}
}

func TestCreateClusterRefusesUnknownExtensionBeforeMutation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	service := &Server{
		cache:        imagecache.New(t.TempDir()),
		hypervisor:   &fakeHypervisor{architecture: hypervisor.ArchitectureARM64},
		vms:          make(map[string]map[string]hypervisor.Machine),
		helperCheck:  func() error { return nil },
		hostPressure: func(string) (hostpressure.Snapshot, error) { return hostpressure.Snapshot{}, nil },
	}
	raw, err := json.Marshal(createArgs{
		Name: "typo", Extensions: []string{"gvisr"},
		Node: cluster.NodeDefaults{MemoryMiB: 1, CPUs: 1, DiskGiB: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.createCluster(raw)
	if err == nil || !strings.Contains(err.Error(), `did you mean "gvisor"`) {
		t.Fatalf("createCluster() error = %v, want an unknown-extension refusal", err)
	}
	if _, loadErr := cluster.Load("typo"); loadErr == nil {
		t.Fatal("createCluster() persisted state despite an unknown extension")
	}
}

// TestStoredClusterNeverRecomposes guards the schematic already recorded on a
// cluster: its extensions were composed at create, so resolving its disk must
// not compose again.
func TestStoredClusterNeverRecomposes(t *testing.T) {
	root := t.TempDir()
	const composed = "composed-schematic-id"
	path := filepath.Join(root, composed, DefaultTalosVersion, "arm64", "disk.raw")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := &Server{
		cache:      imagecache.New(root),
		hypervisor: &fakeHypervisor{architecture: hypervisor.ArchitectureARM64},
	}
	item := cluster.Cluster{
		Name: "sandboxed", Schematic: composed, TalosVersion: DefaultTalosVersion,
		TalosExtensions: []string{"gvisor"}, ImageArchitecture: "arm64",
	}
	got, err := service.cachedDisk(item)
	if err != nil {
		t.Fatalf("cachedDisk() error = %v", err)
	}
	if got != path {
		t.Fatalf("cachedDisk() = %q, want %q", got, path)
	}
}
