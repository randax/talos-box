package mirror

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	"github.com/randax/talos-box/internal/manifests"
)

// portBinding is one upstream registry served on a fixed port.
type portBinding struct {
	Upstream string
	Port     int
}

// baseFor maps an upstream name to its real registry API base.
func baseFor(upstream string) string {
	if upstream == "docker.io" {
		return "https://registry-1.docker.io"
	}
	return "https://" + upstream
}

// Manager serves pull-through mirrors bound to cluster gateway IPs, adding and
// removing a gateway's bind set as its cluster starts and stops — so the mirror
// ports are reachable from guests but never exposed on the host's other
// interfaces (SPEC §5). One listener per (gateway, upstream port).
type Manager struct {
	cacheRoot     string
	ports         []portBinding
	catchAllPort  int
	baseOverride  string // tests only: point every upstream at one fake registry
	serverFactory func(upstream, base, cacheDir string) http.Handler

	mu      sync.Mutex
	bound   map[string][]*http.Server // gateway IP -> its servers
	dynamic map[string]http.Handler
}

// NewManager mirrors manifests.MirrorPorts, caching under cacheRoot.
func NewManager(cacheRoot string) *Manager {
	ports := make([]portBinding, len(manifests.MirrorPorts))
	for i, e := range manifests.MirrorPorts {
		ports[i] = portBinding{Upstream: e.Upstream, Port: e.Port}
	}
	return newManagerWithPorts(cacheRoot, ports, manifests.CatchAllPort)
}

func newManagerWithPorts(cacheRoot string, ports []portBinding, catchAllPort int) *Manager {
	return &Manager{
		cacheRoot:    cacheRoot,
		ports:        ports,
		catchAllPort: catchAllPort,
		bound:        map[string][]*http.Server{},
		dynamic:      map[string]http.Handler{},
	}
}

// Bind starts the mirror listeners on gatewayIP, idempotently. On any listen
// failure it rolls back the partial bind for this gateway.
func (m *Manager) Bind(gatewayIP string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.bound[gatewayIP]; ok {
		return nil
	}
	var servers []*http.Server
	rollback := func() {
		for _, s := range servers {
			_ = s.Close()
		}
	}
	for _, entry := range m.ports {
		listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", gatewayIP, entry.Port))
		if err != nil {
			rollback()
			return fmt.Errorf("mirror %s on %s:%d: %w", entry.Upstream, gatewayIP, entry.Port, err)
		}
		base := baseFor(entry.Upstream)
		if m.baseOverride != "" {
			base = m.baseOverride
		}
		server := &http.Server{Handler: NewServer(base, filepath.Join(m.cacheRoot, entry.Upstream))}
		if m.serverFactory != nil {
			server.Handler = m.serverFactory(entry.Upstream, base, filepath.Join(m.cacheRoot, entry.Upstream))
		}
		servers = append(servers, server)
		go func() {
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				_ = err
			}
		}()
	}
	if m.catchAllPort != 0 {
		listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", gatewayIP, m.catchAllPort))
		if err != nil {
			rollback()
			return fmt.Errorf("mirror catch-all on %s:%d: %w", gatewayIP, m.catchAllPort, err)
		}
		server := &http.Server{Handler: http.HandlerFunc(m.serveCatchAll)}
		servers = append(servers, server)
		go func() {
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				_ = err
			}
		}()
	}
	m.bound[gatewayIP] = servers
	return nil
}

// Unbind stops a gateway's listeners; unknown gateways are a no-op.
func (m *Manager) Unbind(gatewayIP string) {
	m.mu.Lock()
	servers := m.bound[gatewayIP]
	delete(m.bound, gatewayIP)
	m.mu.Unlock()
	for _, s := range servers {
		_ = s.Close()
	}
}

// Close stops every gateway's listeners.
func (m *Manager) Close() {
	m.mu.Lock()
	all := m.bound
	m.bound = map[string][]*http.Server{}
	m.mu.Unlock()
	for _, servers := range all {
		for _, s := range servers {
			_ = s.Close()
		}
	}
}

// DefaultDir is the mirror cache root under the talosbox cache.
func DefaultDir(cacheRoot string) string {
	return filepath.Join(cacheRoot, "mirror")
}

func (m *Manager) serveCatchAll(w http.ResponseWriter, r *http.Request) {
	upstream := strings.TrimSpace(r.URL.Query().Get("ns"))
	if upstream == "" {
		http.Error(w, "missing ns query parameter", http.StatusBadRequest)
		return
	}
	if err := validateUpstreamNamespace(upstream); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	handler := m.handlerForUpstream(upstream)
	clone := r.Clone(r.Context())
	clone.URL = cloneURLWithoutQueryValue(r.URL, "ns")
	handler.ServeHTTP(w, clone)
}

func (m *Manager) handlerForUpstream(upstream string) http.Handler {
	m.mu.Lock()
	defer m.mu.Unlock()
	if handler, ok := m.dynamic[upstream]; ok {
		return handler
	}

	base := baseFor(upstream)
	if m.baseOverride != "" {
		base = m.baseOverride
	}
	cacheDir := filepath.Join(m.cacheRoot, upstream)
	handler := http.Handler(NewServer(base, cacheDir))
	if m.serverFactory != nil {
		handler = m.serverFactory(upstream, base, cacheDir)
	}
	m.dynamic[upstream] = handler
	return handler
}

func cloneURLWithoutQueryValue(source *url.URL, key string) *url.URL {
	clone := *source
	query := clone.Query()
	query.Del(key)
	clone.RawQuery = query.Encode()
	return &clone
}

func validateUpstreamNamespace(upstream string) error {
	ips, err := lookupNamespaceIPs(upstream)
	if err != nil {
		return fmt.Errorf("resolve ns %q: %w", upstream, err)
	}
	hostIPs, err := hostOwnedIPs()
	if err != nil {
		return fmt.Errorf("inspect host addresses: %w", err)
	}
	for _, ip := range ips {
		if namespaceIPBlocked(ip, hostIPs) {
			return fmt.Errorf("refuse ns %q: address %s is not public", upstream, ip.String())
		}
	}
	return nil
}

func lookupNamespaceIPs(host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	return net.DefaultResolver.LookupIP(context.Background(), "ip", host)
}

func hostOwnedIPs() ([]net.IP, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		switch value := addr.(type) {
		case *net.IPNet:
			ips = append(ips, value.IP)
		case *net.IPAddr:
			ips = append(ips, value.IP)
		}
	}
	return ips, nil
}

func namespaceIPBlocked(ip net.IP, hostIPs []net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || inCGNATRange(ip) {
		return true
	}
	for _, hostIP := range hostIPs {
		if hostIP.Equal(ip) {
			return true
		}
	}
	return false
}

func inCGNATRange(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	return ip4[0] == 100 && ip4[1]&0b1100_0000 == 0b0100_0000
}
