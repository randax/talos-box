package daemon

import (
	"fmt"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/hostport"
)

// hostBGPPort is the well-known BGP port the host speaker binds on each
// cluster's gateway address.
const hostBGPPort = 179

// bgpPortSquatterWarning names a foreign any-address listener on the BGP port.
// `tbx bgp enable` used to report plain success beside one — an orphaned
// `nc -l 179` held `*:179` for a whole QA run — and the silence cost half an
// hour of diagnosis before the squatter was even noticed (#359).
//
// Only a wildcard listener is reported. The speaker binds one cluster gateway
// each, so a gateway-bound listener is tbx's own; a wildcard one sits in front
// of every gateway at once and answers any peer that reaches the port by
// another address. It is an advisory, not a refusal: the gateway bind can still
// succeed next to it, and a mode change that did take effect must not be failed
// for a condition the operator may have intended.
func bgpPortSquatterWarning(item cluster.Cluster) string {
	if bgpPortListeners == nil {
		return ""
	}
	listeners, err := bgpPortListeners()
	if err != nil {
		// An inventory the host would not produce says nothing about the port,
		// and guessing from its absence would be worse than staying quiet.
		return ""
	}
	for _, listener := range listeners {
		if !hostport.Wildcard(listener.Address) {
			continue
		}
		return fmt.Sprintf(
			"another process is listening on every address at port %d, ahead of the host BGP speaker's %s bind (%s); identify it with `sudo lsof -nP -iTCP:%d -sTCP:LISTEN` and stop it",
			hostBGPPort,
			cluster.Gateway(item.SubnetIndex),
			listener.Line,
			hostBGPPort,
		)
	}
	return ""
}
