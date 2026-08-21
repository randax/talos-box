package mirror

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

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
	cacheRoot          string
	ports              []portBinding
	catchAllPort       int
	baseOverride       string // tests only: point every upstream at one fake registry
	serverFactory      func(upstream, base, cacheDir string) http.Handler
	resolveUpstreamIPs func(context.Context, string) ([]net.IP, error)
	hostOwnedIPs       func() ([]net.IP, error)
	dialContext        func(context.Context, string, string) (net.Conn, error)
	offline            atomic.Bool

	mu                sync.Mutex
	bound             map[string][]*http.Server // gateway IP -> its servers
	dynamic           map[string]http.Handler
	dynamicServers    map[string]*Server
	dynamicOrder      *list.List
	dynamicLRUEntries map[string]*list.Element
	dynamicClosers    map[string]func()
	dynamicCap        int
	warmTagMu         sync.Mutex
}

const dynamicHandlerCap = 64

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
		resolveUpstreamIPs: func(ctx context.Context, host string) ([]net.IP, error) {
			return lookupNamespaceIPs(ctx, host)
		},
		hostOwnedIPs:   hostOwnedIPs,
		dialContext:    defaultEgressDependencies().dialContext,
		bound:          map[string][]*http.Server{},
		dynamic:        map[string]http.Handler{},
		dynamicServers: map[string]*Server{},
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
		mirrorServer := NewServer(base, filepath.Join(m.cacheRoot, entry.Upstream))
		mirrorServer.namespace = entry.Upstream
		mirrorServer.offline = &m.offline
		server := &http.Server{Handler: mirrorServer}
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
	closers := m.drainDynamicLocked()
	m.mu.Unlock()
	for _, servers := range all {
		for _, s := range servers {
			_ = s.Close()
		}
	}
	for _, closer := range closers {
		closer()
	}
}

// BoundGatewayIPs reports the currently bound gateway IPs as a sorted copy.
func (m *Manager) BoundGatewayIPs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	gateways := make([]string, 0, len(m.bound))
	for gateway := range m.bound {
		gateways = append(gateways, gateway)
	}
	sort.Strings(gateways)
	return gateways
}

// DefaultDir is the mirror cache root under the talosbox cache.
func DefaultDir(cacheRoot string) string {
	return filepath.Join(cacheRoot, "mirror")
}

func (m *Manager) serveCatchAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "mirror is pull-only", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/v2/" {
		// The version check is the spec's liveness probe and carries no
		// namespace of its own; only the content endpoints below need one.
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.WriteHeader(http.StatusOK)
		return
	}
	authority, err := parseUpstreamAuthority(strings.TrimSpace(r.URL.Query().Get("ns")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	clone := r.Clone(r.Context())
	clone.URL = cloneURLWithoutQueryValue(r.URL, "ns")
	if served, cacheErr := m.probeCacheOnly(authority, w, clone); served {
		return
	} else if cacheErr != nil {
		http.Error(w, cacheErr.Error(), cacheErr.status)
		return
	}
	handler, ok := m.dynamicHandler(authority.cacheKey)
	if !ok {
		if err := m.validateResolvedAuthority(r.Context(), authority); err != nil {
			var validationErr *upstreamValidationError
			if errors.As(err, &validationErr) {
				http.Error(w, validationErr.Error(), validationErr.status)
				return
			}
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		handler = m.handlerForUpstream(authority)
	}
	handler.ServeHTTP(w, clone)
}

func (m *Manager) probeCacheOnly(authority upstreamAuthority, w http.ResponseWriter, r *http.Request) (bool, *cacheReplayError) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false, nil
	}
	if r.URL.Path == "/v2/" {
		return false, nil
	}
	server := m.cacheProbeServer(authority)
	digest := ""
	if match := blobPathRe.FindStringSubmatch(r.URL.Path); match != nil {
		digest = match[1]
	}
	isManifest := manifestPathRe.MatchString(r.URL.Path)
	if served, err := server.serveCacheIfAvailable(w, r, digest, isManifest); served || err != nil {
		return served, err
	}
	if server.offlineEnabled() {
		return false, &cacheReplayError{
			status: http.StatusServiceUnavailable,
			err:    fmt.Errorf("mirror offline: content not cached"),
		}
	}
	return false, nil
}

func (m *Manager) cacheProbeServer(authority upstreamAuthority) *Server {
	base := authority.base
	if m.baseOverride != "" {
		base = m.baseOverride
	}
	server := newServerWithEgress(base, filepath.Join(m.cacheRoot, authority.cacheKey), egressDependencies{
		resolve:     m.resolveUpstreamIPs,
		hostIPs:     m.hostOwnedIPs,
		dialContext: m.dialContext,
		blocked:     namespaceIPBlocked,
	})
	server.namespace = authority.canonicalHost
	server.offline = &m.offline
	return server
}

type upstreamAuthority struct {
	cacheKey           string
	canonicalHost      string
	canonicalAuthority string
	base               string
}

func parseUpstreamAuthority(raw string) (upstreamAuthority, error) {
	if raw == "" {
		return upstreamAuthority{}, fmt.Errorf("missing ns query parameter")
	}
	if strings.Contains(raw, "://") || strings.ContainsAny(raw, "/?#@") {
		return upstreamAuthority{}, fmt.Errorf("malformed ns authority %q", raw)
	}
	host, port, err := splitAuthorityHostPort(raw)
	if err != nil {
		return upstreamAuthority{}, fmt.Errorf("malformed ns authority %q", raw)
	}
	canonicalHost, err := canonicalizeAuthorityHost(host)
	if err != nil {
		return upstreamAuthority{}, fmt.Errorf("malformed ns authority %q", raw)
	}
	baseHost := canonicalHost
	if canonicalHost == "docker.io" {
		baseHost = "registry-1.docker.io"
	}
	urlHost := canonicalURLHost(baseHost, port)
	canonicalAuthority := canonicalHost
	if port != "" {
		canonicalAuthority = net.JoinHostPort(canonicalHost, port)
		if strings.Contains(canonicalHost, ":") {
			canonicalAuthority = "[" + canonicalHost + "]:" + port
		}
	}
	return upstreamAuthority{
		cacheKey:           safeCacheKey(canonicalHost, port),
		canonicalHost:      canonicalHost,
		canonicalAuthority: canonicalAuthority,
		base:               "https://" + urlHost,
	}, nil
}

func (m *Manager) validateResolvedAuthority(ctx context.Context, authority upstreamAuthority) error {
	ips, err := m.resolveUpstreamIPs(ctx, authority.canonicalHost)
	if err != nil {
		return &upstreamValidationError{status: http.StatusBadGateway, err: fmt.Errorf("resolve ns %q: %w", authority.canonicalAuthority, err)}
	}
	hostIPs, err := m.hostOwnedIPs()
	if err != nil {
		return &upstreamValidationError{status: http.StatusBadGateway, err: fmt.Errorf("inspect host addresses: %w", err)}
	}
	for _, ip := range ips {
		if namespaceIPBlocked(ip, hostIPs) {
			return &upstreamValidationError{status: http.StatusForbidden, err: fmt.Errorf("refuse ns %q: address %s is not public", authority.canonicalAuthority, ip.String())}
		}
	}
	return nil
}

func (m *Manager) handlerForUpstream(authority upstreamAuthority) http.Handler {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureDynamicStateLocked()
	if handler, ok := m.dynamic[authority.cacheKey]; ok {
		if element := m.dynamicLRUEntries[authority.cacheKey]; element != nil {
			m.dynamicOrder.MoveToBack(element)
		}
		return handler
	}

	base := authority.base
	if m.baseOverride != "" {
		base = m.baseOverride
	}
	cacheDir := filepath.Join(m.cacheRoot, authority.cacheKey)
	var (
		handler        http.Handler
		retainedServer *Server
	)
	if m.serverFactory != nil {
		handler = m.serverFactory(authority.canonicalAuthority, base, cacheDir)
	}
	if configuredServer, ok := handler.(*Server); ok {
		retainedServer = configuredServer
	} else {
		retainedServer = m.newDynamicMirrorServer(base, cacheDir, authority)
		if handler == nil {
			handler = retainedServer
		}
	}
	m.dynamicServers[authority.cacheKey] = retainedServer
	m.dynamic[authority.cacheKey] = handler
	m.dynamicLRUEntries[authority.cacheKey] = m.dynamicOrder.PushBack(authority.cacheKey)
	if closer := dynamicResourceCloser(handler, retainedServer); closer != nil {
		m.dynamicClosers[authority.cacheKey] = closer
	}
	for _, closer := range m.evictDynamicLocked() {
		closer()
	}
	return handler
}

func (m *Manager) newDynamicMirrorServer(base, cacheDir string, authority upstreamAuthority) *Server {
	mirrorServer := newServerWithEgress(base, cacheDir, egressDependencies{
		resolve:     m.resolveUpstreamIPs,
		hostIPs:     m.hostOwnedIPs,
		dialContext: m.dialContext,
		blocked:     namespaceIPBlocked,
	})
	mirrorServer.namespace = authority.canonicalHost
	mirrorServer.offline = &m.offline
	mirrorServer.validateUpstream = func(ctx context.Context) error {
		return m.validateResolvedAuthority(ctx, authority)
	}
	return mirrorServer
}

func (m *Manager) dynamicHandler(cacheKey string) (http.Handler, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureDynamicStateLocked()
	handler, ok := m.dynamic[cacheKey]
	if !ok {
		return nil, false
	}
	if element := m.dynamicLRUEntries[cacheKey]; element != nil {
		m.dynamicOrder.MoveToBack(element)
	}
	return handler, true
}

func (m *Manager) ensureDynamicStateLocked() {
	if m.dynamic == nil {
		m.dynamic = map[string]http.Handler{}
	}
	if m.dynamicServers == nil {
		m.dynamicServers = map[string]*Server{}
	}
	if m.dynamicOrder == nil {
		m.dynamicOrder = list.New()
	}
	if m.dynamicLRUEntries == nil {
		m.dynamicLRUEntries = map[string]*list.Element{}
	}
	if m.dynamicClosers == nil {
		m.dynamicClosers = map[string]func(){}
	}
}

func (m *Manager) effectiveDynamicCap() int {
	if m.dynamicCap > 0 {
		return m.dynamicCap
	}
	return dynamicHandlerCap
}

func (m *Manager) evictDynamicLocked() []func() {
	var closers []func()
	for len(m.dynamic) > m.effectiveDynamicCap() {
		front := m.dynamicOrder.Front()
		if front == nil {
			return closers
		}
		key := front.Value.(string)
		if closer := m.removeDynamicLocked(key); closer != nil {
			closers = append(closers, closer)
		}
	}
	return closers
}

func (m *Manager) drainDynamicLocked() []func() {
	m.ensureDynamicStateLocked()
	keys := make([]string, 0, len(m.dynamic))
	for key := range m.dynamic {
		keys = append(keys, key)
	}
	var closers []func()
	for _, key := range keys {
		if closer := m.removeDynamicLocked(key); closer != nil {
			closers = append(closers, closer)
		}
	}
	return closers
}

func (m *Manager) removeDynamicLocked(key string) func() {
	delete(m.dynamic, key)
	delete(m.dynamicServers, key)
	if element := m.dynamicLRUEntries[key]; element != nil && m.dynamicOrder != nil {
		m.dynamicOrder.Remove(element)
	}
	delete(m.dynamicLRUEntries, key)
	closer := m.dynamicClosers[key]
	delete(m.dynamicClosers, key)
	return closer
}

func dynamicHandlerCloser(handler http.Handler) func() {
	type idleCloser interface {
		CloseIdleConnections()
	}
	if value, ok := handler.(idleCloser); ok {
		return value.CloseIdleConnections
	}
	return nil
}

func dynamicResourceCloser(handler http.Handler, retainedServer *Server) func() {
	var closers []func()
	if retainedServer != nil {
		closers = append(closers, retainedServer.CloseIdleConnections)
	}
	if configuredServer, ok := handler.(*Server); !ok || configuredServer != retainedServer {
		if closer := dynamicHandlerCloser(handler); closer != nil {
			closers = append(closers, closer)
		}
	}
	switch len(closers) {
	case 0:
		return nil
	case 1:
		return closers[0]
	default:
		return func() {
			for _, closer := range closers {
				closer()
			}
		}
	}
}

func (m *Manager) SetOffline(enabled bool) {
	m.offline.Store(enabled)
}

func (m *Manager) Offline() bool {
	return m.offline.Load()
}

func cloneURLWithoutQueryValue(source *url.URL, key string) *url.URL {
	clone := *source
	query := clone.Query()
	query.Del(key)
	clone.RawQuery = query.Encode()
	return &clone
}

func splitAuthorityHostPort(raw string) (string, string, error) {
	if strings.HasPrefix(raw, "[") {
		end := strings.Index(raw, "]")
		if end == -1 {
			return "", "", fmt.Errorf("missing ]")
		}
		host := raw[1:end]
		if end == len(raw)-1 {
			return host, "", nil
		}
		if raw[end+1] != ':' {
			return "", "", fmt.Errorf("unexpected suffix")
		}
		port := raw[end+2:]
		if err := validatePort(port); err != nil {
			return "", "", err
		}
		return host, port, nil
	}
	if addr := net.ParseIP(raw); addr != nil {
		return addr.String(), "", nil
	}
	if host, port, err := net.SplitHostPort(raw); err == nil {
		if err := validatePort(port); err != nil {
			return "", "", err
		}
		return host, port, nil
	}
	if strings.HasSuffix(raw, ":") {
		return "", "", fmt.Errorf("missing port")
	}
	return raw, "", nil
}

func validatePort(port string) error {
	if port == "" {
		return fmt.Errorf("missing port")
	}
	for i := 0; i < len(port); i++ {
		if port[i] < '0' || port[i] > '9' {
			return fmt.Errorf("invalid port")
		}
	}
	value, err := strconv.Atoi(port)
	if err != nil || value < 1 || value > 65535 {
		return fmt.Errorf("invalid port")
	}
	return nil
}

func canonicalizeAuthorityHost(host string) (string, error) {
	if host == "" || strings.ContainsAny(host, " \t\r\n") {
		return "", fmt.Errorf("invalid host")
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	canonical := strings.TrimSuffix(strings.ToLower(host), ".")
	if canonical == "" || strings.Contains(canonical, "..") || strings.Contains(canonical, ":") {
		return "", fmt.Errorf("invalid host")
	}
	if len(canonical) > 253 {
		return "", fmt.Errorf("invalid host")
	}
	labels := strings.Split(canonical, ".")
	for _, label := range labels {
		if !validDNSLabel(label) {
			return "", fmt.Errorf("invalid host")
		}
	}
	return canonical, nil
}

func canonicalURLHost(host, port string) string {
	if port != "" {
		return net.JoinHostPort(host, port)
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

func safeCacheKey(host, port string) string {
	if ip := net.ParseIP(host); ip != nil {
		if ip.To4() != nil {
			if port == "" {
				return "__ipv4_" + host
			}
			return "__ipv4_" + host + "__port_" + port
		}
		escaped := strings.ReplaceAll(host, ":", "-")
		if port == "" {
			return "__ipv6_" + escaped
		}
		return "__ipv6_" + escaped + "__port_" + port
	}
	if port == "" {
		return host
	}
	return host + "__port_" + port
}

func validDNSLabel(label string) bool {
	if len(label) == 0 || len(label) > 63 {
		return false
	}
	for i := 0; i < len(label); i++ {
		character := label[i]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= '0' && character <= '9':
		case character == '-':
			if i == 0 || i == len(label)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func lookupNamespaceIPs(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
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
	if !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || inCGNATRange(ip) {
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
