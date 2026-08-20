package mirror

import (
	"net/http"
	"net/http/httptest"
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
