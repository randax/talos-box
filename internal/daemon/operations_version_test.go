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
	"github.com/randax/talos-box/internal/config"
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
	_, err = service.createCluster(raw, nil)
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
	_, err = service.createCluster(raw, nil)
	if err == nil || !strings.Contains(err.Error(), `"latest"`) {
		t.Fatalf("createCluster() error = %v, want malformed-version refusal", err)
	}
	if _, loadErr := cluster.Load("garbage-version"); loadErr == nil {
		t.Fatal("createCluster() persisted state despite malformed version")
	}
}

func TestResolveImageGuardsEveryRequestPath(t *testing.T) {
	// resolveImage is the shared request chokepoint behind create, up, and
	// cache pull; garbage must never reach image resolution through any of
	// them. Stored cluster state resolves via imageDefaults and stays exempt.
	service := &Server{}
	if _, _, err := service.resolveImage("aaa", "v1.11.9", nil); err == nil {
		t.Fatal("resolveImage() accepted a below-floor version")
	}
	if _, _, err := service.resolveImage("aaa", "not-a-version", nil); err == nil {
		t.Fatal("resolveImage() accepted a malformed version")
	}
	if _, version, err := service.resolveImage("aaa", "", nil); err != nil || version != DefaultTalosVersion {
		t.Fatalf("resolveImage(\"\") = (%q, %v), want the default version", version, err)
	}
	// A cluster persisted before the floor existed keeps working.
	if _, version, err := service.imageDefaults("aaa", "v1.2.3", nil); err != nil || version != "v1.2.3" {
		t.Fatalf("imageDefaults(stored v1.2.3) = (%q, %v), want the stored version untouched", version, err)
	}
}

func TestUpRefusesUnsupportedVersionsBeforeCreatingAnyCluster(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	service := newVersionTestServer(t, "aaa", DefaultTalosVersion)
	raw, err := json.Marshal(upArgs{Clusters: []config.ClusterSpec{
		{Name: "fine", ControlPlanes: 1, Node: cluster.NodeDefaults{MemoryMiB: 1, CPUs: 1, DiskGiB: 1},
			Talos: config.TalosSpec{Version: DefaultTalosVersion, Schematic: "aaa"}},
		{Name: "too-old", ControlPlanes: 1, Node: cluster.NodeDefaults{MemoryMiB: 1, CPUs: 1, DiskGiB: 1},
			Talos: config.TalosSpec{Version: "v1.11.9", Schematic: "aaa"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.up(raw)
	if err == nil || !strings.Contains(err.Error(), "too-old") || !strings.Contains(err.Error(), "v1.11.9") || !strings.Contains(err.Error(), MinTalosVersion) {
		t.Fatalf("up() error = %v, want refusal naming the cluster, v1.11.9, and %s", err, MinTalosVersion)
	}
	if _, loadErr := cluster.Load("fine"); loadErr == nil {
		t.Fatal("up() created the first cluster before refusing the below-floor one")
	}
}

func TestUpStartsExistingClusterPinnedBelowTheFloor(t *testing.T) {
	// tbx echoes the created version into talosbox.yaml, so a cluster made
	// before a floor bump has a file pinning a now-below-floor version. The
	// floor guards creates; starting what already exists must keep working.
	t.Setenv("HOME", t.TempDir())
	service := newVersionTestServer(t, "aaa", "v1.11.0")
	item, err := cluster.New("legacy", 0, 1, 0, cluster.NodeDefaults{MemoryMiB: 1, CPUs: 1, DiskGiB: 1})
	if err != nil {
		t.Fatal(err)
	}
	item.Schematic, item.TalosVersion = "aaa", "v1.11.0"
	item.ImageArchitecture = "arm64"
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(upArgs{Clusters: []config.ClusterSpec{
		{Name: "legacy", ControlPlanes: 1, Node: cluster.NodeDefaults{MemoryMiB: 1, CPUs: 1, DiskGiB: 1},
			Talos: config.TalosSpec{Version: "v1.11.0", Schematic: "aaa"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	actions, err := service.up(raw)
	if err != nil {
		t.Fatalf("up() on an existing below-floor cluster = %v, want it to start", err)
	}
	if len(actions) != 1 || actions[0].Kind == ActionCreate {
		t.Fatalf("up actions = %+v, want a non-create action for the existing cluster", actions)
	}
}

func TestUpAppliesFileLevelVersionFloorToInheritingClusters(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	service := newVersionTestServer(t, "aaa", DefaultTalosVersion)
	raw, err := json.Marshal(upArgs{
		Talos: config.TalosSpec{Version: "v0.14.0", Schematic: "aaa"},
		Clusters: []config.ClusterSpec{
			{Name: "inherits", ControlPlanes: 1, Node: cluster.NodeDefaults{MemoryMiB: 1, CPUs: 1, DiskGiB: 1}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.up(raw); err == nil || !strings.Contains(err.Error(), "v0.14.0") {
		t.Fatalf("up() error = %v, want refusal of the inherited file-level version", err)
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
	result, err := service.createCluster(raw, nil)
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

func TestVersionWarningReachesUserWhenCreatedClusterFailsToStart(t *testing.T) {
	// The failure path drops the summary, so the warning must ride the
	// error — a boot failure on an untested version is exactly the case
	// where the newer-than-tested line is the diagnosis.
	t.Setenv("HOME", t.TempDir())
	service := newVersionTestServer(t, "aaa", "v1.14.0")
	service.hypervisor = &fakeHypervisor{
		architecture: hypervisor.ArchitectureARM64,
		launch: func(context.Context, hypervisor.Spec) (hypervisor.Machine, error) {
			return nil, errors.New("boot failure")
		},
	}
	raw, err := json.Marshal(createArgs{
		Name: "canary", Schematic: "aaa", Version: "v1.14.0",
		Node: cluster.NodeDefaults{MemoryMiB: 1, CPUs: 1, DiskGiB: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.createCluster(raw, nil)
	if err == nil || !strings.Contains(err.Error(), "failed to start") || !strings.Contains(err.Error(), "newer than") {
		t.Fatalf("createCluster() error = %v, want the start failure carrying the newer-than-tested warning", err)
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
