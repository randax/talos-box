package mirror

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/imagecache"
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
	manifestHits atomic.Int64
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
			f.manifestHits.Add(1)
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
	server := newLoopbackMirrorServer(t, f.registry.URL, t.TempDir())
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	return ts
}

func newLoopbackMirrorServer(t *testing.T, base, cacheDir string) *Server {
	t.Helper()
	dialer := &net.Dialer{}
	return newServerWithEgress(base, cacheDir, egressDependencies{
		resolve: func(ctx context.Context, host string) ([]net.IP, error) {
			if ip := net.ParseIP(host); ip != nil {
				return []net.IP{ip}, nil
			}
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		},
		hostIPs: func() ([]net.IP, error) { return nil, nil },
		dialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, address)
		},
		blocked: func(net.IP, []net.IP) bool { return false },
	})
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

func cacheTagDigestRoot(t *testing.T, server *Server, repository, tag string) {
	t.Helper()
	tagPath := manifestRequestPath(repository, tag)
	data, path, err := server.cachedManifest(tagPath)
	if err != nil {
		t.Fatal(err)
	}
	metadata := server.cachedManifestMetadataAtPath(tagPath, path, data)
	digest, err := verifySupportedDigest(data, metadata.DockerContentDigest)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.storeManifest(manifestRequestPath(repository, digest), metadata, data); err != nil {
		t.Fatal(err)
	}
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
	ts := httptest.NewServer(newLoopbackMirrorServer(t, f.registry.URL, dir))
	defer ts.Close()

	_, body := get(t, ts.URL+"/v2/app/blobs/"+realDigest)
	if body != "blob-bytes" {
		t.Fatalf("first pull got %q", body)
	}
	hits := f.blobHits.Load()

	// a NEW server over the same directory (daemon restart) must serve from disk
	restarted := httptest.NewServer(newLoopbackMirrorServer(t, f.registry.URL, dir))
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

func TestOutboundRequestsDialValidatedIPsAndIgnoreEnvironmentProxy(t *testing.T) {
	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", "sha256:"+sha256Hex([]byte(manifestBody)))
		_, _ = fmt.Fprint(w, manifestBody)
	}))
	defer upstream.Close()

	var proxyHits atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		http.Error(w, "proxy should be ignored", http.StatusBadGateway)
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)

	var resolveCalls atomic.Int64
	var dialed atomic.Value
	server := newServerWithEgress(
		aliasedURL(t, upstream.URL, "registry.example"),
		t.TempDir(),
		egressDependencies{
			resolve: func(context.Context, string) ([]net.IP, error) {
				resolveCalls.Add(1)
				return []net.IP{net.ParseIP("203.0.113.10")}, nil
			},
			hostIPs: func() ([]net.IP, error) { return nil, nil },
			dialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				dialed.Store(address)
				return (&net.Dialer{}).DialContext(ctx, network, hostPortOfURL(t, upstream.URL))
			},
		},
	)
	mirror := httptest.NewServer(server)
	defer mirror.Close()

	resp, body := get(t, mirror.URL+"/v2/app/manifests/latest")
	if resp.StatusCode != http.StatusOK || body != manifestBody {
		t.Fatalf("manifest = %d %q", resp.StatusCode, body)
	}
	if resolveCalls.Load() != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolveCalls.Load())
	}
	if got := dialed.Load(); got != "203.0.113.10:"+portOfURL(t, upstream.URL) {
		t.Fatalf("dialed address = %v, want resolved IP with upstream port", got)
	}
	if proxyHits.Load() != 0 {
		t.Fatalf("proxy hits = %d, want 0", proxyHits.Load())
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1", upstreamHits.Load())
	}
}

func TestSafeEgressAllowsPublicRedirects(t *testing.T) {
	var cdnHits atomic.Int64
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnHits.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("blob-bytes"))
	}))
	defer cdn.Close()

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.WriteHeader(http.StatusOK)
		case strings.HasPrefix(r.URL.Path, "/v2/app/blobs/sha256:"):
			http.Redirect(w, r, aliasedURL(t, cdn.URL, "cdn.example")+"/blob", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer registry.Close()

	server := newServerWithEgress(
		aliasedURL(t, registry.URL, "registry.example"),
		t.TempDir(),
		egressForRoutes(
			aliasRoute(t, registry.URL, "registry.example", "203.0.113.10"),
			aliasRoute(t, cdn.URL, "cdn.example", "203.0.113.20"),
		),
	)
	mirror := httptest.NewServer(server)
	defer mirror.Close()

	resp, body := get(t, mirror.URL+"/v2/app/blobs/sha256:"+sha256Hex([]byte("blob-bytes")))
	if resp.StatusCode != http.StatusOK || body != "blob-bytes" {
		t.Fatalf("blob through public redirect = %d %q", resp.StatusCode, body)
	}
	if cdnHits.Load() != 1 {
		t.Fatalf("cdn hits = %d, want 1", cdnHits.Load())
	}
}

func TestSafeEgressBlocksLoopbackRedirects(t *testing.T) {
	var blockedHits atomic.Int64
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blockedHits.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("blocked"))
	}))
	defer blocked.Close()

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, blocked.URL+"/blob", http.StatusFound)
	}))
	defer registry.Close()

	server := newServerWithEgress(
		aliasedURL(t, registry.URL, "registry.example"),
		t.TempDir(),
		egressForRoutes(
			aliasRoute(t, registry.URL, "registry.example", "203.0.113.10"),
			aliasRoute(t, blocked.URL, "127.0.0.1", "127.0.0.1"),
		),
	)
	mirror := httptest.NewServer(server)
	defer mirror.Close()

	resp, body := get(t, mirror.URL+"/v2/app/blobs/sha256:"+sha256Hex([]byte("blob-bytes")))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("blob through blocked redirect = %d %q, want 502", resp.StatusCode, body)
	}
	if blockedHits.Load() != 0 {
		t.Fatalf("blocked redirect hits = %d, want 0", blockedHits.Load())
	}
}

func TestSafeEgressAllowsPublicTokenRealms(t *testing.T) {
	var tokenHits atomic.Int64
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenHits.Add(1)
		_, _ = fmt.Fprint(w, `{"token":"secret-token"}`)
	}))
	defer token.Close()

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm=%q,service="fake-service",scope="repository:app:pull"`, aliasedURL(t, token.URL, "token.example")))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", "sha256:"+sha256Hex([]byte(manifestBody)))
		_, _ = fmt.Fprint(w, manifestBody)
	}))
	defer registry.Close()

	server := newServerWithEgress(
		aliasedURL(t, registry.URL, "registry.example"),
		t.TempDir(),
		egressForRoutes(
			aliasRoute(t, registry.URL, "registry.example", "203.0.113.10"),
			aliasRoute(t, token.URL, "token.example", "203.0.113.30"),
		),
	)
	mirror := httptest.NewServer(server)
	defer mirror.Close()

	resp, body := get(t, mirror.URL+"/v2/app/manifests/latest")
	if resp.StatusCode != http.StatusOK || body != manifestBody {
		t.Fatalf("manifest through public token realm = %d %q", resp.StatusCode, body)
	}
	if tokenHits.Load() != 1 {
		t.Fatalf("token hits = %d, want 1", tokenHits.Load())
	}
}

func TestSafeEgressBlocksLoopbackTokenRealms(t *testing.T) {
	var blockedHits atomic.Int64
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blockedHits.Add(1)
		_, _ = fmt.Fprint(w, `{"token":"blocked-token"}`)
	}))
	defer blocked.Close()

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate",
			fmt.Sprintf(`Bearer realm=%q,service="fake-service",scope="repository:app:pull"`, blocked.URL))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer registry.Close()

	server := newServerWithEgress(
		aliasedURL(t, registry.URL, "registry.example"),
		t.TempDir(),
		egressForRoutes(
			aliasRoute(t, registry.URL, "registry.example", "203.0.113.10"),
			aliasRoute(t, blocked.URL, "127.0.0.1", "127.0.0.1"),
		),
	)
	mirror := httptest.NewServer(server)
	defer mirror.Close()

	resp, body := get(t, mirror.URL+"/v2/app/manifests/latest")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("manifest through blocked token realm = %d %q, want 502", resp.StatusCode, body)
	}
	if blockedHits.Load() != 0 {
		t.Fatalf("blocked token hits = %d, want 0", blockedHits.Load())
	}
}

func TestGuestCancellationStopsOutboundFetchPromptly(t *testing.T) {
	fetchStarted := make(chan struct{})
	fetchReleased := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(fetchStarted)
		<-r.Context().Done()
		close(fetchReleased)
	}))
	defer upstream.Close()

	requestContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/v2/app/manifests/latest", nil).WithContext(requestContext)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		newLoopbackMirrorServer(t, upstream.URL, t.TempDir()).ServeHTTP(recorder, request)
		close(done)
	}()

	<-fetchStarted
	cancel()

	select {
	case <-fetchReleased:
	case <-time.After(time.Second):
		t.Fatal("upstream fetch did not observe guest cancellation")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("mirror request did not return promptly after fetch cancellation")
	}
}

func TestGuestCancellationStopsRedirectFollowPromptly(t *testing.T) {
	redirectStarted := make(chan struct{})
	redirectReleased := make(chan struct{})
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(redirectStarted)
		<-r.Context().Done()
		close(redirectReleased)
	}))
	defer cdn.Close()

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, cdn.URL+"/blob", http.StatusFound)
	}))
	defer registry.Close()

	requestContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/v2/app/blobs/sha256:"+sha256Hex([]byte("blob-bytes")), nil).WithContext(requestContext)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		newLoopbackMirrorServer(t, registry.URL, t.TempDir()).ServeHTTP(recorder, request)
		close(done)
	}()

	<-redirectStarted
	cancel()

	select {
	case <-redirectReleased:
	case <-time.After(time.Second):
		t.Fatal("redirect target did not observe guest cancellation")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("mirror request did not return promptly after redirect cancellation")
	}
}

func TestGuestCancellationStopsTokenRealmFetchPromptly(t *testing.T) {
	tokenStarted := make(chan struct{})
	tokenReleased := make(chan struct{})
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(tokenStarted)
		<-r.Context().Done()
		close(tokenReleased)
	}))
	defer token.Close()

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate",
			fmt.Sprintf(`Bearer realm=%q,service="fake-service",scope="repository:app:pull"`, token.URL))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer registry.Close()

	requestContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/v2/app/manifests/latest", nil).WithContext(requestContext)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		newLoopbackMirrorServer(t, registry.URL, t.TempDir()).ServeHTTP(recorder, request)
		close(done)
	}()

	<-tokenStarted
	cancel()

	select {
	case <-tokenReleased:
	case <-time.After(time.Second):
		t.Fatal("token realm request did not observe guest cancellation")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("mirror request did not return promptly after token cancellation")
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
			server := newLoopbackMirrorServer(t, f.registry.URL, dir)
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
	graphBody, graphDigest, graphBlobs := singlePlatformGraphFixture()
	tests := []struct {
		name          string
		requestRef    string
		contentType   string
		digestHeader  string
		body          string
		wantStatus    int
		wantError     string
		wantCached    bool
		completeGraph bool
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
			name:        "manifest exceeding size limit",
			requestRef:  "latest",
			contentType: "application/vnd.oci.image.manifest.v1+json",
			body:        `{"pad":"` + strings.Repeat("a", maxManifestBytes) + `"}`,
			wantStatus:  http.StatusBadGateway,
			wantError:   "exceeds",
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
			// a digest-addressed request carries no pin to be stale: bytes that
			// hash to something else are corruption, tampering, or a rewriting
			// proxy, so this stays an upstream-integrity failure (#367)
			name:         "requested digest mismatch",
			requestRef:   blobDigest,
			contentType:  "application/vnd.oci.image.manifest.v1+json",
			digestHeader: validDigest,
			body:         manifestBody,
			wantStatus:   http.StatusBadGateway,
			wantError:    "does not match requested digest",
		},
		{
			name:          "valid manifest",
			requestRef:    "latest",
			contentType:   "application/vnd.oci.image.manifest.v1+json",
			digestHeader:  graphDigest,
			body:          graphBody,
			wantStatus:    http.StatusOK,
			wantCached:    true,
			completeGraph: true,
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
				if data, ok := graphBlobs[r.URL.Path]; ok {
					w.Header().Set("Content-Type", "application/octet-stream")
					_, _ = w.Write(data)
					return
				}
				w.Header().Set("Content-Type", test.contentType)
				if test.digestHeader != "" {
					w.Header().Set("Docker-Content-Digest", test.digestHeader)
				}
				_, _ = fmt.Fprint(w, test.body)
			}))

			dir := t.TempDir()
			server := newLoopbackMirrorServer(t, upstream.URL, dir)
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
			if test.wantCached && body != test.body {
				t.Errorf("body = %q, want valid manifest", body)
			}

			if test.completeGraph {
				resp, digestBody := get(t, mirror.URL+"/v2/app/manifests/"+graphDigest)
				if resp.StatusCode != http.StatusOK || digestBody != graphBody {
					t.Fatalf("warm digest manifest = %d %q", resp.StatusCode, digestBody)
				}
				for blobPath, want := range graphBlobs {
					resp, blobBody := get(t, mirror.URL+blobPath)
					if resp.StatusCode != http.StatusOK || blobBody != string(want) {
						t.Fatalf("warm %s = %d %q", blobPath, resp.StatusCode, blobBody)
					}
				}
				status := server.InspectCached(context.Background(), CacheTarget{Repository: "app", Tag: "latest", Platform: Platform{OS: "linux", Architecture: imagecache.Architecture(runtime.GOARCH)}}, InspectOptions{})
				if !status.Complete() {
					t.Fatalf("single-platform graph is incomplete: %s", status.Reason())
				}
			}

			_, err := os.Stat(server.manifestPath(path))
			if gotCached := err == nil; gotCached != test.wantCached {
				t.Fatalf("cached = %t, want %t (stat error: %v)", gotCached, test.wantCached, err)
			}

			upstream.Close()
			resp, cachedBody := get(t, mirror.URL+path)
			if test.wantCached {
				if resp.StatusCode != http.StatusOK || cachedBody != test.body {
					t.Errorf("cached response = %d %q", resp.StatusCode, cachedBody)
				}
			} else if resp.StatusCode == http.StatusOK {
				t.Errorf("invalid manifest was served from cache: %q", cachedBody)
			}
		})
	}
}

// TestManifestDigestMismatchClassifiedByRequestPath puts the two mismatch
// stories side by side. Only a request that named a tag has a pin behind it,
// and only that one can have a stale pin worth telling the operator to update
// (409). A digest-addressed request names the bytes it wants, so bytes hashing
// to anything else are corruption, tampering, or a rewriting proxy — an
// upstream-integrity failure (502), never a pin the operator never wrote (#367).
func TestManifestDigestMismatchClassifiedByRequestPath(t *testing.T) {
	manifestBody := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:` + strings.Repeat("a", 64) + `"},"layers":[]}`
	stalePin := "sha256:" + strings.Repeat("1", 64)
	servedDigest := "sha256:" + sha256Hex([]byte(manifestBody))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		_, _ = fmt.Fprint(w, manifestBody)
	}))
	defer upstream.Close()
	server := newLoopbackMirrorServer(t, upstream.URL, t.TempDir())

	serve := func(ctx context.Context, path string) (int, string) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		return recorder.Code, recorder.Body.String()
	}

	pinned := withManifestValidationReference(context.Background(), stalePin)
	code, body := serve(pinned, "/v2/app/manifests/stable")
	if code != http.StatusConflict || !strings.Contains(body, "pinned digest mismatch") {
		t.Fatalf("tag path validated against a pin = %d %q, want 409 pinned digest mismatch", code, body)
	}
	if !strings.Contains(body, stalePin) || !strings.Contains(body, servedDigest) {
		t.Fatalf("409 body = %q, want both the pin and the served digest", body)
	}

	code, body = serve(context.Background(), "/v2/app/manifests/"+stalePin)
	if code != http.StatusBadGateway || !strings.Contains(body, "does not match requested digest") {
		t.Fatalf("digest-addressed path = %d %q, want 502 upstream integrity failure", code, body)
	}
	if strings.Contains(body, "pinned digest mismatch") {
		t.Fatalf("digest-addressed path body = %q, must not blame a pin the operator never wrote", body)
	}
}

func TestManifestServedWhenCacheWriteFails(t *testing.T) {
	f := newFakeRegistry(t, false)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil { // storeManifest cannot create manifests/
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	ts := httptest.NewServer(newLoopbackMirrorServer(t, f.registry.URL, dir))
	defer ts.Close()

	resp, body := get(t, ts.URL+"/v2/app/manifests/latest")
	if resp.StatusCode != http.StatusOK || body != manifestBody {
		t.Fatalf("manifest with failing cache = %d %q, want 200 with manifest", resp.StatusCode, body)
	}
}

func TestManifestPathCanonicalizesSupportedDigestCase(t *testing.T) {
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
			upperPath := "/v2/app/manifests/" + test.upper
			lowerPath := "/v2/app/manifests/" + test.lower
			if got, want := server.manifestPath(upperPath), server.manifestPath(lowerPath); got != want {
				t.Fatalf("manifestPath(%q) = %q, want same as %q", upperPath, got, lowerPath)
			}
		})
	}
}

func TestManifestPathUsesVersionedKeyForTagsAndUnsupportedDigestLikeRefs(t *testing.T) {
	cacheDir := t.TempDir()
	server := NewServer("https://registry.example", cacheDir)

	for _, path := range []string{
		"/v2/app/manifests/latest",
		"/v2/app/manifests/md5:" + strings.ToUpper(strings.Repeat("ab", 16)),
		"/v2/app/manifests/sha256:ABCD",
	} {
		got := server.manifestPath(path)
		if filepath.Dir(got) != filepath.Join(cacheDir, "manifests") || !strings.HasPrefix(filepath.Base(got), "v2-") {
			t.Fatalf("manifestPath(%q) = %q, want versioned manifest cache key", path, got)
		}
	}
}

func TestLegacyManifestMigrationDoesNotTrustTagsAndVerifiesDigests(t *testing.T) {
	for _, test := range []struct {
		name        string
		requestPath string
		legacyPath  string
		body        string
		wantStatus  int
	}{
		{
			name:        "tag is ignored",
			requestPath: "/v2/a_b/manifests/latest",
			legacyPath:  "/v2/a/b/manifests/latest",
			body:        `{"schemaVersion":2,"repository":"a/b"}`,
			wantStatus:  http.StatusNotFound,
		},
		{
			name:        "verified digest replays",
			requestPath: "/v2/a_b/manifests/sha256:bafebd36189ad3688b7b3915ea55d461e0bfcfbdde11e54b0a123999fb6be50f",
			legacyPath:  "/v2/a/b/manifests/sha256:bafebd36189ad3688b7b3915ea55d461e0bfcfbdde11e54b0a123999fb6be50f",
			body:        `{"schemaVersion":2}`,
			wantStatus:  http.StatusOK,
		},
		{
			name:        "mismatched digest is rejected",
			requestPath: "/v2/a_b/manifests/sha256:bafebd36189ad3688b7b3915ea55d461e0bfcfbdde11e54b0a123999fb6be50f",
			legacyPath:  "/v2/a/b/manifests/sha256:bafebd36189ad3688b7b3915ea55d461e0bfcfbdde11e54b0a123999fb6be50f",
			body:        `{"schemaVersion":2,"not":"the requested digest"}`,
			wantStatus:  http.StatusServiceUnavailable,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := NewServer("https://registry.example", t.TempDir())
			legacyPath := server.legacyManifestPath(test.legacyPath)
			if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(legacyPath, []byte(test.body), 0o644); err != nil {
				t.Fatal(err)
			}
			server.setOfflineMode(true)
			mirror := httptest.NewServer(server)
			defer mirror.Close()

			resp, body := get(t, mirror.URL+test.requestPath)
			if resp.StatusCode != test.wantStatus {
				t.Fatalf("legacy replay = %d %q, want %d", resp.StatusCode, body, test.wantStatus)
			}
			if test.wantStatus == http.StatusOK && body != test.body {
				t.Fatalf("legacy digest body = %q, want %q", body, test.body)
			}
		})
	}
}

func TestDistinctRepositoryManifestTagsDoNotCrossServeWhenUnavailable(t *testing.T) {
	for _, test := range []struct {
		name        string
		unavailable func(*Server, *httptest.Server)
		wantStatus  int
	}{
		{
			name: "offline",
			unavailable: func(server *Server, _ *httptest.Server) {
				server.setOfflineMode(true)
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "upstream failure",
			unavailable: func(_ *Server, upstream *httptest.Server) {
				upstream.Close()
			},
			wantStatus: http.StatusBadGateway,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			const cachedBody = `{"schemaVersion":2,"repository":"a/b"}`
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v2/a/b/manifests/latest" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
				_, _ = fmt.Fprint(w, cachedBody)
			}))
			defer upstream.Close()

			server := newLoopbackMirrorServer(t, upstream.URL, t.TempDir())
			mirror := httptest.NewServer(server)
			defer mirror.Close()

			resp, body := get(t, mirror.URL+"/v2/a/b/manifests/latest")
			if resp.StatusCode != http.StatusOK || body != cachedBody {
				t.Fatalf("warm a/b: status = %d, body = %q", resp.StatusCode, body)
			}

			test.unavailable(server, upstream)
			resp, body = get(t, mirror.URL+"/v2/a_b/manifests/latest")
			if resp.StatusCode != test.wantStatus {
				t.Fatalf("a_b while unavailable: status = %d, want %d; body = %q", resp.StatusCode, test.wantStatus, body)
			}
			if body == cachedBody {
				t.Fatalf("a_b received cached a/b manifest %q", body)
			}
		})
	}
}

func TestManifestOfflineFallback(t *testing.T) {
	manifest, digest, blobs := singlePlatformGraphFixture()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if data, ok := blobs[r.URL.Path]; ok {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(data)
			return
		}
		if r.URL.Path != "/v2/app/manifests/latest" && r.URL.Path != "/v2/app/manifests/"+digest {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", digest)
		_, _ = fmt.Fprint(w, manifest)
	}))
	dir := t.TempDir()
	server := newLoopbackMirrorServer(t, upstream.URL, dir)
	ts := httptest.NewServer(server)
	defer ts.Close()

	for _, path := range append([]string{"/v2/app/manifests/latest", "/v2/app/manifests/" + digest}, mapKeys(blobs)...) {
		resp, body := get(t, ts.URL+path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("warm %s = %d %q", path, resp.StatusCode, body)
		}
	}
	upstream.Close() // the venue wifi dies

	resp, body := get(t, ts.URL+"/v2/app/manifests/latest")
	if resp.StatusCode != http.StatusOK || body != manifest {
		t.Fatalf("offline manifest fallback = %d %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "manifest") {
		t.Errorf("offline manifest content-type %q not preserved", ct)
	}
}

func TestCachedDigestManifestServesWithoutUpstreamByDefault(t *testing.T) {
	validDigest := "sha256:" + sha256Hex([]byte(manifestBody))
	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", validDigest)
		_, _ = fmt.Fprint(w, manifestBody)
	}))
	defer upstream.Close()

	server := newLoopbackMirrorServer(t, upstream.URL, t.TempDir())
	mirror := httptest.NewServer(server)
	defer mirror.Close()
	path := "/v2/app/manifests/" + validDigest

	resp, body := get(t, mirror.URL+path)
	if resp.StatusCode != http.StatusOK || body != manifestBody {
		t.Fatalf("first digest manifest = %d %q", resp.StatusCode, body)
	}
	hitsAfterWarm := upstreamHits.Load()

	resp, body = get(t, mirror.URL+path)
	if resp.StatusCode != http.StatusOK || body != manifestBody {
		t.Fatalf("cached digest manifest = %d %q", resp.StatusCode, body)
	}
	if upstreamHits.Load() != hitsAfterWarm {
		t.Fatalf("cached digest manifest hit upstream again: %d -> %d", hitsAfterWarm, upstreamHits.Load())
	}
}

func TestCorruptCachedDigestManifestRefetchesOnline(t *testing.T) {
	validDigest := "sha256:" + sha256Hex([]byte(manifestBody))
	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", validDigest)
		_, _ = fmt.Fprint(w, manifestBody)
	}))
	defer upstream.Close()

	server := newLoopbackMirrorServer(t, upstream.URL, t.TempDir())
	mirror := httptest.NewServer(server)
	defer mirror.Close()
	path := "/v2/app/manifests/" + validDigest

	resp, body := get(t, mirror.URL+path)
	if resp.StatusCode != http.StatusOK || body != manifestBody {
		t.Fatalf("warm digest manifest = %d %q", resp.StatusCode, body)
	}
	hitsAfterWarm := upstreamHits.Load()
	if err := os.WriteFile(server.manifestPath(path), []byte("corrupt-manifest"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, body = get(t, mirror.URL+path)
	if resp.StatusCode != http.StatusOK || body != manifestBody {
		t.Fatalf("online replay after corruption = %d %q", resp.StatusCode, body)
	}
	if upstreamHits.Load() != hitsAfterWarm+1 {
		t.Fatalf("corrupt cached digest did not refetch upstream: %d -> %d", hitsAfterWarm, upstreamHits.Load())
	}
}

func TestOfflineCorruptCachedDigestManifestFailsFast(t *testing.T) {
	validDigest := "sha256:" + sha256Hex([]byte(manifestBody))
	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", validDigest)
		_, _ = fmt.Fprint(w, manifestBody)
	}))
	defer upstream.Close()

	server := newLoopbackMirrorServer(t, upstream.URL, t.TempDir())
	mirror := httptest.NewServer(server)
	defer mirror.Close()
	path := "/v2/app/manifests/" + validDigest

	resp, body := get(t, mirror.URL+path)
	if resp.StatusCode != http.StatusOK || body != manifestBody {
		t.Fatalf("warm digest manifest = %d %q", resp.StatusCode, body)
	}
	if err := os.WriteFile(server.manifestPath(path), []byte("corrupt-manifest"), 0o644); err != nil {
		t.Fatal(err)
	}
	server.setOfflineMode(true)
	hitsAfterWarm := upstreamHits.Load()

	resp, body = get(t, mirror.URL+path)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("offline replay after corruption = %d %q, want 503", resp.StatusCode, body)
	}
	if !strings.Contains(body, "cached digest corrupted") {
		t.Fatalf("offline corruption error = %q", body)
	}
	if upstreamHits.Load() != hitsAfterWarm {
		t.Fatalf("offline corrupt cached digest hit upstream: %d -> %d", hitsAfterWarm, upstreamHits.Load())
	}
}

func TestOfflineModeServesCachedTagsAndFailsMissesWithoutUpstream(t *testing.T) {
	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		switch r.URL.Path {
		case "/v2/app/manifests/latest":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", "sha256:"+sha256Hex([]byte(manifestBody)))
			_, _ = fmt.Fprint(w, manifestBody)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	server := newLoopbackMirrorServer(t, upstream.URL, t.TempDir())
	mirror := httptest.NewServer(server)
	defer mirror.Close()

	resp, body := get(t, mirror.URL+"/v2/app/manifests/latest")
	if resp.StatusCode != http.StatusOK || body != manifestBody {
		t.Fatalf("warm cached tag = %d %q", resp.StatusCode, body)
	}
	hitsAfterWarm := upstreamHits.Load()
	cacheTagDigestRoot(t, server, "app", "latest")

	server.setOfflineMode(true)

	start := time.Now()
	resp, body = get(t, mirror.URL+"/v2/app/manifests/latest")
	if resp.StatusCode != http.StatusOK || body != manifestBody {
		t.Fatalf("offline cached tag = %d %q", resp.StatusCode, body)
	}
	if upstreamHits.Load() != hitsAfterWarm {
		t.Fatalf("offline cached tag hit upstream again: %d -> %d", hitsAfterWarm, upstreamHits.Load())
	}

	resp, body = get(t, mirror.URL+"/v2/app/manifests/missing")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("offline uncached tag = %d %q, want 404", resp.StatusCode, body)
	}
	if upstreamHits.Load() != hitsAfterWarm {
		t.Fatalf("offline uncached tag hit upstream: %d -> %d", hitsAfterWarm, upstreamHits.Load())
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("offline cached+miss path took %s, want fast local response", elapsed)
	}
}

func TestOfflineModePreventsAllUncachedUpstreamWork(t *testing.T) {
	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		switch {
		case r.URL.Path == "/v2/app/manifests/latest":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", "sha256:"+sha256Hex([]byte(manifestBody)))
			_, _ = fmt.Fprint(w, manifestBody)
		case strings.HasPrefix(r.URL.Path, "/v2/app/blobs/sha256:"):
			_, _ = fmt.Fprint(w, "blob-bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	server := newLoopbackMirrorServer(t, upstream.URL, t.TempDir())
	mirror := httptest.NewServer(server)
	defer mirror.Close()

	// Warm one cached manifest and one cached blob before going offline.
	_, _ = get(t, mirror.URL+"/v2/app/manifests/latest")
	cacheTagDigestRoot(t, server, "app", "latest")
	cachedBlobDigest := "sha256:" + sha256Hex([]byte("blob-bytes"))
	_, _ = get(t, mirror.URL+"/v2/app/blobs/"+cachedBlobDigest)
	hitsAfterWarm := upstreamHits.Load()

	server.setOfflineMode(true)

	resp, body := get(t, mirror.URL+"/v2/app/manifests/latest")
	if resp.StatusCode != http.StatusOK || body != manifestBody {
		t.Fatalf("offline cached manifest = %d %q", resp.StatusCode, body)
	}
	resp, body = get(t, mirror.URL+"/v2/app/blobs/"+cachedBlobDigest)
	if resp.StatusCode != http.StatusOK || body != "blob-bytes" {
		t.Fatalf("offline cached blob = %d %q", resp.StatusCode, body)
	}

	for _, path := range []string{
		mirror.URL + "/v2/app/manifests/missing",
		mirror.URL + "/v2/app/blobs/sha256:" + strings.Repeat("1", 64),
		mirror.URL + "/v2/app/other",
	} {
		resp, _ := get(t, path)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("offline miss %s = %d, want 404", path, resp.StatusCode)
		}
	}
	if upstreamHits.Load() != hitsAfterWarm {
		t.Fatalf("offline uncached paths hit upstream: %d -> %d", hitsAfterWarm, upstreamHits.Load())
	}
}

func TestOfflineToggleDuringValidationPreventsUpstreamFetch(t *testing.T) {
	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", "sha256:"+sha256Hex([]byte(manifestBody)))
		_, _ = fmt.Fprint(w, manifestBody)
	}))
	defer upstream.Close()

	validationStarted := make(chan struct{})
	releaseValidation := make(chan struct{})
	server := newLoopbackMirrorServer(t, upstream.URL, t.TempDir())
	server.validateUpstream = func(context.Context) error {
		close(validationStarted)
		<-releaseValidation
		return nil
	}
	mirror := httptest.NewServer(server)
	defer mirror.Close()

	requestDone := make(chan struct{})
	var responseStatus int
	var responseBody string
	go func() {
		resp, body := get(t, mirror.URL+"/v2/app/manifests/latest")
		responseStatus = resp.StatusCode
		responseBody = body
		close(requestDone)
	}()

	<-validationStarted
	server.setOfflineMode(true)
	close(releaseValidation)

	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("request did not finish after validation release")
	}
	if responseStatus == http.StatusOK {
		t.Fatalf("request unexpectedly succeeded while offline toggled during validation: %q", responseBody)
	}
	if upstreamHits.Load() != 0 {
		t.Fatalf("offline toggle during validation still hit upstream: %d", upstreamHits.Load())
	}
}

func TestCachedManifestResponsesIncludeProtocolHeaders(t *testing.T) {
	validDigest := "sha256:" + sha256Hex([]byte(manifestBody))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", validDigest)
		_, _ = fmt.Fprint(w, manifestBody)
	}))
	defer upstream.Close()

	server := newLoopbackMirrorServer(t, upstream.URL, t.TempDir())
	mirror := httptest.NewServer(server)
	defer mirror.Close()

	// Warm cached digest and tag entries, then force the tag path to serve from cache.
	_, _ = get(t, mirror.URL+"/v2/app/manifests/"+validDigest)
	_, _ = get(t, mirror.URL+"/v2/app/manifests/latest")
	cacheTagDigestRoot(t, server, "app", "latest")
	server.setOfflineMode(true)

	for _, test := range []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantBody   string
		wantDigest string
		wantType   string
		wantLength string
	}{
		{
			name:       "digest get",
			method:     http.MethodGet,
			path:       "/v2/app/manifests/" + validDigest,
			wantStatus: http.StatusOK,
			wantBody:   manifestBody,
			wantDigest: validDigest,
			wantType:   "application/vnd.oci.image.manifest.v1+json",
			wantLength: fmt.Sprintf("%d", len(manifestBody)),
		},
		{
			name:       "digest head",
			method:     http.MethodHead,
			path:       "/v2/app/manifests/" + validDigest,
			wantStatus: http.StatusOK,
			wantDigest: validDigest,
			wantType:   "application/vnd.oci.image.manifest.v1+json",
			wantLength: fmt.Sprintf("%d", len(manifestBody)),
		},
		{
			name:       "tag get",
			method:     http.MethodGet,
			path:       "/v2/app/manifests/latest",
			wantStatus: http.StatusOK,
			wantBody:   manifestBody,
			wantDigest: validDigest,
			wantType:   "application/vnd.oci.image.manifest.v1+json",
			wantLength: fmt.Sprintf("%d", len(manifestBody)),
		},
		{
			name:       "tag head",
			method:     http.MethodHead,
			path:       "/v2/app/manifests/latest",
			wantStatus: http.StatusOK,
			wantDigest: validDigest,
			wantType:   "application/vnd.oci.image.manifest.v1+json",
			wantLength: fmt.Sprintf("%d", len(manifestBody)),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(test.method, mirror.URL+test.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			bodyBytes, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != test.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, test.wantStatus)
			}
			if got := resp.Header.Get("Content-Type"); got != test.wantType {
				t.Fatalf("Content-Type = %q, want %q", got, test.wantType)
			}
			if got := resp.Header.Get("Content-Length"); got != test.wantLength {
				t.Fatalf("Content-Length = %q, want %q", got, test.wantLength)
			}
			if got := resp.Header.Get("Docker-Content-Digest"); got != test.wantDigest {
				t.Fatalf("Docker-Content-Digest = %q, want %q", got, test.wantDigest)
			}
			if string(bodyBytes) != test.wantBody {
				t.Fatalf("body = %q, want %q", string(bodyBytes), test.wantBody)
			}
		})
	}
}

func TestCachedManifestMetadataFallbackRevalidatesCurrentBytes(t *testing.T) {
	sha512Digest := "sha512:" + sha512Hex([]byte(manifestBody))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/app/manifests/latest":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", "sha256:"+sha256Hex([]byte(manifestBody)))
			_, _ = fmt.Fprint(w, manifestBody)
		case "/v2/app/manifests/" + sha512Digest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", sha512Digest)
			_, _ = fmt.Fprint(w, manifestBody)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	server := newLoopbackMirrorServer(t, upstream.URL, t.TempDir())
	mirror := httptest.NewServer(server)
	defer mirror.Close()

	// Warm sha512 digest and tag entries.
	_, _ = get(t, mirror.URL+"/v2/app/manifests/"+sha512Digest)
	_, _ = get(t, mirror.URL+"/v2/app/manifests/latest")
	cacheTagDigestRoot(t, server, "app", "latest")
	server.setOfflineMode(true)

	sha512Path := "/v2/app/manifests/" + sha512Digest
	legacyTagPath := "/v2/app/manifests/latest"

	tests := []struct {
		name              string
		path              string
		breakMeta         func(t *testing.T)
		wantDigest        string
		wantContentType   string
		wantContentLength string
	}{
		{
			name: "missing meta sha512 replays requested digest",
			path: sha512Path,
			breakMeta: func(t *testing.T) {
				t.Helper()
				if err := os.Remove(server.manifestMetadataPath(sha512Path)); err != nil {
					t.Fatal(err)
				}
			},
			wantDigest:        sha512Digest,
			wantContentType:   "application/vnd.oci.image.manifest.v1+json",
			wantContentLength: fmt.Sprintf("%d", len(manifestBody)),
		},
		{
			name: "corrupt meta falls back to ct sidecar for tag",
			path: legacyTagPath,
			breakMeta: func(t *testing.T) {
				t.Helper()
				if err := os.WriteFile(server.manifestMetadataPath(legacyTagPath), []byte("{not-json"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(server.manifestPath(legacyTagPath)+".ct", []byte("application/vnd.oci.image.manifest.v1+json"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantDigest:        "sha256:" + sha256Hex([]byte(manifestBody)),
			wantContentType:   "application/vnd.oci.image.manifest.v1+json",
			wantContentLength: fmt.Sprintf("%d", len(manifestBody)),
		},
		{
			name: "stale meta length and digest are recomputed",
			path: legacyTagPath,
			breakMeta: func(t *testing.T) {
				t.Helper()
				stale := manifestMetadata{
					ContentType:         "application/vnd.oci.image.manifest.v1+json",
					ContentLength:       1,
					DockerContentDigest: "sha256:" + strings.Repeat("1", 64),
				}
				raw, err := json.Marshal(stale)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(server.manifestMetadataPath(legacyTagPath), raw, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantDigest:        "sha256:" + sha256Hex([]byte(manifestBody)),
			wantContentType:   "application/vnd.oci.image.manifest.v1+json",
			wantContentLength: fmt.Sprintf("%d", len(manifestBody)),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.breakMeta(t)
			request, err := http.NewRequest(http.MethodHead, mirror.URL+test.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			bodyBytes, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if len(bodyBytes) != 0 {
				t.Fatalf("HEAD body = %q, want empty", string(bodyBytes))
			}
			if got := resp.Header.Get("Docker-Content-Digest"); got != test.wantDigest {
				t.Fatalf("Docker-Content-Digest = %q, want %q", got, test.wantDigest)
			}
			if got := resp.Header.Get("Content-Type"); got != test.wantContentType {
				t.Fatalf("Content-Type = %q, want %q", got, test.wantContentType)
			}
			if got := resp.Header.Get("Content-Length"); got != test.wantContentLength {
				t.Fatalf("Content-Length = %q, want %q", got, test.wantContentLength)
			}
		})
	}
}

func TestCachedManifestMetadataIgnoresStaleContentType(t *testing.T) {
	server := newLoopbackMirrorServer(t, "http://127.0.0.1", t.TempDir())
	requestPath := "/v2/app/manifests/latest"
	data := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json"}`)

	if err := os.MkdirAll(filepath.Dir(server.manifestMetadataPath(requestPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := manifestMetadata{
		ContentType:         "application/vnd.oci.image.manifest.v1+json",
		ContentLength:       int64(len(data)),
		DockerContentDigest: "sha256:" + sha256Hex([]byte("previous manifest")),
	}
	raw, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server.manifestMetadataPath(requestPath), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	metadata := server.cachedManifestMetadata(requestPath, data)
	if got, want := metadata.ContentType, "application/vnd.oci.image.index.v1+json"; got != want {
		t.Fatalf("ContentType = %q, want %q", got, want)
	}
	if got, want := metadata.DockerContentDigest, "sha256:"+sha256Hex(data); got != want {
		t.Fatalf("DockerContentDigest = %q, want %q", got, want)
	}
}

func TestCachedManifestMetadataUsesLegacySidecarWhenMetaDigestIsStale(t *testing.T) {
	server := newLoopbackMirrorServer(t, "http://127.0.0.1", t.TempDir())
	requestPath := "/v2/app/manifests/latest"
	data := []byte(manifestBody)

	if err := os.MkdirAll(filepath.Dir(server.manifestMetadataPath(requestPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := manifestMetadata{
		ContentType:         "application/vnd.oci.image.index.v1+json",
		ContentLength:       int64(len(data)),
		DockerContentDigest: "sha256:" + sha256Hex([]byte("previous manifest")),
	}
	raw, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server.manifestMetadataPath(requestPath), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server.manifestPath(requestPath)+".ct", []byte("application/vnd.docker.distribution.manifest.v2+json"), 0o644); err != nil {
		t.Fatal(err)
	}

	metadata := server.cachedManifestMetadata(requestPath, data)
	if got, want := metadata.ContentType, "application/vnd.docker.distribution.manifest.v2+json"; got != want {
		t.Fatalf("ContentType = %q, want %q", got, want)
	}
	if got, want := metadata.DockerContentDigest, "sha256:"+sha256Hex(data); got != want {
		t.Fatalf("DockerContentDigest = %q, want %q", got, want)
	}
}

func TestCachedManifestMetadataRejectsUnsafeSidecars(t *testing.T) {
	data := []byte(manifestBody)
	wantContentType := "application/vnd.oci.image.manifest.v1+json"
	wrongContentType := "application/vnd.oci.image.index.v1+json"

	tests := []struct {
		name  string
		setup func(t *testing.T, server *Server, requestPath string)
	}{
		{
			name: "oversized metadata",
			setup: func(t *testing.T, server *Server, requestPath string) {
				t.Helper()
				raw := fmt.Sprintf(`{"contentType":%q,"dockerContentDigest":%q,"padding":%q}`,
					wrongContentType, "sha256:"+sha256Hex(data), strings.Repeat("x", maxCachedManifestSidecarBytes))
				if err := os.WriteFile(server.manifestMetadataPath(requestPath), []byte(raw), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlinked metadata",
			setup: func(t *testing.T, server *Server, requestPath string) {
				t.Helper()
				target := filepath.Join(t.TempDir(), "metadata.json")
				raw := fmt.Sprintf(`{"contentType":%q,"dockerContentDigest":%q}`,
					wrongContentType, "sha256:"+sha256Hex(data))
				if err := os.WriteFile(target, []byte(raw), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, server.manifestMetadataPath(requestPath)); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "oversized legacy content type",
			setup: func(t *testing.T, server *Server, requestPath string) {
				t.Helper()
				if err := os.WriteFile(server.manifestPath(requestPath)+".ct", []byte(strings.Repeat("x", maxCachedManifestSidecarBytes+1)), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlinked legacy content type",
			setup: func(t *testing.T, server *Server, requestPath string) {
				t.Helper()
				target := filepath.Join(t.TempDir(), "content-type")
				if err := os.WriteFile(target, []byte(wrongContentType), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, server.manifestPath(requestPath)+".ct"); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newLoopbackMirrorServer(t, "http://127.0.0.1", t.TempDir())
			requestPath := "/v2/app/manifests/latest"
			if err := os.MkdirAll(filepath.Dir(server.manifestMetadataPath(requestPath)), 0o755); err != nil {
				t.Fatal(err)
			}
			test.setup(t, server, requestPath)

			metadata := server.cachedManifestMetadata(requestPath, data)
			if got := metadata.ContentType; got != wantContentType {
				t.Fatalf("ContentType = %q, want %q", got, wantContentType)
			}
		})
	}
}

func TestManifestDigestSupportIncludesSha512AndRejectsUnsupportedAlgorithms(t *testing.T) {
	sha512Digest := "sha512:" + sha512Hex([]byte(manifestBody))
	tests := []struct {
		name       string
		requestRef string
		header     string
		wantStatus int
		wantCached bool
		wantError  string
	}{
		{
			name:       "sha512 digest fetches and caches",
			requestRef: sha512Digest,
			header:     sha512Digest,
			wantStatus: http.StatusOK,
			wantCached: true,
		},
		{
			name:       "unsupported digest algorithm is rejected",
			requestRef: "md5:" + strings.Repeat("a", 32),
			header:     "md5:" + strings.Repeat("a", 32),
			wantStatus: http.StatusBadGateway,
			wantError:  "unsupported digest algorithm",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var upstreamHits atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamHits.Add(1)
				w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
				w.Header().Set("Docker-Content-Digest", test.header)
				_, _ = fmt.Fprint(w, manifestBody)
			}))

			dir := t.TempDir()
			server := newLoopbackMirrorServer(t, upstream.URL, dir)
			mirror := httptest.NewServer(server)
			defer mirror.Close()
			path := "/v2/app/manifests/" + test.requestRef

			resp, body := get(t, mirror.URL+path)
			if resp.StatusCode != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %q", resp.StatusCode, test.wantStatus, body)
			}
			if test.wantError != "" && !strings.Contains(body, test.wantError) {
				t.Fatalf("error body %q does not contain %q", body, test.wantError)
			}

			upstream.Close()
			resp, cachedBody := get(t, mirror.URL+path)
			if test.wantCached {
				if resp.StatusCode != http.StatusOK || cachedBody != manifestBody {
					t.Fatalf("cached sha512 response = %d %q", resp.StatusCode, cachedBody)
				}
				if upstreamHits.Load() != 1 {
					t.Fatalf("sha512 cache-first follow-up hit upstream again: %d", upstreamHits.Load())
				}
			} else if resp.StatusCode == http.StatusOK {
				t.Fatalf("unsupported digest was cached unexpectedly: %q", cachedBody)
			}
		})
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

func sha512Hex(data []byte) string {
	sum := sha512.Sum512(data)
	return hex.EncodeToString(sum[:])
}

func aliasedURL(t *testing.T, raw, host string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Host = net.JoinHostPort(host, portOfURL(t, raw))
	return parsed.String()
}

func hostPortOfURL(t *testing.T, raw string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Host
}

func portOfURL(t *testing.T, raw string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Port()
}

type testRoute struct {
	host      string
	resolved  net.IP
	actual    string
	aliasBase string
}

func aliasRoute(t *testing.T, raw, host, ip string) testRoute {
	t.Helper()
	return testRoute{
		host:      canonicalLookupHost(host),
		resolved:  net.ParseIP(ip),
		actual:    hostPortOfURL(t, raw),
		aliasBase: aliasedURL(t, raw, host),
	}
}

func egressForRoutes(routes ...testRoute) egressDependencies {
	byHost := make(map[string][]net.IP, len(routes))
	byAddress := make(map[string]string, len(routes))
	for _, route := range routes {
		byHost[route.host] = []net.IP{route.resolved}
		byAddress[net.JoinHostPort(route.resolved.String(), portOfHostPort(route.actual))] = route.actual
	}
	dialer := &net.Dialer{}
	return egressDependencies{
		resolve: func(_ context.Context, host string) ([]net.IP, error) {
			ips, ok := byHost[canonicalLookupHost(host)]
			if !ok {
				return nil, fmt.Errorf("unexpected host %q", host)
			}
			return ips, nil
		},
		hostIPs: func() ([]net.IP, error) { return nil, nil },
		dialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			target, ok := byAddress[address]
			if !ok {
				return nil, fmt.Errorf("unexpected dial %q", address)
			}
			return dialer.DialContext(ctx, network, target)
		},
		blocked: namespaceIPBlocked,
	}
}

func portOfHostPort(hostPort string) string {
	_, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		panic(err)
	}
	return port
}

// A HEAD probe (containerd tries HEAD first) drops the body, so the offline
// miss must carry its reason in headers or the node event is indistinguishable
// from a broken mirror (#363).
func TestOfflineMissReturnsFallbackFriendly404WithReasonHeaders(t *testing.T) {
	f := newFakeRegistry(t, false)
	dir := t.TempDir()
	server := newLoopbackMirrorServer(t, f.registry.URL, dir)
	mirror := httptest.NewServer(server)
	defer mirror.Close()
	server.setOfflineMode(true)

	for _, test := range []struct {
		name   string
		method string
		path   string
	}{
		{name: "manifest tag head", method: http.MethodHead, path: "/v2/app/manifests/latest"},
		{name: "manifest tag get", method: http.MethodGet, path: "/v2/app/manifests/latest"},
		{name: "manifest digest head", method: http.MethodHead, path: "/v2/app/manifests/" + blobDigest},
		{name: "manifest digest get", method: http.MethodGet, path: "/v2/app/manifests/" + blobDigest},
		{name: "blob head", method: http.MethodHead, path: "/v2/app/blobs/" + blobDigest},
		{name: "blob get", method: http.MethodGet, path: "/v2/app/blobs/" + blobDigest},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(test.method, mirror.URL+test.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", resp.StatusCode)
			}
			if got := resp.Header.Get(reasonHeader); got != reasonOfflineNotCached {
				t.Fatalf("%s = %q, want %q", reasonHeader, got, reasonOfflineNotCached)
			}
			if got := resp.Header.Get("Warning"); !strings.Contains(got, offlineNotCachedMessage) {
				t.Fatalf("Warning = %q, want it to carry %q", got, offlineNotCachedMessage)
			}
		})
	}
}

func TestOffline404AllowsResolverFallback(t *testing.T) {
	mirror := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeOfflineMiss(w, offlineNotCachedMessage)
	})
	var fallbackHits atomic.Int64
	fallback := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, manifestBody)
	})

	request := httptest.NewRequest(http.MethodGet, "/v2/app/manifests/latest", nil)
	first := httptest.NewRecorder()
	mirror.ServeHTTP(first, request)
	if first.Code != http.StatusNotFound || first.Header().Get(reasonHeader) != reasonOfflineNotCached {
		t.Fatalf("mirror response = %d, reason %q; want 404, %q", first.Code, first.Header().Get(reasonHeader), reasonOfflineNotCached)
	}
	second := httptest.NewRecorder()
	if first.Code == http.StatusNotFound {
		fallback.ServeHTTP(second, request)
	}
	if second.Code != http.StatusOK || second.Body.String() != manifestBody || fallbackHits.Load() != 1 {
		t.Fatalf("fallback response = %d %q after %d hits, want 200 manifest after 1", second.Code, second.Body.String(), fallbackHits.Load())
	}
}

func TestOnlineCompleteTagHEADServesStaleOnTransientUpstreamStatus(t *testing.T) {
	for _, upstreamStatus := range []int{429, 500, 502, 503, 504} {
		t.Run(http.StatusText(upstreamStatus), func(t *testing.T) {
			server, manifest, digest := newCompleteStaleFixture(t)
			before, err := os.Stat(server.manifestPath(manifestRequestPath("demo", "stable")))
			if err != nil {
				t.Fatal(err)
			}
			var upstreamHits atomic.Int64
			server.client.Transport = roundTripFunc(func(*http.Request) *http.Response {
				upstreamHits.Add(1)
				return retryResponse(upstreamStatus, "3600")
			})
			request := httptest.NewRequest(http.MethodHead, "/v2/demo/manifests/stable", nil)
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK || recorder.Body.Len() != 0 {
				t.Fatalf("stale HEAD = %d %q, want 200 empty", recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("Docker-Content-Digest"); got != digest {
				t.Fatalf("digest = %q, want %q", got, digest)
			}
			if got := recorder.Header().Get("Content-Length"); got != fmt.Sprint(len(manifest)) {
				t.Fatalf("content length = %q, want %d", got, len(manifest))
			}
			after, err := os.Stat(server.manifestPath(manifestRequestPath("demo", "stable")))
			if err != nil {
				t.Fatal(err)
			}
			if upstreamHits.Load() != 1 || !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
				t.Fatalf("stale HEAD used %d upstream requests or mutated cache (%v -> %v)", upstreamHits.Load(), before.ModTime(), after.ModTime())
			}
		})
	}
}

func TestOnlineCompleteTagGETServesStaleOnTransportFailure(t *testing.T) {
	server, manifest, _ := newCompleteStaleFixture(t)
	server.namespace = "registry.example"
	server.client.Transport = roundTripErrorFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("upstream closed")
	})
	logged := captureDaemonLog(t)
	request := httptest.NewRequest(http.MethodGet, "/v2/demo/manifests/stable", nil)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != string(manifest) {
		t.Fatalf("stale GET = %d %q, want 200 %q", recorder.Code, recorder.Body.String(), manifest)
	}
	if line := logged.String(); !strings.Contains(line, "mirror served stale: registry.example/demo:stable") || !strings.Contains(line, "upstream closed") {
		t.Fatalf("daemon log = %q, want transport-failure stale replay", line)
	}
}

func TestOnlineCompleteTagServesStaleOnDNSAndTimeoutFailure(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "dns", err: &net.DNSError{Err: "no such host", Name: "registry.example"}},
		{name: "timeout", err: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, manifest, _ := newCompleteStaleFixture(t)
			server.client.Transport = roundTripErrorFunc(func(*http.Request) (*http.Response, error) {
				return nil, test.err
			})
			start := time.Now()
			request := httptest.NewRequest(http.MethodGet, "/v2/demo/manifests/stable", nil)
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK || recorder.Body.String() != string(manifest) {
				t.Fatalf("stale GET = %d %q, want cached 200", recorder.Code, recorder.Body.String())
			}
			if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
				t.Fatalf("stale fallback took %s, want no retry wait", elapsed)
			}
		})
	}
}

func TestOnlinePartialTagDoesNotServeStaleOn429(t *testing.T) {
	server, _, _ := newCompleteStaleFixture(t)
	missingDigest := "sha256:" + sha256Hex([]byte("layer"))
	missing := server.blobPath(missingDigest)
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	var upstreamHits atomic.Int64
	server.retrySleep = func(context.Context, time.Duration) error { return nil }
	server.client.Transport = roundTripFunc(func(*http.Request) *http.Response {
		upstreamHits.Add(1)
		return retryResponse(http.StatusTooManyRequests, "0")
	})
	request := httptest.NewRequest(http.MethodGet, "/v2/demo/manifests/stable", nil)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("partial cache response = %d, want 429", recorder.Code)
	}
	if upstreamHits.Load() != 3 {
		t.Fatalf("partial cache upstream attempts = %d, want 3", upstreamHits.Load())
	}
	status := server.InspectCached(context.Background(), CacheTarget{Repository: "demo", Tag: "stable", Platform: Platform{OS: "linux", Architecture: imagecache.Architecture(runtime.GOARCH)}}, InspectOptions{})
	if status.Complete() || !strings.Contains(status.Reason(), missingDigest) {
		t.Fatalf("partial status = %q, want missing layer digest", status.Reason())
	}

	server.setOfflineMode(true)
	offline := httptest.NewRecorder()
	server.ServeHTTP(offline, httptest.NewRequest(http.MethodGet, "/v2/demo/manifests/stable", nil))
	if offline.Code != http.StatusNotFound {
		t.Fatalf("offline partial cache response = %d %q, want 404", offline.Code, offline.Body.String())
	}
}

func singlePlatformGraphFixture() (string, string, map[string][]byte) {
	config := []byte("config")
	layer := []byte("layer")
	configDigest := "sha256:" + sha256Hex(config)
	layerDigest := "sha256:" + sha256Hex(layer)
	manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"%s"},"layers":[{"digest":"%s"}]}`, configDigest, layerDigest)
	digest := "sha256:" + sha256Hex([]byte(manifest))
	return manifest, digest, map[string][]byte{
		"/v2/app/blobs/" + configDigest: config,
		"/v2/app/blobs/" + layerDigest:  layer,
	}
}

func mapKeys(values map[string][]byte) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func TestOnlineCompleteTagDoesNotMaskHard404(t *testing.T) {
	server, _, _ := newCompleteStaleFixture(t)
	server.client.Transport = roundTripFunc(func(*http.Request) *http.Response {
		return retryResponse(http.StatusNotFound, "")
	})
	request := httptest.NewRequest(http.MethodGet, "/v2/demo/manifests/stable", nil)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("hard miss response = %d, want 404", recorder.Code)
	}
}

type roundTripErrorFunc func(*http.Request) (*http.Response, error)

func (f roundTripErrorFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func newCompleteStaleFixture(t *testing.T) (*Server, []byte, string) {
	t.Helper()
	server := NewServer("https://registry.example", t.TempDir())
	config := []byte("config")
	layer := []byte("layer")
	configDigest := "sha256:" + sha256Hex(config)
	layerDigest := "sha256:" + sha256Hex(layer)
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"%s"},"layers":[{"digest":"%s"}]}`, configDigest, layerDigest))
	digest := "sha256:" + sha256Hex(manifest)
	metadata := manifestMetadata{ContentType: "application/vnd.oci.image.manifest.v1+json", ContentLength: int64(len(manifest)), DockerContentDigest: digest}
	for _, reference := range []string{"stable", digest} {
		if err := server.storeManifest(manifestRequestPath("demo", reference), metadata, manifest); err != nil {
			t.Fatal(err)
		}
	}
	for digest, data := range map[string][]byte{configDigest: config, layerDigest: layer} {
		path := server.blobPath(digest)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return server, manifest, digest
}

func TestOfflineCorruptedCachedDigestCarriesReasonHeaders(t *testing.T) {
	f := newFakeRegistry(t, false)
	dir := t.TempDir()
	server := newLoopbackMirrorServer(t, f.registry.URL, dir)
	mirror := httptest.NewServer(server)
	defer mirror.Close()

	validDigest := "sha256:" + sha256Hex([]byte(manifestBody))
	path := "/v2/app/manifests/" + validDigest
	if err := os.MkdirAll(filepath.Dir(server.manifestPath(path)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server.manifestPath(path), []byte("corrupted"), 0o644); err != nil {
		t.Fatal(err)
	}
	server.setOfflineMode(true)

	request, err := http.NewRequest(http.MethodHead, mirror.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get(reasonHeader); got != reasonOfflineCacheCorrupted {
		t.Fatalf("%s = %q, want %q", reasonHeader, got, reasonOfflineCacheCorrupted)
	}
	if got := resp.Header.Get("Warning"); !strings.Contains(got, offlineCacheCorruptedMessage) {
		t.Fatalf("Warning = %q, want it to carry %q", got, offlineCacheCorruptedMessage)
	}
}

// The Manager answers an offline miss from its own cache-only probe, before
// the Server's handler runs, so the reason has to reach that 404 too (#363).
func TestManagerOfflineMissCarriesReasonHeaders(t *testing.T) {
	manager := newManagerWithPorts(t.TempDir(), nil, freePort(t))
	manager.offline.Store(true)
	defer manager.Close()
	mirror := httptest.NewServer(http.HandlerFunc(manager.serveCatchAll))
	defer mirror.Close()

	request, err := http.NewRequest(http.MethodHead, mirror.URL+"/v2/demo/manifests/stable?ns=registry.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if got := resp.Header.Get(reasonHeader); got != reasonOfflineNotCached {
		t.Fatalf("%s = %q, want %q", reasonHeader, got, reasonOfflineNotCached)
	}
	if got := resp.Header.Get("Warning"); !strings.Contains(got, offlineNotCachedMessage) {
		t.Fatalf("Warning = %q, want it to carry %q", got, offlineNotCachedMessage)
	}
}
