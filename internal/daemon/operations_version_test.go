package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hostpressure"
	"github.com/randax/talos-box/internal/hypervisor"
	"github.com/randax/talos-box/internal/imagecache"
)

func TestCreateClusterRefusesVersionsBelowFloorBeforeMutation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	service := &Server{}
	raw, err := json.Marshal(createArgs{Name: "too-old", Version: "v0.14.0"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.createCluster(raw)
	if err == nil || !strings.Contains(err.Error(), "v0.14.0") || !strings.Contains(err.Error(), MinTalosVersion) {
		t.Fatalf("createCluster() error = %v, want refusal naming v0.14.0 and the minimum %s", err, MinTalosVersion)
	}
	if _, loadErr := cluster.Load("too-old"); loadErr == nil {
		t.Fatal("createCluster() persisted state despite below-floor version")
	}
}

func TestCreateClusterRefusesMalformedVersionBeforeMutation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	service := &Server{}
	raw, err := json.Marshal(createArgs{Name: "garbage-version", Version: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.createCluster(raw)
	if err == nil || !strings.Contains(err.Error(), `"latest"`) {
		t.Fatalf("createCluster() error = %v, want malformed-version refusal", err)
	}
	if _, loadErr := cluster.Load("garbage-version"); loadErr == nil {
		t.Fatal("createCluster() persisted state despite malformed version")
	}
}

func TestResolveImageGuardsEveryVersionPath(t *testing.T) {
	// resolveImage is the shared chokepoint behind create, up, cache pull,
	// and cachedDisk; garbage must never reach image resolution through any
	// of them.
	service := &Server{}
	if _, _, err := service.resolveImage("aaa", "v1.11.9"); err == nil {
		t.Fatal("resolveImage() accepted a below-floor version")
	}
	if _, _, err := service.resolveImage("aaa", "not-a-version"); err == nil {
		t.Fatal("resolveImage() accepted a malformed version")
	}
	if _, version, err := service.resolveImage("aaa", ""); err != nil || version != DefaultTalosVersion {
		t.Fatalf("resolveImage(\"\") = (%q, %v), want the default version", version, err)
	}
}

func newVersionTestServer(t *testing.T, schematic, version string) *Server {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, schematic, version, "arm64", "disk.raw")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Server{
		cache:        imagecache.New(root),
		hypervisor:   &fakeHypervisor{architecture: hypervisor.ArchitectureARM64},
		vms:          make(map[string]map[string]hypervisor.Machine),
		helperCheck:  func() error { return nil },
		hostPressure: func(string) (hostpressure.Snapshot, error) { return hostpressure.Snapshot{}, nil },
	}
}

func createVersionTestCluster(t *testing.T, service *Server, name, schematic, version string) ClusterSummary {
	t.Helper()
	raw, err := json.Marshal(createArgs{
		Name: name, Schematic: schematic, Version: version,
		Node: cluster.NodeDefaults{MemoryMiB: 1, CPUs: 1, DiskGiB: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.createCluster(raw)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestCreateClusterWarnsOnceAboveTestedDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	service := newVersionTestServer(t, "aaa", "v1.14.0")
	result := createVersionTestCluster(t, service, "canary", "aaa", "v1.14.0")
	if !strings.Contains(result.Warning, "v1.14.0") ||
		!strings.Contains(result.Warning, DefaultTalosVersion) ||
		!strings.Contains(result.Warning, "newer than") {
		t.Fatalf("create warning = %q, want newer-than-tested line naming v1.14.0 and %s", result.Warning, DefaultTalosVersion)
	}
}

func TestCreateClusterStaysSilentWithinFloorAndDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, version := range []string{MinTalosVersion, "v1.12.5", DefaultTalosVersion} {
		service := newVersionTestServer(t, "aaa", version)
		result := createVersionTestCluster(t, service, "pinned-"+strings.ReplaceAll(version, ".", "-"), "aaa", version)
		if strings.Contains(result.Warning, "newer than") {
			t.Fatalf("create warning for %s = %q, want no newer-than-tested line", version, result.Warning)
		}
	}
}

func TestVersionWarningNeverRepeatsInStatusOrStart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	service := newVersionTestServer(t, "aaa", "v1.14.0")
	createVersionTestCluster(t, service, "canary", "aaa", "v1.14.0")

	raw, err := json.Marshal(statusArgs{Name: "canary"})
	if err != nil {
		t.Fatal(err)
	}
	clusterStatus, err := service.status(raw)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := json.Marshal(clusterStatus)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), "newer than") {
		t.Fatalf("status repeats the newer-than-tested warning: %s", rendered)
	}

	startRaw, err := json.Marshal(startArgs{Name: "canary"})
	if err != nil {
		t.Fatal(err)
	}
	startResult, err := service.startCluster(startRaw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(startResult.Warning, "newer than") {
		t.Fatalf("start repeats the newer-than-tested warning: %q", startResult.Warning)
	}
}
