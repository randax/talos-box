package mirror

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
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
	cacheRoot    string
	ports        []portBinding
	baseOverride string // tests only: point every upstream at one fake registry

	mu    sync.Mutex
	bound map[string][]*http.Server // gateway IP -> its servers
}

// NewManager mirrors manifests.MirrorPorts, caching under cacheRoot.
func NewManager(cacheRoot string) *Manager {
	ports := make([]portBinding, len(manifests.MirrorPorts))
	for i, e := range manifests.MirrorPorts {
		ports[i] = portBinding{Upstream: e.Upstream, Port: e.Port}
	}
	return newManagerWithPorts(cacheRoot, ports)
}

func newManagerWithPorts(cacheRoot string, ports []portBinding) *Manager {
	return &Manager{cacheRoot: cacheRoot, ports: ports, bound: map[string][]*http.Server{}}
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
