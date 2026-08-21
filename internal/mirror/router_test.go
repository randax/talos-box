package mirror

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// The OCI distribution version check is the standard liveness probe; requiring
// the containerd `ns` parameter on it defeats hand-verification of the mirror
// while adding nothing (#408). Content endpoints still need `ns`.
func TestCatchAllVersionPingAnswersWithoutNamespace(t *testing.T) {
	manager := newManagerWithPorts(t.TempDir(), nil, 0)
	defer manager.Close()

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		recorder := httptest.NewRecorder()
		manager.serveCatchAll(recorder, httptest.NewRequest(method, "/v2/", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s /v2/ = %d %q, want 200", method, recorder.Code, recorder.Body.String())
		}
		if got := recorder.Header().Get("Docker-Distribution-API-Version"); got != "registry/2.0" {
			t.Fatalf("%s /v2/ Docker-Distribution-API-Version = %q, want registry/2.0", method, got)
		}
		if body := recorder.Body.String(); body != "" {
			t.Fatalf("%s /v2/ body = %q, want empty", method, body)
		}
	}
}

func TestCatchAllContentEndpointsStillRequireNamespace(t *testing.T) {
	manager := newManagerWithPorts(t.TempDir(), nil, 0)
	defer manager.Close()

	for _, path := range []string{"/v2/app/manifests/1.0", "/v2/app/blobs/sha256:" + strings.Repeat("a", 64)} {
		recorder := httptest.NewRecorder()
		manager.serveCatchAll(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("GET %s = %d, want 400 for a missing ns", path, recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), "missing ns query parameter") {
			t.Fatalf("GET %s body = %q, want it to name the missing ns parameter", path, recorder.Body.String())
		}
	}
}

func TestCatchAllRejectsWritesBeforeRequiringNamespace(t *testing.T) {
	manager := newManagerWithPorts(t.TempDir(), nil, 0)
	defer manager.Close()

	recorder := httptest.NewRecorder()
	manager.serveCatchAll(recorder, httptest.NewRequest(http.MethodPost, "/v2/", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v2/ = %d %q, want 405", recorder.Code, recorder.Body.String())
	}
}

// The manifest cache key is an opaque `v2-<hash>`, so the sidecar has to name
// the repository and reference it stands for or the on-disk cache cannot be
// answered with grep (#406).
func TestStoreManifestRecordsRepositoryAndReferenceInSidecar(t *testing.T) {
	server := &Server{cacheDir: t.TempDir()}
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:` + strings.Repeat("a", 64) + `"},"layers":[]}`)
	requestPath := "/v2/library/busybox/manifests/1.37"
	metadata := manifestMetadata{
		ContentType:         "application/vnd.oci.image.manifest.v1+json",
		ContentLength:       int64(len(body)),
		DockerContentDigest: "sha256:" + sha256Hex(body),
	}
	if err := server.storeManifest(requestPath, metadata, body); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(server.manifestMetadataPath(requestPath))
	if err != nil {
		t.Fatal(err)
	}
	var stored manifestMetadata
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Repository != "library/busybox" || stored.Reference != "1.37" {
		t.Fatalf("sidecar = %+v, want repository library/busybox reference 1.37", stored)
	}
	if !strings.Contains(string(raw), "library/busybox") {
		t.Fatalf("sidecar %q is not greppable by repository", raw)
	}
	// The replay path still reads what it always did.
	if got := server.cachedManifestMetadata(requestPath, body); got.DockerContentDigest != metadata.DockerContentDigest || got.ContentType != metadata.ContentType {
		t.Fatalf("cached metadata = %+v, want %+v", got, metadata)
	}
}
