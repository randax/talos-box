package mirror

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/imagecache"
)

func TestCheckPassesOfflineAfterCompletedWarm(t *testing.T) {
	fixture := newCheckManifestFixture(t)
	defer fixture.manager.Close()

	summary, err := fixture.manager.Warm(context.Background(), []string{fixture.ref}, imagecache.ArchitectureAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Warmed != 1 || summary.Failed != 0 {
		t.Fatalf("warm summary = %+v", summary)
	}
	fixture.upstream.Close()

	summaryCheck, err := fixture.manager.Check(context.Background(), []string{fixture.ref}, imagecache.ArchitectureAMD64, false)
	if err != nil {
		t.Fatal(err)
	}
	if summaryCheck.Complete != 1 || summaryCheck.Failed != 0 {
		t.Fatalf("check summary = %+v", summaryCheck)
	}
}

func TestCheckFailsWhenChildManifestIsMissing(t *testing.T) {
	fixture := newCheckIndexFixture(t)
	defer fixture.manager.Close()

	summary, err := fixture.manager.Warm(context.Background(), []string{fixture.ref}, imagecache.ArchitectureAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Warmed != 1 || summary.Failed != 0 {
		t.Fatalf("warm summary = %+v", summary)
	}
	if err := os.Remove(fixture.server.manifestPath("/v2/demo/manifests/" + fixture.childDigest)); err != nil {
		t.Fatal(err)
	}
	fixture.upstream.Close()

	checkSummary, err := fixture.manager.Check(context.Background(), []string{fixture.ref}, imagecache.ArchitectureAMD64, false)
	if err != nil {
		t.Fatal(err)
	}
	if checkSummary.Complete != 0 || checkSummary.Failed != 1 {
		t.Fatalf("check summary = %+v", checkSummary)
	}
	if len(checkSummary.Results) != 1 || !strings.Contains(checkSummary.Results[0].Error, fixture.childDigest) {
		t.Fatalf("check result = %+v, want missing child manifest digest", checkSummary.Results)
	}
}

func TestCheckDeepCatchesCorruptBlobButPlainCheckDoesNot(t *testing.T) {
	fixture := newCheckManifestFixture(t)
	defer fixture.manager.Close()

	summary, err := fixture.manager.Warm(context.Background(), []string{fixture.ref}, imagecache.ArchitectureAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Warmed != 1 || summary.Failed != 0 {
		t.Fatalf("warm summary = %+v", summary)
	}
	if err := os.WriteFile(fixture.server.blobPath(fixture.layerDigest), []byte("wrong"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.upstream.Close()

	plainSummary, err := fixture.manager.Check(context.Background(), []string{fixture.ref}, imagecache.ArchitectureAMD64, false)
	if err != nil {
		t.Fatal(err)
	}
	if plainSummary.Complete != 1 || plainSummary.Failed != 0 {
		t.Fatalf("plain check summary = %+v", plainSummary)
	}

	deepSummary, err := fixture.manager.Check(context.Background(), []string{fixture.ref}, imagecache.ArchitectureAMD64, true)
	if err != nil {
		t.Fatal(err)
	}
	if deepSummary.Complete != 0 || deepSummary.Failed != 1 {
		t.Fatalf("deep check summary = %+v", deepSummary)
	}
	if len(deepSummary.Results) != 1 || !strings.Contains(deepSummary.Results[0].Error, fixture.layerDigest) {
		t.Fatalf("deep check result = %+v, want corrupt blob digest", deepSummary.Results)
	}
}

type checkManifestFixture struct {
	ref         string
	layerDigest string
	manager     *Manager
	server      *Server
	upstream    *httptest.Server
}

func newCheckManifestFixture(t *testing.T) checkManifestFixture {
	t.Helper()

	cacheRoot := t.TempDir()
	mirrorRoot := filepath.Join(cacheRoot, "mirror")
	configBody := []byte("cfg")
	layerBody := []byte("layer")
	configDigest := "sha256:" + sha256Hex(configBody)
	layerDigest := "sha256:" + sha256Hex(layerBody)
	manifestBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%d},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"%s","size":%d}]}`,
		configDigest, len(configBody), layerDigest, len(layerBody))
	manifestDigest := "sha256:" + sha256Hex([]byte(manifestBody))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/demo/manifests/stable", "/v2/demo/manifests/" + manifestDigest:
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

	manager := newManagerWithPorts(mirrorRoot, nil, freePort(t))
	manager.baseOverride = aliasedURL(t, upstream.URL, "registry.example")
	egress := egressForRoutes(aliasRoute(t, upstream.URL, "registry.example", "203.0.113.10"))
	manager.resolveUpstreamIPs = egress.resolve
	manager.hostOwnedIPs = egress.hostIPs
	manager.dialContext = egress.dialContext

	return checkManifestFixture{
		ref:         "registry.example/demo:stable",
		layerDigest: layerDigest,
		manager:     manager,
		server:      NewServer("https://registry.example", filepath.Join(mirrorRoot, "registry.example")),
		upstream:    upstream,
	}
}

type checkIndexFixture struct {
	ref         string
	childDigest string
	manager     *Manager
	server      *Server
	upstream    *httptest.Server
}

func newCheckIndexFixture(t *testing.T) checkIndexFixture {
	t.Helper()

	cacheRoot := t.TempDir()
	mirrorRoot := filepath.Join(cacheRoot, "mirror")
	configBody := []byte("cfg")
	layerBody := []byte("layer")
	configDigest := "sha256:" + sha256Hex(configBody)
	layerDigest := "sha256:" + sha256Hex(layerBody)
	childManifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%d},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"%s","size":%d}]}`,
		configDigest, len(configBody), layerDigest, len(layerBody))
	childDigest := "sha256:" + sha256Hex([]byte(childManifest))
	indexBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"%s","size":%d,"platform":{"architecture":"amd64","os":"linux"}}]}`,
		childDigest, len(childManifest))
	indexDigest := "sha256:" + sha256Hex([]byte(indexBody))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/demo/manifests/stable", "/v2/demo/manifests/" + indexDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			w.Header().Set("Docker-Content-Digest", indexDigest)
			_, _ = fmt.Fprint(w, indexBody)
		case "/v2/demo/manifests/" + childDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", childDigest)
			_, _ = fmt.Fprint(w, childManifest)
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

	manager := newManagerWithPorts(mirrorRoot, nil, freePort(t))
	manager.baseOverride = aliasedURL(t, upstream.URL, "registry.example")
	egress := egressForRoutes(aliasRoute(t, upstream.URL, "registry.example", "203.0.113.10"))
	manager.resolveUpstreamIPs = egress.resolve
	manager.hostOwnedIPs = egress.hostIPs
	manager.dialContext = egress.dialContext

	return checkIndexFixture{
		ref:         "registry.example/demo:stable",
		childDigest: childDigest,
		manager:     manager,
		server:      NewServer("https://registry.example", filepath.Join(mirrorRoot, "registry.example")),
		upstream:    upstream,
	}
}
