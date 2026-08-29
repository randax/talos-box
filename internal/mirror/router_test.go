package mirror

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestRouteCatchAllRequest(t *testing.T) {
	tests := []struct {
		name          string
		rawURL        string
		wantAuthority string
		wantPath      string
		wantQuery     string
		wantError     string
	}{
		{
			name:          "namespace keeps repository path",
			rawURL:        "/v2/docker/library/golang/manifests/latest?ns=public.ecr.aws",
			wantAuthority: "public.ecr.aws",
			wantPath:      "/v2/docker/library/golang/manifests/latest",
		},
		{
			name:          "path prefix strips one authority segment",
			rawURL:        "/v2/public.ecr.aws/docker/library/golang/manifests/latest",
			wantAuthority: "public.ecr.aws",
			wantPath:      "/v2/docker/library/golang/manifests/latest",
		},
		{
			name:          "namespace wins over host-looking repository root",
			rawURL:        "/v2/repository.example/team/app/manifests/latest?ns=public.ecr.aws",
			wantAuthority: "public.ecr.aws",
			wantPath:      "/v2/repository.example/team/app/manifests/latest",
		},
		{
			name:          "path route preserves additional query parameters",
			rawURL:        "/v2/public.ecr.aws/team/app/manifests/latest?variant=debug&view=full",
			wantAuthority: "public.ecr.aws",
			wantPath:      "/v2/team/app/manifests/latest",
			wantQuery:     "variant=debug&view=full",
		},
		{
			name:          "namespace route removes only namespace",
			rawURL:        "/v2/team/app/manifests/latest?variant=debug&ns=public.ecr.aws&view=full",
			wantAuthority: "public.ecr.aws",
			wantPath:      "/v2/team/app/manifests/latest",
			wantQuery:     "variant=debug&view=full",
		},
		{
			name:      "ordinary repository root needs namespace",
			rawURL:    "/v2/library/busybox/manifests/latest",
			wantError: "missing ns query parameter",
		},
		{
			name:      "host-only prefix is incomplete",
			rawURL:    "/v2/public.ecr.aws/",
			wantError: "repository operation",
		},
		{
			name:      "empty first segment is rejected",
			rawURL:    "/v2//public.ecr.aws/app/manifests/latest",
			wantError: "missing ns query parameter",
		},
		{
			name:      "double slash after authority is rejected",
			rawURL:    "/v2/public.ecr.aws//app/manifests/latest",
			wantError: "repository operation",
		},
		{
			name:          "uppercase trailing dot canonicalizes",
			rawURL:        "/v2/PUBLIC.ECR.AWS./team/app/manifests/latest",
			wantAuthority: "public.ecr.aws",
			wantPath:      "/v2/team/app/manifests/latest",
		},
		{
			name:          "explicit port is accepted",
			rawURL:        "/v2/registry.example:5443/team/app/blobs/sha256:abc",
			wantAuthority: "registry.example:5443",
			wantPath:      "/v2/team/app/blobs/sha256:abc",
		},
		{
			name:          "bracketed IPv6 is accepted",
			rawURL:        "/v2/[2001:DB8::1]:5443/team/app/manifests/latest",
			wantAuthority: "[2001:db8::1]:5443",
			wantPath:      "/v2/team/app/manifests/latest",
		},
		{name: "encoded slash is rejected", rawURL: "/v2/public.ecr.aws%2Fattacker/team/app/manifests/latest", wantError: "malformed path-prefixed authority"},
		{name: "encoded repository slash is rejected", rawURL: "/v2/public.ecr.aws/team%2Fapp/manifests/latest", wantError: "encoded slash in repository path"},
		{name: "at sign is rejected", rawURL: "/v2/user@public.ecr.aws/team/app/manifests/latest", wantError: "malformed ns authority"},
		{name: "encoded question mark is rejected", rawURL: "/v2/public.ecr.aws%3Fquery/team/app/manifests/latest", wantError: "malformed ns authority"},
		{name: "encoded fragment is rejected", rawURL: "/v2/public.ecr.aws%23fragment/team/app/manifests/latest", wantError: "malformed ns authority"},
		{name: "scheme is rejected", rawURL: "/v2/https:%2F%2Fpublic.ecr.aws/team/app/manifests/latest", wantError: "malformed path-prefixed authority"},
		{name: "invalid port is rejected", rawURL: "/v2/public.ecr.aws:70000/team/app/manifests/latest", wantError: "malformed ns authority"},
		{name: "underscore is rejected", rawURL: "/v2/public_ecr.aws/team/app/manifests/latest", wantError: "malformed ns authority"},
		{name: "malformed brackets are rejected", rawURL: "/v2/[2001:db8::1/team/app/manifests/latest", wantError: "malformed ns authority"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, err := url.Parse(test.rawURL)
			if err != nil {
				t.Fatal(err)
			}
			route, err := routeCatchAllRequest(raw)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("routeCatchAllRequest(%q) error = %v, want containing %q", test.rawURL, err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if route.Authority.canonicalAuthority != test.wantAuthority {
				t.Fatalf("authority = %q, want %q", route.Authority.canonicalAuthority, test.wantAuthority)
			}
			if route.Target.EscapedPath() != test.wantPath {
				t.Fatalf("target path = %q, want %q", route.Target.EscapedPath(), test.wantPath)
			}
			if route.Target.RawQuery != test.wantQuery {
				t.Fatalf("target query = %q, want %q", route.Target.RawQuery, test.wantQuery)
			}
		})
	}
}

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

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		recorder := httptest.NewRecorder()
		manager.serveCatchAll(recorder, httptest.NewRequest(method, "/v2/public_ecr.aws/app/manifests/latest", nil))
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s malformed path = %d %q, want 405", method, recorder.Code, recorder.Body.String())
		}
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
