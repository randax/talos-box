package daemon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
	"github.com/randax/talos-box/internal/imagecache"
)

func TestListCacheIncludesMirrorSectionForWarmedLayout(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	diskPath := filepath.Join(root, "test-schematic", "v1.2.3", "amd64", "disk.raw")
	if err := os.MkdirAll(filepath.Dir(diskPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(diskPath, []byte("disk-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	manifestPath := filepath.Join(root, "mirror", "docker.io", "manifests", "demo_latest")
	blobPath := filepath.Join(root, "mirror", "docker.io", "blobs", "sha256-abc")
	for _, item := range []struct {
		path string
		body string
	}{
		{manifestPath, "manifest"},
		{blobPath, "blob-data"},
		{manifestPath + ".meta", `{"contentType":"application/vnd.oci.image.manifest.v1+json","contentLength":8,"dockerContentDigest":"sha256:abc"}`},
	} {
		if err := os.MkdirAll(filepath.Dir(item.path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(item.path, []byte(item.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	service := &Server{
		cache:      imagecache.New(root),
		hypervisor: &fakeHypervisor{architecture: hypervisor.ArchitectureAMD64},
	}
	result, err := service.listCache()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Images) != 1 || result.Images[0].Architecture != "amd64" {
		t.Fatalf("images = %+v", result.Images)
	}
	if len(result.Mirror) != 1 || result.Mirror[0].Upstream != "docker.io" {
		t.Fatalf("mirror = %+v", result.Mirror)
	}
	if result.Mirror[0].BlobCount != 1 || result.Mirror[0].ManifestCount != 1 {
		t.Fatalf("mirror stats = %+v", result.Mirror[0])
	}
}

func TestDestroyClusterLeavesMirrorCacheByteForByteIntact(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	clusterItem, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(clusterItem); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	service := &Server{
		cache:      imagecache.New(root),
		hypervisor: &fakeHypervisor{architecture: hypervisor.ArchitectureAMD64},
		vms:        make(map[string]map[string]hypervisor.Machine),
	}
	manifestPath := filepath.Join(root, "mirror", "registry.example", "manifests", "demo_latest")
	blobPath := filepath.Join(root, "mirror", "registry.example", "blobs", "sha256-abc")
	for _, item := range []struct {
		path string
		body string
	}{
		{manifestPath, "manifest"},
		{blobPath, "blob-data"},
	} {
		if err := os.MkdirAll(filepath.Dir(item.path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(item.path, []byte(item.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshotTree(t, filepath.Join(root, "mirror"))

	_, err = service.destroyCluster(mustRawJSON(t, map[string]any{"name": "demo", "force": true}))
	if err != nil {
		t.Fatal(err)
	}
	after := snapshotTree(t, filepath.Join(root, "mirror"))
	if !bytes.Equal(before, after) {
		t.Fatalf("mirror tree changed across destroy:\nBEFORE %q\nAFTER  %q", before, after)
	}

	recreated, err := cluster.New("demo-recreated", 1, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.Save(recreated); err != nil {
		t.Fatal(err)
	}
	afterRecreate := snapshotTree(t, filepath.Join(root, "mirror"))
	if !bytes.Equal(before, afterRecreate) {
		t.Fatalf("mirror tree changed after creating a new cluster:\nBEFORE %q\nAFTER  %q", before, afterRecreate)
	}
}

func TestPruneCacheScopesReportDeletedBytesAndIsolateDiskFromMirror(t *testing.T) {
	for _, test := range []struct {
		name            string
		scope           CachePruneScope
		wantImageCount  int
		wantImageBytes  int64
		wantBlobCount   int
		wantBlobBytes   int64
		wantDiskExists  bool
		wantMirrorExist bool
	}{
		{
			name:            "images",
			scope:           CachePruneScopeImages,
			wantImageCount:  1,
			wantImageBytes:  int64(len("disk") + len("archive")),
			wantBlobCount:   0,
			wantBlobBytes:   0,
			wantMirrorExist: true,
		},
		{
			name:            "mirror",
			scope:           CachePruneScopeMirror,
			wantBlobCount:   1,
			wantBlobBytes:   int64(len("blob")),
			wantDiskExists:  true,
			wantMirrorExist: false,
		},
		{
			name:            "all",
			scope:           CachePruneScopeAll,
			wantImageCount:  1,
			wantImageBytes:  int64(len("disk") + len("archive")),
			wantBlobCount:   1,
			wantBlobBytes:   int64(len("blob")),
			wantMirrorExist: false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			root := t.TempDir()
			diskPath := filepath.Join(root, "test-schematic", "v1.2.3", "amd64", "disk.raw")
			archivePath := filepath.Join(root, "test-schematic", "v1.2.3", "amd64", "metal-amd64.raw.xz")
			blobPath := filepath.Join(root, "mirror", "docker.io", "blobs", "sha256-abc")
			for _, item := range []struct {
				path string
				body string
			}{
				{diskPath, "disk"},
				{archivePath, "archive"},
				{blobPath, "blob"},
			} {
				if err := os.MkdirAll(filepath.Dir(item.path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(item.path, []byte(item.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			service := &Server{
				cache:      imagecache.New(root),
				hypervisor: &fakeHypervisor{architecture: hypervisor.ArchitectureAMD64},
			}
			raw := mustRawJSON(t, CachePruneArgs{Scope: test.scope})
			result, err := service.pruneCache(raw)
			if err != nil {
				t.Fatal(err)
			}
			if result.Scope != test.scope {
				t.Fatalf("Scope = %q, want %q", result.Scope, test.scope)
			}
			if result.ImageCount != test.wantImageCount || result.ImageBytes != test.wantImageBytes {
				t.Fatalf("image prune result = %+v, want count=%d bytes=%d", result, test.wantImageCount, test.wantImageBytes)
			}
			if result.Mirror.BlobCount != test.wantBlobCount || result.Mirror.BlobBytes != test.wantBlobBytes {
				t.Fatalf("mirror prune result = %+v, want blobCount=%d blobBytes=%d", result.Mirror, test.wantBlobCount, test.wantBlobBytes)
			}
			if _, err := os.Stat(diskPath); test.wantDiskExists != (err == nil) {
				t.Fatalf("disk exists = %t, want %t (err %v)", err == nil, test.wantDiskExists, err)
			}
			if _, err := os.Stat(blobPath); test.wantMirrorExist != (err == nil) {
				t.Fatalf("mirror exists = %t, want %t (err %v)", err == nil, test.wantMirrorExist, err)
			}
		})
	}
}

func TestListCacheIncludesMirrorServingStatusFromBindings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	service := &Server{
		cache:               imagecache.New(root),
		hypervisor:          &fakeHypervisor{architecture: hypervisor.ArchitectureAMD64},
		boundMirrorGateways: func() []string { return nil },
	}

	result, err := service.listCache()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.MirrorBoundGatewayIPs) != 0 {
		t.Fatalf("initial mirror gateway IPs = %v, want empty", result.MirrorBoundGatewayIPs)
	}

	service.boundMirrorGateways = func() []string { return []string{"172.30.3.1"} }
	result, err = service.listCache()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.MirrorBoundGatewayIPs) != 1 || result.MirrorBoundGatewayIPs[0] != "172.30.3.1" {
		t.Fatalf("bound mirror gateway IPs = %v, want [172.30.3.1]", result.MirrorBoundGatewayIPs)
	}

	service.boundMirrorGateways = func() []string { return nil }
	result, err = service.listCache()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.MirrorBoundGatewayIPs) != 0 {
		t.Fatalf("unbound mirror gateway IPs = %v, want empty", result.MirrorBoundGatewayIPs)
	}
}

func snapshotTree(t *testing.T, root string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		if info.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		buffer.WriteString(relative)
		buffer.WriteByte(0)
		buffer.Write(data)
		buffer.WriteByte(0)
		return nil
	})
	return buffer.Bytes()
}

func mustRawJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestListCacheClassifiesInUsePinnedDefaultAndOrphanCombinations(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	service := newCacheReferenceTestServer(t, root)

	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.Schematic = "in-use-schematic"
	item.TalosVersion = "v1.2.3"
	item.ImageArchitecture = "amd64"
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	if err := service.cache.RecordDefaultSchematic("default-schematic"); err != nil {
		t.Fatal(err)
	}
	for _, schematic := range []string{"in-use-schematic", "pinned-schematic", "orphan-schematic"} {
		writeCachedDisk(t, root, schematic, "v1.2.3", "amd64")
	}
	writeCachedDisk(t, root, "default-schematic", DefaultTalosVersion, "amd64")
	if err := service.cache.Pin("pinned-schematic", "v1.2.3", imagecache.ArchitectureAMD64); err != nil {
		t.Fatal(err)
	}

	result, err := service.listCache()
	if err != nil {
		t.Fatal(err)
	}
	statuses := make(map[string]CacheImageStatus, len(result.Images))
	clusters := make(map[string][]string, len(result.Images))
	for _, image := range result.Images {
		statuses[image.Schematic] = image.Status
		clusters[image.Schematic] = image.Clusters
	}
	want := map[string]CacheImageStatus{
		"in-use-schematic":  CacheImageStatusInUse,
		"pinned-schematic":  CacheImageStatusPinned,
		"default-schematic": CacheImageStatusDefault,
		"orphan-schematic":  CacheImageStatusOrphan,
	}
	for schematic, status := range want {
		if statuses[schematic] != status {
			t.Fatalf("status of %s = %q, want %q", schematic, statuses[schematic], status)
		}
	}
	if got := clusters["in-use-schematic"]; len(got) != 1 || got[0] != "demo" {
		t.Fatalf("in-use clusters = %v, want [demo]", got)
	}
	if got := clusters["orphan-schematic"]; len(got) != 0 {
		t.Fatalf("orphan clusters = %v, want empty", got)
	}
}

func TestPruneCacheImagesRemovesOnlyOrphanCombinations(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	service := newCacheReferenceTestServer(t, root)

	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.Schematic = "in-use-schematic"
	item.TalosVersion = "v1.2.3"
	item.ImageArchitecture = "amd64"
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	if err := service.cache.RecordDefaultSchematic("default-schematic"); err != nil {
		t.Fatal(err)
	}
	for _, schematic := range []string{"in-use-schematic", "pinned-schematic", "orphan-schematic"} {
		writeCachedDisk(t, root, schematic, "v1.2.3", "amd64")
	}
	writeCachedDisk(t, root, "default-schematic", DefaultTalosVersion, "amd64")
	if err := service.cache.Pin("pinned-schematic", "v1.2.3", imagecache.ArchitectureAMD64); err != nil {
		t.Fatal(err)
	}

	result, err := service.pruneCache(mustRawJSON(t, CachePruneArgs{}))
	if err != nil {
		t.Fatal(err)
	}
	if result.ImageCount != 1 || result.ImageBytes != int64(len("disk")) {
		t.Fatalf("prune result = %+v, want one image of %d bytes", result, len("disk"))
	}
	if len(result.Images) != 1 || result.Images[0].Schematic != "orphan-schematic" {
		t.Fatalf("removed images = %+v, want only orphan-schematic", result.Images)
	}
	if result.Images[0].Size != int64(len("disk")) || result.Images[0].Status != CacheImageStatusOrphan {
		t.Fatalf("removed image = %+v, want %d bytes and status %q", result.Images[0], len("disk"), CacheImageStatusOrphan)
	}
	for _, schematic := range []string{"in-use-schematic", "pinned-schematic", "default-schematic"} {
		version := "v1.2.3"
		if schematic == "default-schematic" {
			version = DefaultTalosVersion
		}
		if _, err := os.Stat(filepath.Join(root, schematic, version, "amd64", "disk.raw")); err != nil {
			t.Fatalf("spared image missing after prune: %s (%v)", schematic, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "orphan-schematic")); !os.IsNotExist(err) {
		t.Fatalf("orphan image survived prune: %v", err)
	}
}

func TestPruneCacheAllClearsPinsAndSparesNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	service := newCacheReferenceTestServer(t, root)

	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.Schematic = "in-use-schematic"
	item.TalosVersion = "v1.2.3"
	item.ImageArchitecture = "amd64"
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	writeCachedDisk(t, root, "in-use-schematic", "v1.2.3", "amd64")
	if err := service.cache.Pin("in-use-schematic", "v1.2.3", imagecache.ArchitectureAMD64); err != nil {
		t.Fatal(err)
	}

	if _, err := service.pruneCache(mustRawJSON(t, CachePruneArgs{Scope: CachePruneScopeAll})); err != nil {
		t.Fatal(err)
	}
	pinned, err := service.cache.Pinned("in-use-schematic", "v1.2.3", imagecache.ArchitectureAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if pinned {
		t.Fatal("cache prune --all left the pin marker in place")
	}
	if _, err := os.Stat(filepath.Join(root, "in-use-schematic")); !os.IsNotExist(err) {
		t.Fatalf("cache prune --all spared an image: %v", err)
	}
}

func TestDestroyClusterLeavesItsImagePrunableButUntouched(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	service := newCacheReferenceTestServer(t, root)
	service.vms = make(map[string]map[string]hypervisor.Machine)

	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.Schematic = "in-use-schematic"
	item.TalosVersion = "v1.2.3"
	item.ImageArchitecture = "amd64"
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	diskPath := writeCachedDisk(t, root, "in-use-schematic", "v1.2.3", "amd64")

	if _, err := service.destroyCluster(mustRawJSON(t, map[string]any{"name": "demo", "force": true})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(diskPath); err != nil {
		t.Fatalf("destroy removed the cached image: %v", err)
	}
	listed, err := service.listCache()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Images) != 1 || listed.Images[0].Status != CacheImageStatusOrphan {
		t.Fatalf("images after destroy = %+v, want a single orphan", listed.Images)
	}

	result, err := service.pruneCache(mustRawJSON(t, CachePruneArgs{}))
	if err != nil {
		t.Fatal(err)
	}
	if result.ImageCount != 1 {
		t.Fatalf("ImageCount = %d, want 1", result.ImageCount)
	}
	if _, err := os.Stat(diskPath); !os.IsNotExist(err) {
		t.Fatalf("prune left the orphaned image behind: %v", err)
	}
}

func TestClusterReferencesResolveDefaultsWithoutTheFactory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	service := newCacheReferenceTestServer(t, root)

	// A cluster created before per-cluster pinning records neither a
	// schematic nor a version; it still references the default combination.
	item, err := cluster.New("legacy", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.ImageArchitecture = "amd64"
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	if err := service.cache.RecordDefaultSchematic("default-schematic"); err != nil {
		t.Fatal(err)
	}
	writeCachedDisk(t, root, "default-schematic", DefaultTalosVersion, "amd64")

	result, err := service.listCache()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Images) != 1 || result.Images[0].Status != CacheImageStatusInUse {
		t.Fatalf("images = %+v, want the default combination in use", result.Images)
	}
	if got := result.Images[0].Clusters; len(got) != 1 || got[0] != "legacy" {
		t.Fatalf("clusters = %v, want [legacy]", got)
	}
}

func newCacheReferenceTestServer(t *testing.T, root string) *Server {
	t.Helper()
	return &Server{
		cache:      imagecache.New(root),
		hypervisor: &fakeHypervisor{architecture: hypervisor.ArchitectureAMD64},
	}
}

func writeCachedDisk(t *testing.T, root, schematic, version, architecture string) string {
	t.Helper()
	path := filepath.Join(root, schematic, version, architecture, "disk.raw")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPruneCacheRemovesExactlyWhatListCallsOrphaned(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	service := newCacheReferenceTestServer(t, root)

	item, err := cluster.New("demo", 0, 1, 0, cluster.NodeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	item.Schematic = "in-use-schematic"
	item.TalosVersion = "v1.2.3"
	item.ImageArchitecture = "amd64"
	if err := cluster.Save(item); err != nil {
		t.Fatal(err)
	}
	if err := service.cache.RecordDefaultSchematic("default-schematic"); err != nil {
		t.Fatal(err)
	}
	for _, schematic := range []string{"in-use-schematic", "pinned-schematic", "orphan-a", "orphan-b"} {
		writeCachedDisk(t, root, schematic, "v1.2.3", "amd64")
	}
	writeCachedDisk(t, root, "default-schematic", DefaultTalosVersion, "amd64")
	if err := service.cache.Pin("pinned-schematic", "v1.2.3", imagecache.ArchitectureAMD64); err != nil {
		t.Fatal(err)
	}

	listed, err := service.listCache()
	if err != nil {
		t.Fatal(err)
	}
	var orphans []string
	for _, image := range listed.Images {
		if image.Status == CacheImageStatusOrphan {
			orphans = append(orphans, image.Schematic)
		}
	}

	result, err := service.pruneCache(mustRawJSON(t, CachePruneArgs{}))
	if err != nil {
		t.Fatal(err)
	}
	var removed []string
	for _, image := range result.Images {
		removed = append(removed, image.Schematic)
	}
	sort.Strings(orphans)
	sort.Strings(removed)
	if len(removed) != len(orphans) || len(removed) != 2 {
		t.Fatalf("removed = %v, want the listed orphans %v", removed, orphans)
	}
	for i := range removed {
		if removed[i] != orphans[i] {
			t.Fatalf("removed = %v, want the listed orphans %v", removed, orphans)
		}
	}
}

func TestListCachePreviewsOrphanedIncompleteCombinations(t *testing.T) {
	// The reported bug: prune reclaimed an archive-only combination the
	// preceding list never showed, then summarized it as 0 image(s).
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	service := newCacheReferenceTestServer(t, root)

	writeCachedDisk(t, root, "ready-schematic", "v1.2.3", "amd64")
	archive := filepath.Join(root, "orphan-schematic", "v1.2.3", "amd64", "metal-amd64.raw.xz")
	if err := os.MkdirAll(filepath.Dir(archive), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, []byte("orphan-archive"), 0o600); err != nil {
		t.Fatal(err)
	}

	listed, err := service.listCache()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Images) != 2 {
		t.Fatalf("images = %+v, want the ready image and the incomplete orphan", listed.Images)
	}
	var orphan CacheImageEntry
	for _, image := range listed.Images {
		if image.Schematic == "orphan-schematic" {
			orphan = image
		}
	}
	if orphan.Status != CacheImageStatusOrphan || !orphan.Incomplete {
		t.Fatalf("orphan entry = %+v, want an incomplete orphan", orphan)
	}
	if want := int64(len("orphan-archive")); orphan.Size != want {
		t.Fatalf("orphan size = %d, want the artifact bytes %d", orphan.Size, want)
	}

	result, err := service.pruneCache(mustRawJSON(t, CachePruneArgs{}))
	if err != nil {
		t.Fatal(err)
	}
	if result.ImageCount != len(result.Images) || result.ImageCount != 2 {
		t.Fatalf("ImageCount = %d with %d itemized combination(s), want both to be 2", result.ImageCount, len(result.Images))
	}
}
