package mirror

import (
	"fmt"
	"net"
	"net/http"
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
	defer m.Close()

	if err := m.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}

	resp, body := get(t, fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/latest", legacyPort))
	if resp.StatusCode != http.StatusOK || body != manifestBody {
		t.Fatalf("legacy mirror manifest = %d %q", resp.StatusCode, body)
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

func freePort(t *testing.T) int {
	t.Helper()
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	p := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return p
}
