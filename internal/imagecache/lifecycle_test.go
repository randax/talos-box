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
