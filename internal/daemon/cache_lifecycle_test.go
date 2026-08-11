package daemon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hypervisor"
	"github.com/randax/talos-box/internal/imagecache"
)

func TestListCacheIncludesMirrorSectionForWarmedLayout(t *testing.T) {
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
