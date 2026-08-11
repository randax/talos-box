package mirror

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// two ephemeral test ports so we don't collide with a running tbxd's 5055+.
func testPorts(t *testing.T) []portBinding {
	t.Helper()
	var ports []portBinding
	for _, up := range []string{"docker.io", "registry.k8s.io"} {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		p := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
		ports = append(ports, portBinding{Upstream: up, Port: p})
	}
	return ports
}

func TestBindListensOnGatewayNotWildcard(t *testing.T) {
	ports := testPorts(t)
	m := newManagerWithPorts(t.TempDir(), ports, freePort(t))
	defer m.Close()

	if err := m.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	// the gateway IP:port is now serving
	for _, p := range ports {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", p.Port), time.Second)
		if err != nil {
			t.Errorf("gateway 127.0.0.1:%d not reachable: %v", p.Port, err)
		} else {
			_ = conn.Close()
		}
		// crucially, 0.0.0.0:port is NOT held by us — a wildcard listen still works
		wildcard, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", p.Port))
		if err != nil {
			t.Errorf("port %d appears bound on 0.0.0.0 (should be gateway-specific): %v", p.Port, err)
		} else {
			_ = wildcard.Close()
		}
	}
}

func TestUnbindReleasesPorts(t *testing.T) {
	ports := testPorts(t)
	m := newManagerWithPorts(t.TempDir(), ports, freePort(t))
	defer m.Close()
	if err := m.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	m.Unbind("127.0.0.1")
	time.Sleep(50 * time.Millisecond)
	// the gateway ports are free again
	for _, p := range ports {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p.Port))
		if err != nil {
			t.Errorf("port %d not released after Unbind: %v", p.Port, err)
		} else {
			_ = l.Close()
		}
	}
}

func TestBindIsIdempotent(t *testing.T) {
	ports := testPorts(t)
	m := newManagerWithPorts(t.TempDir(), ports, freePort(t))
	defer m.Close()
	if err := m.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Bind("127.0.0.1"); err != nil {
		t.Errorf("second Bind of the same gateway should be a no-op, got %v", err)
	}
}

func TestUnbindUnknownGatewayIsNoOp(t *testing.T) {
	m := newManagerWithPorts(t.TempDir(), testPorts(t), freePort(t))
	defer m.Close()
	m.Unbind("172.30.9.1") // never bound; must not panic
}

func TestMirrorServesThroughGatewayBinding(t *testing.T) {
	f := newFakeRegistry(t, false)
	ports := []portBinding{{Upstream: "test", Port: freePort(t)}}
	m := &Manager{cacheRoot: t.TempDir(), ports: ports, bound: map[string][]*http.Server{}, baseOverride: f.registry.URL}
	defer m.Close()
	if err := m.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	resp, _ := get(t, fmt.Sprintf("http://127.0.0.1:%d/v2/", ports[0].Port))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("mirror through gateway binding /v2/ = %d", resp.StatusCode)
	}
}

func TestCatchAllMirrorRoutesByNamespaceCachesUnderUpstreamAndMapsDockerIO(t *testing.T) {
	f := newFakeRegistry(t, false)
	cacheRoot := t.TempDir()
	catchAllPort := freePort(t)
	var upstreamHost atomic.Value

	m := newManagerWithPorts(cacheRoot, nil, catchAllPort)
	m.serverFactory = func(_ string, base, cacheDir string) http.Handler {
		server := NewServer(base, cacheDir)
		server.client.Transport = rewriteTransport{
			target:  mustURL(t, f.registry.URL),
			wrapped: http.DefaultTransport,
			record: func(host string) {
				upstreamHost.Store(host)
			},
		}
		return server
	}
	defer m.Close()

	if err := m.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}

	resp, body := get(t, fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/latest?ns=docker.io", catchAllPort))
	if resp.StatusCode != http.StatusOK || body != manifestBody {
		t.Fatalf("catch-all manifest = %d %q", resp.StatusCode, body)
	}
	if got := upstreamHost.Load(); got != "registry-1.docker.io" {
		t.Fatalf("docker.io upstream host = %v, want registry-1.docker.io", got)
	}

	manifestCache := filepath.Join(cacheRoot, "docker.io", "manifests", "app_manifests_latest")
	if _, err := os.Stat(manifestCache); err != nil {
		t.Fatalf("manifest cache missing at %s: %v", manifestCache, err)
	}
}

func TestCatchAllMirrorRefusesNonPublicNamespaces(t *testing.T) {
	catchAllPort := freePort(t)
	var upstreamHits atomic.Int64

	m := newManagerWithPorts(t.TempDir(), nil, catchAllPort)
	m.serverFactory = func(_ string, base, cacheDir string) http.Handler {
		upstreamHits.Add(1)
		return NewServer(base, cacheDir)
	}
	defer m.Close()

	if err := m.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}

	for _, ns := range []string{"localhost", "10.0.0.7", "100.64.0.7", "169.254.1.7"} {
		t.Run(ns, func(t *testing.T) {
			resp, _ := get(t, fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/latest?ns=%s", catchAllPort, url.QueryEscape(ns)))
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status for ns=%s = %d, want 403", ns, resp.StatusCode)
			}
		})
	}

	if upstreamHits.Load() != 0 {
		t.Fatalf("blocked namespaces reached upstream %d times, want 0", upstreamHits.Load())
	}
}

func TestLegacyMirrorPortStillServesWhenCatchAllIsBound(t *testing.T) {
	f := newFakeRegistry(t, false)
	legacyPort := freePort(t)
	m := &Manager{
		cacheRoot:    t.TempDir(),
		ports:        []portBinding{{Upstream: "docker.io", Port: legacyPort}},
		catchAllPort: freePort(t),
		bound:        map[string][]*http.Server{},
		dynamic:      map[string]http.Handler{},
		baseOverride: f.registry.URL,
	}
	m.serverFactory = func(_ string, base, cacheDir string) http.Handler {
		return newLoopbackMirrorServer(t, base, cacheDir)
	}
	defer m.Close()

	if err := m.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}

	resp, body := get(t, fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/latest", legacyPort))
	if resp.StatusCode != http.StatusOK || body != manifestBody {
		t.Fatalf("legacy mirror manifest = %d %q", resp.StatusCode, body)
	}
}

func TestLegacyMirrorPortHonorsDefaultProxyTransport(t *testing.T) {
	var proxyHits atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", "sha256:"+sha256Hex([]byte(manifestBody)))
		_, _ = w.Write([]byte(manifestBody))
	}))
	defer proxy.Close()
	parsedProxy := mustURL(t, proxy.URL)
	originalTransport := http.DefaultTransport
	clonedTransport := http.DefaultTransport.(*http.Transport).Clone()
	clonedTransport.Proxy = func(*http.Request) (*url.URL, error) {
		return parsedProxy, nil
	}
	http.DefaultTransport = clonedTransport
	defer func() {
		http.DefaultTransport = originalTransport
		clonedTransport.CloseIdleConnections()
	}()

	legacyPort := freePort(t)
	m := &Manager{
		cacheRoot:    t.TempDir(),
		ports:        []portBinding{{Upstream: "docker.io", Port: legacyPort}},
		catchAllPort: freePort(t),
		bound:        map[string][]*http.Server{},
		dynamic:      map[string]http.Handler{},
		baseOverride: "http://registry.example",
	}
	defer m.Close()

	if err := m.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}

	resp, body := get(t, fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/latest", legacyPort))
	if resp.StatusCode != http.StatusOK || body != manifestBody {
		t.Fatalf("legacy mirror via default proxy transport = %d %q", resp.StatusCode, body)
	}
	if proxyHits.Load() == 0 {
		t.Fatal("legacy fixed mirror did not use the default transport proxy")
	}
}

func TestCatchAllMirrorCanonicalizesAuthorities(t *testing.T) {
	tests := []struct {
		name          string
		requestNS     string
		repeatNS      string
		wantBase      string
		wantCacheBase string
	}{
		{
			name:          "docker.io equivalent forms share canonical mapping",
			requestNS:     "DOCKER.IO.",
			repeatNS:      "docker.io",
			wantBase:      "https://registry-1.docker.io",
			wantCacheBase: "docker.io",
		},
		{
			name:          "explicit port is preserved",
			requestNS:     "docker.io:5443",
			wantBase:      "https://registry-1.docker.io:5443",
			wantCacheBase: "docker.io_5443",
		},
		{
			name:          "ipv6 authority is bracketed and cache-safe",
			requestNS:     "[2001:DB8::1]:5000",
			wantBase:      "https://[2001:db8::1]:5000",
			wantCacheBase: "2001_db8__1_5000",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			catchAllPort := freePort(t)
			var bases []string
			var cacheBases []string

			m := newManagerWithPorts(t.TempDir(), nil, catchAllPort)
			m.resolveUpstreamIPs = func(context.Context, string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("203.0.113.10")}, nil
			}
			m.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
			m.serverFactory = func(_ string, base, cacheDir string) http.Handler {
				bases = append(bases, base)
				cacheBases = append(cacheBases, filepath.Base(cacheDir))
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(manifestBody))
				})
			}
			defer m.Close()

			if err := m.Bind("127.0.0.1"); err != nil {
				t.Fatal(err)
			}

			for _, ns := range []string{test.requestNS, test.repeatNS} {
				if ns == "" {
					continue
				}
				resp, body := get(t, fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/latest?ns=%s", catchAllPort, url.QueryEscape(ns)))
				if resp.StatusCode != http.StatusOK || body != manifestBody {
					t.Fatalf("ns=%s -> %d %q", ns, resp.StatusCode, body)
				}
			}

			if len(bases) != 1 {
				t.Fatalf("serverFactory calls = %d, want 1", len(bases))
			}
			if bases[0] != test.wantBase {
				t.Fatalf("base = %q, want %q", bases[0], test.wantBase)
			}
			if cacheBases[0] != test.wantCacheBase {
				t.Fatalf("cache base = %q, want %q", cacheBases[0], test.wantCacheBase)
			}
		})
	}
}

func TestCatchAllMirrorRejectsMalformedAuthorities(t *testing.T) {
	catchAllPort := freePort(t)
	m := newManagerWithPorts(t.TempDir(), nil, catchAllPort)
	defer m.Close()

	if err := m.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}

	for _, ns := range []string{
		"http://docker.io",
		"user@docker.io",
		"docker.io/path",
		"docker.io?query=yes",
		"docker.io#fragment",
		"docker.io:",
		"[2001:db8::1",
	} {
		t.Run(ns, func(t *testing.T) {
			resp, _ := get(t, fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/latest?ns=%s", catchAllPort, url.QueryEscape(ns)))
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status for malformed ns=%s = %d, want 400", ns, resp.StatusCode)
			}
		})
	}
}

func TestCatchAllMirrorRejectsBlockedHostPortAuthorities(t *testing.T) {
	catchAllPort := freePort(t)
	m := newManagerWithPorts(t.TempDir(), nil, catchAllPort)
	m.resolveUpstreamIPs = func(_ context.Context, host string) ([]net.IP, error) {
		switch canonicalLookupHost(host) {
		case "localhost":
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		default:
			return []net.IP{net.ParseIP(canonicalLookupHost(host))}, nil
		}
	}
	m.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	defer m.Close()

	if err := m.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}

	for _, ns := range []string{"127.0.0.1:5000", "10.0.0.7:5000", "localhost:5000", "[::1]:5000"} {
		t.Run(ns, func(t *testing.T) {
			resp, _ := get(t, fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/latest?ns=%s", catchAllPort, url.QueryEscape(ns)))
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status for blocked ns=%s = %d, want 403", ns, resp.StatusCode)
			}
		})
	}
}

func TestCatchAllMirrorRejectsNonGlobalAuthorities(t *testing.T) {
	catchAllPort := freePort(t)
	m := newManagerWithPorts(t.TempDir(), nil, catchAllPort)
	m.resolveUpstreamIPs = func(_ context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP(canonicalLookupHost(host))}, nil
	}
	m.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	defer m.Close()

	if err := m.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}

	for _, ns := range []string{"0.0.0.0", "::", "255.255.255.255"} {
		t.Run(ns, func(t *testing.T) {
			resp, _ := get(t, fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/latest?ns=%s", catchAllPort, url.QueryEscape(ns)))
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status for non-global ns=%s = %d, want 403", ns, resp.StatusCode)
			}
		})
	}
}

func TestCatchAllMirrorBoundsDynamicHandlersAndClosesEvictedEntries(t *testing.T) {
	catchAllPort := freePort(t)
	var created atomic.Int64
	var closed atomic.Int64

	m := newManagerWithPorts(t.TempDir(), nil, catchAllPort)
	m.dynamicCap = 4
	m.resolveUpstreamIPs = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("203.0.113.10")}, nil
	}
	m.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	m.serverFactory = func(_ string, _, _ string) http.Handler {
		created.Add(1)
		return &closableHandler{closed: &closed}
	}
	defer m.Close()

	if err := m.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}

	totalAuthorities := int(m.dynamicCap) + 3
	for i := 0; i < totalAuthorities; i++ {
		ns := fmt.Sprintf("registry-%d.example", i)
		resp, body := get(t, fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/latest?ns=%s", catchAllPort, url.QueryEscape(ns)))
		if resp.StatusCode != http.StatusOK || body != manifestBody {
			t.Fatalf("ns=%s -> %d %q", ns, resp.StatusCode, body)
		}
	}

	if got := len(m.dynamic); got != int(m.dynamicCap) {
		t.Fatalf("retained dynamic handlers = %d, want %d", got, m.dynamicCap)
	}
	if created.Load() != int64(totalAuthorities) {
		t.Fatalf("created handlers = %d, want %d", created.Load(), totalAuthorities)
	}
	if closed.Load() != int64(totalAuthorities-int(m.dynamicCap)) {
		t.Fatalf("closed handlers after eviction = %d, want %d", closed.Load(), totalAuthorities-int(m.dynamicCap))
	}

	m.Close()
	if got := len(m.dynamic); got != 0 {
		t.Fatalf("dynamic handlers after Close = %d, want 0", got)
	}
	if closed.Load() != int64(totalAuthorities) {
		t.Fatalf("closed handlers after Close = %d, want %d", closed.Load(), totalAuthorities)
	}
}

type rewriteTransport struct {
	target  *url.URL
	wrapped http.RoundTripper
	record  func(string)
}

func (t rewriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t.record != nil {
		t.record(request.URL.Host)
	}
	clone := request.Clone(request.Context())
	cloneURL := *clone.URL
	clone.URL = &cloneURL
	clone.URL.Scheme = t.target.Scheme
	clone.URL.Host = t.target.Host
	return t.wrapped.RoundTrip(clone)
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

type closableHandler struct {
	closed *atomic.Int64
}

func (h *closableHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(manifestBody))
}

func (h *closableHandler) CloseIdleConnections() {
	h.closed.Add(1)
}

func freePort(t *testing.T) int {
	t.Helper()
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	p := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return p
}
