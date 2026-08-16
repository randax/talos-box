package imagecache

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPinSurvivesANewCacheInstance is the point of the marker: a pull pins,
// the daemon restarts, and the pin is still there because it lives on disk
// beside the image rather than in the process.
func TestPinSurvivesANewCacheInstance(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := New(root).Pin("aaa", "v1.13.6", ArchitectureARM64); err != nil {
		t.Fatalf("Pin() error = %v", err)
	}

	restarted := New(root)
	pinned, err := restarted.Pinned("aaa", "v1.13.6", ArchitectureARM64)
	if err != nil {
		t.Fatalf("Pinned() error = %v", err)
	}
	if !pinned {
		t.Fatal("Pinned() = false, want true after a restart")
	}
	for _, other := range []struct{ schematic, version string }{
		{"bbb", "v1.13.6"},
		{"aaa", "v1.14.0"},
	} {
		pinned, err := restarted.Pinned(other.schematic, other.version, ArchitectureARM64)
		if err != nil {
			t.Fatalf("Pinned(%s, %s) error = %v", other.schematic, other.version, err)
		}
		if pinned {
			t.Fatalf("Pinned(%s, %s) = true, want false", other.schematic, other.version)
		}
	}
	if pinned, err := restarted.Pinned("aaa", "v1.13.6", ArchitectureAMD64); err != nil || pinned {
		t.Fatalf("Pinned(amd64) = (%v, %v), want (false, nil): a pin is per architecture", pinned, err)
	}
}

// TestPinIsIdempotent guards repeated pulls of the same combination.
func TestPinIsIdempotent(t *testing.T) {
	t.Parallel()

	cache := New(t.TempDir())
	for range 2 {
		if err := cache.Pin("aaa", "v1.13.6", ArchitectureAMD64); err != nil {
			t.Fatalf("Pin() error = %v", err)
		}
	}
	if pinned, err := cache.Pinned("aaa", "v1.13.6", ArchitectureAMD64); err != nil || !pinned {
		t.Fatalf("Pinned() = (%v, %v), want (true, nil)", pinned, err)
	}
}

func TestPinRejectsPathTraversal(t *testing.T) {
	t.Parallel()

	cache := New(t.TempDir())
	if err := cache.Pin("../escape", "v1.13.6", ArchitectureAMD64); err == nil {
		t.Fatal("Pin() accepted a traversing schematic")
	}
	if _, err := cache.Pinned("aaa", "..", ArchitectureAMD64); err == nil {
		t.Fatal("Pinned() accepted a traversing version")
	}
}

// TestPruneRemovesPinMarkers keeps `cache prune --all` able to empty the cache
// directory: a marker left behind would keep the image directories alive.
func TestPruneRemovesPinMarkers(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cache := New(root)
	diskPath := filepath.Join(root, "aaa", "v1.13.6", "arm64", "disk.raw")
	if err := os.MkdirAll(filepath.Dir(diskPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(diskPath, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cache.Pin("aaa", "v1.13.6", ArchitectureARM64); err != nil {
		t.Fatal(err)
	}
	result, err := cache.PruneDisk()
	if err != nil {
		t.Fatalf("PruneDisk() error = %v", err)
	}
	if result.ImageCount != 1 || result.ImageBytes != int64(len("disk")) {
		t.Fatalf("PruneDisk() = (%d images, %d bytes), want (1, %d)", result.ImageCount, result.ImageBytes, len("disk"))
	}
	if _, err := os.Stat(filepath.Join(root, "aaa")); !os.IsNotExist(err) {
		t.Fatalf("stat pruned schematic directory = %v, want it removed", err)
	}
}
