package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/config"
	"github.com/randax/talos-box/internal/hostpressure"
	"github.com/randax/talos-box/internal/hypervisor"
	"github.com/randax/talos-box/internal/imagecache"
)

const (
	pullDefaultSchematic = "default-schematic-id"
	pullComposedID       = "composed-schematic-id"
)

// newPullTestServer seeds a cache that already holds everything the Factory
// would have answered, so the pull under test resolves offline. The cache
// keeps the real Factory URL: a request would have to leave the machine.
func newPullTestServer(t *testing.T) (*Server, *imagecache.Cache) {
	t.Helper()

	root := t.TempDir()
	cache := imagecache.New(root)
	if err := cache.RecordDefaultSchematic(pullDefaultSchematic); err != nil {
		t.Fatal(err)
	}
	if err := cache.RecordComposition("brought", DefaultTalosVersion, []string{"gvisor"}, pullComposedID); err != nil {
		t.Fatal(err)
	}
	for _, seed := range []struct{ schematic, version string }{
		{pullDefaultSchematic, DefaultTalosVersion},
		{pullDefaultSchematic, "v1.14.0"},
		{pullComposedID, DefaultTalosVersion},
	} {
		path := filepath.Join(root, seed.schematic, seed.version, "arm64", "disk.raw")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("disk"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &Server{
		cache:        cache,
		hypervisor:   &fakeHypervisor{architecture: hypervisor.ArchitectureARM64},
		vms:          make(map[string]map[string]hypervisor.Machine),
		helperCheck:  func() error { return nil },
		hostPressure: func(string) (hostpressure.Snapshot, error) { return hostpressure.Snapshot{}, nil },
	}, cache
}

// TestPullCacheFetchesEachDistinctCombinationOncePinned covers a multi-cluster
// file: inheritance is already applied by the client, so two clusters sharing
// a pin collapse into one fetch, and every fetched combination is pinned.
func TestPullCacheFetchesEachDistinctCombinationOncePinned(t *testing.T) {
	service, cache := newPullTestServer(t)
	raw, err := json.Marshal(CachePullArgs{Combinations: []CachePullCombination{
		{},
		{},
		{Version: "v1.14.0"},
		{Schematic: "brought", Extensions: []string{"gvisor"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.pullCache(raw)
	if err != nil {
		t.Fatalf("pullCache() error = %v", err)
	}
	want := []CachePullImage{
		{Schematic: pullDefaultSchematic, Version: DefaultTalosVersion},
		{Schematic: pullDefaultSchematic, Version: "v1.14.0"},
		{Schematic: pullComposedID, Version: DefaultTalosVersion},
	}
	if len(result.Images) != len(want) {
		t.Fatalf("pullCache() images = %+v, want %d distinct combinations", result.Images, len(want))
	}
	for i, image := range result.Images {
		if image.Schematic != want[i].Schematic || image.Version != want[i].Version {
			t.Fatalf("image %d = (%s, %s), want (%s, %s)", i, image.Schematic, image.Version, want[i].Schematic, want[i].Version)
		}
		if image.Architecture != hypervisor.ArchitectureARM64 || image.Path == "" {
			t.Fatalf("image %d = %+v, want the host architecture and a path", i, image)
		}
		pinned, err := cache.Pinned(image.Schematic, image.Version, imagecache.Architecture(image.Architecture))
		if err != nil {
			t.Fatal(err)
		}
		if !pinned {
			t.Fatalf("image %d (%s %s) is not pinned", i, image.Schematic, image.Version)
		}
	}
	// The scalar fields stay populated for a tbx that predates the list.
	if result.Schematic != want[0].Schematic || result.Version != want[0].Version {
		t.Fatalf("legacy result fields = (%s, %s), want (%s, %s)",
			result.Schematic, result.Version, want[0].Schematic, want[0].Version)
	}
}

// TestPullCacheAdHocCombinationPins keeps the flag-driven single pull working
// and gives it the same pin as a file-aware one.
func TestPullCacheAdHocCombinationPins(t *testing.T) {
	service, cache := newPullTestServer(t)
	raw, err := json.Marshal(CachePullArgs{Schematic: pullDefaultSchematic, Version: "v1.14.0"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.pullCache(raw)
	if err != nil {
		t.Fatalf("pullCache() error = %v", err)
	}
	if len(result.Images) != 1 || result.Schematic != pullDefaultSchematic || result.Version != "v1.14.0" {
		t.Fatalf("pullCache() = %+v, want a single v1.14.0 image", result)
	}
	pinned, err := cache.Pinned(pullDefaultSchematic, "v1.14.0", imagecache.ArchitectureARM64)
	if err != nil {
		t.Fatal(err)
	}
	if !pinned {
		t.Fatal("an ad-hoc pull left the combination unpinned")
	}
}

func TestPullCacheRefusesBelowFloorCombination(t *testing.T) {
	service, _ := newPullTestServer(t)
	raw, err := json.Marshal(CachePullArgs{Combinations: []CachePullCombination{{Version: "v1.11.0"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.pullCache(raw); err == nil {
		t.Fatal("pullCache() accepted a below-floor version")
	}
}

// TestUpAfterPullCreatesEveryClusterOffline is the promise of a file-aware
// pull: the same file that was pulled creates every cluster from cache, with
// the Factory out of reach.
func TestUpAfterPullCreatesEveryClusterOffline(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	service, _ := newPullTestServer(t)
	specs := []config.ClusterSpec{
		{Name: "stable", ControlPlanes: 1, Node: cluster.NodeDefaults{MemoryMiB: 1, CPUs: 1, DiskGiB: 1}},
		{Name: "canary", ControlPlanes: 1, Node: cluster.NodeDefaults{MemoryMiB: 1, CPUs: 1, DiskGiB: 1},
			Talos: config.TalosSpec{Schematic: "brought", Extensions: []string{"gvisor"}}},
	}
	pullRaw, err := json.Marshal(CachePullArgs{Combinations: []CachePullCombination{
		{},
		{Schematic: "brought", Extensions: []string{"gvisor"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.pullCache(pullRaw); err != nil {
		t.Fatalf("pullCache() error = %v", err)
	}

	// A restarted daemon over the same cache: nothing resolvable is held in
	// memory any more, so every id below comes off disk.
	restarted := &Server{
		cache:        service.cache,
		hypervisor:   &fakeHypervisor{architecture: hypervisor.ArchitectureARM64},
		vms:          make(map[string]map[string]hypervisor.Machine),
		helperCheck:  func() error { return nil },
		hostPressure: func(string) (hostpressure.Snapshot, error) { return hostpressure.Snapshot{}, nil },
	}
	upRaw, err := json.Marshal(upArgs{Clusters: specs})
	if err != nil {
		t.Fatal(err)
	}
	actions, err := restarted.up(upRaw)
	if err != nil {
		t.Fatalf("up() after pull error = %v", err)
	}
	if len(actions) != 2 || actions[0].Kind != ActionCreate || actions[1].Kind != ActionCreate {
		t.Fatalf("up actions = %+v, want two creates", actions)
	}
	for name, wantSchematic := range map[string]string{"stable": pullDefaultSchematic, "canary": pullComposedID} {
		item, err := cluster.Load(name)
		if err != nil {
			t.Fatal(err)
		}
		if item.Schematic != wantSchematic {
			t.Fatalf("cluster %s schematic = %q, want %q", name, item.Schematic, wantSchematic)
		}
	}
}

// TestFileAwarePullWarmsEveryClusterImageSet is the auto-warm promise: the
// same file that was pulled has its container images replayed into the mirror,
// per cluster's declared intent, without a second command.
func TestFileAwarePullWarmsEveryClusterImageSet(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	service, _ := newPullTestServer(t)
	var warmed []string
	service.warmCache = func(_ context.Context, refs []string, _ imagecache.Architecture) (CacheWarmResult, error) {
		warmed = append(warmed, refs...)
		return CacheWarmResult{Warmed: len(refs)}, nil
	}
	raw, err := json.Marshal(CachePullArgs{FromFile: true, Combinations: []CachePullCombination{
		{Intent: cluster.ProvisioningIntent{CNI: cluster.CNICilium, CSI: cluster.CSILonghorn}},
		{Version: "v1.14.0", Intent: cluster.ProvisioningIntent{CNI: cluster.CNIFlannel, LB: true}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.pullCache(raw)
	if err != nil {
		t.Fatalf("pullCache() error = %v", err)
	}
	if result.Warm == nil || result.Warm.Warmed != len(warmed) {
		t.Fatalf("pullCache() warm = %+v, want the %d warmed refs reported", result.Warm, len(warmed))
	}
	for _, want := range []string{
		"docker.io/longhornio/longhorn-manager:v1.12.0",
		"factory.talos.dev/metal-installer/" + pullDefaultSchematic + ":" + DefaultTalosVersion,
		"factory.talos.dev/metal-installer/" + pullDefaultSchematic + ":v1.14.0",
	} {
		if !slices.Contains(warmed, want) {
			t.Errorf("warmed refs missing %q, got %v", want, warmed)
		}
	}
	for _, ref := range warmed {
		if err := ValidateWarmRef(ref); err != nil {
			t.Errorf("warmed ref %q is not warmable: %v", ref, err)
		}
	}
}

// TestFileAwarePullSkipsImagesOnRequest keeps --no-images an opt-out of the
// image warming only; the disk images are still fetched.
func TestFileAwarePullSkipsImagesOnRequest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	service, _ := newPullTestServer(t)
	service.warmCache = func(context.Context, []string, imagecache.Architecture) (CacheWarmResult, error) {
		t.Error("pullCache() warmed images with --no-images")
		return CacheWarmResult{}, nil
	}
	raw, err := json.Marshal(CachePullArgs{FromFile: true, SkipImages: true, Combinations: []CachePullCombination{
		{Intent: cluster.ProvisioningIntent{CNI: cluster.CNICilium}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.pullCache(raw)
	if err != nil {
		t.Fatalf("pullCache() error = %v", err)
	}
	if result.Warm != nil {
		t.Fatalf("pullCache() warm = %+v, want none", result.Warm)
	}
	if len(result.Images) != 1 {
		t.Fatalf("pullCache() images = %+v, want the disk image still fetched", result.Images)
	}
}

// TestFileAwarePullReportsStraysWithoutDeleting covers the retention rule: a
// pin nothing claims any more is named, and left exactly where it is. The
// built-in default combination is pinned too, and must stay unreported: prune
// and `cache list` spare it, so calling it stray would be a lie.
func TestFileAwarePullReportsStraysWithoutDeleting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	service, cache := newPullTestServer(t)
	for _, version := range []string{"v1.14.0", DefaultTalosVersion} {
		if err := cache.Pin(pullDefaultSchematic, version, imagecache.ArchitectureARM64); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(CachePullArgs{FromFile: true, Combinations: []CachePullCombination{
		{Schematic: "brought", Extensions: []string{"gvisor"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.pullCache(raw)
	if err != nil {
		t.Fatalf("pullCache() error = %v", err)
	}
	if len(result.Strays) != 1 || result.Strays[0].Schematic != pullDefaultSchematic || result.Strays[0].Version != "v1.14.0" {
		t.Fatalf("pullCache() strays = %+v, want the unclaimed v1.14.0 pin", result.Strays)
	}
	pinned, err := cache.Pinned(pullDefaultSchematic, "v1.14.0", imagecache.ArchitectureARM64)
	if err != nil {
		t.Fatal(err)
	}
	if !pinned {
		t.Fatal("reporting a stray removed its pin")
	}
	entries, err := cache.List()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(entries, func(entry imagecache.Entry) bool {
		return entry.Schematic == pullDefaultSchematic && entry.Version == "v1.14.0"
	}) {
		t.Fatalf("reporting a stray removed its image: %+v", entries)
	}
}
