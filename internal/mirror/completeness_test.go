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

func TestInspectCachedReportsMissingSelectedPlatformWithoutEmptyDigestSlot(t *testing.T) {
	fixture := newCompletenessFixture(t)
	target := fixture.target("stable")
	target.Platform = Platform{OS: "linux", Architecture: imagecache.Architecture("s390x")}

	status := fixture.server.InspectCached(context.Background(), target, InspectOptions{})
	if status.Complete() {
		t.Fatal("InspectCached() unexpectedly reported missing platform complete")
	}
	if got, want := status.Reason(), "index present; no linux/s390x manifest in index"; got != want {
		t.Fatalf("Reason() = %q, want %q", got, want)
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
	pathInfo, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateOpenedRegularFile(target, pathInfo, info); err == nil || !strings.Contains(err.Error(), "changed during open") {
		t.Fatalf("path swap validation = %v", err)
	}
}

func TestInspectCachedWalksNestedSelectedIndexes(t *testing.T) {
	fixture := newNestedCompletenessFixture(t, false)

	status := fixture.server.InspectCached(context.Background(), fixture.target("stable"), InspectOptions{})
	if status.Complete() {
		t.Fatal("InspectCached() = complete, want nested manifest blob gap")
	}
	if len(status.Gaps) == 0 || status.Gaps[0].Kind != CacheGapConfig {
		t.Fatalf("status = %+v, want first gap %q", status, CacheGapConfig)
	}
	if got := status.Reason(); !strings.Contains(got, fixture.configDigest) {
		t.Fatalf("Reason() = %q, want nested config digest %q", got, fixture.configDigest)
	}
}

func TestInspectCachedReportsNestedSelectedIndexesCompleteWhenBlobsPresent(t *testing.T) {
	fixture := newNestedCompletenessFixture(t, true)

	status := fixture.server.InspectCached(context.Background(), fixture.target("stable"), InspectOptions{})
	if !status.Complete() {
		t.Fatalf("InspectCached() = %s, want complete", status.Reason())
	}
}

func TestInspectCachedReportsStructurallyInvalidRootManifest(t *testing.T) {
	server := NewServer("https://registry.example", t.TempDir())
	body := []byte(`{"schemaVersion":2}`)
	digest := "sha256:" + sha256Hex(body)
	storeManifestFixture(t, server, digest, digest, string(body))

	status := server.InspectCached(context.Background(), CacheTarget{
		Repository: "demo",
		Digest:     digest,
		Platform:   Platform{OS: "linux", Architecture: imagecache.ArchitectureAMD64},
	}, InspectOptions{})
	if status.Complete() || len(status.Gaps) != 1 {
		t.Fatalf("InspectCached() = %+v, want one invalid root-manifest gap", status)
	}
	gap := status.Gaps[0]
	if gap.Kind != CacheGapRootManifest || !strings.Contains(gap.Detail, "invalid manifest: missing config descriptor") {
		t.Fatalf("gap = %+v, want invalid root-manifest detail", gap)
	}
}

func TestInspectCachedReportsNestedIndexSelfLoopAsPlatformManifestGap(t *testing.T) {
	server := NewServer("https://registry.example", t.TempDir())
	status := &CacheStatus{Target: CacheTarget{Platform: Platform{OS: "linux", Architecture: imagecache.ArchitectureAMD64}}}
	selected := platformDescriptor{Digest: "sha256:" + strings.Repeat("1", 64), OS: "linux", Architecture: "amd64"}

	_, err := server.inspectSelectedManifest(context.Background(), status.Target, cachedGraph{}, selected, 1, map[string]struct{}{selected.Digest: {}}, status)
	if err == nil || len(status.Gaps) == 0 || status.Gaps[0].Kind != CacheGapPlatformManifest {
		t.Fatalf("status = %+v err=%v, want platform-manifest cycle gap", status, err)
	}
	if got := status.Gaps[0].Detail; !strings.Contains(got, "cycles") {
		t.Fatalf("detail = %q, want cycle detail", got)
	}
}

func TestInspectCachedReportsNestedIndexRootLoopAsPlatformManifestGap(t *testing.T) {
	server := NewServer("https://registry.example", t.TempDir())
	status := &CacheStatus{Target: CacheTarget{Repository: "demo", Platform: Platform{OS: "linux", Architecture: imagecache.ArchitectureAMD64}}}
	rootDigest := "sha256:" + strings.Repeat("2", 64)
	childBody := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"` + rootDigest + `","platform":{"os":"linux","architecture":"amd64"}}]}`
	childDigest := "sha256:" + sha256Hex([]byte(childBody))
	storeManifestFixture(t, server, childDigest, childDigest, childBody)
	selected := platformDescriptor{Digest: childDigest, OS: "linux", Architecture: "amd64"}

	_, err := server.inspectSelectedManifest(context.Background(), status.Target, cachedGraph{}, selected, 1, map[string]struct{}{rootDigest: {}}, status)
	if err == nil || len(status.Gaps) == 0 || status.Gaps[0].Kind != CacheGapPlatformManifest {
		t.Fatalf("status = %+v err=%v, want platform-manifest cycle gap", status, err)
	}
	if got := status.Gaps[0].Detail; !strings.Contains(got, "cycles") {
		t.Fatalf("detail = %q, want cycle detail", got)
	}
}

func TestInspectCachedReportsNestedIndexDepthOverflowAsPlatformManifestGap(t *testing.T) {
	server := NewServer("https://registry.example", t.TempDir())
	status := &CacheStatus{Target: CacheTarget{Platform: Platform{OS: "linux", Architecture: imagecache.ArchitectureAMD64}}}
	selected := platformDescriptor{Digest: "sha256:" + strings.Repeat("3", 64), OS: "linux", Architecture: "amd64"}

	_, err := server.inspectSelectedManifest(context.Background(), status.Target, cachedGraph{}, selected, maxSelectedIndexDepth+1, map[string]struct{}{}, status)
	if err == nil || len(status.Gaps) == 0 || status.Gaps[0].Kind != CacheGapPlatformManifest {
		t.Fatalf("status = %+v err=%v, want platform-manifest depth gap", status, err)
	}
	if got := status.Gaps[0].Detail; !strings.Contains(got, "exceeds depth") {
		t.Fatalf("detail = %q, want depth detail", got)
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
		path := server.blobPath(digest)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
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

type nestedCompletenessFixture struct {
	server         *Server
	rootDigest     string
	foreignDigest  string
	configDigest   string
	layerDigest    string
	manifestDigest string
}

func newNestedCompletenessFixture(t *testing.T, includeBlobs bool) nestedCompletenessFixture {
	return newNestedCompletenessFixtureAt(t, t.TempDir(), includeBlobs)
}

func newNestedCompletenessFixtureAt(t *testing.T, cacheDir string, includeBlobs bool) nestedCompletenessFixture {
	t.Helper()
	server := NewServer("https://registry.example", cacheDir)
	config := []byte("nested-config")
	layer := []byte("nested-layer")
	configDigest := "sha256:" + sha256Hex(config)
	layerDigest := "sha256:" + sha256Hex(layer)
	manifestBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"%s"},"layers":[{"digest":"%s"}]}`, configDigest, layerDigest)
	manifestDigest := "sha256:" + sha256Hex([]byte(manifestBody))
	nestedBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"%s","platform":{"os":"linux","architecture":"amd64"}}]}`, manifestDigest)
	nestedDigest := "sha256:" + sha256Hex([]byte(nestedBody))
	foreignBody := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:` + strings.Repeat("e", 64) + `"},"layers":[]}`
	foreignDigest := "sha256:" + sha256Hex([]byte(foreignBody))
	rootBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"%s","platform":{"os":"linux","architecture":"amd64"}},{"digest":"%s","platform":{"os":"linux","architecture":"arm64"}}]}`, nestedDigest, foreignDigest)
	rootDigest := "sha256:" + sha256Hex([]byte(rootBody))
	store := func(ref, media, digest, body string) {
		t.Helper()
		if err := server.storeManifest(manifestRequestPath("demo", ref), manifestMetadata{ContentType: media, DockerContentDigest: digest}, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	store("stable", "application/vnd.oci.image.index.v1+json", rootDigest, rootBody)
	store(rootDigest, "application/vnd.oci.image.index.v1+json", rootDigest, rootBody)
	store(nestedDigest, "application/vnd.oci.image.index.v1+json", nestedDigest, nestedBody)
	store(manifestDigest, "application/vnd.oci.image.manifest.v1+json", manifestDigest, manifestBody)
	store(foreignDigest, "application/vnd.oci.image.manifest.v1+json", foreignDigest, foreignBody)
	if includeBlobs {
		for digest, data := range map[string][]byte{configDigest: config, layerDigest: layer} {
			path := server.blobPath(digest)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return nestedCompletenessFixture{
		server:         server,
		rootDigest:     rootDigest,
		foreignDigest:  foreignDigest,
		configDigest:   configDigest,
		layerDigest:    layerDigest,
		manifestDigest: manifestDigest,
	}
}

func (f nestedCompletenessFixture) target(tag string) CacheTarget {
	return CacheTarget{Repository: "demo", Tag: tag, Digest: f.rootDigest, Platform: Platform{OS: "linux", Architecture: imagecache.ArchitectureAMD64}}
}

func storeManifestFixture(t *testing.T, server *Server, ref, digest, body string) {
	t.Helper()
	if err := server.storeManifest(manifestRequestPath("demo", ref), manifestMetadata{ContentType: "application/vnd.oci.image.index.v1+json", DockerContentDigest: digest}, []byte(body)); err != nil {
		t.Fatal(err)
	}
}
