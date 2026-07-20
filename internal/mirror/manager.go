package mirror

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"

	"github.com/randax/talos-box/internal/manifests"
)

// baseFor maps an upstream name to its real registry API base.
func baseFor(upstream string) string {
	if upstream == "docker.io" {
		return "https://registry-1.docker.io"
	}
	return "https://" + upstream
}

// StartAll serves one pull-through mirror per manifests.MirrorPorts entry.
//
// NOTE: listeners currently bind 0.0.0.0, so guests reach them via their
// gateway IP but the ports are also exposed on the host's other interfaces.
// SPEC §5 wants them bound to the cluster gateway IPs only; doing so needs
// per-cluster rebinding as clusters come and go (gateway IPs exist only while
// a cluster's vmnet interface is up) — tracked as a follow-up. The mirror
// serves anonymous public images read-only, so LAN exposure is low-risk.
//
// Returns a stop function; a port already in use fails startup (another
// daemon or AirPlay-style squatter — surface it, don't half-serve).
func StartAll(cacheRoot string) (stop func(), err error) {
	var servers []*http.Server
	stop = func() {
		for _, server := range servers {
			_ = server.Close()
		}
	}
	for _, entry := range manifests.MirrorPorts {
		listener, listenErr := net.Listen("tcp", fmt.Sprintf(":%d", entry.Port))
		if listenErr != nil {
			stop()
			return nil, fmt.Errorf("mirror %s on :%d: %w", entry.Upstream, entry.Port, listenErr)
		}
		server := &http.Server{
			Handler: NewServer(baseFor(entry.Upstream), filepath.Join(cacheRoot, entry.Upstream)),
		}
		servers = append(servers, server)
		go func() {
			if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				_ = serveErr // listener owned by Close; nothing to do
			}
		}()
	}
	return stop, nil
}

// DefaultDir is the mirror cache root under the talosbox cache.
func DefaultDir(cacheRoot string) string {
	return filepath.Join(cacheRoot, "mirror")
}
