package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/randax/talos-box/internal/hypervisor"
	"github.com/randax/talos-box/internal/imagecache"
)

// TestListCacheReportsAllocatedSize carries the on-disk footprint to the
// client: the apparent size of a sparse disk.raw is not what it costs.
func TestListCacheReportsAllocatedSize(t *testing.T) {
	root := t.TempDir()
	diskPath := filepath.Join(root, "schematic", "v1.9.0", "amd64", "disk.raw")
	if err := os.MkdirAll(filepath.Dir(diskPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(diskPath, []byte("disk-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	service := &Server{
		cache:       imagecache.New(root),
		hypervisors: singleFakeRegistry(&fakeHypervisor{architecture: hypervisor.ArchitectureAMD64}),
	}
	result, err := service.listCache()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Images) != 1 {
		t.Fatalf("images = %+v, want one entry", result.Images)
	}
	if result.Images[0].AllocatedSize <= 0 {
		t.Fatalf("allocatedSize = %d, want the bytes the image occupies", result.Images[0].AllocatedSize)
	}
}
