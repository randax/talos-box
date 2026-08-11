package mirror

import (
	"context"
	"fmt"
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

func TestCheckVerifiesHostBlobsWhenIndexRepeatsChildDigestAcrossPlatforms(t *testing.T) {
	cacheRoot := t.TempDir()
	mirrorRoot := filepath.Join(cacheRoot, "mirror")
	manager := newManagerWithPorts(mirrorRoot, nil, freePort(t))
	defer manager.Close()

	server := NewServer("https://registry.example", filepath.Join(mirrorRoot, "registry.example"))
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
	requestPath := "/v2/demo/manifests/" + indexDigest
	if err := server.storeManifest(requestPath, manifestMetadata{
		ContentType:         "application/vnd.oci.image.index.v1+json",
		ContentLength:       int64(len(indexBody)),
		DockerContentDigest: indexDigest,
	}, []byte(indexBody)); err != nil {
		t.Fatal(err)
	}
	if err := server.storeManifest("/v2/demo/manifests/"+childDigest, manifestMetadata{
		ContentType:         "application/vnd.oci.image.manifest.v1+json",
		ContentLength:       int64(len(childManifest)),
		DockerContentDigest: childDigest,
	}, []byte(childManifest)); err != nil {
		t.Fatal(err)
	}

	summary, err := manager.Check(context.Background(), []string{"registry.example/demo@" + indexDigest}, imagecache.ArchitectureAMD64, false)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Complete != 0 || summary.Failed != 1 {
		t.Fatalf("check summary = %+v", summary)
	}
	if len(summary.Results) != 1 || !strings.Contains(summary.Results[0].Error, configDigest) {
		t.Fatalf("check result = %+v, want missing host config blob digest", summary.Results)
	}
}

func TestCheckFailsWhenHostBlobIsMissing(t *testing.T) {
	fixture := newCheckManifestFixture(t)
	defer fixture.manager.Close()

	summary, err := fixture.manager.Warm(context.Background(), []string{fixture.ref}, imagecache.ArchitectureAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Warmed != 1 || summary.Failed != 0 {
		t.Fatalf("warm summary = %+v", summary)
	}
	if err := os.Remove(fixture.server.blobPath(fixture.layerDigest)); err != nil {
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
	if len(checkSummary.Results) != 1 || !strings.Contains(checkSummary.Results[0].Error, fixture.layerDigest) {
		t.Fatalf("check result = %+v, want missing blob digest", checkSummary.Results)
	}
}

func TestCheckFailsWhenHostBlobIsSymlinked(t *testing.T) {
	fixture := newCheckManifestFixture(t)
	defer fixture.manager.Close()

	summary, err := fixture.manager.Warm(context.Background(), []string{fixture.ref}, imagecache.ArchitectureAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Warmed != 1 || summary.Failed != 0 {
		t.Fatalf("warm summary = %+v", summary)
	}

	target := filepath.Join(t.TempDir(), "foreign-blob")
	if err := os.WriteFile(target, []byte("layer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fixture.server.blobPath(fixture.layerDigest)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, fixture.server.blobPath(fixture.layerDigest)); err != nil {
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
	if len(checkSummary.Results) != 1 || !strings.Contains(checkSummary.Results[0].Error, fixture.layerDigest) {
		t.Fatalf("check result = %+v, want symlinked blob digest", checkSummary.Results)
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

func TestCheckRejectsMalformedBlobDigestWithoutPathEscape(t *testing.T) {
	cacheRoot := t.TempDir()
	mirrorRoot := filepath.Join(cacheRoot, "mirror")
	manager := newManagerWithPorts(mirrorRoot, nil, freePort(t))
	defer manager.Close()

	server := NewServer("https://registry.example", filepath.Join(mirrorRoot, "registry.example"))
	manifestBody := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"../sentinel","size":8},"layers":[]}`)
	manifestDigest := "sha256:" + sha256Hex(manifestBody)
	requestPath := "/v2/demo/manifests/" + manifestDigest
	if err := server.storeManifest(requestPath, manifestMetadata{
		ContentType:         "application/vnd.oci.image.manifest.v1+json",
		ContentLength:       int64(len(manifestBody)),
		DockerContentDigest: manifestDigest,
	}, manifestBody); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(server.cacheDir, "sentinel")
	if err := os.WriteFile(sentinel, []byte("escaped"), 0o644); err != nil {
		t.Fatal(err)
	}

	summary, err := manager.Check(context.Background(), []string{"registry.example/demo@" + manifestDigest}, imagecache.ArchitectureAMD64, false)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Complete != 0 || summary.Failed != 1 {
		t.Fatalf("check summary = %+v", summary)
	}
	if len(summary.Results) != 1 || !strings.Contains(summary.Results[0].Error, "../sentinel") {
		t.Fatalf("check result = %+v, want malformed digest failure", summary.Results)
	}
}

func TestCheckRejectsMalformedChildManifestDigest(t *testing.T) {
	cacheRoot := t.TempDir()
	mirrorRoot := filepath.Join(cacheRoot, "mirror")
	manager := newManagerWithPorts(mirrorRoot, nil, freePort(t))
	defer manager.Close()

	server := NewServer("https://registry.example", filepath.Join(mirrorRoot, "registry.example"))
	indexBody := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"../child","size":8,"platform":{"architecture":"amd64","os":"linux"}}]}`)
	indexDigest := "sha256:" + sha256Hex(indexBody)
	requestPath := "/v2/demo/manifests/" + indexDigest
	if err := server.storeManifest(requestPath, manifestMetadata{
		ContentType:         "application/vnd.oci.image.index.v1+json",
		ContentLength:       int64(len(indexBody)),
		DockerContentDigest: indexDigest,
	}, indexBody); err != nil {
		t.Fatal(err)
	}

	summary, err := manager.Check(context.Background(), []string{"registry.example/demo@" + indexDigest}, imagecache.ArchitectureAMD64, false)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Complete != 0 || summary.Failed != 1 {
		t.Fatalf("check summary = %+v", summary)
	}
	if len(summary.Results) != 1 || !strings.Contains(summary.Results[0].Error, "../child") {
		t.Fatalf("check result = %+v, want malformed child digest failure", summary.Results)
	}
}

func TestCheckRejectsSymlinkedBlobInPlainMode(t *testing.T) {
	fixture := newCheckManifestFixture(t)
	defer fixture.manager.Close()

	summary, err := fixture.manager.Warm(context.Background(), []string{fixture.ref}, imagecache.ArchitectureAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Warmed != 1 || summary.Failed != 0 {
		t.Fatalf("warm summary = %+v", summary)
	}

	realBlob := fixture.server.blobPath(fixture.layerDigest)
	blobData, err := os.ReadFile(realBlob)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "layer")
	if err := os.WriteFile(target, blobData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(realBlob); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, realBlob); err != nil {
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
	if len(checkSummary.Results) != 1 || !strings.Contains(checkSummary.Results[0].Error, fixture.layerDigest) {
		t.Fatalf("check result = %+v, want symlinked blob failure", checkSummary.Results)
	}
}

func TestCheckAttemptsEveryRefAndReportsEachResult(t *testing.T) {
	cacheRoot := t.TempDir()
	mirrorRoot := filepath.Join(cacheRoot, "mirror")
	manager := newManagerWithPorts(mirrorRoot, nil, freePort(t))
	defer manager.Close()

	server := NewServer("https://registry.example", filepath.Join(mirrorRoot, "registry.example"))
	goodManifest, goodDigest, goodBlob := cacheCheckManifestFixture(t, "good")
	badManifest, badDigest, _ := cacheCheckManifestFixture(t, "bad")
	for _, manifest := range []struct {
		repo   string
		digest string
		body   []byte
	}{
		{repo: "good", digest: goodDigest, body: goodManifest},
		{repo: "bad", digest: badDigest, body: badManifest},
	} {
		path := "/v2/" + manifest.repo + "/manifests/" + manifest.digest
		if err := server.storeManifest(path, manifestMetadata{
			ContentType:         "application/vnd.oci.image.manifest.v1+json",
			ContentLength:       int64(len(manifest.body)),
			DockerContentDigest: manifest.digest,
		}, manifest.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(server.blobPath(goodBlob.digest)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server.blobPath(goodBlob.digest), goodBlob.body, 0o644); err != nil {
		t.Fatal(err)
	}

	summary, err := manager.Check(context.Background(), []string{
		"registry.example/bad@" + badDigest,
		"registry.example/good@" + goodDigest,
	}, imagecache.ArchitectureAMD64, false)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Complete != 1 || summary.Failed != 1 {
		t.Fatalf("check summary = %+v", summary)
	}
	if len(summary.Results) != 2 {
		t.Fatalf("results len = %d, want 2", len(summary.Results))
	}
	if summary.Results[0].Ref != "registry.example/bad@"+badDigest || summary.Results[0].Error == "" {
		t.Fatalf("first result = %+v, want failed bad ref", summary.Results[0])
	}
	if summary.Results[1].Ref != "registry.example/good@"+goodDigest || summary.Results[1].Error != "" {
		t.Fatalf("second result = %+v, want complete good ref", summary.Results[1])
	}
}

func TestCheckAllowsEmptyBlobInPlainMode(t *testing.T) {
	cacheRoot := t.TempDir()
	mirrorRoot := filepath.Join(cacheRoot, "mirror")
	configBody := []byte{}
	configDigest := "sha256:" + sha256Hex(configBody)
	manifestBody := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":0},"layers":[]}`,
		configDigest)
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

	summary, err := manager.Warm(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Warmed != 1 || summary.Failed != 0 {
		t.Fatalf("warm summary = %+v", summary)
	}
	upstream.Close()

	checkSummary, err := manager.Check(context.Background(), []string{"registry.example/demo:stable"}, imagecache.ArchitectureAMD64, false)
	if err != nil {
		t.Fatal(err)
	}
	if checkSummary.Complete != 1 || checkSummary.Failed != 0 {
		t.Fatalf("check summary = %+v", checkSummary)
	}
}

func TestCheckUsesNoNetworkWhenCacheIsComplete(t *testing.T) {
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

	var resolves atomic.Int64
	var dials atomic.Int64
	fixture.manager.resolveUpstreamIPs = func(context.Context, string) ([]net.IP, error) {
		resolves.Add(1)
		return nil, fmt.Errorf("unexpected resolve")
	}
	fixture.manager.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	fixture.manager.dialContext = func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, fmt.Errorf("unexpected dial")
	}

	checkSummary, err := fixture.manager.Check(context.Background(), []string{fixture.ref}, imagecache.ArchitectureAMD64, false)
	if err != nil {
		t.Fatal(err)
	}
	if checkSummary.Complete != 1 || checkSummary.Failed != 0 {
		t.Fatalf("check summary = %+v", checkSummary)
	}
	if resolves.Load() != 0 || dials.Load() != 0 {
		t.Fatalf("offline check touched network: resolves=%d dials=%d", resolves.Load(), dials.Load())
	}
}

func TestCheckRejectsOversizedCachedManifest(t *testing.T) {
	cacheRoot := t.TempDir()
	mirrorRoot := filepath.Join(cacheRoot, "mirror")
	manager := newManagerWithPorts(mirrorRoot, nil, freePort(t))
	defer manager.Close()

	server := NewServer("https://registry.example", filepath.Join(mirrorRoot, "registry.example"))
	oversizedBody := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","padding":"` + strings.Repeat("a", maxManifestBytes) + `"}`)
	manifestDigest := "sha256:" + sha256Hex(oversizedBody)
	requestPath := "/v2/demo/manifests/" + manifestDigest
	if err := os.MkdirAll(filepath.Dir(server.manifestPath(requestPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server.manifestPath(requestPath), oversizedBody, 0o644); err != nil {
		t.Fatal(err)
	}

	summary, err := manager.Check(context.Background(), []string{"registry.example/demo@" + manifestDigest}, imagecache.ArchitectureAMD64, false)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Complete != 0 || summary.Failed != 1 {
		t.Fatalf("check summary = %+v", summary)
	}
	if len(summary.Results) != 1 || !strings.Contains(summary.Results[0].Error, "exceeds") {
		t.Fatalf("check result = %+v, want oversized manifest failure", summary.Results)
	}
}

func TestCheckIgnoresOversizedMetadataSidecar(t *testing.T) {
	cacheRoot := t.TempDir()
	mirrorRoot := filepath.Join(cacheRoot, "mirror")
	manager := newManagerWithPorts(mirrorRoot, nil, freePort(t))
	defer manager.Close()

	server := NewServer("https://registry.example", filepath.Join(mirrorRoot, "registry.example"))
	configBody := []byte("cfg")
	configDigest := "sha256:" + sha256Hex(configBody)
	manifestBody := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%d},"layers":[]}`,
		configDigest, len(configBody)))
	manifestDigest := "sha256:" + sha256Hex(manifestBody)
	requestPath := "/v2/demo/manifests/" + manifestDigest
	if err := server.storeManifest(requestPath, manifestMetadata{
		ContentType:         "application/vnd.oci.image.manifest.v1+json",
		ContentLength:       int64(len(manifestBody)),
		DockerContentDigest: manifestDigest,
	}, manifestBody); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(server.blobPath(configDigest)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server.blobPath(configDigest), configBody, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server.manifestMetadataPath(requestPath), []byte(`{"dockerContentDigest":"sha512:`+strings.Repeat("a", 128)+`","padding":"`+strings.Repeat("b", maxCachedManifestSidecarBytes)+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	summary, err := manager.Check(context.Background(), []string{"registry.example/demo@" + manifestDigest}, imagecache.ArchitectureAMD64, false)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Complete != 1 || summary.Failed != 0 {
		t.Fatalf("check summary = %+v", summary)
	}
}

func TestOpenCheckedRegularFileRejectsPathSwapAfterOpen(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "manifest.json")
	original := []byte("one")
	replacement := filepath.Join(root, "replacement.json")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacement, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := openCheckedRegularFile(path, func() {
		if renameErr := os.Rename(path, filepath.Join(root, "manifest.old")); renameErr != nil {
			t.Fatal(renameErr)
		}
		if renameErr := os.Rename(replacement, path); renameErr != nil {
			t.Fatal(renameErr)
		}
	})
	if err == nil || !strings.Contains(err.Error(), "changed during open") {
		t.Fatalf("openCheckedRegularFile() error = %v, want changed-during-open failure", err)
	}
}

func TestCheckRejectsSymlinkedManifestInPlainMode(t *testing.T) {
	fixture := newCheckManifestFixture(t)
	defer fixture.manager.Close()

	summary, err := fixture.manager.Warm(context.Background(), []string{fixture.ref}, imagecache.ArchitectureAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Warmed != 1 || summary.Failed != 0 {
		t.Fatalf("warm summary = %+v", summary)
	}

	digestPath := fixture.server.manifestPath("/v2/demo/manifests/" + fixture.manifestDigest)
	manifestData, err := os.ReadFile(digestPath)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(target, manifestData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(digestPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, digestPath); err != nil {
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
	if len(checkSummary.Results) != 1 || !strings.Contains(checkSummary.Results[0].Error, fixture.manifestDigest) {
		t.Fatalf("check result = %+v, want symlinked manifest failure", checkSummary.Results)
	}
}

type cacheCheckBlobFixture struct {
	digest string
	body   []byte
}

func cacheCheckManifestFixture(t *testing.T, payload string) ([]byte, string, cacheCheckBlobFixture) {
	t.Helper()
	blob := cacheCheckBlobFixture{
		body:   []byte(payload),
		digest: "sha256:" + sha256Hex([]byte(payload)),
	}
	manifestBody := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%d},"layers":[]}`,
		blob.digest, len(blob.body)))
	return manifestBody, "sha256:" + sha256Hex(manifestBody), blob
}

type checkManifestFixture struct {
	ref            string
	manifestDigest string
	layerDigest    string
	manager        *Manager
	server         *Server
	upstream       *httptest.Server
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
		ref:            "registry.example/demo:stable",
		manifestDigest: manifestDigest,
		layerDigest:    layerDigest,
		manager:        manager,
		server:         NewServer("https://registry.example", filepath.Join(mirrorRoot, "registry.example")),
		upstream:       upstream,
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
