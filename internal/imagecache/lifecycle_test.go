package imagecache

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPruneDiskPreservesMirrorCache(t *testing.T) {
	root := t.TempDir()
	cache := New(root)

	diskPath := filepath.Join(root, "schematic", "v1.2.3", "amd64", "disk.raw")
	if err := os.MkdirAll(filepath.Dir(diskPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(diskPath, []byte("disk-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	mirrorManifest := filepath.Join(root, "mirror", "docker.io", "manifests", "app_manifests_latest")
	if err := os.MkdirAll(filepath.Dir(mirrorManifest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mirrorManifest, []byte("manifest-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := cache.PruneDisk()
	if err != nil {
		t.Fatal(err)
	}
	if result.ImageCount != 1 {
		t.Fatalf("ImageCount = %d, want 1", result.ImageCount)
	}
	if _, err := os.Stat(diskPath); !os.IsNotExist(err) {
		t.Fatalf("disk path still exists after prune: %v", err)
	}
	if _, err := os.Stat(mirrorManifest); err != nil {
		t.Fatalf("mirror manifest missing after disk prune: %v", err)
	}
}

func TestPruneDiskPreservesNonImageRootFiles(t *testing.T) {
	root := t.TempDir()
	cache := New(root)

	diskPath := filepath.Join(root, "schematic", "v1.2.3", "amd64", "disk.raw")
	if err := os.MkdirAll(filepath.Dir(diskPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(diskPath, []byte("disk-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	notePath := filepath.Join(root, "README.txt")
	if err := os.WriteFile(notePath, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := cache.PruneDisk(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(diskPath); !os.IsNotExist(err) {
		t.Fatalf("disk path still exists after prune: %v", err)
	}
	if _, err := os.Stat(notePath); err != nil {
		t.Fatalf("non-image root file missing after disk prune: %v", err)
	}
}

func TestPruneMirrorAndAllScopes(t *testing.T) {
	for _, test := range []struct {
		name            string
		prune           func(*Cache) (CachePruneResult, error)
		wantImageCount  int
		wantDiskExists  bool
		wantMirrorExist bool
	}{
		{name: "mirror only", prune: (*Cache).PruneMirror, wantImageCount: 0, wantDiskExists: true},
		{name: "all", prune: (*Cache).PruneAll, wantImageCount: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			cache := New(root)

			diskPath := filepath.Join(root, "schematic", "v1.2.3", "amd64", "disk.raw")
			if err := os.MkdirAll(filepath.Dir(diskPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(diskPath, []byte("disk-bytes"), 0o600); err != nil {
				t.Fatal(err)
			}

			mirrorBlob := filepath.Join(root, "mirror", "ghcr.io", "blobs", "sha256-abc")
			if err := os.MkdirAll(filepath.Dir(mirrorBlob), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(mirrorBlob, []byte("blob"), 0o600); err != nil {
				t.Fatal(err)
			}

			result, err := test.prune(cache)
			if err != nil {
				t.Fatal(err)
			}
			if result.ImageCount != test.wantImageCount {
				t.Fatalf("ImageCount = %d, want %d", result.ImageCount, test.wantImageCount)
			}
			if result.Mirror.BlobCount != 1 {
				t.Fatalf("Mirror.BlobCount = %d, want 1", result.Mirror.BlobCount)
			}
			if _, err := os.Stat(diskPath); test.wantDiskExists != (err == nil) {
				t.Fatalf("disk exists = %t, want %t (err %v)", err == nil, test.wantDiskExists, err)
			}
			if _, err := os.Stat(mirrorBlob); test.wantMirrorExist != (err == nil) {
				t.Fatalf("mirror exists = %t, want %t (err %v)", err == nil, test.wantMirrorExist, err)
			}
		})
	}
}

func TestMirrorStatsIgnoresMetadataAndTempFiles(t *testing.T) {
	root := t.TempDir()
	cache := New(root)

	upstream := filepath.Join(root, "mirror", "docker.io")
	manifest := filepath.Join(upstream, "manifests", "app_manifests_latest")
	metadata := manifest + ".meta"
	legacyType := manifest + ".ct"
	partialManifest := filepath.Join(upstream, "manifests", ".partial-x")
	blob := filepath.Join(upstream, "blobs", "sha256-abc")
	partialBlob := filepath.Join(upstream, "blobs", ".partial-y")
	for _, item := range []struct {
		path string
		body string
	}{
		{manifest, "manifest"},
		{metadata, "meta"},
		{legacyType, "ct"},
		{partialManifest, "tmp"},
		{blob, "blob"},
		{partialBlob, "tmp"},
	} {
		if err := os.MkdirAll(filepath.Dir(item.path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(item.path, []byte(item.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	stats, total, err := cache.MirrorStats()
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 {
		t.Fatalf("stats len = %d, want 1", len(stats))
	}
	if stats[0].ManifestCount != 1 || stats[0].BlobCount != 1 {
		t.Fatalf("stats = %+v, want 1 manifest and 1 blob", stats[0])
	}
	if total.ManifestCount != 1 || total.BlobCount != 1 {
		t.Fatalf("total = %+v, want 1 manifest and 1 blob", total)
	}
}

func TestPruneDiskRemovesOnlyKnownDiskArtifactsAndCountsRemovedBytes(t *testing.T) {
	root := t.TempDir()
	cache := New(root)

	paths := []struct {
		path string
		body string
	}{
		{filepath.Join(root, "schematic", "v1.2.3", "amd64", "disk.raw"), "amd64-disk"},
		{filepath.Join(root, "schematic", "v1.2.3", "amd64", "metal-amd64.raw.xz"), "amd64-archive"},
		{filepath.Join(root, "schematic", "v1.2.3", "arm64", "metal-arm64.raw.xz"), "arm64-archive"},
		{filepath.Join(root, "schematic", "v1.2.3", "disk.raw"), "legacy-arm64-disk"},
		{filepath.Join(root, "schematic", "v1.2.3", "amd64", "keep.txt"), "sentinel"},
		{filepath.Join(root, "schematic", "v1.2.3", "notes.txt"), "version-note"},
		{filepath.Join(root, "README.txt"), "root-note"},
		{filepath.Join(root, "mirror", "docker.io", "manifests", "demo_latest"), "manifest"},
	}
	var wantBytes int64
	for _, item := range paths {
		if err := os.MkdirAll(filepath.Dir(item.path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(item.path, []byte(item.body), 0o600); err != nil {
			t.Fatal(err)
		}
		switch filepath.Base(item.path) {
		case "disk.raw", "metal-amd64.raw.xz", "metal-arm64.raw.xz":
			if filepath.Base(filepath.Dir(item.path)) == "amd64" || filepath.Base(filepath.Dir(item.path)) == "arm64" || filepath.Base(item.path) == "disk.raw" {
				wantBytes += int64(len(item.body))
			}
		}
	}

	result, err := cache.PruneDisk()
	if err != nil {
		t.Fatal(err)
	}
	if result.ImageCount != 2 {
		t.Fatalf("ImageCount = %d, want 2", result.ImageCount)
	}
	if result.ImageBytes != wantBytes {
		t.Fatalf("ImageBytes = %d, want %d", result.ImageBytes, wantBytes)
	}
	for _, path := range []string{
		filepath.Join(root, "schematic", "v1.2.3", "amd64", "disk.raw"),
		filepath.Join(root, "schematic", "v1.2.3", "amd64", "metal-amd64.raw.xz"),
		filepath.Join(root, "schematic", "v1.2.3", "arm64", "metal-arm64.raw.xz"),
		filepath.Join(root, "schematic", "v1.2.3", "disk.raw"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("known artifact still exists after prune: %s (%v)", path, err)
		}
	}
	for _, path := range []string{
		filepath.Join(root, "schematic", "v1.2.3", "amd64", "keep.txt"),
		filepath.Join(root, "schematic", "v1.2.3", "notes.txt"),
		filepath.Join(root, "README.txt"),
		filepath.Join(root, "mirror", "docker.io", "manifests", "demo_latest"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("preserved path missing after prune: %s (%v)", path, err)
		}
	}
}

func TestPruneDiskIsIdempotentWhenArtifactsAreAlreadyGone(t *testing.T) {
	root := t.TempDir()
	cache := New(root)

	diskPath := filepath.Join(root, "schematic", "v1.2.3", "amd64", "disk.raw")
	archivePath := filepath.Join(root, "schematic", "v1.2.3", "amd64", "metal-amd64.raw.xz")
	for _, item := range []struct {
		path string
		body string
	}{
		{diskPath, "disk"},
		{archivePath, "archive"},
	} {
		if err := os.MkdirAll(filepath.Dir(item.path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(item.path, []byte(item.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := cache.PruneDisk(); err != nil {
		t.Fatal(err)
	}
	result, err := cache.PruneDisk()
	if err != nil {
		t.Fatal(err)
	}
	if result.ImageCount != 0 || result.ImageBytes != 0 {
		t.Fatalf("second prune result = %+v, want zero counts", result)
	}
}

func TestPruneDiskRejectsSymlinkArtifactsWithoutDeletingCache(t *testing.T) {
	root := t.TempDir()
	cache := New(root)

	target := filepath.Join(root, "outside.raw")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	diskPath := filepath.Join(root, "schematic", "v1.2.3", "amd64", "disk.raw")
	if err := os.MkdirAll(filepath.Dir(diskPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, diskPath); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(root, "schematic", "v1.2.3", "amd64", "keep.txt")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := cache.PruneDisk(); err == nil {
		t.Fatal("PruneDisk accepted symlink artifact")
	}
	if _, err := os.Lstat(diskPath); err != nil {
		t.Fatalf("symlink artifact missing after failed prune: %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("sentinel missing after failed prune: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("symlink target missing after failed prune: %v", err)
	}
}

func TestPruneDiskRejectsSuspiciousPathsWithoutPartialDeletion(t *testing.T) {
	root := t.TempDir()
	cache := New(root)

	diskPath := filepath.Join(root, "schematic", "v1.2.3", "amd64", "disk.raw")
	if err := os.MkdirAll(filepath.Dir(diskPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(diskPath, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(root, "outside.raw")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	suspicious := filepath.Join(root, "schematic", "v1.2.3", "arm64", "disk.raw")
	if err := os.MkdirAll(filepath.Dir(suspicious), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, suspicious); err != nil {
		t.Fatal(err)
	}

	if _, err := cache.PruneDisk(); err == nil {
		t.Fatal("PruneDisk accepted suspicious path")
	}
	if _, err := os.Stat(diskPath); err != nil {
		t.Fatalf("disk artifact was partially deleted: %v", err)
	}
	if _, err := os.Lstat(suspicious); err != nil {
		t.Fatalf("suspicious path missing after failed prune: %v", err)
	}
}

func TestPruneDiskAccountsForCrashTempArtifacts(t *testing.T) {
	root := t.TempDir()
	cache := New(root)

	paths := []struct {
		path string
		body string
	}{
		{filepath.Join(root, "schematic", "v1.2.3", "amd64", ".disk.raw-stale"), "temp-disk"},
		{filepath.Join(root, "schematic", "v1.2.3", "amd64", ".metal-amd64.raw.xz-stale"), "temp-archive"},
		{filepath.Join(root, "schematic", "v1.2.3", "amd64", "keep.txt"), "keep"},
	}
	var wantBytes int64
	for _, item := range paths {
		if err := os.MkdirAll(filepath.Dir(item.path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(item.path, []byte(item.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if filepath.Base(item.path) != "keep.txt" {
			wantBytes += int64(len(item.body))
		}
	}

	result, err := cache.PruneDisk()
	if err != nil {
		t.Fatal(err)
	}
	if result.ImageCount != 0 {
		t.Fatalf("ImageCount = %d, want 0", result.ImageCount)
	}
	if result.ImageBytes != wantBytes {
		t.Fatalf("ImageBytes = %d, want %d", result.ImageBytes, wantBytes)
	}
	for _, path := range []string{
		filepath.Join(root, "schematic", "v1.2.3", "amd64", ".disk.raw-stale"),
		filepath.Join(root, "schematic", "v1.2.3", "amd64", ".metal-amd64.raw.xz-stale"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("crash temp still exists after prune: %s (%v)", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "schematic", "v1.2.3", "amd64", "keep.txt")); err != nil {
		t.Fatalf("sentinel missing after prune: %v", err)
	}
}

func TestPruneDiskRejectsSymlinkCacheRoot(t *testing.T) {
	parent := t.TempDir()
	actualRoot := filepath.Join(parent, "actual")
	if err := os.MkdirAll(filepath.Join(actualRoot, "schematic", "v1.2.3", "amd64"), 0o755); err != nil {
		t.Fatal(err)
	}
	diskPath := filepath.Join(actualRoot, "schematic", "v1.2.3", "amd64", "disk.raw")
	if err := os.WriteFile(diskPath, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkRoot := filepath.Join(parent, "cache-link")
	if err := os.Symlink(actualRoot, symlinkRoot); err != nil {
		t.Fatal(err)
	}

	if _, err := New(symlinkRoot).PruneDisk(); err == nil {
		t.Fatal("PruneDisk accepted symlink cache root")
	}
	if _, err := os.Stat(diskPath); err != nil {
		t.Fatalf("cache contents changed after symlink-root failure: %v", err)
	}
}

func TestPruneMirrorRejectsSymlinkRoot(t *testing.T) {
	root := t.TempDir()
	cache := New(root)

	target := filepath.Join(root, "real-mirror")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	mirrorLink := filepath.Join(root, "mirror")
	if err := os.Symlink(target, mirrorLink); err != nil {
		t.Fatal(err)
	}

	if _, err := cache.PruneMirror(); err == nil {
		t.Fatal("PruneMirror accepted symlink root")
	}
	if _, err := os.Lstat(mirrorLink); err != nil {
		t.Fatalf("mirror symlink missing after failed prune: %v", err)
	}
}
