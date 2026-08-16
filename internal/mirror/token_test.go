package mirror

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/randax/talos-box/internal/imagecache"
)

func TestParseBearerChallenge(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   bearerChallenge
		wantOK bool
	}{
		{
			name:   "empty header",
			header: "",
		},
		{
			name:   "basic scheme",
			header: `Basic realm="registry"`,
		},
		{
			// registry-1.docker.io answers repository requests without a scope
			name:   "docker hub scopeless",
			header: `Bearer realm="https://auth.docker.io/token",service="registry.docker.io"`,
			want:   bearerChallenge{realm: "https://auth.docker.io/token", service: "registry.docker.io"},
			wantOK: true,
		},
		{
			name:   "scoped and case-insensitive scheme",
			header: `bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:app:pull",error="insufficient_scope"`,
			want:   bearerChallenge{realm: "https://ghcr.io/token", service: "ghcr.io", scope: "repository:app:pull"},
			wantOK: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := parseBearerChallenge(test.header)
			if ok != test.wantOK {
				t.Fatalf("parseBearerChallenge(%q) ok = %v, want %v", test.header, ok, test.wantOK)
			}
			if got != test.want {
				t.Fatalf("parseBearerChallenge(%q) = %+v, want %+v", test.header, got, test.want)
			}
		})
	}
}

// tokenAuthRegistry is a Docker-Hub-shaped upstream: it demands a bearer token
// scoped to the repository but issues a challenge that carries no scope.
type tokenAuthRegistry struct {
	registry  *httptest.Server
	token     *httptest.Server
	realm     string // challenge realm, overridable for aliased egress routing
	tokenHits atomic.Int64
	scopes    chan string
	authSeen  chan string

	mu   sync.Mutex
	used map[string]bool
}

type tokenAuthOptions struct {
	includeScope bool
	expiresIn    int
	singleUse    bool // each issued token is accepted exactly once
}

func newTokenAuthRegistry(t *testing.T, options tokenAuthOptions, serve http.HandlerFunc) *tokenAuthRegistry {
	t.Helper()
	registry := &tokenAuthRegistry{
		scopes:   make(chan string, 8),
		authSeen: make(chan string, 32),
		used:     map[string]bool{},
	}
	var issued atomic.Int64

	registry.token = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.tokenHits.Add(1)
		registry.scopes <- r.URL.Query().Get("scope")
		if r.URL.Query().Get("service") != "fake-service" {
			http.Error(w, "wrong service", http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("scope") == "" {
			// mirrors Docker Hub: a scopeless token grants no repository access
			http.Error(w, "no scope", http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprintf(w, `{"token":%q,"expires_in":%d}`, fmt.Sprintf("secret-token-%d", issued.Add(1)), options.expiresIn)
	}))
	t.Cleanup(registry.token.Close)
	registry.realm = registry.token.URL

	registry.registry = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		registry.authSeen <- authorization
		if !registry.accepts(authorization, options.singleUse) {
			challenge := fmt.Sprintf(`Bearer realm=%q,service="fake-service"`, registry.realm)
			if options.includeScope {
				challenge += `,scope="` + scopeOf(r.URL.Path) + `"`
			}
			w.Header().Set("WWW-Authenticate", challenge)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(w, `{"errors":[{"code":"UNAUTHORIZED"}]}`)
			return
		}
		serve(w, r)
	}))
	t.Cleanup(registry.registry.Close)
	return registry
}

func (r *tokenAuthRegistry) accepts(authorization string, singleUse bool) bool {
	value, ok := strings.CutPrefix(authorization, "Bearer ")
	if !ok || !strings.HasPrefix(value, "secret-token-") {
		return false
	}
	if !singleUse {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.used[value] {
		return false
	}
	r.used[value] = true
	return true
}

func (r *tokenAuthRegistry) nextScope(t *testing.T) string {
	t.Helper()
	select {
	case scope := <-r.scopes:
		return scope
	case <-time.After(time.Second):
		t.Fatal("token endpoint was never called")
		return ""
	}
}

const tokenTestBlob = "blob-bytes"

func serveTokenTestRepository(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/library/pause/manifests/3.10":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", "sha256:"+sha256Hex([]byte(manifestBody)))
			_, _ = fmt.Fprint(w, manifestBody)
		case "/v2/library/pause/blobs/sha256:" + sha256Hex([]byte(tokenTestBlob)):
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = fmt.Fprint(w, tokenTestBlob)
		default:
			http.NotFound(w, r)
		}
	}
}

// A scopeless challenge is the Docker Hub case from #242: without deriving the
// scope from the request path the token grants nothing and the retry 401s.
func TestScopelessChallengeUsesRequestDerivedScope(t *testing.T) {
	upstream := newTokenAuthRegistry(t, tokenAuthOptions{}, serveTokenTestRepository(t))
	mirror := httptest.NewServer(newLoopbackMirrorServer(t, upstream.registry.URL, t.TempDir()))
	defer mirror.Close()

	resp, body := get(t, mirror.URL+"/v2/library/pause/manifests/3.10")
	if resp.StatusCode != http.StatusOK || body != manifestBody {
		t.Fatalf("manifest through scopeless challenge = %d %q, want 200", resp.StatusCode, body)
	}
	if scope := upstream.nextScope(t); scope != "repository:library/pause:pull" {
		t.Fatalf("token scope = %q, want repository:library/pause:pull", scope)
	}
	if hits := upstream.tokenHits.Load(); hits != 1 {
		t.Fatalf("token hits = %d, want 1", hits)
	}
}

func TestChallengeScopeWinsOverRequestScope(t *testing.T) {
	upstream := newTokenAuthRegistry(t, tokenAuthOptions{includeScope: true}, serveTokenTestRepository(t))
	mirror := httptest.NewServer(newLoopbackMirrorServer(t, upstream.registry.URL, t.TempDir()))
	defer mirror.Close()

	resp, _ := get(t, mirror.URL+"/v2/library/pause/manifests/3.10")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manifest through scoped challenge = %d, want 200", resp.StatusCode)
	}
	if scope := upstream.nextScope(t); scope != "repository:library/pause:pull" {
		t.Fatalf("token scope = %q, want repository:library/pause:pull", scope)
	}
}

func TestUpstreamTokenReusedAcrossRequests(t *testing.T) {
	upstream := newTokenAuthRegistry(t, tokenAuthOptions{expiresIn: 300}, serveTokenTestRepository(t))
	mirror := httptest.NewServer(newLoopbackMirrorServer(t, upstream.registry.URL, t.TempDir()))
	defer mirror.Close()

	blobPath := "/v2/library/pause/blobs/sha256:" + sha256Hex([]byte(tokenTestBlob))
	for _, path := range []string{"/v2/library/pause/manifests/3.10", blobPath} {
		resp, body := get(t, mirror.URL+path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d %q, want 200", path, resp.StatusCode, body)
		}
	}
	if hits := upstream.tokenHits.Load(); hits != 1 {
		t.Fatalf("token hits = %d, want 1 (token cached per scope)", hits)
	}
	// the second request must have carried the cached token on its first try
	if got := drainAuthorizations(upstream.authSeen); got != 3 {
		t.Fatalf("upstream requests = %d, want 3 (401, retry, cached)", got)
	}
}

func TestExpiredUpstreamTokenIsRenegotiated(t *testing.T) {
	upstream := newTokenAuthRegistry(t, tokenAuthOptions{expiresIn: 60}, serveTokenTestRepository(t))
	server := newLoopbackMirrorServer(t, upstream.registry.URL, t.TempDir())
	now := time.Now()
	server.now = func() time.Time { return now }
	mirror := httptest.NewServer(server)
	defer mirror.Close()

	blobPath := "/v2/library/pause/blobs/sha256:" + sha256Hex([]byte(tokenTestBlob))
	if resp, body := get(t, mirror.URL+"/v2/library/pause/manifests/3.10"); resp.StatusCode != http.StatusOK {
		t.Fatalf("manifest = %d %q, want 200", resp.StatusCode, body)
	}
	now = now.Add(2 * time.Minute)
	if resp, body := get(t, mirror.URL+blobPath); resp.StatusCode != http.StatusOK {
		t.Fatalf("blob = %d %q, want 200", resp.StatusCode, body)
	}
	if hits := upstream.tokenHits.Load(); hits != 2 {
		t.Fatalf("token hits = %d, want 2 (expired token renegotiated)", hits)
	}
}

// A cached token the upstream rejects must be discarded and renegotiated, not
// replayed on every later request.
func TestRejectedCachedTokenIsRefreshed(t *testing.T) {
	upstream := newTokenAuthRegistry(t, tokenAuthOptions{expiresIn: 300, singleUse: true}, serveTokenTestRepository(t))
	mirror := httptest.NewServer(newLoopbackMirrorServer(t, upstream.registry.URL, t.TempDir()))
	defer mirror.Close()

	blobPath := "/v2/library/pause/blobs/sha256:" + sha256Hex([]byte(tokenTestBlob))
	for _, path := range []string{"/v2/library/pause/manifests/3.10", blobPath, "/v2/library/pause/manifests/3.10"} {
		resp, body := get(t, mirror.URL+path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d %q, want 200", path, resp.StatusCode, body)
		}
	}
	if hits := upstream.tokenHits.Load(); hits != 3 {
		t.Fatalf("token hits = %d, want 3 (rejected token refreshed each time)", hits)
	}
}

func TestRegistryWithoutChallengeIsUntouched(t *testing.T) {
	authorizations := make(chan string, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", "sha256:"+sha256Hex([]byte(manifestBody)))
		_, _ = fmt.Fprint(w, manifestBody)
	}))
	defer upstream.Close()

	mirror := httptest.NewServer(newLoopbackMirrorServer(t, upstream.URL, t.TempDir()))
	defer mirror.Close()

	resp, body := get(t, mirror.URL+"/v2/library/pause/manifests/3.10")
	if resp.StatusCode != http.StatusOK || body != manifestBody {
		t.Fatalf("manifest from open registry = %d %q, want 200", resp.StatusCode, body)
	}
	close(authorizations)
	for authorization := range authorizations {
		if authorization != "" {
			t.Fatalf("open registry saw Authorization %q, want none", authorization)
		}
	}
}

// A 401 without any challenge is the upstream's answer, not ours to translate.
func TestUnauthorizedWithoutChallengeIsPassedThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, "denied")
	}))
	defer upstream.Close()

	mirror := httptest.NewServer(newLoopbackMirrorServer(t, upstream.URL, t.TempDir()))
	defer mirror.Close()

	resp, body := get(t, mirror.URL+"/v2/library/pause/manifests/3.10")
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "denied") {
		t.Fatalf("challengeless 401 = %d %q, want 401 passed through", resp.StatusCode, body)
	}
}

// The warm replay path shares the upstream client, so it must negotiate too.
func TestWarmNegotiatesScopelessChallenge(t *testing.T) {
	blobDigest := "sha256:" + sha256Hex([]byte(tokenTestBlob))
	manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%d},"layers":[]}`,
		blobDigest, len(tokenTestBlob))
	manifestDigest := "sha256:" + sha256Hex([]byte(manifest))

	upstream := newTokenAuthRegistry(t, tokenAuthOptions{expiresIn: 300}, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/library/pause/manifests/3.10", "/v2/library/pause/manifests/" + manifestDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = fmt.Fprint(w, manifest)
		case "/v2/library/pause/blobs/" + blobDigest:
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = fmt.Fprint(w, tokenTestBlob)
		default:
			http.NotFound(w, r)
		}
	})

	// the mirror refuses loopback upstreams, so both the registry and its token
	// realm are reached through public-looking aliases
	upstream.realm = aliasedURL(t, upstream.token.URL, "token.example")

	manager := newManagerWithPorts(t.TempDir(), nil, freePort(t))
	manager.baseOverride = aliasedURL(t, upstream.registry.URL, "registry.example")
	egress := egressForRoutes(
		aliasRoute(t, upstream.registry.URL, "registry.example", "203.0.113.10"),
		aliasRoute(t, upstream.token.URL, "token.example", "203.0.113.30"),
	)
	manager.resolveUpstreamIPs = egress.resolve
	manager.hostOwnedIPs = egress.hostIPs
	manager.dialContext = egress.dialContext
	defer manager.Close()

	summary, err := manager.Warm(context.Background(), []string{"registry.example/library/pause:3.10"}, imagecache.ArchitectureAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Failed != 0 || summary.Warmed != 1 {
		t.Fatalf("warm summary = %+v, want one warmed ref", summary)
	}
	if scope := upstream.nextScope(t); scope != "repository:library/pause:pull" {
		t.Fatalf("token scope = %q, want repository:library/pause:pull", scope)
	}
	if hits := upstream.tokenHits.Load(); hits != 1 {
		t.Fatalf("token hits = %d, want 1 (token cached across the warm graph)", hits)
	}
}

func drainAuthorizations(seen chan string) int {
	count := 0
	for {
		select {
		case <-seen:
			count++
		default:
			return count
		}
	}
}
