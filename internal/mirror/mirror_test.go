package mirror

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

const blobDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
const manifestBody = `{"schemaVersion":2}`

// fakeRegistry stands in for an upstream: manifest at /v2/app/manifests/latest,
// blob behind a 302 to a "CDN", optional bearer-token gate.
type fakeRegistry struct {
	registry *httptest.Server
	cdn      *httptest.Server
	token    *httptest.Server

	requireToken bool
	blobHits     atomic.Int64
	tokenHits    atomic.Int64
}

func newFakeRegistry(t *testing.T, requireToken bool) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{requireToken: requireToken}

	f.cdn = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.blobHits.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("blob-bytes"))
	}))
	t.Cleanup(f.cdn.Close)

	f.token = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.tokenHits.Add(1)
		if r.URL.Query().Get("service") != "fake-service" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprint(w, `{"token":"secret-token"}`)
	}))
	t.Cleanup(f.token.Close)

	f.registry = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.requireToken && r.Header.Get("Authorization") != "Bearer secret-token" {
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm=%q,service="fake-service",scope="repository:app:pull"`, f.token.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/v2/":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/v2/app/manifests/latest":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", "sha256:"+sha256Hex([]byte(manifestBody)))
			_, _ = fmt.Fprint(w, manifestBody)
		case strings.HasPrefix(r.URL.Path, "/v2/app/blobs/sha256:"):
			http.Redirect(w, r, f.cdn.URL+"/cdn-blob", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.registry.Close)
	return f
}

func startMirror(t *testing.T, f *fakeRegistry) *httptest.Server {
	t.Helper()
	server := NewServer(f.registry.URL, t.TempDir())
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	return ts
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

func TestPullThroughManifestAndBlob(t *testing.T) {
	f := newFakeRegistry(t, false)
	mirror := startMirror(t, f)

	resp, _ := get(t, mirror.URL+"/v2/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v2/ = %d", resp.StatusCode)
	}

	resp, body := get(t, mirror.URL+"/v2/app/manifests/latest")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "schemaVersion") {
		t.Fatalf("manifest = %d %q", resp.StatusCode, body)
	}
	if got, want := resp.Header.Get("Docker-Content-Digest"), "sha256:"+sha256Hex([]byte(manifestBody)); got != want {
		t.Errorf("digest header %q not forwarded", got)
	}

	// blob: mirror must follow the CDN redirect server-side
	realDigest := "sha256:" + sha256Hex([]byte("blob-bytes"))
	resp, body = get(t, mirror.URL+"/v2/app/blobs/"+realDigest)
	if resp.StatusCode != http.StatusOK || body != "blob-bytes" {
		t.Fatalf("blob = %d %q", resp.StatusCode, body)
	}
	if resp.Request.URL.Host != strings.TrimPrefix(mirror.URL, "http://") {
		t.Errorf("client was redirected off-mirror to %s", resp.Request.URL)
	}
}

func TestBlobCacheSurvivesRestart(t *testing.T) {
	f := newFakeRegistry(t, false)
	realDigest := "sha256:" + sha256Hex([]byte("blob-bytes"))
	dir := t.TempDir()
	ts := httptest.NewServer(NewServer(f.registry.URL, dir))
	defer ts.Close()

	_, body := get(t, ts.URL+"/v2/app/blobs/"+realDigest)
	if body != "blob-bytes" {
		t.Fatalf("first pull got %q", body)
	}
	hits := f.blobHits.Load()

	// a NEW server over the same directory (daemon restart) must serve from disk
	restarted := httptest.NewServer(NewServer(f.registry.URL, dir))
	defer restarted.Close()
	_, body = get(t, restarted.URL+"/v2/app/blobs/"+realDigest)
	if body != "blob-bytes" {
		t.Fatalf("cached pull got %q", body)
	}
	if f.blobHits.Load() != hits {
		t.Errorf("upstream hit again after restart: %d -> %d", hits, f.blobHits.Load())
	}
}

func TestAnonymousTokenFlow(t *testing.T) {
	f := newFakeRegistry(t, true)
	mirror := startMirror(t, f)

	resp, body := get(t, mirror.URL+"/v2/app/manifests/latest")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "schemaVersion") {
		t.Fatalf("manifest through token gate = %d %q", resp.StatusCode, body)
	}
	if f.tokenHits.Load() == 0 {
		t.Error("token endpoint never consulted")
	}
	// token is reused for the next request within its lifetime
	_, _ = get(t, mirror.URL+"/v2/app/manifests/latest")
	if f.tokenHits.Load() > 1 {
		t.Errorf("token fetched %d times, want cached reuse", f.tokenHits.Load())
	}
}

func TestVersionPingAnsweredLocally(t *testing.T) {
	// ghcr/quay deny scopeless anonymous tokens, so /v2/ must never depend on
	// upstream auth — the mirror answers it itself
	f := newFakeRegistry(t, true)
	registryHits := f.tokenHits.Load()
	mirror := startMirror(t, f)
	resp, _ := get(t, mirror.URL+"/v2/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v2/ = %d, want 200 without upstream involvement", resp.StatusCode)
	}
	if f.tokenHits.Load() != registryHits {
		t.Error("/v2/ ping consulted the upstream token endpoint")
	}
}

func TestBlobIntegrityBeforeServing(t *testing.T) {
	tests := []struct {
		name       string
		digest     string
		wantStatus int
		wantBody   string
		wantCached bool
	}{
		{
			name:       "digest mismatch is rejected",
			digest:     blobDigest,
			wantStatus: http.StatusBadGateway,
			wantCached: false,
		},
		{
			name:       "verified blob is served and cached",
			digest:     "sha256:" + sha256Hex([]byte("blob-bytes")),
			wantStatus: http.StatusOK,
			wantBody:   "blob-bytes",
			wantCached: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newFakeRegistry(t, false)
			dir := t.TempDir()
			server := NewServer(f.registry.URL, dir)
			ts := httptest.NewServer(server)
			defer ts.Close()

			resp, body := get(t, ts.URL+"/v2/app/blobs/"+test.digest)
			if resp.StatusCode != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %q", resp.StatusCode, test.wantStatus, body)
			}
			if test.wantBody != "" && body != test.wantBody {
				t.Fatalf("body = %q, want %q", body, test.wantBody)
			}
			if !test.wantCached && body == "blob-bytes" {
				t.Fatal("corrupt bytes were served to the first puller")
			}

			_, err := os.Stat(server.blobPath(test.digest))
			if gotCached := err == nil; gotCached != test.wantCached {
				t.Fatalf("cached = %t, want %t (stat error: %v)", gotCached, test.wantCached, err)
			}

			hits := f.blobHits.Load()
			_, _ = get(t, ts.URL+"/v2/app/blobs/"+test.digest)
			if gotHitAgain := f.blobHits.Load() > hits; gotHitAgain == test.wantCached {
				t.Errorf("upstream hit behavior after first request does not match cached=%t", test.wantCached)
			}
		})
	}
}

func TestManifestIntegrityBeforeCachingOrServing(t *testing.T) {
	validBody := []byte(manifestBody)
	validDigest := "sha256:" + sha256Hex(validBody)
	tests := []struct {
		name         string
		requestRef   string
		contentType  string
		digestHeader string
		body         string
		wantStatus   int
		wantError    string
		wantCached   bool
	}{
		{
			name:        "HTML block page",
			requestRef:  "latest",
			contentType: "text/html; charset=utf-8",
			body:        "<html>blocked by policy</html>",
			wantStatus:  http.StatusBadGateway,
			wantError:   "looks like a web-filter/proxy block page",
		},
		{
			name:        "HTML-shaped body with misleading content type",
			requestRef:  "latest",
			contentType: "application/json",
			body:        "<html>blocked by policy</html>",
			wantStatus:  http.StatusBadGateway,
			wantError:   "looks like a web-filter/proxy block page",
		},
		{
			name:        "unsupported manifest media type",
			requestRef:  "latest",
			contentType: "application/octet-stream",
			body:        manifestBody,
			wantStatus:  http.StatusBadGateway,
			wantError:   "unsupported Content-Type",
		},
		{
			name:        "invalid manifest JSON",
			requestRef:  "latest",
			contentType: "application/vnd.oci.image.manifest.v1+json",
			body:        "not-json",
			wantStatus:  http.StatusBadGateway,
			wantError:   "not valid JSON",
		},
		{
			name:         "digest header mismatch",
			requestRef:   "latest",
			contentType:  "application/vnd.oci.image.manifest.v1+json",
			digestHeader: blobDigest,
			body:         manifestBody,
			wantStatus:   http.StatusBadGateway,
			wantError:    "Docker-Content-Digest",
		},
		{
			name:         "requested digest mismatch",
			requestRef:   blobDigest,
			contentType:  "application/vnd.oci.image.manifest.v1+json",
			digestHeader: validDigest,
			body:         manifestBody,
			wantStatus:   http.StatusBadGateway,
			wantError:    "requested digest",
		},
		{
			name:         "valid manifest",
			requestRef:   "latest",
			contentType:  "application/vnd.oci.image.manifest.v1+json",
			digestHeader: validDigest,
			body:         manifestBody,
			wantStatus:   http.StatusOK,
			wantCached:   true,
		},
		{
			name:         "valid manifest requested by digest",
			requestRef:   validDigest,
			contentType:  "application/vnd.oci.image.manifest.v1+json",
			digestHeader: validDigest,
			body:         manifestBody,
			wantStatus:   http.StatusOK,
			wantCached:   true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				if test.digestHeader != "" {
					w.Header().Set("Docker-Content-Digest", test.digestHeader)
				}
				_, _ = fmt.Fprint(w, test.body)
			}))

			dir := t.TempDir()
			server := NewServer(upstream.URL, dir)
			mirror := httptest.NewServer(server)
			defer mirror.Close()
			path := "/v2/app/manifests/" + test.requestRef

			resp, body := get(t, mirror.URL+path)
			if resp.StatusCode != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %q", resp.StatusCode, test.wantStatus, body)
			}
			if test.wantError != "" && !strings.Contains(body, test.wantError) {
				t.Errorf("error body %q does not contain %q", body, test.wantError)
			}
			if test.wantError == "looks like a web-filter/proxy block page" && !strings.Contains(body, upstream.URL) {
				t.Errorf("block-page error %q does not name upstream URL %q", body, upstream.URL)
			}
			if test.wantCached && body != manifestBody {
				t.Errorf("body = %q, want valid manifest", body)
			}

			_, err := os.Stat(server.manifestPath(path))
			if gotCached := err == nil; gotCached != test.wantCached {
				t.Fatalf("cached = %t, want %t (stat error: %v)", gotCached, test.wantCached, err)
			}

			upstream.Close()
			resp, cachedBody := get(t, mirror.URL+path)
			if test.wantCached {
				if resp.StatusCode != http.StatusOK || cachedBody != manifestBody {
					t.Errorf("cached response = %d %q", resp.StatusCode, cachedBody)
				}
			} else if resp.StatusCode == http.StatusOK {
				t.Errorf("invalid manifest was served from cache: %q", cachedBody)
			}
		})
	}
}

func TestManifestOfflineFallback(t *testing.T) {
	f := newFakeRegistry(t, false)
	dir := t.TempDir()
	ts := httptest.NewServer(NewServer(f.registry.URL, dir))
	defer ts.Close()

	_, body := get(t, ts.URL+"/v2/app/manifests/latest")
	if !strings.Contains(body, "schemaVersion") {
		t.Fatalf("online manifest = %q", body)
	}
	f.registry.Close() // the venue wifi dies

	resp, body := get(t, ts.URL+"/v2/app/manifests/latest")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "schemaVersion") {
		t.Fatalf("offline manifest fallback = %d %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "manifest") {
		t.Errorf("offline manifest content-type %q not preserved", ct)
	}
}

func TestNonGetRejected(t *testing.T) {
	f := newFakeRegistry(t, false)
	mirror := startMirror(t, f)
	resp, err := http.Post(mirror.URL+"/v2/app/blobs/uploads/", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405 (mirror is pull-only)", resp.StatusCode)
	}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
