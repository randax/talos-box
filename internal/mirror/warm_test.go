package mirror

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/imagecache"
)

func TestWarmRerunOfCompleteTagUsesNoUpstreamByDefault(t *testing.T) {
	manager, calls, _, _ := newTransportWarmManager(t)

	ref := "registry.example/demo:stable"
	first, err := manager.Warm(context.Background(), []string{ref}, imagecache.ArchitectureAMD64, WarmOptions{})
	if err != nil || first.Warmed != 1 {
		t.Fatalf("first warm = %+v, %v", first, err)
	}
	before := calls.Load()
	second, err := manager.Warm(context.Background(), []string{ref}, imagecache.ArchitectureAMD64, WarmOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if second.AlreadyComplete != 1 || second.Failed != 0 || second.Results[0].Outcome != WarmOutcomeAlreadyComplete {
		t.Fatalf("second warm = %+v", second)
	}
	if calls.Load() != before {
		t.Fatalf("upstream calls = %d, want unchanged %d", calls.Load(), before)
	}
}

func TestWarmRerunOfCompleteDigestUsesNoUpstream(t *testing.T) {
	manager, calls, _, manifestDigest := newTransportWarmManager(t)

	if first, err := manager.Warm(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64, WarmOptions{}); err != nil || first.Warmed != 1 {
		t.Fatalf("first warm = %+v, %v", first, err)
	}
	before := calls.Load()
	ref := "registry.example/demo@" + manifestDigest
	second, err := manager.Warm(context.Background(), []string{ref}, imagecache.ArchitectureAMD64, WarmOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if second.AlreadyComplete != 1 || second.Failed != 0 || second.Results[0].Outcome != WarmOutcomeAlreadyComplete {
		t.Fatalf("second warm = %+v", second)
	}
	if calls.Load() != before {
		t.Fatalf("upstream calls = %d, want unchanged %d", calls.Load(), before)
	}
}

func TestWarmRefreshCompleteUnchangedTagIsAlreadyComplete(t *testing.T) {
	manager, _, _, _ := newTransportWarmManager(t)
	ref := "registry.example/demo:stable"
	if first, err := manager.Warm(context.Background(), []string{ref}, imagecache.ArchitectureAMD64, WarmOptions{}); err != nil || first.Warmed != 1 {
		t.Fatalf("first warm = %+v, %v", first, err)
	}
	refresh, err := manager.Warm(context.Background(), []string{ref}, imagecache.ArchitectureAMD64, WarmOptions{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if refresh.AlreadyComplete != 1 || refresh.Warmed != 0 || !refresh.Results[0].ReResolvedTag {
		t.Fatalf("refresh = %+v", refresh)
	}
}

func newTransportWarmManager(t *testing.T) (*Manager, *atomic.Int64, string, string) {
	t.Helper()
	config := []byte("config")
	layer := []byte("layer")
	configDigest := "sha256:" + sha256Hex(config)
	layerDigest := "sha256:" + sha256Hex(layer)
	manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"%s"},"layers":[{"digest":"%s"}]}`, configDigest, layerDigest)
	manifestDigest := "sha256:" + sha256Hex([]byte(manifest))
	calls := &atomic.Int64{}
	manager := newManagerWithPorts(t.TempDir(), nil, 0)
	manager.serverFactory = func(_, base, cacheDir string) http.Handler {
		server := NewServer(base, cacheDir)
		server.client.Transport = warmRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			response := &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Request: request}
			switch request.URL.Path {
			case manifestRequestPath("demo", "stable"), manifestRequestPath("demo", manifestDigest):
				response.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
				response.Header.Set("Docker-Content-Digest", manifestDigest)
				response.Body = io.NopCloser(strings.NewReader(manifest))
			case "/v2/demo/blobs/" + configDigest:
				response.Body = io.NopCloser(bytes.NewReader(config))
			case "/v2/demo/blobs/" + layerDigest:
				response.Body = io.NopCloser(bytes.NewReader(layer))
			default:
				response.StatusCode = http.StatusNotFound
				response.Status = "404 Not Found"
				response.Body = io.NopCloser(strings.NewReader("not found"))
			}
			return response, nil
		})
		return server
	}
	t.Cleanup(manager.Close)
	return manager, calls, manifest, manifestDigest
}

func TestWarmFetchesOnlySelectedLinuxPlatformChild(t *testing.T) {
	selectedConfig := "sha256:" + strings.Repeat("a", 64)
	selectedLayer := "sha256:" + strings.Repeat("b", 64)
	selectedBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"%s"},"layers":[{"digest":"%s"}]}`, selectedConfig, selectedLayer)
	selectedDigest := "sha256:" + sha256Hex([]byte(selectedBody))
	armDigest := "sha256:" + strings.Repeat("c", 64)
	windowsDigest := "sha256:" + strings.Repeat("d", 64)
	index := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"%s","platform":{"os":"linux","architecture":"amd64"}},{"digest":"%s","platform":{"os":"linux","architecture":"arm64"}},{"digest":"%s","platform":{"os":"windows","architecture":"amd64"}}]}`, selectedDigest, armDigest, windowsDigest)

	hits := map[string]int{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++
		switch r.URL.Path {
		case manifestRequestPath("demo", selectedDigest):
			w.Header().Set("Docker-Content-Digest", selectedDigest)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, selectedBody)
		case "/v2/demo/blobs/" + selectedConfig, "/v2/demo/blobs/" + selectedLayer:
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	})
	result := WarmResult{}
	err := warmManifestGraph(context.Background(), handler, NewServer("https://registry.example", t.TempDir()), "demo", []byte(index), "amd64", map[string]bool{}, map[string]bool{}, map[string]bool{}, &result)
	if err != nil {
		t.Fatal(err)
	}
	if hits[manifestRequestPath("demo", selectedDigest)] != 1 || hits[manifestRequestPath("demo", armDigest)] != 0 || hits[manifestRequestPath("demo", windowsDigest)] != 0 {
		t.Fatalf("manifest hits = %+v", hits)
	}
}

func TestWarmRefreshCompleteTagReportsHard404AsRevalidateFailure(t *testing.T) {
	manager, fixture := newCachedWarmManagerWithStatus(t, http.StatusNotFound)
	summary, err := manager.Warm(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64, WarmOptions{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if summary.FailedRevalidate != 1 || summary.FailedMissing != 0 || summary.Results[0].Outcome != WarmOutcomeFailedRevalidate {
		t.Fatalf("summary = %+v", summary)
	}
	if status := fixture.server.InspectCached(context.Background(), fixture.target("stable"), InspectOptions{}); !status.Complete() {
		t.Fatalf("old cache changed: %s", status.Reason())
	}
}

func TestWarmMissingTag429IsMissingFailure(t *testing.T) {
	manager := newManagerWithPorts(t.TempDir(), nil, 0)
	var calls atomic.Int64
	manager.serverFactory = warmCountingStatusServerFactory(http.StatusTooManyRequests, &calls)
	summary, err := manager.Warm(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64, WarmOptions{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if summary.FailedMissing != 1 || summary.FailedRevalidate != 0 || summary.Results[0].Outcome != WarmOutcomeFailedMissing {
		t.Fatalf("summary = %+v", summary)
	}
	if calls.Load() != 3 {
		t.Fatalf("upstream attempts = %d, want 3", calls.Load())
	}
}

func TestWarmRefreshCompleteTagTreats429AsAlreadyComplete(t *testing.T) {
	manager, fixture := newCachedWarmManagerWithStatus(t, http.StatusTooManyRequests)
	summary, err := manager.Warm(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64, WarmOptions{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if summary.AlreadyComplete != 1 || summary.Failed != 0 || summary.Results[0].RefreshWarning != "upstream 429, not revalidated" {
		t.Fatalf("summary = %+v", summary)
	}
	if status := fixture.server.InspectCached(context.Background(), fixture.target("stable"), InspectOptions{}); !status.Complete() {
		t.Fatalf("old cache changed: %s", status.Reason())
	}
}

func newCachedWarmManagerWithStatus(t *testing.T, status int) (*Manager, completenessFixture) {
	t.Helper()
	cacheRoot := t.TempDir()
	fixture := newCompletenessFixtureAt(t, filepath.Join(cacheRoot, "registry.example"))
	manager := newManagerWithPorts(cacheRoot, nil, 0)
	manager.serverFactory = warmStatusServerFactory(status)
	return manager, fixture
}

func warmStatusServerFactory(status int) func(string, string, string) http.Handler {
	return warmCountingStatusServerFactory(status, nil)
}

func warmCountingStatusServerFactory(status int, calls *atomic.Int64) func(string, string, string) http.Handler {
	return func(_, base, cacheDir string) http.Handler {
		server := NewServer(base, cacheDir)
		server.retrySleep = func(context.Context, time.Duration) error { return nil }
		server.client.Transport = warmRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			if calls != nil {
				calls.Add(1)
			}
			return &http.Response{
				StatusCode: status,
				Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(http.StatusText(status))),
				Request:    request,
			}, nil
		})
		return server
	}
}

type warmRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn warmRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

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

	result, err := manager.Warm(context.Background(), []string{"registry.example/demo@" + indexDigest}, imagecache.ArchitectureAMD64, WarmOptions{})
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
		"/v2/demo/blobs/" + amd64ConfigDigest,
		"/v2/demo/blobs/" + amd64LayerDigest,
	} {
		resp, body := get(t, mirror.URL+path+"?ns=registry.example")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d %q, want 200", path, resp.StatusCode, body)
		}
	}
	for _, path := range []string{
		"/v2/demo/manifests/" + arm64ManifestDigest,
		"/v2/demo/manifests/" + windowsManifestDigest,
	} {
		resp, body := get(t, mirror.URL+path+"?ns=registry.example")
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("%s unexpectedly cached: %q", path, body)
		}
	}

	resp, body := get(t, mirror.URL+"/v2/demo/blobs/"+arm64ConfigDigest+"?ns=registry.example")
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("arm64 blob unexpectedly cached: %q", body)
	}
	if hits.arm64Blob.Load() != 0 {
		t.Fatalf("arm64 blob hits = %d, want 0", hits.arm64Blob.Load())
	}
	if hits.index.Load() != 1 || hits.amd64Manifest.Load() != 1 || hits.amd64Blob.Load() != 2 || hits.arm64Manifest.Load() != 0 || hits.windowsManifest.Load() != 0 {
		t.Fatalf("upstream hits: index=%d amd64-manifest=%d amd64-blobs=%d arm64-manifest=%d windows-manifest=%d; want 1, 1, 2, 0, 0",
			hits.index.Load(), hits.amd64Manifest.Load(), hits.amd64Blob.Load(), hits.arm64Manifest.Load(), hits.windowsManifest.Load())
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

func TestWarmPopulatesMirrorStatsForCacheList(t *testing.T) {
	cacheRoot := t.TempDir()
	mirrorRoot := filepath.Join(cacheRoot, "mirror")

	manifestBody := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":7},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","size":5}]}`
	manifestDigest := "sha256:" + sha256Hex([]byte(manifestBody))
	configBody := []byte("config!")
	layerBody := []byte("layer")
	configDigest := "sha256:" + sha256Hex(configBody)
	layerDigest := "sha256:" + sha256Hex(layerBody)
	manifestBody = fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%d},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"%s","size":%d}]}`,
		configDigest, len(configBody), layerDigest, len(layerBody))
	manifestDigest = "sha256:" + sha256Hex([]byte(manifestBody))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/demo/manifests/stable":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = fmt.Fprint(w, manifestBody)
		case "/v2/demo/manifests/" + manifestDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = fmt.Fprint(w, manifestBody)
		case "/v2/demo/blobs/" + configDigest:
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(configBody)
		case "/v2/demo/blobs/" + layerDigest:
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(layerBody)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	manager := newManagerWithPorts(mirrorRoot, nil, freePort(t))
	manager.baseOverride = aliasedURL(t, upstream.URL, "registry.example")
	egress := egressForRoutes(aliasRoute(t, upstream.URL, "registry.example", "203.0.113.10"))
	manager.resolveUpstreamIPs = egress.resolve
	manager.hostOwnedIPs = egress.hostIPs
	manager.dialContext = egress.dialContext
	defer manager.Close()

	result, err := manager.Warm(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64, WarmOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Warmed != 1 || result.AlreadyComplete != 0 || result.Failed != 0 {
		t.Fatalf("warm result = %+v", result)
	}

	stats, total, err := imagecache.New(cacheRoot).MirrorStats()
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].Upstream != "registry.example" {
		t.Fatalf("mirror stats = %+v", stats)
	}
	if stats[0].BlobCount != 2 || stats[0].ManifestCount != 2 {
		t.Fatalf("mirror counts = %+v", stats[0])
	}
	if total.BlobCount != stats[0].BlobCount || total.ManifestCount != stats[0].ManifestCount {
		t.Fatalf("mirror total = %+v, stats = %+v", total, stats[0])
	}
	if stats[0].BlobBytes <= 0 || stats[0].ManifestBytes <= 0 {
		t.Fatalf("mirror sizes = %+v", stats[0])
	}
}

func TestWarmUpdatesMirrorStatsPerUpstreamFromZero(t *testing.T) {
	manifestOne := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%d}}`,
		"sha256:"+sha256Hex([]byte("config-one")), len("config-one"))
	digestOne := "sha256:" + sha256Hex([]byte(manifestOne))
	configOne := "config-one"
	configOneDigest := "sha256:" + sha256Hex([]byte(configOne))

	manifestTwo := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%d}}`,
		"sha256:"+sha256Hex([]byte("config-two")), len("config-two"))
	digestTwo := "sha256:" + sha256Hex([]byte(manifestTwo))
	configTwo := "config-two"
	configTwoDigest := "sha256:" + sha256Hex([]byte(configTwo))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/one/manifests/" + digestOne:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", digestOne)
			_, _ = fmt.Fprint(w, manifestOne)
		case "/v2/two/manifests/" + digestTwo:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", digestTwo)
			_, _ = fmt.Fprint(w, manifestTwo)
		case "/v2/one/blobs/" + configOneDigest:
			_, _ = io.WriteString(w, configOne)
		case "/v2/two/blobs/" + configTwoDigest:
			_, _ = io.WriteString(w, configTwo)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	cacheRoot := t.TempDir()
	mirrorRoot := filepath.Join(cacheRoot, "mirror")
	stats, total, err := imagecache.New(cacheRoot).MirrorStats()
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 0 || total != (imagecache.MirrorTotals{}) {
		t.Fatalf("initial mirror stats = %+v total=%+v, want zero", stats, total)
	}

	manager := newManagerWithPorts(mirrorRoot, nil, freePort(t))
	manager.baseOverride = aliasedURL(t, upstream.URL, "registry.example")
	manager.resolveUpstreamIPs = func(_ context.Context, host string) ([]net.IP, error) {
		switch host {
		case "registry.one", "registry.two", "registry.example":
			return []net.IP{net.ParseIP("203.0.113.10")}, nil
		default:
			return nil, fmt.Errorf("unexpected host %q", host)
		}
	}
	manager.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	manager.dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, hostPortOfURL(t, upstream.URL))
	}
	defer manager.Close()

	summary, err := manager.Warm(context.Background(), []string{
		"registry.one/one@" + digestOne,
		"registry.two/two@" + digestTwo,
	}, imagecache.ArchitectureAMD64, WarmOptions{})

	if err != nil {
		t.Fatal(err)
	}
	if summary.Warmed != 2 || summary.Failed != 0 || summary.AlreadyComplete != 0 {
		t.Fatalf("warm summary = %+v", summary)
	}

	stats, total, err = imagecache.New(cacheRoot).MirrorStats()
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 2 {
		t.Fatalf("stats len = %d, want 2", len(stats))
	}
	for _, stat := range stats {
		if stat.BlobCount != 1 || stat.ManifestCount != 1 {
			t.Fatalf("per-upstream stats = %+v, want 1 blob and 1 manifest", stat)
		}
		if stat.BlobBytes <= 0 || stat.ManifestBytes <= 0 {
			t.Fatalf("per-upstream bytes = %+v, want non-zero", stat)
		}
	}
	if total.BlobCount != 2 || total.ManifestCount != 2 {
		t.Fatalf("mirror total = %+v, want 2 blobs and 2 manifests", total)
	}
	if total.BlobBytes != stats[0].BlobBytes+stats[1].BlobBytes || total.ManifestBytes != stats[0].ManifestBytes+stats[1].ManifestBytes {
		t.Fatalf("mirror total bytes = %+v, stats = %+v", total, stats)
	}
}

func TestWarmRerunOfCompleteDigestKeepsUpstreamCountersUnchanged(t *testing.T) {
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
	if _, err := manager.Warm(context.Background(), []string{ref}, imagecache.ArchitectureAMD64, WarmOptions{}); err != nil {
		t.Fatal(err)
	}
	firstManifestHits := manifestHits.Load()
	firstBlobHits := blobHits.Load()
	if firstManifestHits == 0 || firstBlobHits == 0 {
		t.Fatalf("first warm hits = manifests %d blobs %d", firstManifestHits, firstBlobHits)
	}

	result, err := manager.Warm(context.Background(), []string{ref}, imagecache.ArchitectureAMD64, WarmOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Warmed != 0 || result.AlreadyComplete != 1 || result.Failed != 0 {
		t.Fatalf("second warm result = %+v", result)
	}
	if got := manifestHits.Load(); got != firstManifestHits {
		t.Fatalf("manifest hits after rerun = %d, want %d", got, firstManifestHits)
	}
	if got := blobHits.Load(); got != firstBlobHits {
		t.Fatalf("blob hits after rerun = %d, want %d", got, firstBlobHits)
	}
}

func TestWarmTagAtDigestUsesDigestPathAndPublishesTagMapping(t *testing.T) {
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

	result, err := manager.Warm(context.Background(), []string{"registry.example/demo/app:v1.0.0@" + manifestDigest}, imagecache.ArchitectureAMD64, WarmOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Warmed != 1 || tagHits.Load() != 0 || digestHits.Load() != 1 {
		t.Fatalf("warm result = %+v, tag hits = %d, digest hits = %d", result, tagHits.Load(), digestHits.Load())
	}
	server := NewServer("https://registry.example", filepath.Join(manager.cacheRoot, "registry.example"))
	for _, target := range []CacheTarget{
		{Repository: "demo/app", Digest: manifestDigest, Platform: Platform{OS: "linux", Architecture: imagecache.ArchitectureAMD64}},
		{Repository: "demo/app", Tag: "v1.0.0", Digest: manifestDigest, Platform: Platform{OS: "linux", Architecture: imagecache.ArchitectureAMD64}},
	} {
		if status := server.InspectCached(context.Background(), target, InspectOptions{}); !status.Complete() {
			t.Fatalf("target %+v incomplete: %s", target, status.Reason())
		}
	}
}

func TestWarmUppercasePinnedDigestCanonicalizesValidationAndDigestRequestPath(t *testing.T) {
	configDigest := "sha256:" + sha256Hex([]byte("config"))
	manifestBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%d},"layers":[]}`,
		configDigest, len("config"))
	manifestDigest := "sha256:" + sha256Hex([]byte(manifestBody))
	uppercaseDigest := "sha256:" + strings.ToUpper(strings.TrimPrefix(manifestDigest, "sha256:"))

	tests := []struct {
		name          string
		ref           string
		wantTagHits   int64
		wantLowerHits int64
		wantUpperHits int64
	}{
		{
			name:          "digest-only ref uses canonical digest path once",
			ref:           "registry.example/demo/app@" + uppercaseDigest,
			wantLowerHits: 1,
		},
		{
			name:          "tag-at-digest uses canonical digest path once",
			ref:           "registry.example/demo/app:v1.0.0@" + uppercaseDigest,
			wantLowerHits: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var tagHits atomic.Int64
			var lowerDigestHits atomic.Int64
			var upperDigestHits atomic.Int64

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v2/demo/app/manifests/v1.0.0":
					tagHits.Add(1)
					w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
					w.Header().Set("Docker-Content-Digest", manifestDigest)
					_, _ = fmt.Fprint(w, manifestBody)
				case "/v2/demo/app/manifests/" + manifestDigest:
					lowerDigestHits.Add(1)
					w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
					w.Header().Set("Docker-Content-Digest", manifestDigest)
					_, _ = fmt.Fprint(w, manifestBody)
				case "/v2/demo/app/manifests/" + uppercaseDigest:
					upperDigestHits.Add(1)
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

			result, err := manager.Warm(context.Background(), []string{test.ref}, imagecache.ArchitectureAMD64, WarmOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if result.Warmed != 1 || result.Failed != 0 {
				t.Fatalf("warm result = %+v", result)
			}
			if got := tagHits.Load(); got != test.wantTagHits {
				t.Fatalf("tag hits = %d, want %d", got, test.wantTagHits)
			}
			if got := lowerDigestHits.Load(); got != test.wantLowerHits {
				t.Fatalf("lower digest hits = %d, want %d", got, test.wantLowerHits)
			}
			if got := upperDigestHits.Load(); got != test.wantUpperHits {
				t.Fatalf("upper digest hits = %d, want %d", got, test.wantUpperHits)
			}
		})
	}
}

func TestParseWarmReferenceRejectsEmptyPinnedDigestSuffixAndPreservesValidRefs(t *testing.T) {
	validDigest := "sha256:" + strings.Repeat("ab", 32)

	tests := []struct {
		name    string
		ref     string
		want    warmReference
		wantErr string
	}{
		{
			name:    "digest-only empty suffix rejected",
			ref:     "registry.example/repo@",
			wantErr: `invalid pinned digest ""`,
		},
		{
			name:    "tag plus empty suffix rejected",
			ref:     "registry.example/repo:stable@",
			wantErr: `invalid pinned digest ""`,
		},
		{
			name: "tag unchanged",
			ref:  "registry.example/repo:stable",
			want: warmReference{
				upstream:   "registry.example",
				repository: "repo",
				listedRef:  "stable",
			},
		},
		{
			name: "digest-only unchanged",
			ref:  "registry.example/repo@" + validDigest,
			want: warmReference{
				upstream:     "registry.example",
				repository:   "repo",
				listedRef:    validDigest,
				pinnedDigest: validDigest,
			},
		},
		{
			name: "tag-at-digest unchanged",
			ref:  "registry.example/repo:stable@" + validDigest,
			want: warmReference{
				upstream:     "registry.example",
				repository:   "repo",
				listedRef:    "stable",
				pinnedDigest: validDigest,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseWarmReference(test.ref)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("parseWarmReference(%q) error = %v, want substring %q", test.ref, err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseWarmReference(%q) unexpected error: %v", test.ref, err)
			}
			if got != test.want {
				t.Fatalf("parseWarmReference(%q) = %+v, want %+v", test.ref, got, test.want)
			}
		})
	}
}

func TestWarmTagAndTagAtDigestWithoutDigestHeaderUseValidatedCanonicalDigest(t *testing.T) {
	tagManifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"%s"},"layers":[]}`, "sha256:"+sha256Hex([]byte("config")))
	tagDigest := "sha256:" + sha256Hex([]byte(tagManifest))
	pinnedManifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"%s"},"layers":[]}`, "sha512:"+sha512Hex([]byte("config")))
	pinnedDigest := "sha512:" + sha512Hex([]byte(pinnedManifest))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/demo/manifests/stable":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = fmt.Fprint(w, tagManifest)
		case "/v2/demo/manifests/v2.0.0":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = fmt.Fprint(w, pinnedManifest)
		case "/v2/demo/manifests/" + tagDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = fmt.Fprint(w, tagManifest)
		case "/v2/demo/manifests/" + pinnedDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = fmt.Fprint(w, pinnedManifest)
		case "/v2/demo/blobs/sha256:" + sha256Hex([]byte("config")):
			_, _ = io.WriteString(w, "config")
		case "/v2/demo/blobs/sha512:" + sha512Hex([]byte("config")):
			_, _ = io.WriteString(w, "config")
		case "/v2/demo/manifests/":
			t.Fatal("warm requested empty manifest path")
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
		"registry.example/demo:stable",
		"registry.example/demo:v2.0.0@" + pinnedDigest,
	}, imagecache.ArchitectureAMD64, WarmOptions{})

	if err != nil {
		t.Fatal(err)
	}
	if summary.Warmed != 2 || summary.Failed != 0 {
		t.Fatalf("summary = %+v", summary)
	}

	manager.offline.Store(true)
	upstream.Close()
	mirror := httptest.NewServer(http.HandlerFunc(manager.serveCatchAll))
	defer mirror.Close()

	for _, path := range []struct {
		ref        string
		wantDigest string
		wantBody   string
	}{
		{"/v2/demo/manifests/stable", tagDigest, tagManifest},
		{"/v2/demo/manifests/" + tagDigest, tagDigest, tagManifest},
		{"/v2/demo/manifests/v2.0.0", pinnedDigest, pinnedManifest},
		{"/v2/demo/manifests/" + pinnedDigest, pinnedDigest, pinnedManifest},
	} {
		resp, body := get(t, mirror.URL+path.ref+"?ns=registry.example")
		if resp.StatusCode != http.StatusOK || body != path.wantBody {
			t.Fatalf("%s = %d %q", path.ref, resp.StatusCode, body)
		}
		if got := resp.Header.Get("Docker-Content-Digest"); got != path.wantDigest {
			t.Fatalf("%s digest header = %q, want %q", path.ref, got, path.wantDigest)
		}
	}
}

func TestWarmDigestAndTagAtDigestRejectBadDigestHeaders(t *testing.T) {
	tagManifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"%s"},"layers":[]}`, "sha256:"+sha256Hex([]byte("config")))
	tagDigest := "sha256:" + sha256Hex([]byte(tagManifest))
	pinnedManifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"%s"},"layers":[]}`, "sha512:"+sha512Hex([]byte("config")))
	pinnedDigest := "sha512:" + sha512Hex([]byte(pinnedManifest))

	var corrupt atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/demo/manifests/stable":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			if corrupt.Load() {
				w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("1", 64))
			} else {
				w.Header().Set("Docker-Content-Digest", tagDigest)
			}
			_, _ = fmt.Fprint(w, tagManifest)
		case "/v2/demo/manifests/v2.0.0":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			if corrupt.Load() {
				w.Header().Set("Docker-Content-Digest", "sha512:not-hex")
			}
			_, _ = fmt.Fprint(w, pinnedManifest)
		case "/v2/demo/manifests/" + tagDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", tagDigest)
			_, _ = fmt.Fprint(w, tagManifest)
		case "/v2/demo/manifests/" + pinnedDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			if corrupt.Load() {
				w.Header().Set("Docker-Content-Digest", "sha512:not-hex")
			} else {
				w.Header().Set("Docker-Content-Digest", pinnedDigest)
			}
			_, _ = fmt.Fprint(w, pinnedManifest)
		case "/v2/demo/blobs/sha256:" + sha256Hex([]byte("config")):
			_, _ = io.WriteString(w, "config")
		case "/v2/demo/blobs/sha512:" + sha512Hex([]byte("config")):
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

	if summary, err := manager.Warm(context.Background(), []string{
		"registry.example/demo:stable",
		"registry.example/demo:v2.0.0@" + pinnedDigest,
	}, imagecache.ArchitectureAMD64, WarmOptions{}); err != nil || summary.Warmed != 2 {
		t.Fatalf("initial warm = %+v, %v", summary, err)
	}

	corrupt.Store(true)
	summary, err := manager.Warm(context.Background(), []string{
		"registry.example/demo:stable",
		"registry.example/demo:v2.0.0@" + pinnedDigest,
	}, imagecache.ArchitectureAMD64, WarmOptions{Refresh: true})

	if err != nil {
		t.Fatal(err)
	}
	if summary.Failed != 1 || summary.FailedRevalidate != 1 || summary.AlreadyComplete != 1 {
		t.Fatalf("bad header rerun summary = %+v", summary)
	}

	manager.offline.Store(true)
	upstream.Close()
	mirror := httptest.NewServer(http.HandlerFunc(manager.serveCatchAll))
	defer mirror.Close()
	for _, path := range []struct {
		ref      string
		wantBody string
	}{
		{"/v2/demo/manifests/stable", tagManifest},
		{"/v2/demo/manifests/v2.0.0", pinnedManifest},
		{"/v2/demo/manifests/" + tagDigest, tagManifest},
		{"/v2/demo/manifests/" + pinnedDigest, pinnedManifest},
	} {
		resp, body := get(t, mirror.URL+path.ref+"?ns=registry.example")
		if resp.StatusCode != http.StatusOK || body != path.wantBody {
			t.Fatalf("%s = %d %q", path.ref, resp.StatusCode, body)
		}
	}
}

func TestWarmSucceedsWhenServerFactoryWrapsMirrorServer(t *testing.T) {
	configDigest := "sha256:" + sha256Hex([]byte("config"))
	manifestBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%d},"layers":[]}`,
		configDigest, len("config"))
	manifestDigest := "sha256:" + sha256Hex([]byte(manifestBody))
	var wrapperCalls atomic.Int64

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/demo/manifests/stable":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = fmt.Fprint(w, manifestBody)
		case "/v2/demo/manifests/" + manifestDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = fmt.Fprint(w, manifestBody)
		case "/v2/demo/blobs/" + configDigest:
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
	manager.serverFactory = func(_ string, base, cacheDir string) http.Handler {
		server := newServerWithEgress(base, cacheDir, egressDependencies{
			resolve:     manager.resolveUpstreamIPs,
			hostIPs:     manager.hostOwnedIPs,
			dialContext: manager.dialContext,
			blocked:     namespaceIPBlocked,
		})
		server.offline = &manager.offline
		server.validateUpstream = func(ctx context.Context) error {
			authority, err := parseUpstreamAuthority("registry.example")
			if err != nil {
				return err
			}
			return manager.validateResolvedAuthority(ctx, authority)
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			wrapperCalls.Add(1)
			server.ServeHTTP(w, r)
		})
	}
	defer manager.Close()

	summary, err := manager.Warm(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64, WarmOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Warmed != 1 || summary.Failed != 0 || summary.AlreadyComplete != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	if wrapperCalls.Load() == 0 {
		t.Fatal("delegating wrapper was never invoked")
	}
}

func TestWarmCachesAndServesSHA512BlobOffline(t *testing.T) {
	configBody := []byte("sha256-config")
	configDigest := "sha256:" + sha256Hex(configBody)
	layerBody := []byte("sha512-layer")
	layerDigest := "sha512:" + sha512Hex(layerBody)
	manifestBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%d},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"%s","size":%d}]}`,
		configDigest, len(configBody), layerDigest, len(layerBody))
	manifestDigest := "sha256:" + sha256Hex([]byte(manifestBody))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/demo/manifests/stable":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = fmt.Fprint(w, manifestBody)
		case "/v2/demo/manifests/" + manifestDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = fmt.Fprint(w, manifestBody)
		case "/v2/demo/blobs/" + configDigest:
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(configBody)
		case "/v2/demo/blobs/" + layerDigest:
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(layerBody)
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

	summary, err := manager.Warm(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64, WarmOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Warmed != 1 || summary.Failed != 0 {
		t.Fatalf("summary = %+v", summary)
	}

	manager.offline.Store(true)
	upstream.Close()
	mirror := httptest.NewServer(http.HandlerFunc(manager.serveCatchAll))
	defer mirror.Close()

	resp, body := get(t, mirror.URL+"/v2/demo/blobs/"+configDigest+"?ns=registry.example")
	if resp.StatusCode != http.StatusOK || body != string(configBody) {
		t.Fatalf("offline config blob = %d %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Docker-Content-Digest"); got != configDigest {
		t.Fatalf("config digest header = %q, want %q", got, configDigest)
	}

	resp, body = get(t, mirror.URL+"/v2/demo/blobs/"+layerDigest+"?ns=registry.example")
	if resp.StatusCode != http.StatusOK || body != string(layerBody) {
		t.Fatalf("offline sha512 layer = %d %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Docker-Content-Digest"); got != layerDigest {
		t.Fatalf("layer digest header = %q, want %q", got, layerDigest)
	}
}

func TestWarmRejectsSHA512BlobDigestMismatch(t *testing.T) {
	configBody := []byte("sha256-config")
	configDigest := "sha256:" + sha256Hex(configBody)
	layerDigest := "sha512:" + sha512Hex([]byte("sha512-layer"))
	manifestBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%d},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"%s","size":%d}]}`,
		configDigest, len(configBody), layerDigest, len("sha512-layer"))
	manifestDigest := "sha256:" + sha256Hex([]byte(manifestBody))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/demo/manifests/stable":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = fmt.Fprint(w, manifestBody)
		case "/v2/demo/manifests/" + manifestDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = fmt.Fprint(w, manifestBody)
		case "/v2/demo/blobs/" + configDigest:
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(configBody)
		case "/v2/demo/blobs/" + layerDigest:
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.WriteString(w, "corrupt")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	cacheRoot := t.TempDir()
	manager := newManagerWithPorts(cacheRoot, nil, freePort(t))
	manager.baseOverride = aliasedURL(t, upstream.URL, "registry.example")
	egress := egressForRoutes(aliasRoute(t, upstream.URL, "registry.example", "203.0.113.10"))
	manager.resolveUpstreamIPs = egress.resolve
	manager.hostOwnedIPs = egress.hostIPs
	manager.dialContext = egress.dialContext
	defer manager.Close()

	summary, err := manager.Warm(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64, WarmOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Warmed != 0 || summary.Failed != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if len(summary.Results) != 1 || !strings.Contains(summary.Results[0].Error, "sha512") {
		t.Fatalf("results = %+v, want sha512 mismatch failure", summary.Results)
	}

	server := NewServer("https://registry.example", filepath.Join(cacheRoot, "registry.example"))
	if _, err := os.Stat(server.blobPath(layerDigest)); !os.IsNotExist(err) {
		t.Fatalf("sha512 layer unexpectedly cached after mismatch: %v", err)
	}
	if _, err := os.Stat(server.blobPath(configDigest)); err != nil {
		t.Fatalf("sha256 config should still be cached before sha512 mismatch aborts: %v", err)
	}
}

func TestBlobPathCanonicalizesSupportedDigestCase(t *testing.T) {
	server := NewServer("https://registry.example", t.TempDir())

	for _, test := range []struct {
		name  string
		upper string
		lower string
	}{
		{
			name:  "sha256",
			upper: "sha256:" + strings.ToUpper(strings.Repeat("ab", 32)),
			lower: "sha256:" + strings.Repeat("ab", 32),
		},
		{
			name:  "sha512",
			upper: "sha512:" + strings.ToUpper(strings.Repeat("ab", 64)),
			lower: "sha512:" + strings.Repeat("ab", 64),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got, want := server.blobPath(test.upper), server.blobPath(test.lower); got != want {
				t.Fatalf("blobPath(%q) = %q, want same as %q", test.upper, got, want)
			}
		})
	}
}

func TestWarmCachesAndServesUppercaseManifestDigestsOffline(t *testing.T) {
	tests := []struct {
		name string
		sum  func([]byte) string
	}{
		{
			name: "sha256",
			sum: func(data []byte) string {
				return "sha256:" + sha256Hex(data)
			},
		},
		{
			name: "sha512",
			sum: func(data []byte) string {
				return "sha512:" + sha512Hex(data)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configBody := []byte("manifest-config")
			configDigest := "sha256:" + sha256Hex(configBody)
			manifestBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%d},"layers":[]}`,
				configDigest, len(configBody))
			manifestDigest := test.sum([]byte(manifestBody))
			offlineRequest := manifestDigest[:len(test.name)+1] + strings.ToUpper(manifestDigest[len(test.name)+1:])

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v2/demo/manifests/stable":
					w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
					w.Header().Set("Docker-Content-Digest", manifestDigest)
					_, _ = fmt.Fprint(w, manifestBody)
				case "/v2/demo/manifests/" + manifestDigest:
					w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
					w.Header().Set("Docker-Content-Digest", manifestDigest)
					_, _ = fmt.Fprint(w, manifestBody)
				case "/v2/demo/blobs/" + configDigest:
					w.Header().Set("Content-Type", "application/octet-stream")
					_, _ = w.Write(configBody)
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

			summary, err := manager.Warm(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64, WarmOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if summary.Warmed != 1 || summary.Failed != 0 {
				t.Fatalf("summary = %+v", summary)
			}

			manager.offline.Store(true)
			upstream.Close()
			mirror := httptest.NewServer(http.HandlerFunc(manager.serveCatchAll))
			defer mirror.Close()

			resp, body := get(t, mirror.URL+"/v2/demo/manifests/"+offlineRequest+"?ns=registry.example")
			if resp.StatusCode != http.StatusOK || body != manifestBody {
				t.Fatalf("offline uppercase manifest = %d %q", resp.StatusCode, body)
			}
			if got := resp.Header.Get("Docker-Content-Digest"); got != manifestDigest {
				t.Fatalf("digest header = %q, want %q", got, manifestDigest)
			}
		})
	}
}

func TestWarmCachesAndServesUppercaseBlobDigestsOffline(t *testing.T) {
	tests := []struct {
		name           string
		configBody     []byte
		configDigest   string
		layerBody      []byte
		layerDigest    string
		offlineRequest string
		offlineDigest  string
		offlineBody    string
	}{
		{
			name:           "uppercase sha256 config digest",
			configBody:     []byte("sha256-config"),
			configDigest:   "sha256:" + strings.ToUpper(sha256Hex([]byte("sha256-config"))),
			layerBody:      nil,
			layerDigest:    "",
			offlineRequest: "sha256:" + strings.ToUpper(sha256Hex([]byte("sha256-config"))),
			offlineDigest:  "sha256:" + sha256Hex([]byte("sha256-config")),
			offlineBody:    "sha256-config",
		},
		{
			name:           "uppercase sha512 layer digest",
			configBody:     []byte("sha256-config"),
			configDigest:   "sha256:" + sha256Hex([]byte("sha256-config")),
			layerBody:      []byte("sha512-layer"),
			layerDigest:    "sha512:" + strings.ToUpper(sha512Hex([]byte("sha512-layer"))),
			offlineRequest: "sha512:" + strings.ToUpper(sha512Hex([]byte("sha512-layer"))),
			offlineDigest:  "sha512:" + sha512Hex([]byte("sha512-layer")),
			offlineBody:    "sha512-layer",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifestBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%d},"layers":%s}`,
				test.configDigest, len(test.configBody), uppercaseLayerListJSON(test.layerDigest, len(test.layerBody)))
			manifestDigest := "sha256:" + sha256Hex([]byte(manifestBody))

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v2/demo/manifests/stable":
					w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
					w.Header().Set("Docker-Content-Digest", manifestDigest)
					_, _ = fmt.Fprint(w, manifestBody)
				case "/v2/demo/manifests/" + manifestDigest:
					w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
					w.Header().Set("Docker-Content-Digest", manifestDigest)
					_, _ = fmt.Fprint(w, manifestBody)
				case "/v2/demo/blobs/" + test.configDigest:
					w.Header().Set("Content-Type", "application/octet-stream")
					_, _ = w.Write(test.configBody)
				case "/v2/demo/blobs/" + test.layerDigest:
					w.Header().Set("Content-Type", "application/octet-stream")
					_, _ = w.Write(test.layerBody)
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

			summary, err := manager.Warm(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64, WarmOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if summary.Warmed != 1 || summary.Failed != 0 {
				t.Fatalf("summary = %+v", summary)
			}

			manager.offline.Store(true)
			upstream.Close()
			mirror := httptest.NewServer(http.HandlerFunc(manager.serveCatchAll))
			defer mirror.Close()

			resp, body := get(t, mirror.URL+"/v2/demo/blobs/"+test.offlineRequest+"?ns=registry.example")
			if resp.StatusCode != http.StatusOK || body != test.offlineBody {
				t.Fatalf("offline uppercase blob = %d %q", resp.StatusCode, body)
			}
			if got := resp.Header.Get("Docker-Content-Digest"); got != test.offlineDigest {
				t.Fatalf("digest header = %q, want %q", got, test.offlineDigest)
			}
		})
	}
}

func uppercaseLayerListJSON(digest string, size int) string {
	if digest == "" {
		return "[]"
	}
	return fmt.Sprintf(`[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"%s","size":%d}]`, digest, size)
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

func TestWarmManifestGraphSkipsAlreadyCachedBlobs(t *testing.T) {
	configDigest := "sha256:" + sha256Hex([]byte("config"))
	manifestBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"%s"},"layers":[]}`, configDigest)
	server := NewServer("https://registry.example", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(server.blobPath(configDigest)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server.blobPath(configDigest), []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}

	handlerCalls := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalls++
		t.Fatalf("warm should not hit handler for cached blob path %s", r.URL.Path)
	})

	result := &WarmResult{Ref: "registry.example/demo:1.0.0", AlreadyComplete: true}
	if err := warmManifestGraph(context.Background(), handler, server, "demo", []byte(manifestBody), string(imagecache.ArchitectureAMD64), map[string]bool{}, map[string]bool{}, map[string]bool{}, result); err != nil {
		t.Fatal(err)
	}
	if !result.AlreadyComplete {
		t.Fatal("cached blob should keep warm result already-complete")
	}
	if handlerCalls != 0 {
		t.Fatalf("handler calls = %d, want 0", handlerCalls)
	}
}

func TestDiscardWarmResponseWriteReturnsOriginalLengthAndCapsBuffer(t *testing.T) {
	response := newDiscardWarmResponse()
	response.WriteHeader(http.StatusBadGateway)
	payload := []byte(strings.Repeat("x", maxWarmErrorBodyBytes*2))
	written, err := response.Write(payload)
	if err != nil {
		t.Fatal(err)
	}
	if written != len(payload) {
		t.Fatalf("written = %d, want %d", written, len(payload))
	}
	if response.errorBody.Len() != maxWarmErrorBodyBytes {
		t.Fatalf("buffered error length = %d, want %d", response.errorBody.Len(), maxWarmErrorBodyBytes)
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
	}, imagecache.ArchitectureAMD64, WarmOptions{})

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

func TestWarmCachesHostBlobsWhenIndexRepeatsChildDigestAcrossPlatforms(t *testing.T) {
	configBody := []byte("cfg")
	layerBody := []byte("layer")
	configDigest := "sha256:" + sha256Hex(configBody)
	layerDigest := "sha256:" + sha256Hex(layerBody)
	childManifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%d},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"%s","size":%d}]}`,
		configDigest, len(configBody), layerDigest, len(layerBody))
	childDigest := "sha256:" + sha256Hex([]byte(childManifest))
	indexBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"%s","size":%d,"platform":{"architecture":"arm64","os":"linux"}},{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"%s","size":%d,"platform":{"architecture":"amd64","os":"linux"}}]}`,
		childDigest, len(childManifest), childDigest, len(childManifest))
	indexDigest := "sha256:" + sha256Hex([]byte(indexBody))

	var blobHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/demo/manifests/" + indexDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			w.Header().Set("Docker-Content-Digest", indexDigest)
			_, _ = fmt.Fprint(w, indexBody)
		case "/v2/demo/manifests/" + childDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", childDigest)
			_, _ = fmt.Fprint(w, childManifest)
		case "/v2/demo/blobs/" + configDigest:
			blobHits.Add(1)
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(configBody)
		case "/v2/demo/blobs/" + layerDigest:
			blobHits.Add(1)
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(layerBody)
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

	result, err := manager.Warm(context.Background(), []string{"registry.example/demo@" + indexDigest}, imagecache.ArchitectureAMD64, WarmOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Warmed != 1 || result.AlreadyComplete != 0 || result.Failed != 0 {
		t.Fatalf("warm result = %+v", result)
	}
	if blobHits.Load() != 2 {
		t.Fatalf("blob hits = %d, want 2 host blobs fetched", blobHits.Load())
	}

	server := NewServer("https://registry.example", filepath.Join(manager.cacheRoot, "registry.example"))
	for _, digest := range []string{configDigest, layerDigest} {
		if _, err := os.Stat(server.blobPath(digest)); err != nil {
			t.Fatalf("blob %s missing after warm: %v", digest, err)
		}
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
	if summary, err := manager.Warm(context.Background(), []string{ref}, imagecache.ArchitectureAMD64, WarmOptions{}); err != nil || summary.Warmed != 1 {
		t.Fatalf("initial warm = %+v, %v", summary, err)
	}

	corrupt.Store(true)
	summary, err := manager.Warm(context.Background(), []string{ref}, imagecache.ArchitectureAMD64, WarmOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Warmed != 0 || summary.AlreadyComplete != 1 || summary.Failed != 0 {
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

func TestWarmPinnedTagMismatchPreservesPreviouslyCachedTag(t *testing.T) {
	oldConfigDigest := "sha256:" + sha256Hex([]byte("old-config"))
	oldManifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"%s"},"layers":[]}`, oldConfigDigest)
	oldDigest := "sha256:" + sha256Hex([]byte(oldManifest))
	newManifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:` + strings.Repeat("d", 64) + `"},"layers":[]}`

	var mismatch atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/demo/manifests/stable":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			if mismatch.Load() {
				_, _ = fmt.Fprint(w, newManifest)
				return
			}
			w.Header().Set("Docker-Content-Digest", oldDigest)
			_, _ = fmt.Fprint(w, oldManifest)
		case "/v2/demo/manifests/" + oldDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", oldDigest)
			_, _ = fmt.Fprint(w, oldManifest)
		case "/v2/demo/blobs/" + oldConfigDigest:
			_, _ = io.WriteString(w, "old-config")
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

	ref := "registry.example/demo:stable@" + oldDigest
	if summary, err := manager.Warm(context.Background(), []string{ref}, imagecache.ArchitectureAMD64, WarmOptions{}); err != nil || summary.Warmed != 1 {
		t.Fatalf("initial warm = %+v, %v", summary, err)
	}

	mismatch.Store(true)
	summary, err := manager.Warm(context.Background(), []string{ref}, imagecache.ArchitectureAMD64, WarmOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Failed != 0 || summary.AlreadyComplete != 1 {
		t.Fatalf("digest-pinned rerun summary = %+v", summary)
	}

	upstream.Close()
	manager.offline.Store(true)
	mirror := httptest.NewServer(http.HandlerFunc(manager.serveCatchAll))
	defer mirror.Close()
	resp, body := get(t, mirror.URL+"/v2/demo/manifests/stable?ns=registry.example")
	if resp.StatusCode != http.StatusOK || body != oldManifest {
		t.Fatalf("cached tag after pinned mismatch = %d %q", resp.StatusCode, body)
	}
}

func TestWarmTagRefreshFailurePreservesPreviouslyCachedTag(t *testing.T) {
	oldConfigDigest := "sha256:" + sha256Hex([]byte("old-config"))
	oldManifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"%s"},"layers":[]}`, oldConfigDigest)
	oldDigest := "sha256:" + sha256Hex([]byte(oldManifest))
	newConfigDigest := "sha256:" + strings.Repeat("c", 64)
	newManifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"%s"},"layers":[]}`, newConfigDigest)
	newDigest := "sha256:" + sha256Hex([]byte(newManifest))

	var refreshBroken atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/demo/manifests/stable":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			if refreshBroken.Load() {
				w.Header().Set("Docker-Content-Digest", newDigest)
				_, _ = fmt.Fprint(w, newManifest)
				return
			}
			w.Header().Set("Docker-Content-Digest", oldDigest)
			_, _ = fmt.Fprint(w, oldManifest)
		case "/v2/demo/manifests/" + oldDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", oldDigest)
			_, _ = fmt.Fprint(w, oldManifest)
		case "/v2/demo/manifests/" + newDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", newDigest)
			_, _ = fmt.Fprint(w, newManifest)
		case "/v2/demo/blobs/" + oldConfigDigest:
			_, _ = io.WriteString(w, "old-config")
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

	if summary, err := manager.Warm(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64, WarmOptions{}); err != nil || summary.Warmed != 1 {
		t.Fatalf("initial warm = %+v, %v", summary, err)
	}

	refreshBroken.Store(true)
	summary, err := manager.Warm(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64, WarmOptions{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Failed != 1 || summary.FailedRevalidate != 1 || summary.Warmed != 0 {
		t.Fatalf("refresh failure summary = %+v", summary)
	}

	upstream.Close()
	manager.offline.Store(true)
	mirror := httptest.NewServer(http.HandlerFunc(manager.serveCatchAll))
	defer mirror.Close()

	resp, body := get(t, mirror.URL+"/v2/demo/manifests/stable?ns=registry.example")
	if resp.StatusCode != http.StatusOK || body != oldManifest {
		t.Fatalf("cached tag after failed refresh = %d %q", resp.StatusCode, body)
	}
}

func TestWarmRefreshCompleteTagTreatsTransientFailureAsAlreadyComplete(t *testing.T) {
	configDigest := "sha256:" + sha256Hex([]byte("config"))
	manifestBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"%s"},"layers":[]}`, configDigest)
	manifestDigest := "sha256:" + sha256Hex([]byte(manifestBody))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/demo/manifests/stable":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = fmt.Fprint(w, manifestBody)
		case "/v2/demo/manifests/" + manifestDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = fmt.Fprint(w, manifestBody)
		case "/v2/demo/blobs/" + configDigest:
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

	if summary, err := manager.Warm(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64, WarmOptions{}); err != nil || summary.Warmed != 1 {
		t.Fatalf("initial warm = %+v, %v", summary, err)
	}

	upstream.Close()
	summary, err := manager.Warm(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64, WarmOptions{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Failed != 0 || summary.AlreadyComplete != 1 || summary.Results[0].RefreshWarning == "" {
		t.Fatalf("network failure summary = %+v", summary)
	}

	manager.offline.Store(true)
	mirror := httptest.NewServer(http.HandlerFunc(manager.serveCatchAll))
	defer mirror.Close()
	resp, body := get(t, mirror.URL+"/v2/demo/manifests/stable?ns=registry.example")
	if resp.StatusCode != http.StatusOK || body != manifestBody {
		t.Fatalf("offline replay after refresh failure = %d %q", resp.StatusCode, body)
	}
}

func TestDiscardWarmResponseBoundsErrorCapture(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, strings.Repeat("x", maxWarmErrorBodyBytes*2))
	})

	err := warmBlobRequest(context.Background(), handler, "demo", "sha256:"+strings.Repeat("1", 64))
	if err == nil {
		t.Fatal("warmBlobRequest succeeded, want error")
	}
	if len(err.Error()) > maxWarmErrorBodyBytes+128 {
		t.Fatalf("error length = %d, want bounded capture", len(err.Error()))
	}
}

func TestWarmColdTagPublishesTagAndDigestFromOneRootResponse(t *testing.T) {
	configDigest := "sha256:" + sha256Hex([]byte("config"))
	manifestBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"%s"},"layers":[]}`, configDigest)
	manifestDigest := "sha256:" + sha256Hex([]byte(manifestBody))

	var tagHits, digestHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/demo/manifests/stable":
			tagHits.Add(1)
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = fmt.Fprint(w, manifestBody)
		case "/v2/demo/manifests/" + manifestDigest:
			digestHits.Add(1)
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

	summary, err := manager.Warm(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64, WarmOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Warmed != 1 || summary.AlreadyComplete != 0 || summary.Failed != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	if tagHits.Load() != 1 || digestHits.Load() != 0 {
		t.Fatalf("root hits: tag=%d digest=%d", tagHits.Load(), digestHits.Load())
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

func TestWarmOneBatchSupportsMultipleRegistriesWithIsolatedOfflineServing(t *testing.T) {
	configOne := "cfg-one"
	configOneDigest := "sha256:" + sha256Hex([]byte(configOne))
	manifestOne := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"` + configOneDigest + `"},"layers":[]}`
	digestOne := "sha256:" + sha256Hex([]byte(manifestOne))
	configTwo := "cfg-two"
	configTwoDigest := "sha256:" + sha256Hex([]byte(configTwo))
	manifestTwo := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"` + configTwoDigest + `"},"layers":[]}`
	digestTwo := "sha256:" + sha256Hex([]byte(manifestTwo))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/one/manifests/" + digestOne:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", digestOne)
			_, _ = fmt.Fprint(w, manifestOne)
		case "/v2/two/manifests/" + digestTwo:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", digestTwo)
			_, _ = fmt.Fprint(w, manifestTwo)
		case "/v2/one/blobs/" + configOneDigest:
			_, _ = io.WriteString(w, configOne)
		case "/v2/two/blobs/" + configTwoDigest:
			_, _ = io.WriteString(w, configTwo)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	cacheRoot := t.TempDir()
	manager := newManagerWithPorts(cacheRoot, nil, freePort(t))
	manager.baseOverride = aliasedURL(t, upstream.URL, "registry.example")
	manager.resolveUpstreamIPs = func(_ context.Context, host string) ([]net.IP, error) {
		switch host {
		case "registry.one", "registry.two", "registry.example":
			return []net.IP{net.ParseIP("203.0.113.10")}, nil
		default:
			return nil, fmt.Errorf("unexpected host %q", host)
		}
	}
	manager.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	manager.dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, hostPortOfURL(t, upstream.URL))
	}
	defer manager.Close()

	summary, err := manager.Warm(context.Background(), []string{
		"registry.one/one@" + digestOne,
		"registry.two/two@" + digestTwo,
	}, imagecache.ArchitectureAMD64, WarmOptions{})

	if err != nil {
		t.Fatal(err)
	}
	if summary.Warmed != 2 || summary.Failed != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	for _, directory := range []string{"registry.one", "registry.two"} {
		if _, err := os.Stat(filepath.Join(cacheRoot, directory)); err != nil {
			t.Fatalf("cache directory %s missing: %v", directory, err)
		}
	}

	manager.offline.Store(true)
	upstream.Close()
	mirror := httptest.NewServer(http.HandlerFunc(manager.serveCatchAll))
	defer mirror.Close()
	for _, test := range []struct {
		ns       string
		path     string
		wantBody string
	}{
		{"registry.one", "/v2/one/manifests/" + digestOne, manifestOne},
		{"registry.two", "/v2/two/manifests/" + digestTwo, manifestTwo},
	} {
		resp, body := get(t, mirror.URL+test.path+"?ns="+test.ns)
		if resp.StatusCode != http.StatusOK || body != test.wantBody {
			t.Fatalf("%s%s = %d %q", test.ns, test.path, resp.StatusCode, body)
		}
	}
}

// Re-running a warm while the mirror is offline must answer from the cache the
// checker reads, not 503 on content that `warm --check` calls complete (#297).
func TestWarmOfflineRerunServesCachedDigestManifestAndAgreesWithCheck(t *testing.T) {
	configDigest := "sha256:" + sha256Hex([]byte("config"))
	layerDigest := "sha256:" + sha256Hex([]byte("layer"))
	manifestBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%d},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"%s","size":%d}]}`,
		configDigest, len("config"), layerDigest, len("layer"))
	manifestDigest := "sha256:" + sha256Hex([]byte(manifestBody))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/pause/manifests/" + manifestDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = fmt.Fprint(w, manifestBody)
		case "/v2/pause/blobs/" + configDigest:
			_, _ = io.WriteString(w, "config")
		case "/v2/pause/blobs/" + layerDigest:
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

	ref := "registry.example/pause@" + manifestDigest
	if _, err := manager.Warm(context.Background(), []string{ref}, imagecache.ArchitectureAMD64, WarmOptions{}); err != nil {
		t.Fatal(err)
	}

	upstream.Close()
	manager.SetOffline(true)

	check, err := manager.Check(context.Background(), []string{ref}, imagecache.ArchitectureAMD64, true)
	if err != nil {
		t.Fatal(err)
	}
	if check.Complete != 1 || check.Failed != 0 {
		t.Fatalf("check summary = %+v", check)
	}

	summary, err := manager.Warm(context.Background(), []string{ref}, imagecache.ArchitectureAMD64, WarmOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Failed != 0 || summary.AlreadyComplete != 1 || summary.Warmed != 0 {
		t.Fatalf("offline warm summary = %+v (results %+v)", summary, summary.Results)
	}
}

func TestWarmOfflineRerunServesCachedTagManifest(t *testing.T) {
	configDigest := "sha256:" + sha256Hex([]byte("config"))
	manifestBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"%s"},"layers":[]}`, configDigest)
	manifestDigest := "sha256:" + sha256Hex([]byte(manifestBody))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/demo/manifests/latest", "/v2/demo/manifests/" + manifestDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = fmt.Fprint(w, manifestBody)
		case "/v2/demo/blobs/" + configDigest:
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

	ref := "registry.example/demo:latest"
	if _, err := manager.Warm(context.Background(), []string{ref}, imagecache.ArchitectureAMD64, WarmOptions{}); err != nil {
		t.Fatal(err)
	}

	upstream.Close()
	manager.SetOffline(true)

	summary, err := manager.Warm(context.Background(), []string{ref}, imagecache.ArchitectureAMD64, WarmOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Failed != 0 || summary.AlreadyComplete != 1 || summary.Warmed != 0 {
		t.Fatalf("offline warm summary = %+v (results %+v)", summary, summary.Results)
	}
}

// The serving handler must replay a cached digest manifest offline even when
// the request asks for a manifest refresh, since no upstream can answer.
func TestOfflineManifestRefreshRequestServesCachedDigestManifest(t *testing.T) {
	manifestBody := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:` + sha256Hex([]byte("config")) + `"},"layers":[]}`
	manifestDigest := "sha256:" + sha256Hex([]byte(manifestBody))

	server := NewServer("https://registry.example", t.TempDir())
	server.setOfflineMode(true)
	requestPath := "/v2/pause/manifests/" + manifestDigest
	if err := server.storeManifest(requestPath, manifestMetadata{
		ContentType:         "application/vnd.oci.image.manifest.v1+json",
		ContentLength:       int64(len(manifestBody)),
		DockerContentDigest: manifestDigest,
	}, []byte(manifestBody)); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, requestPath, nil).WithContext(withManifestRefresh(context.Background()))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("offline refresh request = %d %q, want 200", recorder.Code, strings.TrimSpace(recorder.Body.String()))
	}
	if recorder.Body.String() != manifestBody {
		t.Fatalf("body = %q, want %q", recorder.Body.String(), manifestBody)
	}
	if got := recorder.Header().Get("Docker-Content-Digest"); got != manifestDigest {
		t.Fatalf("Docker-Content-Digest = %q, want %q", got, manifestDigest)
	}
}

func TestWarmDefaultRerunDoesNotReResolveTags(t *testing.T) {
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

	refs := []string{"registry.example/demo:stable", "registry.example/demo@" + manifestDigest}
	first, err := manager.Warm(context.Background(), refs, imagecache.ArchitectureAMD64, WarmOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if first.ReResolvedTags != 1 {
		t.Fatalf("first warm ReResolvedTags = %d, want the one tag-pinned ref", first.ReResolvedTags)
	}

	second, err := manager.Warm(context.Background(), refs, imagecache.ArchitectureAMD64, WarmOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if second.Warmed != 0 || second.AlreadyComplete != 2 {
		t.Fatalf("re-warm summary = %+v, want everything already complete", second)
	}
	if second.ReResolvedTags != 0 {
		t.Fatalf("re-warm ReResolvedTags = %d, want no default refresh", second.ReResolvedTags)
	}
	for _, result := range second.Results {
		if strings.Contains(result.Ref, "@") && result.ReResolvedTag {
			t.Fatalf("digest-pinned ref reported as re-resolved: %+v", result)
		}
	}
}

func TestWarmCheckAndOfflineTagReplayAgreeOnNestedSelectedManifestCompleteness(t *testing.T) {
	const ref = "registry.example/demo:stable"

	hostArch := imagecache.Architecture(runtime.GOARCH)
	nested := newNestedWarmGraphFixture(hostArch)
	cacheRoot := t.TempDir()
	manager := newManagerWithPorts(cacheRoot, nil, 0)
	manager.serverFactory = func(_ string, base, cacheDir string) http.Handler {
		server := NewServer(base, cacheDir)
		server.client.Transport = warmRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			response := &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Request:    request,
			}
			switch request.URL.Path {
			case "/v2/demo/manifests/stable", "/v2/demo/manifests/" + nested.rootDigest:
				response.Header.Set("Content-Type", "application/vnd.oci.image.index.v1+json")
				response.Header.Set("Docker-Content-Digest", nested.rootDigest)
				response.Body = io.NopCloser(strings.NewReader(nested.rootBody))
			case "/v2/demo/manifests/" + nested.indexDigest:
				response.Header.Set("Content-Type", "application/vnd.oci.image.index.v1+json")
				response.Header.Set("Docker-Content-Digest", nested.indexDigest)
				response.Body = io.NopCloser(strings.NewReader(nested.indexBody))
			case "/v2/demo/manifests/" + nested.manifestDigest:
				response.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
				response.Header.Set("Docker-Content-Digest", nested.manifestDigest)
				response.Body = io.NopCloser(strings.NewReader(nested.manifestBody))
			case "/v2/demo/blobs/" + nested.configDigest:
				response.Body = io.NopCloser(strings.NewReader("nested-config"))
			case "/v2/demo/blobs/" + nested.layerDigest:
				response.Body = io.NopCloser(strings.NewReader("nested-layer"))
			default:
				response.StatusCode = http.StatusNotFound
				response.Status = "404 Not Found"
				response.Body = io.NopCloser(strings.NewReader("not found"))
			}
			return response, nil
		})
		return server
	}
	defer manager.Close()

	summary, err := manager.Warm(context.Background(), []string{ref}, hostArch, WarmOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Warmed != 1 || summary.Failed != 0 {
		t.Fatalf("warm summary = %+v", summary)
	}

	check, err := manager.Check(context.Background(), []string{ref}, hostArch, true)
	if err != nil {
		t.Fatal(err)
	}
	if check.Complete != 1 || check.Failed != 0 {
		t.Fatalf("check summary = %+v", check)
	}

	manager.SetOffline(true)
	request := httptest.NewRequest(http.MethodHead, "/v2/demo/manifests/stable?ns=registry.example", nil)
	recorder := httptest.NewRecorder()
	manager.serveCatchAll(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("offline nested HEAD = %d, want 200", recorder.Code)
	}

	cache := NewServer("https://registry.example", filepath.Join(cacheRoot, "registry.example"))
	if err := os.Remove(cache.blobPath(nested.layerDigest)); err != nil {
		t.Fatal(err)
	}

	check, err = manager.Check(context.Background(), []string{ref}, hostArch, true)
	if err != nil {
		t.Fatal(err)
	}
	if check.Complete != 0 || check.Failed != 1 {
		t.Fatalf("missing-blob check summary = %+v", check)
	}
	if got := check.Results[0].Error; !strings.Contains(got, nested.layerDigest) {
		t.Fatalf("missing-blob check error = %q, want %q", got, nested.layerDigest)
	}

	request = httptest.NewRequest(http.MethodHead, "/v2/demo/manifests/stable?ns=registry.example", nil)
	recorder = httptest.NewRecorder()
	manager.serveCatchAll(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("offline nested HEAD after blob removal = %d, want 404", recorder.Code)
	}
}

type nestedWarmGraphFixture struct {
	rootBody       string
	rootDigest     string
	indexBody      string
	indexDigest    string
	manifestBody   string
	manifestDigest string
	configDigest   string
	layerDigest    string
}

func newNestedWarmGraphFixture(targetArch imagecache.Architecture) nestedWarmGraphFixture {
	configDigest := "sha256:" + sha256Hex([]byte("nested-config"))
	layerDigest := "sha256:" + sha256Hex([]byte("nested-layer"))
	manifestBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"%s"},"layers":[{"digest":"%s"}]}`, configDigest, layerDigest)
	manifestDigest := "sha256:" + sha256Hex([]byte(manifestBody))
	foreignArch := "arm64"
	if string(targetArch) == foreignArch {
		foreignArch = "amd64"
	}
	foreignDigest := "sha256:" + strings.Repeat("f", 64)
	indexBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"%s","platform":{"os":"linux","architecture":"%s"}},{"digest":"%s","platform":{"os":"linux","architecture":"%s"}}]}`, manifestDigest, targetArch, foreignDigest, foreignArch)
	indexDigest := "sha256:" + sha256Hex([]byte(indexBody))
	rootBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"%s","platform":{"os":"linux","architecture":"%s"}}]}`, indexDigest, targetArch)
	rootDigest := "sha256:" + sha256Hex([]byte(rootBody))
	return nestedWarmGraphFixture{
		rootBody:       rootBody,
		rootDigest:     rootDigest,
		indexBody:      indexBody,
		indexDigest:    indexDigest,
		manifestBody:   manifestBody,
		manifestDigest: manifestDigest,
		configDigest:   configDigest,
		layerDigest:    layerDigest,
	}
}
