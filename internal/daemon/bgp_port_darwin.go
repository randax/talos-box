//go:build darwin

package daemon

import (
	"context"
	"os/exec"
	"time"

	"github.com/randax/talos-box/internal/hostport"
)

// bgpPortListeners inventories the host's listeners on the BGP port. netstat is
// the source deliberately: an unprivileged `lsof -iTCP:179` cannot see a
// root-owned socket on macOS, which is what a squatter on this port usually is
// (#359). It is a var so tests do not shell out.
var bgpPortListeners = darwinBGPPortListeners

// bgpPortProbeTimeout bounds the inventory: it runs on the request path of
// `bgp enable`, and a stalled system utility must not hold the verb.
const bgpPortProbeTimeout = 5 * time.Second

func darwinBGPPortListeners() ([]hostport.Listener, error) {
	ctx, cancel := context.WithTimeout(context.Background(), bgpPortProbeTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, "netstat", "-an", "-p", "tcp").Output()
	if err != nil {
		return nil, err
	}
	return hostport.ParseNetstatListeners(output, hostBGPPort), nil
}
