package mirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

func TestManifestOfflineFallback(t *testing.T) {
	f := newFakeRegistry(t, false)
	dir := t.TempDir()
	ts := httptest.NewServer(newLoopbackMirrorServer(t, f.registry.URL, dir))
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
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("offline uncached tag unexpectedly succeeded: %q", body)
	}
	if upstreamHits.Load() != hitsAfterWarm {
		t.Fatalf("offline uncached tag hit upstream: %d -> %d", hitsAfterWarm, upstreamHits.Load())
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("offline cached+miss path took %s, want fast local response", elapsed)
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
