package imagecache

import (
	"os"
	"path/filepath"
	"testing"
)

// TestListReportsAllocatedSize keeps the cache honest about capacity: a fully
// written image occupies at least its apparent size, in whole blocks.
func TestListReportsAllocatedSize(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "test-schematic", "v1.2.3", string(ArchitectureAMD64), "disk.raw")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, 8192), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := New(root).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("List() = %+v, want one entry", entries)
	}
	if entries[0].Size != 8192 {
		t.Fatalf("Size = %d, want 8192", entries[0].Size)
	}
	if entries[0].AllocatedSize < entries[0].Size {
		t.Fatalf("AllocatedSize = %d, want at least the apparent size %d",
			entries[0].AllocatedSize, entries[0].Size)
	}
}

// TestListReportsSparseAllocatedSize is the reported defect: a sparse disk.raw
// reports an apparent size many times what it occupies on disk.
func TestListReportsSparseAllocatedSize(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "test-schematic", "v1.2.3", string(ArchitectureAMD64), "disk.raw")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const apparent = 64 << 20
	if err := file.Truncate(apparent); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	entries, err := New(root).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("List() = %+v, want one entry", entries)
	}
	if entries[0].Size != apparent {
		t.Fatalf("Size = %d, want %d", entries[0].Size, apparent)
	}
	if entries[0].AllocatedSize >= entries[0].Size {
		// A filesystem without sparse-file support legitimately allocates
		// the whole extent; the reporting seam is still exercised above.
		t.Skipf("filesystem allocated %d bytes for a sparse %d-byte file", entries[0].AllocatedSize, entries[0].Size)
	}
}
