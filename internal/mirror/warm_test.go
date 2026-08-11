package mirror

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
			name:          "tag-at-digest keeps tag listed ref",
			ref:           "registry.example/demo/app:v1.0.0@" + uppercaseDigest,
			wantTagHits:   1,
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

			result, err := manager.Warm(context.Background(), []string{test.ref}, imagecache.ArchitectureAMD64)
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
	}, imagecache.ArchitectureAMD64)
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
	}, imagecache.ArchitectureAMD64); err != nil || summary.Warmed != 2 {
		t.Fatalf("initial warm = %+v, %v", summary, err)
	}

	corrupt.Store(true)
	summary, err := manager.Warm(context.Background(), []string{
		"registry.example/demo:stable",
		"registry.example/demo:v2.0.0@" + pinnedDigest,
	}, imagecache.ArchitectureAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Failed != 2 {
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

	summary, err := manager.Warm(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64)
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

	summary, err := manager.Warm(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64)
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

	summary, err := manager.Warm(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64)
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

			summary, err := manager.Warm(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64)
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

			summary, err := manager.Warm(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64)
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
	if err := warmManifestGraph(context.Background(), handler, server, "demo", []byte(manifestBody), string(imagecache.ArchitectureAMD64), map[string]bool{}, map[string]bool{}, result); err != nil {
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
	if summary, err := manager.Warm(context.Background(), []string{ref}, imagecache.ArchitectureAMD64); err != nil || summary.Warmed != 1 {
		t.Fatalf("initial warm = %+v, %v", summary, err)
	}

	mismatch.Store(true)
	summary, err := manager.Warm(context.Background(), []string{ref}, imagecache.ArchitectureAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Failed != 1 {
		t.Fatalf("mismatch rerun summary = %+v", summary)
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

	if summary, err := manager.Warm(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64); err != nil || summary.Warmed != 1 {
		t.Fatalf("initial warm = %+v, %v", summary, err)
	}

	refreshBroken.Store(true)
	summary, err := manager.Warm(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Failed != 1 || summary.Warmed != 0 {
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

func TestWarmRefreshNetworkFailureDoesNotFallbackToCachedManifest(t *testing.T) {
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

	if summary, err := manager.Warm(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64); err != nil || summary.Warmed != 1 {
		t.Fatalf("initial warm = %+v, %v", summary, err)
	}

	upstream.Close()
	summary, err := manager.Warm(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Failed != 1 || summary.Results[0].Error == "" {
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
	}, imagecache.ArchitectureAMD64)
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
