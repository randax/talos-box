package mirror

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/randax/talos-box/internal/imagecache"
)

func TestWarmCachesDigestPinnedIndexForOfflineGuestPull(t *testing.T) {
	amd64Config := []byte(`{"arch":"amd64-config"}`)
	amd64Layer := []byte("amd64-layer")
	arm64Config := []byte(`{"arch":"arm64-config"}`)
	arm64Layer := []byte("arm64-layer")

	amd64ConfigDigest := "sha256:" + sha256Hex(amd64Config)
	amd64LayerDigest := "sha256:" + sha256Hex(amd64Layer)
	arm64ConfigDigest := "sha256:" + sha256Hex(arm64Config)
	arm64LayerDigest := "sha256:" + sha256Hex(arm64Layer)

	amd64Manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%d},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"%s","size":%d}]}`,
		amd64ConfigDigest, len(amd64Config), amd64LayerDigest, len(amd64Layer))
	arm64Manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%d},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"%s","size":%d}]}`,
		arm64ConfigDigest, len(arm64Config), arm64LayerDigest, len(arm64Layer))
	windowsManifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%d},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"%s","size":%d}]}`,
		arm64ConfigDigest, len(arm64Config), arm64LayerDigest, len(arm64Layer))
	amd64ManifestDigest := "sha256:" + sha256Hex([]byte(amd64Manifest))
	arm64ManifestDigest := "sha256:" + sha256Hex([]byte(arm64Manifest))
	windowsManifestDigest := "sha256:" + sha256Hex([]byte(windowsManifest))

	indexBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"%s","size":%d,"platform":{"architecture":"amd64","os":"linux"}},{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"%s","size":%d,"platform":{"architecture":"arm64","os":"linux"}},{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"%s","size":%d,"platform":{"architecture":"amd64","os":"windows"}}]}`,
		amd64ManifestDigest, len(amd64Manifest), arm64ManifestDigest, len(arm64Manifest), windowsManifestDigest, len(windowsManifest))
	indexDigest := "sha256:" + sha256Hex([]byte(indexBody))

	var hits syncWarmHits
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/demo/manifests/" + indexDigest:
			hits.index.Add(1)
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			w.Header().Set("Docker-Content-Digest", indexDigest)
			_, _ = fmt.Fprint(w, indexBody)
		case "/v2/demo/manifests/" + amd64ManifestDigest:
			hits.amd64Manifest.Add(1)
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", amd64ManifestDigest)
			_, _ = fmt.Fprint(w, amd64Manifest)
		case "/v2/demo/manifests/" + arm64ManifestDigest:
			hits.arm64Manifest.Add(1)
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", arm64ManifestDigest)
			_, _ = fmt.Fprint(w, arm64Manifest)
		case "/v2/demo/manifests/" + windowsManifestDigest:
			hits.windowsManifest.Add(1)
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", windowsManifestDigest)
			_, _ = fmt.Fprint(w, windowsManifest)
		case "/v2/demo/blobs/" + amd64ConfigDigest:
			hits.amd64Blob.Add(1)
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(amd64Config)
		case "/v2/demo/blobs/" + amd64LayerDigest:
			hits.amd64Blob.Add(1)
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(amd64Layer)
		case "/v2/demo/blobs/" + arm64ConfigDigest:
			hits.arm64Blob.Add(1)
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(arm64Config)
		case "/v2/demo/blobs/" + arm64LayerDigest:
			hits.arm64Blob.Add(1)
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(arm64Layer)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	manager := newManagerWithPorts(t.TempDir(), nil, freePort(t))
	manager.baseOverride = aliasedURL(t, upstream.URL, "registry.example")
	egress := egressForRoutes(aliasRoute(t, upstream.URL, "registry.example", "203.0.113.10"))
	manager.resolveUpstreamIPs = egress.resolve
	manager.hostOwnedIPs = egress.hostIPs
	manager.dialContext = egress.dialContext
	defer manager.Close()

	result, err := manager.Warm(context.Background(), []string{"registry.example/demo@" + indexDigest}, imagecache.ArchitectureAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if result.Warmed != 1 || result.AlreadyComplete != 0 || result.Failed != 0 {
		t.Fatalf("warm result = %+v", result)
	}

	upstream.Close()
	mirror := httptest.NewServer(http.HandlerFunc(manager.serveCatchAll))
	defer mirror.Close()

	for _, path := range []string{
		"/v2/demo/manifests/" + indexDigest,
		"/v2/demo/manifests/" + amd64ManifestDigest,
		"/v2/demo/manifests/" + arm64ManifestDigest,
		"/v2/demo/manifests/" + windowsManifestDigest,
		"/v2/demo/blobs/" + amd64ConfigDigest,
		"/v2/demo/blobs/" + amd64LayerDigest,
	} {
		resp, body := get(t, mirror.URL+path+"?ns=registry.example")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d %q, want 200", path, resp.StatusCode, body)
		}
	}

	resp, body := get(t, mirror.URL+"/v2/demo/blobs/"+arm64ConfigDigest+"?ns=registry.example")
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("arm64 blob unexpectedly cached: %q", body)
	}
	if hits.arm64Blob.Load() != 0 {
		t.Fatalf("arm64 blob hits = %d, want 0", hits.arm64Blob.Load())
	}
}

type syncWarmHits struct {
	index           atomic.Int64
	amd64Manifest   atomic.Int64
	arm64Manifest   atomic.Int64
	windowsManifest atomic.Int64
	amd64Blob       atomic.Int64
	arm64Blob       atomic.Int64
}

func TestWarmRerunSkipsBlobDownloadsButRefreshesManifestRequests(t *testing.T) {
	manifestBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%d},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"%s","size":%d}]}`,
		"sha256:"+sha256Hex([]byte("config")), len("config"), "sha256:"+sha256Hex([]byte("layer")), len("layer"))
	manifestDigest := "sha256:" + sha256Hex([]byte(manifestBody))
	configDigest := "sha256:" + sha256Hex([]byte("config"))
	layerDigest := "sha256:" + sha256Hex([]byte("layer"))

	var manifestHits atomic.Int64
	var blobHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/demo/manifests/" + manifestDigest:
			manifestHits.Add(1)
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = fmt.Fprint(w, manifestBody)
		case "/v2/demo/blobs/" + configDigest:
			blobHits.Add(1)
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.WriteString(w, "config")
		case "/v2/demo/blobs/" + layerDigest:
			blobHits.Add(1)
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.WriteString(w, "layer")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	manager := newManagerWithPorts(t.TempDir(), nil, freePort(t))
	manager.baseOverride = aliasedURL(t, upstream.URL, "registry.example")
	egress := egressForRoutes(aliasRoute(t, upstream.URL, "registry.example", "203.0.113.10"))
	manager.resolveUpstreamIPs = egress.resolve
	manager.hostOwnedIPs = egress.hostIPs
	manager.dialContext = egress.dialContext
	defer manager.Close()

	ref := "registry.example/demo@" + manifestDigest
	if _, err := manager.Warm(context.Background(), []string{ref}, imagecache.ArchitectureAMD64); err != nil {
		t.Fatal(err)
	}
	firstManifestHits := manifestHits.Load()
	firstBlobHits := blobHits.Load()
	if firstManifestHits == 0 || firstBlobHits == 0 {
		t.Fatalf("first warm hits = manifests %d blobs %d", firstManifestHits, firstBlobHits)
	}

	result, err := manager.Warm(context.Background(), []string{ref}, imagecache.ArchitectureAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if result.Warmed != 0 || result.AlreadyComplete != 1 || result.Failed != 0 {
		t.Fatalf("second warm result = %+v", result)
	}
	if got := manifestHits.Load(); got <= firstManifestHits {
		t.Fatalf("manifest hits after rerun = %d, want > %d", got, firstManifestHits)
	}
	if got := blobHits.Load(); got != firstBlobHits {
		t.Fatalf("blob hits after rerun = %d, want %d", got, firstBlobHits)
	}
}

func TestWarmTagAtDigestUsesTagRequestPathAndPinnedDigest(t *testing.T) {
	configDigest := "sha256:" + sha256Hex([]byte("config"))
	manifestBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%d},"layers":[]}`,
		configDigest, len("config"))
	manifestDigest := "sha256:" + sha256Hex([]byte(manifestBody))
	var tagHits atomic.Int64
	var digestHits atomic.Int64

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/demo/app/manifests/v1.0.0":
			tagHits.Add(1)
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = fmt.Fprint(w, manifestBody)
		case "/v2/demo/app/manifests/" + manifestDigest:
			digestHits.Add(1)
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = fmt.Fprint(w, manifestBody)
		case "/v2/demo/app/blobs/" + configDigest:
			_, _ = io.WriteString(w, "config")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	manager := newManagerWithPorts(t.TempDir(), nil, freePort(t))
	manager.baseOverride = aliasedURL(t, upstream.URL, "registry.example")
	egress := egressForRoutes(aliasRoute(t, upstream.URL, "registry.example", "203.0.113.10"))
	manager.resolveUpstreamIPs = egress.resolve
	manager.hostOwnedIPs = egress.hostIPs
	manager.dialContext = egress.dialContext
	defer manager.Close()

	result, err := manager.Warm(context.Background(), []string{"registry.example/demo/app:v1.0.0@" + manifestDigest}, imagecache.ArchitectureAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if result.Warmed != 1 || tagHits.Load() == 0 || digestHits.Load() == 0 {
		t.Fatalf("warm result = %+v, tag hits = %d, digest hits = %d", result, tagHits.Load(), digestHits.Load())
	}
}

func TestWarmBlobRequestDiscardsResponseBody(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(*httptest.ResponseRecorder); ok {
			t.Fatal("warmBlobRequest used ResponseRecorder and buffered blob bytes")
		}
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 8; i++ {
			_, _ = io.WriteString(w, strings.Repeat("chunk", 2048))
		}
	})

	if err := warmBlobRequest(context.Background(), handler, "demo", "sha256:"+strings.Repeat("1", 64)); err != nil {
		t.Fatal(err)
	}
}

func TestWarmContinuesAfterFailureAndReportsMixedSummary(t *testing.T) {
	configDigest := "sha256:" + sha256Hex([]byte("config"))
	manifestBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"%s"},"layers":[]}`, configDigest)
	manifestDigest := "sha256:" + sha256Hex([]byte(manifestBody))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/good/manifests/" + manifestDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = fmt.Fprint(w, manifestBody)
		case "/v2/good/blobs/" + configDigest:
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.WriteString(w, "config")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	manager := newManagerWithPorts(t.TempDir(), nil, freePort(t))
	manager.baseOverride = aliasedURL(t, upstream.URL, "registry.example")
	egress := egressForRoutes(aliasRoute(t, upstream.URL, "registry.example", "203.0.113.10"))
	manager.resolveUpstreamIPs = egress.resolve
	manager.hostOwnedIPs = egress.hostIPs
	manager.dialContext = egress.dialContext
	defer manager.Close()

	summary, err := manager.Warm(context.Background(), []string{
		"registry.example/good@" + manifestDigest,
		"registry.example/missing@sha256:" + strings.Repeat("b", 64),
	}, imagecache.ArchitectureAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Warmed != 1 || summary.AlreadyComplete != 0 || summary.Failed != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if len(summary.Results) != 2 {
		t.Fatalf("results len = %d, want 2", len(summary.Results))
	}
	if summary.Results[0].Ref != "registry.example/good@"+manifestDigest || summary.Results[0].Error != "" {
		t.Fatalf("good result = %+v", summary.Results[0])
	}
	if summary.Results[1].Ref != "registry.example/missing@sha256:"+strings.Repeat("b", 64) || summary.Results[1].Error == "" {
		t.Fatalf("failed result = %+v", summary.Results[1])
	}
}

func TestWarmFailurePreservesPreviouslyCachedManifest(t *testing.T) {
	configDigest := "sha256:" + sha256Hex([]byte("config"))
	goodManifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"%s"},"layers":[]}`, configDigest)
	goodDigest := "sha256:" + sha256Hex([]byte(goodManifest))
	badManifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},"layers":[]}`

	var corrupt atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/demo/manifests/" + goodDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", goodDigest)
			if corrupt.Load() {
				_, _ = fmt.Fprint(w, badManifest)
				return
			}
			_, _ = fmt.Fprint(w, goodManifest)
		case "/v2/demo/blobs/" + configDigest:
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.WriteString(w, "config")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	manager := newManagerWithPorts(t.TempDir(), nil, freePort(t))
	manager.baseOverride = aliasedURL(t, upstream.URL, "registry.example")
	egress := egressForRoutes(aliasRoute(t, upstream.URL, "registry.example", "203.0.113.10"))
	manager.resolveUpstreamIPs = egress.resolve
	manager.hostOwnedIPs = egress.hostIPs
	manager.dialContext = egress.dialContext
	defer manager.Close()

	ref := "registry.example/demo@" + goodDigest
	if summary, err := manager.Warm(context.Background(), []string{ref}, imagecache.ArchitectureAMD64); err != nil || summary.Warmed != 1 {
		t.Fatalf("initial warm = %+v, %v", summary, err)
	}

	corrupt.Store(true)
	summary, err := manager.Warm(context.Background(), []string{ref}, imagecache.ArchitectureAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Warmed != 0 || summary.AlreadyComplete != 0 || summary.Failed != 1 {
		t.Fatalf("corrupt rerun summary = %+v", summary)
	}

	upstream.Close()
	mirror := httptest.NewServer(http.HandlerFunc(manager.serveCatchAll))
	defer mirror.Close()

	resp, body := get(t, mirror.URL+"/v2/demo/manifests/"+goodDigest+"?ns=registry.example")
	if resp.StatusCode != http.StatusOK || body != goodManifest {
		t.Fatalf("cached manifest after failed rerun = %d %q", resp.StatusCode, body)
	}
}

func TestWarmTagReferenceCachesListedTagAndResolvedDigest(t *testing.T) {
	configDigest := "sha256:" + sha256Hex([]byte("config"))
	manifestBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"%s"},"layers":[]}`, configDigest)
	manifestDigest := "sha256:" + sha256Hex([]byte(manifestBody))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/demo/manifests/stable", "/v2/demo/manifests/" + manifestDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = fmt.Fprint(w, manifestBody)
		case "/v2/demo/blobs/" + configDigest:
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.WriteString(w, "config")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	manager := newManagerWithPorts(t.TempDir(), nil, freePort(t))
	manager.baseOverride = aliasedURL(t, upstream.URL, "registry.example")
	egress := egressForRoutes(aliasRoute(t, upstream.URL, "registry.example", "203.0.113.10"))
	manager.resolveUpstreamIPs = egress.resolve
	manager.hostOwnedIPs = egress.hostIPs
	manager.dialContext = egress.dialContext
	defer manager.Close()

	summary, err := manager.Warm(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Warmed != 1 || summary.AlreadyComplete != 0 || summary.Failed != 0 {
		t.Fatalf("summary = %+v", summary)
	}

	manager.offline.Store(true)
	upstream.Close()
	mirror := httptest.NewServer(http.HandlerFunc(manager.serveCatchAll))
	defer mirror.Close()

	for _, path := range []string{
		"/v2/demo/manifests/stable",
		"/v2/demo/manifests/" + manifestDigest,
	} {
		resp, body := get(t, mirror.URL+path+"?ns=registry.example")
		if resp.StatusCode != http.StatusOK || body != manifestBody {
			t.Fatalf("%s = %d %q", path, resp.StatusCode, body)
		}
	}
}
