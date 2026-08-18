package daemon

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A sweep over a large cluster is not free: every silent node costs a dial
// timeout, and a cluster with enough nodes can spend more on one pass than the
// whole budget allows. The wait must therefore observe its context between
// nodes, not only after the complete pass — otherwise a shutdown waits for the
// sweep it interrupted.
func TestBootWaitObservesItsContextBetweenNodes(t *testing.T) {
	service, item := runningLonghornClusterForNodeMutation(t, 1, 2)
	if len(item.Nodes) != 3 {
		t.Fatalf("cluster has %d nodes, want 3 for a multi-node sweep", len(item.Nodes))
	}
	restoreInterval(t, time.Millisecond, time.Minute)
	service.nodeIPLookup = func(string, int) string { return "10.0.0.2" }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var probes int
	service.nodeProbe = func(string) ProbeResult {
		probes++
		// the daemon goes down while the first node is being probed
		cancel()
		return ProbeResult{}
	}

	warning := service.waitForNodesBootedContext(ctx, item.Name, nil)
	if probes != 1 {
		t.Fatalf("probed %d nodes after the context was cancelled, want the sweep to stop at the first", probes)
	}
	// The nodes the sweep never reached are unanswered as far as the operator
	// is concerned, and the advisory has to name all of them.
	if !strings.Contains(warning, "3 of 3 node(s) had not answered") {
		t.Fatalf("warning = %q, want every unprobed node reported as unanswered", warning)
	}
	for _, node := range item.Nodes {
		if !strings.Contains(warning, node.Name) {
			t.Fatalf("warning = %q, want node %s named", warning, node.Name)
		}
	}
}

// The wait's own probe has to answer to the deadline too: bounding the loop
// while each probe blocks on its dial timeouts only moves the overrun.
func TestProbeHonoursACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A blackhole address the dial would otherwise sit on for its full timeout;
	// the cancelled context must beat it. No packet leaves the host.
	if probe := probeAPIDContext(ctx, "127.0.0.1"); probe.Dialed {
		t.Fatalf("probe = %+v against a cancelled context, want no dial", probe)
	}
}
