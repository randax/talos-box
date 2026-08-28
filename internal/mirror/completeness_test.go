package mirror

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/imagecache"
)

func TestInspectCachedCompleteRequiresTagMappingDigestRootPlatformManifestConfigAndLayers(t *testing.T) {
	fixture := newCompletenessFixture(t)
	target := fixture.target("stable")
	if status := fixture.server.InspectCached(context.Background(), target, InspectOptions{}); !status.Complete() {
		t.Fatalf("complete fixture: %s", status.Reason())
	}

	tests := []struct {
		name string
		path string
		kind CacheGapKind
	}{
		{"tag mapping", fixture.server.manifestPath(manifestRequestPath("demo", "stable")), CacheGapTagMapping},
		{"root manifest", fixture.server.manifestPath(manifestRequestPath("demo", fixture.rootDigest)), CacheGapRootManifest},
		{"platform manifest", fixture.server.manifestPath(manifestRequestPath("demo", fixture.platformDigest)), CacheGapPlatformManifest},
		{"config", fixture.server.blobPath(fixture.configDigest), CacheGapConfig},
		{"layer", fixture.server.blobPath(fixture.layerDigests[0]), CacheGapLayer},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data, err := os.ReadFile(test.path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(test.path); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.WriteFile(test.path, data, 0o644) })
			status := fixture.server.InspectCached(context.Background(), target, InspectOptions{})
			if status.Complete() || len(status.Gaps) == 0 || status.Gaps[0].Kind != test.kind {
				t.Fatalf("status = %+v, want first gap %q", status, test.kind)
			}
		})
	}
}

func TestInspectCachedIgnoresUnselectedPlatformManifestAndBlobs(t *testing.T) {
	fixture := newCompletenessFixture(t)
	if err := os.Remove(fixture.server.manifestPath(manifestRequestPath("demo", fixture.foreignDigest))); err != nil {
		t.Fatal(err)
	}
	status := fixture.server.InspectCached(context.Background(), fixture.target("stable"), InspectOptions{})
	if !status.Complete() {
		t.Fatalf("unselected platform affected completeness: %s", status.Reason())
	}
}

func TestInspectCachedReportsAllMissingLayers(t *testing.T) {
	fixture := newCompletenessFixture(t)
	for _, digest := range fixture.layerDigests[:2] {
		if err := os.Remove(fixture.server.blobPath(digest)); err != nil {
			t.Fatal(err)
		}
	}
	status := fixture.server.InspectCached(context.Background(), fixture.target("stable"), InspectOptions{})
	if got := status.Reason(); !strings.Contains(got, "2 of 7 linux/amd64 layers not cached") || !strings.Contains(got, fixture.layerDigests[0]) || !strings.Contains(got, fixture.layerDigests[1]) {
		t.Fatalf("reason = %q", got)
	}
}

func TestInspectCachedRequiresTagAtDigestMappingToPinnedDigest(t *testing.T) {
	fixture := newCompletenessFixture(t)
	wrongData := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:` + strings.Repeat("f", 64) + `"},"layers":[]}`)
	wrong := "sha256:" + sha256Hex(wrongData)
	path := manifestRequestPath("demo", "stable")
	if err := fixture.server.storeManifest(path, manifestMetadata{ContentType: "application/vnd.oci.image.manifest.v1+json", DockerContentDigest: wrong}, wrongData); err != nil {
		t.Fatal(err)
	}
	status := fixture.server.InspectCached(context.Background(), fixture.target("stable"), InspectOptions{})
	if status.Complete() || status.Gaps[0].Kind != CacheGapTagMapping {
		t.Fatalf("status = %+v", status)
	}
}

func TestInspectCachedDeepHashesBlobsButPlainChecksSafePresence(t *testing.T) {
	fixture := newCompletenessFixture(t)
	if err := os.WriteFile(fixture.server.blobPath(fixture.layerDigests[0]), []byte("wrong"), 0o644); err != nil {
		t.Fatal(err)
	}
	if status := fixture.server.InspectCached(context.Background(), fixture.target("stable"), InspectOptions{}); !status.Complete() {
		t.Fatalf("plain = %s", status.Reason())
	}
	status := fixture.server.InspectCached(context.Background(), fixture.target("stable"), InspectOptions{Deep: true})
	if status.Complete() || status.Gaps[0].Kind != CacheGapCorrupt || !strings.Contains(status.Reason(), "content is sha256:") {
		t.Fatalf("deep = %+v (%s)", status, status.Reason())
	}
}

func TestInspectCachedRejectsSymlinkAndPathSwap(t *testing.T) {
	fixture := newCompletenessFixture(t)
	path := fixture.server.blobPath(fixture.layerDigests[0])
	target := filepath.Join(t.TempDir(), "layer")
	if err := os.WriteFile(target, []byte("layer-0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	status := fixture.server.InspectCached(context.Background(), fixture.target("stable"), InspectOptions{})
	if status.Complete() || status.Gaps[0].Kind != CacheGapLayer {
		t.Fatalf("symlink status = %+v", status)
	}

	opened, info, err := openCheckedRegularFile(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	replacement := filepath.Join(filepath.Dir(target), "replacement")
	if err := os.WriteFile(replacement, []byte("replacement"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, target); err != nil {
		t.Fatal(err)
	}
	if err := validateOpenedRegularFile(target, info); err == nil || !strings.Contains(err.Error(), "changed during open") {
		t.Fatalf("path swap validation = %v", err)
	}
}

type completenessFixture struct {
	server                                                  *Server
	rootDigest, platformDigest, foreignDigest, configDigest string
	layerDigests                                            []string
}

func newCompletenessFixture(t *testing.T) completenessFixture {
	return newCompletenessFixtureAt(t, t.TempDir())
}

func newCompletenessFixtureAt(t *testing.T, cacheDir string) completenessFixture {
	t.Helper()
	server := NewServer("https://registry.example", cacheDir)
	config := []byte("config")
	configDigest := "sha256:" + sha256Hex(config)
	layers := make([]string, 7)
	for i := range layers {
		layers[i] = "sha256:" + sha256Hex([]byte(fmt.Sprintf("layer-%d", i)))
	}
	platformBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"%s"},"layers":[%s]}`,
		configDigest, descriptorJSON(layers))
	platformDigest := "sha256:" + sha256Hex([]byte(platformBody))
	foreignBody := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:` + strings.Repeat("e", 64) + `"},"layers":[]}`
	foreignDigest := "sha256:" + sha256Hex([]byte(foreignBody))
	rootBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"%s","platform":{"os":"linux","architecture":"amd64"}},{"digest":"%s","platform":{"os":"linux","architecture":"arm64"}}]}`, platformDigest, foreignDigest)
	rootDigest := "sha256:" + sha256Hex([]byte(rootBody))
	store := func(ref, media, digest, body string) {
		t.Helper()
		if err := server.storeManifest(manifestRequestPath("demo", ref), manifestMetadata{ContentType: media, DockerContentDigest: digest}, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	store("stable", "application/vnd.oci.image.index.v1+json", rootDigest, rootBody)
	store(rootDigest, "application/vnd.oci.image.index.v1+json", rootDigest, rootBody)
	store(platformDigest, "application/vnd.oci.image.manifest.v1+json", platformDigest, platformBody)
	store(foreignDigest, "application/vnd.oci.image.manifest.v1+json", foreignDigest, foreignBody)
	writeBlob := func(digest string, data []byte) {
		p := server.blobPath(digest)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeBlob(configDigest, config)
	for i, digest := range layers {
		writeBlob(digest, []byte(fmt.Sprintf("layer-%d", i)))
	}
	return completenessFixture{server: server, rootDigest: rootDigest, platformDigest: platformDigest, foreignDigest: foreignDigest, configDigest: configDigest, layerDigests: layers}
}

func (f completenessFixture) target(tag string) CacheTarget {
	return CacheTarget{Repository: "demo", Tag: tag, Digest: f.rootDigest, Platform: Platform{OS: "linux", Architecture: imagecache.ArchitectureAMD64}}
}

func descriptorJSON(digests []string) string {
	parts := make([]string, len(digests))
	for i, digest := range digests {
		parts[i] = fmt.Sprintf(`{"digest":"%s"}`, digest)
	}
	return strings.Join(parts, ",")
}
