package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/shellquote"
)

// NodeBootTimeout bounds how long a create waits for the nodes it started to
// answer. It is the stall observer's own threshold: a node still silent at 3×
// the promised boot window is stuck, not slow (#288), and holding the request
// past that point only trades one silence for another. It is exported because
// the CLI states a create's deadline up front, and this wait runs before the
// provisioning budget starts (#307).
const NodeBootTimeout = nodeStallThreshold

var (
	// nodeBootTimeout is the budget the wait actually honours. A var so tests
	// do not have to sleep through a real boot.
	nodeBootTimeout = NodeBootTimeout
	// nodeBootPollInterval is how often the wait re-probes. A var so tests do
	// not have to sleep through a real boot.
	nodeBootPollInterval = 2 * time.Second
)

// waitForNodesBooted blocks until every node of the cluster answers on apid —
// maintenance or configured — so create's past-tense success line is true when
// it is printed instead of racing the boot it started (#263). It narrates each
// node as it comes up and returns an advisory finding if the budget runs out:
// the cluster is created either way, and refusing it because a node booted
// slowly would destroy work the operator already paid for.
//
// The wait answers to the daemon lifecycle, not just the wall clock: it runs on
// the request goroutine Shutdown waits for, so a shutdown that could not
// interrupt it would hold the daemon for the full boot budget and risk the
// supervisor killing the VMs hard instead of closing them.
//
// It also answers to the cluster's other operators: a destroy queued on the
// cluster's mutation lock interrupts the wait, because nodes finishing their
// boot is not something a cluster about to be deleted needs, and the create
// must not hold the destroy behind a budget nobody wants spent.
func (s *Server) waitForNodesBooted(name string, progress stageFunc) string {
	deadline, cancelDeadline := s.lifecycleTimeoutContext(nodeBootTimeout)
	defer cancelDeadline()
	// The cause distinguishes the three ends this wait can come to — budget,
	// shutdown, handover — which the warning has to report apart.
	ctx, cancel := context.WithCancelCause(deadline)
	defer cancel(nil)
	release := s.preemptions.register(name, func() { cancel(errBootWaitPreempted) })
	defer release()
	return s.waitForNodesBootedContext(ctx, name, progress)
}

// errBootWaitPreempted is the wait's end when another operation is queued on
// the cluster's mutation lock. The create still succeeded — the substrate is
// there — but it says so with the interruption named, so an operator whose
// destroy took the cluster next is not handed an unqualified success for it.
var errBootWaitPreempted = errors.New("another operation was waiting for the cluster")

// waitForNodesBootedContext is waitForNodesBooted with an injectable context,
// so both the boot budget and the shutdown signal arrive through one channel.
func (s *Server) waitForNodesBootedContext(ctx context.Context, name string, progress stageFunc) string {
	item, err := cluster.Load(name)
	if err != nil {
		// Nothing to wait on that we can name; the operation's own result
		// already reported what it did.
		return ""
	}
	if len(item.Nodes) == 0 {
		return ""
	}
	lookupIP := s.nodeIPLookup
	if lookupIP == nil {
		lookupIP = cluster.LookupIP
	}
	probe := s.nodeProbe
	if probe == nil {
		// Bind the real probe to this wait's context so no single dial can
		// outlive the budget: probing is the sweep's whole cost, and a cluster
		// large enough to matter has more silent nodes than the budget has dial
		// timeouts to spend on them.
		probe = func(ip string) ProbeResult { return probeAPIDContext(ctx, ip) }
	}
	progress.stage("waiting for %d node(s) to boot", len(item.Nodes))
	pending := append([]cluster.Node(nil), item.Nodes...)
	for {
		remaining := make([]cluster.Node, 0, len(pending))
		for i, node := range pending {
			// A sweep is not free — every silent node costs a dial timeout — so
			// the deadline and the shutdown signal are observed between nodes,
			// not only after the whole pass. The nodes this sweep never reached
			// are unanswered as far as the operator is concerned.
			if err := context.Cause(ctx); err != nil {
				remaining = append(remaining, pending[i:]...)
				return unansweredNodesWarning(item, remaining, err)
			}
			// The VM map is daemon state: read it under the operation lock,
			// which this wait deliberately does not hold — a boot takes
			// minutes, and status must stay answerable throughout.
			s.opMu.Lock()
			running := s.nodeRunning(item.Name, node.Name)
			s.opMu.Unlock()
			status := nodeStatusWith(node, item.SubnetIndex, running, lookupIP, probe)
			if status.Phase == PhaseMaintenance || status.Phase == PhaseConfigured {
				progress.stage("node %s reached %s", node.Name, status.Phase)
				continue
			}
			remaining = append(remaining, node)
		}
		pending = remaining
		if len(pending) == 0 {
			progress.stage("all %d node(s) booted", len(item.Nodes))
			return ""
		}
		if err := context.Cause(ctx); err != nil {
			return unansweredNodesWarning(item, pending, err)
		}
		timer := time.NewTimer(nodeBootPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			// A cancelled lifecycle means the daemon is going down and the
			// VMs still need closing; give the create its advisory answer now
			// rather than holding shutdown for the rest of the budget.
			return unansweredNodesWarning(item, pending, context.Cause(ctx))
		case <-timer.C:
		}
	}
}

// unansweredNodesWarning names the nodes still silent when the wait ended, and
// says which end it was: a blown budget is the node's problem, a cancelled
// lifecycle is the daemon's, and conflating them would misreport both.
func unansweredNodesWarning(item cluster.Cluster, pending []cluster.Node, cause error) string {
	names := make([]string, 0, len(pending))
	for _, node := range pending {
		names = append(names, node.Name)
	}
	ended := fmt.Sprintf("after %s", formatBootWindow(nodeBootTimeout))
	switch {
	case errors.Is(cause, errBootWaitPreempted):
		ended = "before another operation on the cluster interrupted the wait"
	case errors.Is(cause, context.Canceled):
		ended = "before the daemon stopped waiting"
	}
	// The name is quoted: a cluster name may contain shell metacharacters,
	// and this line invites a paste (SPEC §10).
	return fmt.Sprintf("%d of %d node(s) had not answered %s (%s); watch them with: tbx status %s",
		len(pending), len(item.Nodes), ended, strings.Join(names, ", "), shellquote.Quote(item.Name))
}
