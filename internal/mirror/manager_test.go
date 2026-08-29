package mirror

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
		// A wildcard listener would also accept traffic on another loopback IP.
		other, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.2:%d", p.Port), 100*time.Millisecond)
		if err == nil {
			_ = other.Close()
			t.Errorf("port %d accepts traffic outside the requested gateway address", p.Port)
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

func TestBoundGatewayIPsTracksActualBindAndUnbind(t *testing.T) {
	m := newManagerWithPorts(t.TempDir(), testPorts(t), freePort(t))
	defer m.Close()
	if got := m.BoundGatewayIPs(); len(got) != 0 {
		t.Fatalf("initial bound gateways = %v, want empty", got)
	}
	if err := m.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	got := m.BoundGatewayIPs()
	if len(got) != 1 || got[0] != "127.0.0.1" {
		t.Fatalf("bound gateways = %v, want [127.0.0.1]", got)
	}
	got[0] = "mutated"
	if gotAgain := m.BoundGatewayIPs(); len(gotAgain) != 1 || gotAgain[0] != "127.0.0.1" {
		t.Fatalf("bound gateways copy mutated internal state: %v", gotAgain)
	}
	m.Unbind("127.0.0.1")
	if got := m.BoundGatewayIPs(); len(got) != 0 {
		t.Fatalf("bound gateways after unbind = %v, want empty", got)
	}
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

	manifestCache := NewServer("https://registry-1.docker.io", filepath.Join(cacheRoot, "docker.io")).manifestPath("/v2/app/manifests/latest")
	if _, err := os.Stat(manifestCache); err != nil {
		t.Fatalf("manifest cache missing at %s: %v", manifestCache, err)
	}
}

func TestCatchAllPathPrefixRoutesToSameUpstreamAndCacheAsNamespace(t *testing.T) {
	digest := "sha256:" + sha256Hex([]byte(manifestBody))
	cacheRoot := t.TempDir()
	m := newManagerWithPorts(cacheRoot, nil, 0)
	m.resolveUpstreamIPs = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("203.0.113.10")}, nil
	}
	m.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	m.serverFactory = func(_ string, base, cacheDir string) http.Handler {
		server := NewServer(base, cacheDir)
		server.client.Transport = roundTripFunc(func(r *http.Request) *http.Response {
			if r.URL.Path != "/v2/docker/library/golang/manifests/latest" {
				return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("not found"))}
			}
			header := make(http.Header)
			header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			header.Set("Docker-Content-Digest", digest)
			return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(manifestBody))}
		})
		return server
	}
	defer m.Close()

	queryRecorder := httptest.NewRecorder()
	m.serveCatchAll(queryRecorder, httptest.NewRequest(http.MethodGet, "/v2/docker/library/golang/manifests/latest?ns=PUBLIC.ECR.AWS.", nil))
	if queryRecorder.Code != http.StatusOK || queryRecorder.Body.String() != manifestBody {
		t.Fatalf("namespace form = %d %q", queryRecorder.Code, queryRecorder.Body.String())
	}

	pathRecorder := httptest.NewRecorder()
	m.serveCatchAll(pathRecorder, httptest.NewRequest(http.MethodHead, "/v2/public.ecr.aws/docker/library/golang/manifests/latest", nil))
	if pathRecorder.Code != http.StatusOK {
		t.Fatalf("path HEAD = %d, want 200", pathRecorder.Code)
	}
	if got := pathRecorder.Header().Get("Docker-Content-Digest"); got != digest {
		t.Fatalf("path HEAD Docker-Content-Digest = %q, want %q", got, digest)
	}
	if got := len(m.dynamic); got != 1 {
		t.Fatalf("dynamic handlers = %d, want one shared handler", got)
	}
	entries, err := os.ReadDir(cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "public.ecr.aws" {
		t.Fatalf("cache roots = %v, want only public.ecr.aws", entries)
	}
}

func TestCatchAllPathPrefixMapsDockerIOToRegistryOne(t *testing.T) {
	m := newManagerWithPorts(t.TempDir(), nil, 0)
	m.resolveUpstreamIPs = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("203.0.113.10")}, nil
	}
	m.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	var gotAuthority, gotBase, gotCacheDir string
	m.serverFactory = func(authority, base, cacheDir string) http.Handler {
		gotAuthority, gotBase, gotCacheDir = authority, base, cacheDir
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	}
	defer m.Close()

	recorder := httptest.NewRecorder()
	m.serveCatchAll(recorder, httptest.NewRequest(http.MethodGet, "/v2/docker.io/library/busybox/manifests/latest", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("path-prefixed docker.io = %d %q", recorder.Code, recorder.Body.String())
	}
	if gotAuthority != "docker.io" || gotBase != "https://registry-1.docker.io" || filepath.Base(gotCacheDir) != "docker.io" {
		t.Fatalf("factory arguments = (%q, %q, %q), want docker.io canonical mapping", gotAuthority, gotBase, gotCacheDir)
	}
}

func TestCatchAllPathPrefixPreservesRepositoryPath(t *testing.T) {
	m := newManagerWithPorts(t.TempDir(), nil, 0)
	m.resolveUpstreamIPs = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("203.0.113.10")}, nil
	}
	m.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	var gotPath, gotQuery string
	m.serverFactory = func(string, string, string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath, gotQuery = r.URL.EscapedPath(), r.URL.RawQuery
			w.WriteHeader(http.StatusOK)
		})
	}
	defer m.Close()

	recorder := httptest.NewRecorder()
	m.serveCatchAll(recorder, httptest.NewRequest(http.MethodGet, "/v2/xpkg.crossplane.io/crossplane/provider-aws/manifests/v1.0.0?variant=debug", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("path-prefixed request = %d %q", recorder.Code, recorder.Body.String())
	}
	if gotPath != "/v2/crossplane/provider-aws/manifests/v1.0.0" || gotQuery != "variant=debug" {
		t.Fatalf("forwarded target = %q?%s", gotPath, gotQuery)
	}
}

func TestCatchAllPathPrefixRejectsEncodedRepositorySlash(t *testing.T) {
	m := newManagerWithPorts(t.TempDir(), nil, 0)
	m.resolveUpstreamIPs = func(context.Context, string) ([]net.IP, error) {
		t.Fatal("encoded repository slash reached authority resolution")
		return nil, nil
	}
	defer m.Close()

	recorder := httptest.NewRecorder()
	m.serveCatchAll(recorder, httptest.NewRequest(http.MethodGet, "/v2/public.ecr.aws/team%2Fapp/manifests/latest", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("encoded repository slash = %d %q, want 400", recorder.Code, recorder.Body.String())
	}
}

func TestCatchAllMirrorRefusesNonPublicNamespaces(t *testing.T) {
	var dials atomic.Int64

	m := newManagerWithPorts(t.TempDir(), nil, 0)
	m.resolveUpstreamIPs = func(_ context.Context, host string) ([]net.IP, error) {
		switch host {
		case "loopback.registry.example":
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		case "10.0.0.7", "100.64.0.7", "169.254.1.7":
			return []net.IP{net.ParseIP(host)}, nil
		default:
			return nil, fmt.Errorf("unexpected host %q", host)
		}
	}
	m.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	m.dialContext = func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, fmt.Errorf("unexpected dial")
	}
	defer m.Close()

	for _, ns := range []string{"loopback.registry.example", "10.0.0.7", "100.64.0.7", "169.254.1.7"} {
		t.Run(ns, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/v2/app/manifests/latest?ns="+url.QueryEscape(ns), nil)
			m.serveCatchAll(recorder, request)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status for ns=%s = %d, want 403", ns, recorder.Code)
			}
		})
	}

	if dials.Load() != 0 {
		t.Fatalf("blocked namespaces dialed upstream %d times, want 0", dials.Load())
	}
}

func TestCatchAllPathPrefixRefusesNonPublicAuthorities(t *testing.T) {
	var dials atomic.Int64
	m := newManagerWithPorts(t.TempDir(), nil, 0)
	m.resolveUpstreamIPs = func(_ context.Context, host string) ([]net.IP, error) {
		switch host {
		case "loopback.registry.example":
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		case "10.0.0.7", "100.64.0.7", "169.254.1.7":
			return []net.IP{net.ParseIP(host)}, nil
		default:
			return nil, fmt.Errorf("unexpected host %q", host)
		}
	}
	m.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	m.dialContext = func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, fmt.Errorf("unexpected dial")
	}
	defer m.Close()

	for _, authority := range []string{"loopback.registry.example", "10.0.0.7", "100.64.0.7", "169.254.1.7"} {
		t.Run(authority, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/v2/"+authority+"/app/manifests/latest", nil)
			m.serveCatchAll(recorder, request)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status for authority %s = %d, want 403", authority, recorder.Code)
			}
		})
	}
	if dials.Load() != 0 {
		t.Fatalf("blocked path authorities dialed upstream %d times, want 0", dials.Load())
	}
}

func TestCatchAllPathPrefixLoopbackRedirectMatchesNamespaceForm(t *testing.T) {
	m := newManagerWithPorts(t.TempDir(), nil, 0)
	defer m.Close()

	locations := make([]string, 0, 2)
	for _, requestURL := range []string{
		"/v2/app/manifests/latest?variant=debug&ns=localhost%3A30500",
		"/v2/localhost:30500/app/manifests/latest?variant=debug",
	} {
		recorder := httptest.NewRecorder()
		m.serveCatchAll(recorder, httptest.NewRequest(http.MethodGet, requestURL, nil))
		if recorder.Code != http.StatusTemporaryRedirect {
			t.Fatalf("GET %s = %d %q, want 307", requestURL, recorder.Code, recorder.Body.String())
		}
		locations = append(locations, recorder.Header().Get("Location"))
	}
	if locations[0] != locations[1] {
		t.Fatalf("redirect locations differ: namespace=%q path=%q", locations[0], locations[1])
	}
}

func TestCatchAllPathPrefixOfflineMissDoesNotCreateDynamicHandler(t *testing.T) {
	var handlers atomic.Int64
	m := newManagerWithPorts(t.TempDir(), nil, 0)
	m.SetOffline(true)
	m.serverFactory = func(string, string, string) http.Handler {
		handlers.Add(1)
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	}
	defer m.Close()

	recorder := httptest.NewRecorder()
	m.serveCatchAll(recorder, httptest.NewRequest(http.MethodGet, "/v2/public.ecr.aws/app/manifests/missing", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("offline path miss = %d %q, want 404", recorder.Code, recorder.Body.String())
	}
	if handlers.Load() != 0 || len(m.dynamic) != 0 {
		t.Fatalf("offline path miss allocated handlers: factory=%d dynamic=%d", handlers.Load(), len(m.dynamic))
	}
}

func TestCatchAllMirrorRedirectsSyntacticLoopbackAuthorities(t *testing.T) {
	tests := []struct {
		name         string
		method       string
		namespace    string
		wantLocation string
		offline      bool
	}{
		{
			name:         "localhost node port",
			method:       http.MethodGet,
			namespace:    "localhost:30500",
			wantLocation: "http://localhost:30500/v2/app/manifests/latest?platform=linux%2Famd64",
		},
		{
			name:         "loopback IPv4",
			method:       http.MethodHead,
			namespace:    "127.0.0.2:5000",
			wantLocation: "http://127.0.0.2:5000/v2/app/manifests/latest?platform=linux%2Famd64",
		},
		{
			name:         "loopback IPv6 while mirror offline",
			method:       http.MethodGet,
			namespace:    "[::1]:5000",
			wantLocation: "http://[::1]:5000/v2/app/manifests/latest?platform=linux%2Famd64",
			offline:      true,
		},
		{
			name:         "default TLS port",
			method:       http.MethodGet,
			namespace:    "localhost:443",
			wantLocation: "https://localhost:443/v2/app/manifests/latest?platform=linux%2Famd64",
		},
		{
			name:         "implicit TLS port",
			method:       http.MethodGet,
			namespace:    "localhost",
			wantLocation: "https://localhost/v2/app/manifests/latest?platform=linux%2Famd64",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := newManagerWithPorts(t.TempDir(), nil, 0)
			manager.offline.Store(test.offline)
			manager.resolveUpstreamIPs = func(context.Context, string) ([]net.IP, error) {
				t.Fatal("loopback redirect resolved the authority")
				return nil, nil
			}
			manager.hostOwnedIPs = func() ([]net.IP, error) {
				t.Fatal("loopback redirect inspected host addresses")
				return nil, nil
			}
			manager.dialContext = func(context.Context, string, string) (net.Conn, error) {
				t.Fatal("loopback redirect dialed from the host")
				return nil, nil
			}
			defer manager.Close()

			requestURL := "/v2/app/manifests/latest?platform=linux%2Famd64&ns=" + url.QueryEscape(test.namespace)
			recorder := httptest.NewRecorder()
			manager.serveCatchAll(recorder, httptest.NewRequest(test.method, requestURL, nil))

			if recorder.Code != http.StatusTemporaryRedirect {
				t.Fatalf("%s ns=%s = %d %q, want 307", test.method, test.namespace, recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("Location"); got != test.wantLocation {
				t.Fatalf("Location = %q, want %q", got, test.wantLocation)
			}
		})
	}
}

func TestCatchAllMirrorOfflineStillRejectsUncachedPublicPull(t *testing.T) {
	var upstreamHandlers atomic.Int64
	manager := newManagerWithPorts(t.TempDir(), nil, 0)
	manager.offline.Store(true)
	manager.serverFactory = func(string, string, string) http.Handler {
		upstreamHandlers.Add(1)
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
	}
	defer manager.Close()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v2/app/manifests/latest?ns=registry.example", nil)
	manager.serveCatchAll(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("offline public pull = %d %q, want 404", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "mirror offline: content not cached") {
		t.Fatalf("offline public pull body = %q, want cache-miss reason", recorder.Body.String())
	}
	if upstreamHandlers.Load() != 0 {
		t.Fatalf("offline public pull created %d upstream handlers, want 0", upstreamHandlers.Load())
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
			wantCacheBase: "docker.io__port_5443",
		},
		{
			name:          "ipv6 authority is bracketed and cache-safe",
			requestNS:     "[2001:DB8::1]:5000",
			wantBase:      "https://[2001:db8::1]:5000",
			wantCacheBase: "__ipv6_2001-db8--1__port_5000",
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
		"docker.io:+443",
		"docker.io:４４３",
		"[2001:db8::1",
		"foo_bar.example",
		"foo+bar.example",
		"bucher-\u00e4.example",
		"-edge.example",
		"edge-.example",
		"edge..example",
	} {
		t.Run(ns, func(t *testing.T) {
			resp, _ := get(t, fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/latest?ns=%s", catchAllPort, url.QueryEscape(ns)))
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status for malformed ns=%s = %d, want 400", ns, resp.StatusCode)
			}
		})
	}
}

func TestCatchAllMirrorCancellationStopsDefaultNamespaceValidation(t *testing.T) {
	catchAllPort := freePort(t)
	var handlerCalls atomic.Int64

	originalResolver := net.DefaultResolver
	resolveStarted := make(chan struct{})
	resolveReleased := make(chan struct{})
	releaseResolver := make(chan struct{})
	var signalStart sync.Once
	var signalRelease sync.Once
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			signalStart.Do(func() { close(resolveStarted) })
			select {
			case <-ctx.Done():
				signalRelease.Do(func() { close(resolveReleased) })
				return nil, ctx.Err()
			case <-releaseResolver:
				return nil, context.Canceled
			}
		},
	}
	defer func() {
		close(releaseResolver)
		net.DefaultResolver = originalResolver
	}()

	m := newManagerWithPorts(t.TempDir(), nil, catchAllPort)
	m.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	m.serverFactory = func(string, string, string) http.Handler {
		handlerCalls.Add(1)
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
	}
	defer m.Close()

	requestContext, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/latest?ns=docker.io", catchAllPort), nil).WithContext(requestContext)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		m.serveCatchAll(recorder, request)
		close(done)
	}()

	select {
	case <-resolveStarted:
	case <-time.After(time.Second):
		t.Fatal("default resolver Dial did not start")
	}
	cancel()

	select {
	case <-resolveReleased:
	case <-time.After(time.Second):
		t.Fatal("namespace validation did not observe request cancellation")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("catch-all handler did not return promptly after cancellation")
	}
	if recorder.Code == http.StatusOK {
		t.Fatalf("status = %d, want non-success after cancellation", recorder.Code)
	}
	if handlerCalls.Load() != 0 {
		t.Fatalf("dynamic handler calls = %d, want 0", handlerCalls.Load())
	}
}

func TestCatchAllMirrorHandlerCreationDoesNotPanicWhenDefaultTransportIsCustomRoundTripper(t *testing.T) {
	originalTransport := http.DefaultTransport
	http.DefaultTransport = rewriteTransport{
		target:  mustURL(t, "http://example.invalid"),
		wrapped: originalTransport,
	}
	defer func() { http.DefaultTransport = originalTransport }()

	m := newManagerWithPorts(t.TempDir(), nil, freePort(t))
	defer m.Close()
	authority, err := parseUpstreamAuthority("docker.io")
	if err != nil {
		t.Fatal(err)
	}

	handler := m.handlerForUpstream(authority)
	server, ok := handler.(*Server)
	if !ok {
		t.Fatalf("handler type = %T, want *Server", handler)
	}
	transport, ok := server.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", server.client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("safe transport proxy is not disabled")
	}
	if transport.DialContext == nil {
		t.Fatal("safe transport DialContext is nil")
	}
}

func TestCatchAllMirrorHandlerCreationDoesNotPanicWhenDefaultTransportIsTypedNilTransport(t *testing.T) {
	originalTransport := http.DefaultTransport
	var typedNil *http.Transport
	http.DefaultTransport = typedNil
	defer func() { http.DefaultTransport = originalTransport }()

	m := newManagerWithPorts(t.TempDir(), nil, freePort(t))
	defer m.Close()
	authority, err := parseUpstreamAuthority("docker.io")
	if err != nil {
		t.Fatal(err)
	}

	handler := m.handlerForUpstream(authority)
	server, ok := handler.(*Server)
	if !ok {
		t.Fatalf("handler type = %T, want *Server", handler)
	}
	transport, ok := server.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", server.client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("safe transport proxy is not disabled")
	}
	if transport.DialContext == nil {
		t.Fatal("safe transport DialContext is nil")
	}
}

func TestCatchAllMirrorDistinctAcceptedAuthoritiesDoNotShareHandlersOrCacheDirs(t *testing.T) {
	catchAllPort := freePort(t)
	type served struct {
		authority string
		cacheBase string
	}
	results := map[string]served{}

	m := newManagerWithPorts(t.TempDir(), nil, catchAllPort)
	m.resolveUpstreamIPs = func(_ context.Context, host string) ([]net.IP, error) {
		switch canonicalLookupHost(host) {
		case "docker.io":
			return []net.IP{net.ParseIP("203.0.113.10")}, nil
		case "192.0.2.10":
			return []net.IP{net.ParseIP("192.0.2.10")}, nil
		case "2001:db8::1":
			return []net.IP{net.ParseIP("2001:db8::1")}, nil
		default:
			return nil, fmt.Errorf("unexpected host %q", host)
		}
	}
	m.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	m.serverFactory = func(authority, _, cacheDir string) http.Handler {
		cacheBase := filepath.Base(cacheDir)
		results[authority] = served{authority: authority, cacheBase: cacheBase}
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(authority + "|" + cacheBase))
		})
	}
	defer m.Close()

	if err := m.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}

	requests := []string{"docker.io:5443", "docker.io:5444", "192.0.2.10", "[2001:db8::1]"}
	bodies := map[string]string{}
	for _, ns := range requests {
		resp, body := get(t, fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/latest?ns=%s", catchAllPort, url.QueryEscape(ns)))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("ns=%s -> %d %q", ns, resp.StatusCode, body)
		}
		bodies[ns] = body
	}

	for _, ns := range requests {
		resp, body := get(t, fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/latest?ns=%s", catchAllPort, url.QueryEscape(ns)))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("repeat ns=%s -> %d %q", ns, resp.StatusCode, body)
		}
		if body != bodies[ns] {
			t.Fatalf("repeat body for ns=%s = %q, want %q", ns, body, bodies[ns])
		}
	}

	seenBodies := map[string]string{}
	for _, ns := range requests {
		if other, ok := seenBodies[bodies[ns]]; ok {
			t.Fatalf("ns=%s reused body %q already served for %s", ns, bodies[ns], other)
		}
		seenBodies[bodies[ns]] = ns
	}

	seenCacheBases := map[string]string{}
	for authority, result := range results {
		if other, ok := seenCacheBases[result.cacheBase]; ok {
			t.Fatalf("authority %s reused cache base %q already assigned to %s", authority, result.cacheBase, other)
		}
		seenCacheBases[result.cacheBase] = authority
	}
}

func TestCatchAllMirrorRejectsBlockedHostPortAuthorities(t *testing.T) {
	var dials atomic.Int64
	m := newManagerWithPorts(t.TempDir(), nil, 0)
	m.resolveUpstreamIPs = func(_ context.Context, host string) ([]net.IP, error) {
		switch canonicalLookupHost(host) {
		case "loopback.registry.example":
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		default:
			return []net.IP{net.ParseIP(canonicalLookupHost(host))}, nil
		}
	}
	m.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	m.dialContext = func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, fmt.Errorf("unexpected dial")
	}
	defer m.Close()

	for _, ns := range []string{"10.0.0.7:5000", "172.16.0.7:5000", "192.168.0.7:5000", "loopback.registry.example:5000"} {
		t.Run(ns, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/v2/app/manifests/latest?ns="+url.QueryEscape(ns), nil)
			m.serveCatchAll(recorder, request)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status for blocked ns=%s = %d, want 403", ns, recorder.Code)
			}
		})
	}
	if dials.Load() != 0 {
		t.Fatalf("blocked authorities dialed upstream %d times, want 0", dials.Load())
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

func TestCatchAllPathPrefixUsesTheSameLRUCap(t *testing.T) {
	var created atomic.Int64
	var closed atomic.Int64

	m := newManagerWithPorts(t.TempDir(), nil, 0)
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

	totalAuthorities := int(m.dynamicCap) + 3
	for i := 0; i < totalAuthorities; i++ {
		authority := fmt.Sprintf("registry-%d.example", i)
		recorder := httptest.NewRecorder()
		m.serveCatchAll(recorder, httptest.NewRequest(http.MethodGet, "/v2/"+authority+"/app/manifests/latest", nil))
		if recorder.Code != http.StatusOK || recorder.Body.String() != manifestBody {
			t.Fatalf("authority=%s -> %d %q", authority, recorder.Code, recorder.Body.String())
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

func TestDynamicWrappedHandlerCleanupClosesRetainedMirrorServerAndWrapper(t *testing.T) {
	authorityOne, err := parseUpstreamAuthority("registry-one.example")
	if err != nil {
		t.Fatal(err)
	}
	authorityTwo, err := parseUpstreamAuthority("registry-two.example")
	if err != nil {
		t.Fatal(err)
	}

	var wrapperClosed atomic.Int64
	m := newManagerWithPorts(t.TempDir(), nil, 0)
	m.dynamicCap = 1
	m.serverFactory = func(_ string, _, _ string) http.Handler {
		return &closableWrappedHandler{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(manifestBody))
			}),
			closed: &wrapperClosed,
		}
	}
	defer m.Close()

	_ = m.handlerForUpstream(authorityOne)
	retainedOne := m.dynamicServers[authorityOne.cacheKey]
	if retainedOne == nil {
		t.Fatal("retained mirror server missing for wrapped handler")
	}
	retainedTransport := &countingIdleTransport{}
	retainedOne.client.Transport = retainedTransport

	_ = m.handlerForUpstream(authorityTwo)

	if got := wrapperClosed.Load(); got != 1 {
		t.Fatalf("wrapped handler close count = %d, want 1 after eviction", got)
	}
	if got := retainedTransport.closed.Load(); got != 1 {
		t.Fatalf("retained mirror transport close count = %d, want 1 after eviction", got)
	}

	m.Close()
	if got := wrapperClosed.Load(); got != 2 {
		t.Fatalf("wrapped handler close count after Manager.Close = %d, want 2", got)
	}
}

func TestDynamicServerCleanupDoesNotDoubleCloseSameServer(t *testing.T) {
	authority, err := parseUpstreamAuthority("registry.example")
	if err != nil {
		t.Fatal(err)
	}

	server := NewServer("https://registry.example", t.TempDir())
	transport := &countingIdleTransport{}
	server.client.Transport = transport

	m := newManagerWithPorts(t.TempDir(), nil, 0)
	m.serverFactory = func(_ string, _, _ string) http.Handler {
		return server
	}
	defer m.Close()

	_ = m.handlerForUpstream(authority)
	if retained := m.dynamicServers[authority.cacheKey]; retained != server {
		t.Fatalf("retained server = %p, want factory server %p", retained, server)
	}
	m.Close()

	if got := transport.closed.Load(); got != 1 {
		t.Fatalf("shared server transport close count = %d, want 1", got)
	}
}

func TestCatchAllMirrorInvalidNamespacesDoNotAllocateDynamicHandlers(t *testing.T) {
	catchAllPort := freePort(t)
	var created atomic.Int64

	m := newManagerWithPorts(t.TempDir(), nil, catchAllPort)
	m.resolveUpstreamIPs = func(_ context.Context, host string) ([]net.IP, error) {
		switch canonicalLookupHost(host) {
		case "registry-ok.example":
			return []net.IP{net.ParseIP("203.0.113.10")}, nil
		case "loopback.registry.example":
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		default:
			return nil, fmt.Errorf("unexpected host %q", host)
		}
	}
	m.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	m.serverFactory = func(authority, _, _ string) http.Handler {
		created.Add(1)
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(authority))
		})
	}
	defer m.Close()

	if err := m.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}

	resp, body := get(t, fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/latest?ns=%s", catchAllPort, url.QueryEscape("registry-ok.example")))
	if resp.StatusCode != http.StatusOK || body != "registry-ok.example" {
		t.Fatalf("valid ns = %d %q", resp.StatusCode, body)
	}
	if created.Load() != 1 {
		t.Fatalf("created handlers after valid request = %d, want 1", created.Load())
	}
	if got := len(m.dynamic); got != 1 {
		t.Fatalf("dynamic handlers after valid request = %d, want 1", got)
	}

	for i := 0; i < 3; i++ {
		resp, _ := get(t, fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/latest?ns=%s", catchAllPort, url.QueryEscape("loopback.registry.example")))
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("invalid ns request %d = %d, want 403", i, resp.StatusCode)
		}
	}

	if created.Load() != 1 {
		t.Fatalf("created handlers after invalid requests = %d, want 1", created.Load())
	}
	if got := len(m.dynamic); got != 1 {
		t.Fatalf("dynamic handlers after invalid requests = %d, want 1", got)
	}
}

func TestManagerOfflineToggleAffectsLegacyAndDynamicMirrors(t *testing.T) {
	f := newFakeRegistry(t, false)
	legacyPort := freePort(t)
	catchAllPort := freePort(t)
	cacheRoot := t.TempDir()

	m := &Manager{
		cacheRoot:    cacheRoot,
		ports:        []portBinding{{Upstream: "docker.io", Port: legacyPort}},
		catchAllPort: catchAllPort,
		resolveUpstreamIPs: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("203.0.113.10")}, nil
		},
		hostOwnedIPs: func() ([]net.IP, error) { return nil, nil },
		bound:        map[string][]*http.Server{},
		dynamic:      map[string]http.Handler{},
	}
	m.serverFactory = func(_ string, _, cacheDir string) http.Handler {
		server := newLoopbackMirrorServer(t, f.registry.URL, cacheDir)
		server.offline = &m.offline
		return server
	}
	defer m.Close()

	if err := m.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}

	legacyPath := fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/latest", legacyPort)
	dynamicPath := fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/latest?ns=docker.io", catchAllPort)
	for _, path := range []string{legacyPath, dynamicPath} {
		resp, body := get(t, path)
		if resp.StatusCode != http.StatusOK || body != manifestBody {
			t.Fatalf("warm path %s = %d %q", path, resp.StatusCode, body)
		}
	}
	cacheTagDigestRoot(t, NewServer("https://registry-1.docker.io", filepath.Join(cacheRoot, "docker.io")), "app", "latest")
	manifestHits := f.manifestHits.Load()

	m.SetOffline(true)

	for _, path := range []string{legacyPath, dynamicPath} {
		resp, body := get(t, path)
		if resp.StatusCode != http.StatusOK || body != manifestBody {
			t.Fatalf("offline cached path %s = %d %q", path, resp.StatusCode, body)
		}
	}
	if f.manifestHits.Load() != manifestHits {
		t.Fatalf("offline cached requests hit upstream manifest path: %d -> %d", manifestHits, f.manifestHits.Load())
	}

	for _, path := range []string{
		fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/missing", legacyPort),
		fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/missing?ns=docker.io", catchAllPort),
	} {
		resp, _ := get(t, path)
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("offline uncached path %s unexpectedly succeeded", path)
		}
	}
	if f.manifestHits.Load() != manifestHits {
		t.Fatalf("offline uncached requests hit upstream manifest path: %d -> %d", manifestHits, f.manifestHits.Load())
	}
}

func TestCatchAllPathPrefixCachedDigestSurvivesResolverFailure(t *testing.T) {
	validDigest := "sha256:" + sha256Hex([]byte(manifestBody))
	var failResolve atomic.Bool
	var resolves atomic.Int64
	var upstreamRequests atomic.Int64
	m := newManagerWithPorts(t.TempDir(), nil, 0)
	m.resolveUpstreamIPs = func(context.Context, string) ([]net.IP, error) {
		resolves.Add(1)
		if failResolve.Load() {
			return nil, fmt.Errorf("resolver unavailable")
		}
		return []net.IP{net.ParseIP("203.0.113.10")}, nil
	}
	m.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	m.serverFactory = func(_ string, base, cacheDir string) http.Handler {
		server := NewServer(base, cacheDir)
		server.client.Transport = roundTripFunc(func(*http.Request) *http.Response {
			upstreamRequests.Add(1)
			header := make(http.Header)
			header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			header.Set("Docker-Content-Digest", validDigest)
			return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(manifestBody))}
		})
		return server
	}
	defer m.Close()

	path := "/v2/docker.io/app/manifests/" + url.PathEscape(validDigest)
	warm := httptest.NewRecorder()
	m.serveCatchAll(warm, httptest.NewRequest(http.MethodGet, path, nil))
	if warm.Code != http.StatusOK || warm.Body.String() != manifestBody {
		t.Fatalf("warm digest = %d %q", warm.Code, warm.Body.String())
	}
	requestsAfterWarm := upstreamRequests.Load()
	resolvesAfterWarm := resolves.Load()
	failResolve.Store(true)

	cached := httptest.NewRecorder()
	m.serveCatchAll(cached, httptest.NewRequest(http.MethodGet, path, nil))
	if cached.Code != http.StatusOK || cached.Body.String() != manifestBody {
		t.Fatalf("cached digest = %d %q", cached.Code, cached.Body.String())
	}
	if upstreamRequests.Load() != requestsAfterWarm {
		t.Fatalf("cached digest contacted upstream again: %d -> %d", requestsAfterWarm, upstreamRequests.Load())
	}
	if resolves.Load() != resolvesAfterWarm {
		t.Fatalf("cached digest resolved upstream again: %d -> %d", resolvesAfterWarm, resolves.Load())
	}
}

func TestCatchAllPathPrefixOfflineCachedTagServesWithoutResolveOrDial(t *testing.T) {
	var failResolve atomic.Bool
	var resolves atomic.Int64
	var upstreamRequests atomic.Int64
	cacheRoot := t.TempDir()
	m := newManagerWithPorts(cacheRoot, nil, 0)
	m.resolveUpstreamIPs = func(context.Context, string) ([]net.IP, error) {
		resolves.Add(1)
		if failResolve.Load() {
			return nil, fmt.Errorf("resolver unavailable")
		}
		return []net.IP{net.ParseIP("203.0.113.10")}, nil
	}
	m.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	m.serverFactory = func(_ string, base, cacheDir string) http.Handler {
		server := NewServer(base, cacheDir)
		server.client.Transport = roundTripFunc(func(r *http.Request) *http.Response {
			upstreamRequests.Add(1)
			header := make(http.Header)
			if r.URL.Path != "/v2/app/manifests/latest" {
				return &http.Response{StatusCode: http.StatusNotFound, Header: header, Body: io.NopCloser(strings.NewReader("not found"))}
			}
			header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			header.Set("Docker-Content-Digest", "sha256:"+sha256Hex([]byte(manifestBody)))
			return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(manifestBody))}
		})
		return server
	}
	defer m.Close()

	cachedTag := "/v2/docker.io/app/manifests/latest"
	warm := httptest.NewRecorder()
	m.serveCatchAll(warm, httptest.NewRequest(http.MethodGet, cachedTag, nil))
	if warm.Code != http.StatusOK || warm.Body.String() != manifestBody {
		t.Fatalf("warm tag = %d %q", warm.Code, warm.Body.String())
	}
	cacheTagDigestRoot(t, NewServer("https://registry-1.docker.io", filepath.Join(cacheRoot, "docker.io")), "app", "latest")
	requestsAfterWarm := upstreamRequests.Load()
	resolvesAfterWarm := resolves.Load()

	m.SetOffline(true)
	failResolve.Store(true)

	cached := httptest.NewRecorder()
	m.serveCatchAll(cached, httptest.NewRequest(http.MethodGet, cachedTag, nil))
	if cached.Code != http.StatusOK || cached.Body.String() != manifestBody {
		t.Fatalf("offline cached tag = %d %q", cached.Code, cached.Body.String())
	}
	for _, path := range []string{
		"/v2/docker.io/app/blobs/sha256:" + strings.Repeat("1", 64),
		"/v2/docker.io/app/manifests/missing",
		"/v2/docker.io/app/other",
	} {
		recorder := httptest.NewRecorder()
		m.serveCatchAll(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code == http.StatusOK {
			t.Fatalf("offline miss %s unexpectedly succeeded", path)
		}
	}
	if upstreamRequests.Load() != requestsAfterWarm {
		t.Fatalf("offline paths contacted upstream: %d -> %d", requestsAfterWarm, upstreamRequests.Load())
	}
	if resolves.Load() != resolvesAfterWarm {
		t.Fatalf("offline paths resolved upstream: %d -> %d", resolvesAfterWarm, resolves.Load())
	}
}

func TestFreshManagerCachedDigestServesWithoutResolverOrDial(t *testing.T) {
	validDigest := "sha256:" + sha256Hex([]byte(manifestBody))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", validDigest)
		_, _ = fmt.Fprint(w, manifestBody)
	}))
	defer upstream.Close()

	cacheRoot := t.TempDir()
	warm := newManagerWithPorts(cacheRoot, nil, freePort(t))
	warm.baseOverride = upstream.URL
	warm.resolveUpstreamIPs = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("203.0.113.10")}, nil
	}
	warm.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	warm.dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, hostPortOfURL(t, upstream.URL))
	}
	if err := warm.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/%s?ns=docker.io", warm.catchAllPort, url.QueryEscape(validDigest))
	resp, body := get(t, path)
	if resp.StatusCode != http.StatusOK || body != manifestBody {
		t.Fatalf("warm digest = %d %q", resp.StatusCode, body)
	}
	warm.Close()

	var resolves atomic.Int64
	var dials atomic.Int64
	restarted := newManagerWithPorts(cacheRoot, nil, freePort(t))
	restarted.baseOverride = upstream.URL
	restarted.resolveUpstreamIPs = func(context.Context, string) ([]net.IP, error) {
		resolves.Add(1)
		return nil, fmt.Errorf("resolver unavailable")
	}
	restarted.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	restarted.dialContext = func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, fmt.Errorf("unexpected dial")
	}
	if err := restarted.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()

	path = fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/%s?ns=docker.io", restarted.catchAllPort, url.QueryEscape(validDigest))
	resp, body = get(t, path)
	if resp.StatusCode != http.StatusOK || body != manifestBody {
		t.Fatalf("restarted cached digest = %d %q", resp.StatusCode, body)
	}
	if resolves.Load() != 0 || dials.Load() != 0 {
		t.Fatalf("cached replay touched network: resolves=%d dials=%d", resolves.Load(), dials.Load())
	}
	if got := len(restarted.dynamic); got != 0 {
		t.Fatalf("dynamic handlers after cold cached digest = %d, want 0", got)
	}
}

func TestFreshManagerOfflineCachedTagServesAndColdMissDoesNotResolveOrDial(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	cacheRoot := t.TempDir()
	warm := newManagerWithPorts(cacheRoot, nil, freePort(t))
	warm.baseOverride = upstream.URL
	warm.resolveUpstreamIPs = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("203.0.113.10")}, nil
	}
	warm.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	warm.dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, hostPortOfURL(t, upstream.URL))
	}
	if err := warm.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	resp, body := get(t, fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/latest?ns=docker.io", warm.catchAllPort))
	if resp.StatusCode != http.StatusOK || body != manifestBody {
		t.Fatalf("warm tag = %d %q", resp.StatusCode, body)
	}
	cacheTagDigestRoot(t, NewServer("https://registry-1.docker.io", filepath.Join(cacheRoot, "docker.io")), "app", "latest")
	warm.Close()

	var resolves atomic.Int64
	var dials atomic.Int64
	restarted := newManagerWithPorts(cacheRoot, nil, freePort(t))
	restarted.baseOverride = upstream.URL
	restarted.SetOffline(true)
	restarted.resolveUpstreamIPs = func(context.Context, string) ([]net.IP, error) {
		resolves.Add(1)
		return nil, fmt.Errorf("resolver unavailable")
	}
	restarted.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	restarted.dialContext = func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, fmt.Errorf("unexpected dial")
	}
	if err := restarted.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()

	resp, body = get(t, fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/latest?ns=docker.io", restarted.catchAllPort))
	if resp.StatusCode != http.StatusOK || body != manifestBody {
		t.Fatalf("offline restarted cached tag = %d %q", resp.StatusCode, body)
	}
	resp, body = get(t, fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/missing?ns=docker.io", restarted.catchAllPort))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("offline restarted miss = %d %q, want 404", resp.StatusCode, body)
	}
	if resolves.Load() != 0 || dials.Load() != 0 {
		t.Fatalf("offline cold paths touched network: resolves=%d dials=%d", resolves.Load(), dials.Load())
	}
	if got := len(restarted.dynamic); got != 0 {
		t.Fatalf("dynamic handlers after offline cold paths = %d, want 0", got)
	}
}

func TestFreshManagerCacheProbeDoesNotBypassMethodGuard(t *testing.T) {
	validDigest := "sha256:" + sha256Hex([]byte(manifestBody))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", validDigest)
		_, _ = fmt.Fprint(w, manifestBody)
	}))
	defer upstream.Close()

	cacheRoot := t.TempDir()
	warm := newManagerWithPorts(cacheRoot, nil, freePort(t))
	warm.baseOverride = upstream.URL
	warm.resolveUpstreamIPs = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("203.0.113.10")}, nil
	}
	warm.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	warm.dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, hostPortOfURL(t, upstream.URL))
	}
	if err := warm.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/%s?ns=docker.io", warm.catchAllPort, url.QueryEscape(validDigest))
	_, _ = get(t, path)
	warm.Close()

	var resolves atomic.Int64
	restarted := newManagerWithPorts(cacheRoot, nil, freePort(t))
	restarted.baseOverride = upstream.URL
	restarted.resolveUpstreamIPs = func(context.Context, string) ([]net.IP, error) {
		resolves.Add(1)
		return nil, fmt.Errorf("resolver unavailable")
	}
	restarted.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	restarted.dialContext = func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("unexpected dial")
		return nil, nil
	}
	if err := restarted.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()

	request, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/%s?ns=docker.io", restarted.catchAllPort, url.QueryEscape(validDigest)), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST cached digest = %d, want 405", resp.StatusCode)
	}
	if resolves.Load() != 0 {
		t.Fatalf("POST method guard touched resolver %d times", resolves.Load())
	}
}

func TestFreshManagerVersionPingStillAnswersLocallyWithoutDNS(t *testing.T) {
	var resolves atomic.Int64
	restarted := newManagerWithPorts(t.TempDir(), nil, freePort(t))
	restarted.SetOffline(true)
	restarted.resolveUpstreamIPs = func(context.Context, string) ([]net.IP, error) {
		resolves.Add(1)
		return nil, fmt.Errorf("resolver unavailable")
	}
	restarted.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	restarted.dialContext = func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("unexpected dial")
		return nil, nil
	}
	if err := restarted.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()

	resp, body := get(t, fmt.Sprintf("http://127.0.0.1:%d/v2/?ns=docker.io", restarted.catchAllPort))
	if resp.StatusCode != http.StatusOK || body != "" {
		t.Fatalf("version ping = %d %q, want 200 empty", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Docker-Distribution-API-Version"); got != "registry/2.0" {
		t.Fatalf("version ping header = %q, want registry/2.0", got)
	}
	if resolves.Load() != 0 {
		t.Fatalf("version ping touched resolver %d times", resolves.Load())
	}
}

func TestFreshManagerOfflineCorruptCachedDigestFailsWithoutNetworkAndOnlineRefetches(t *testing.T) {
	validDigest := "sha256:" + sha256Hex([]byte(manifestBody))
	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", validDigest)
		_, _ = fmt.Fprint(w, manifestBody)
	}))
	defer upstream.Close()

	cacheRoot := t.TempDir()
	warm := newManagerWithPorts(cacheRoot, nil, freePort(t))
	warm.baseOverride = upstream.URL
	warm.resolveUpstreamIPs = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("203.0.113.10")}, nil
	}
	warm.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	warm.dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, hostPortOfURL(t, upstream.URL))
	}
	if err := warm.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/%s?ns=docker.io", warm.catchAllPort, url.QueryEscape(validDigest))
	resp, body := get(t, path)
	if resp.StatusCode != http.StatusOK || body != manifestBody {
		t.Fatalf("warm digest = %d %q", resp.StatusCode, body)
	}
	warm.Close()

	corruptPath := NewServer("https://registry-1.docker.io", filepath.Join(cacheRoot, "docker.io")).manifestPath("/v2/app/manifests/" + validDigest)
	if err := os.WriteFile(corruptPath, []byte("corrupt-manifest"), 0o644); err != nil {
		t.Fatal(err)
	}

	var resolves atomic.Int64
	var dials atomic.Int64
	offline := newManagerWithPorts(cacheRoot, nil, freePort(t))
	offline.baseOverride = upstream.URL
	offline.SetOffline(true)
	offline.resolveUpstreamIPs = func(context.Context, string) ([]net.IP, error) {
		resolves.Add(1)
		return nil, fmt.Errorf("resolver unavailable")
	}
	offline.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	offline.dialContext = func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, fmt.Errorf("unexpected dial")
	}
	if err := offline.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	resp, body = get(t, fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/%s?ns=docker.io", offline.catchAllPort, url.QueryEscape(validDigest)))
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "cached digest corrupted") {
		t.Fatalf("offline corrupt replay = %d %q", resp.StatusCode, body)
	}
	if resolves.Load() != 0 || dials.Load() != 0 {
		t.Fatalf("offline corrupt replay touched network: resolves=%d dials=%d", resolves.Load(), dials.Load())
	}
	if got := len(offline.dynamic); got != 0 {
		t.Fatalf("dynamic handlers after offline corrupt replay = %d, want 0", got)
	}
	offline.Close()

	online := newManagerWithPorts(cacheRoot, nil, freePort(t))
	online.baseOverride = upstream.URL
	online.resolveUpstreamIPs = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("203.0.113.10")}, nil
	}
	online.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	online.dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, hostPortOfURL(t, upstream.URL))
	}
	if err := online.Bind("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	defer online.Close()
	hitsBefore := upstreamHits.Load()
	resp, body = get(t, fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/%s?ns=docker.io", online.catchAllPort, url.QueryEscape(validDigest)))
	if resp.StatusCode != http.StatusOK || body != manifestBody {
		t.Fatalf("online refetch after corruption = %d %q", resp.StatusCode, body)
	}
	if upstreamHits.Load() != hitsBefore+1 {
		t.Fatalf("online refetch did not hit upstream: %d -> %d", hitsBefore, upstreamHits.Load())
	}
}

func TestCatchAllMirrorCancellationStopsNamespaceValidation(t *testing.T) {
	catchAllPort := freePort(t)
	resolveStarted := make(chan struct{})
	resolveReleased := make(chan struct{})
	var dials atomic.Int64

	m := newManagerWithPorts(t.TempDir(), nil, catchAllPort)
	m.resolveUpstreamIPs = func(ctx context.Context, host string) ([]net.IP, error) {
		if host != "docker.io" {
			t.Fatalf("resolve host = %q, want docker.io", host)
		}
		close(resolveStarted)
		<-ctx.Done()
		close(resolveReleased)
		return nil, ctx.Err()
	}
	m.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	m.dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		return nil, fmt.Errorf("unexpected dial %s %s", network, address)
	}
	defer m.Close()

	requestContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/v2/app/manifests/latest?ns=docker.io", catchAllPort), nil).WithContext(requestContext)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		m.serveCatchAll(recorder, request)
		close(done)
	}()

	<-resolveStarted
	cancel()

	select {
	case <-resolveReleased:
	case <-time.After(time.Second):
		t.Fatal("namespace validation did not observe request cancellation")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("catch-all handler did not return promptly after cancellation")
	}
	if dials.Load() != 0 {
		t.Fatalf("validation cancellation still dialed upstream %d times, want 0", dials.Load())
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

func TestCatchAllServesStaleTagOnResolutionFailure(t *testing.T) {
	cacheRoot := t.TempDir()
	_, manifest, _ := newCompleteStaleFixtureAt(t, filepath.Join(cacheRoot, "registry.example"))
	manager := newManagerWithPorts(cacheRoot, nil, 0)
	manager.resolveUpstreamIPs = func(context.Context, string) ([]net.IP, error) {
		return nil, &net.DNSError{Err: "no such host", Name: "registry.example"}
	}
	manager.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	manager.dialContext = func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("unexpected dial on stale replay")
		return nil, nil
	}
	defer manager.Close()

	logged := captureDaemonLog(t)
	authority, err := parseUpstreamAuthority("registry.example")
	if err != nil {
		t.Fatal(err)
	}
	if stale := manager.cacheProbeServer(authority).staleCandidate(httptest.NewRequest(http.MethodGet, "/v2/demo/manifests/stable", nil)); !stale.Complete() {
		t.Fatalf("stale candidate = %s", stale.Reason())
	}
	request := httptest.NewRequest(http.MethodGet, "/v2/demo/manifests/stable?ns=registry.example", nil)
	recorder := httptest.NewRecorder()
	manager.serveCatchAll(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != string(manifest) {
		t.Fatalf("stale catch-all = %d %q, want 200 %q", recorder.Code, recorder.Body.String(), manifest)
	}
	if got := recorder.Header().Get(reasonHeader); got != "served-stale" {
		t.Fatalf("%s = %q, want served-stale", reasonHeader, got)
	}
	if line := logged.String(); !strings.Contains(line, "mirror served stale: registry.example/demo:stable") || !strings.Contains(line, "no such host") {
		t.Fatalf("daemon log = %q, want stale resolution replay", line)
	}
}

func TestCatchAllDoesNotServeStaleTagOnPolicyRejection(t *testing.T) {
	cacheRoot := t.TempDir()
	_, _, _ = newCompleteStaleFixtureAt(t, filepath.Join(cacheRoot, "registry.example"))
	manager := newManagerWithPorts(cacheRoot, nil, 0)
	manager.resolveUpstreamIPs = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	manager.hostOwnedIPs = func() ([]net.IP, error) { return nil, nil }
	defer manager.Close()

	request := httptest.NewRequest(http.MethodGet, "/v2/demo/manifests/stable?ns=registry.example", nil)
	recorder := httptest.NewRecorder()
	manager.serveCatchAll(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("policy rejection = %d %q, want 403", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), manifestBody) {
		t.Fatalf("policy rejection served cached manifest: %q", recorder.Body.String())
	}
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

type closableWrappedHandler struct {
	http.Handler
	closed *atomic.Int64
}

func (h *closableWrappedHandler) CloseIdleConnections() {
	h.closed.Add(1)
}

type countingIdleTransport struct {
	closed atomic.Int64
}

func (t *countingIdleTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("unexpected round trip")
}

func (t *countingIdleTransport) CloseIdleConnections() {
	t.closed.Add(1)
}

func freePort(t *testing.T) int {
	t.Helper()
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	p := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return p
}
