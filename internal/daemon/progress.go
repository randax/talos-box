package daemon

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/randax/talos-box/internal/shellquote"
)

// stageFunc narrates one stage of an in-flight operation to the client that
// asked for progress. The nil stageFunc is the silent default, so an operation
// can narrate without knowing whether anyone is listening — which is what lets
// the same code serve a narrating request, an older CLI, and a test.
type stageFunc func(string)

// stage narrates one line. A nil stageFunc drops it.
func (f stageFunc) stage(format string, args ...any) {
	if f == nil {
		return
	}
	f(fmt.Sprintf(format, args...))
}

// progressSink writes stage responses onto the request's own connection, ahead
// of its single final response. It is closed before that response is written,
// so a straggling goroutine can never interleave narration with the result.
type progressSink struct {
	mu      sync.Mutex
	encoder *json.Encoder
	closed  bool
}

func (p *progressSink) emit(stage string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	// A client that stopped reading must not fail the operation it asked
	// about: the write error surfaces again when the result is sent.
	_ = p.encoder.Encode(Response{Stage: stage})
}

func (p *progressSink) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
}

// formatBudget renders a phase budget the compact way the CLI renders its own
// deadline ("10m", "45s"), so a named phase budget and the request-wide bound
// read as comparable numbers in the same stream (#423).
func formatBudget(budget time.Duration) string {
	if budget >= time.Minute && budget%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(budget/time.Minute))
	}
	return fmt.Sprintf("%ds", int(budget.Round(time.Second).Seconds()))
}

// convergenceHint is the closing narration of a verb that left the cluster
// booting: the operation is done, the nodes are not, and status is where that
// is watched (#273). The name is quoted: the line is meant to be pasted, and a
// cluster name may carry shell metacharacters.
func convergenceHint(clusterName string) string {
	return fmt.Sprintf("nodes are booting; watch them converge with: tbx status %s", shellquote.Quote(clusterName))
}

// stoppedNodeHint is the closing narration of a `node stop` that left the
// cluster running: the VM is off when the verb answers, but the members that
// stay up need a moment to agree it is gone, and status is where that is
// watched (#414).
func stoppedNodeHint(clusterName string) string {
	return fmt.Sprintf("the cluster reconverges without it; watch it with: tbx status %s", shellquote.Quote(clusterName))
}

// opWaitNarrationInterval is how often a request queued behind the daemon's
// operation lock repeats that it is still waiting. It is a var only so tests
// can shorten it.
var opWaitNarrationInterval = 30 * time.Second

// clusterStages narrates on behalf of one cluster inside a multi-cluster pass,
// naming it on every line so an operator watching an `up` can tell whose image
// fetch or boot the stage belongs to. A nil sink stays nil, so a pass nobody
// listens to keeps costing nothing.
func clusterStages(progress stageFunc, clusterName string) stageFunc {
	if progress == nil {
		return nil
	}
	return func(line string) {
		progress(clusterName + ": " + line)
	}
}

// lockOperation takes the daemon's operation lock, narrating the wait. The
// client's liveness bound measures silence and is re-armed by every stage, so a
// verb queued behind a long-running operation would otherwise count its queue
// time as silence and fail with "tbxd stopped reporting progress" while the
// daemon is healthy and about to serve it (#392).
func (s *Server) lockOperation(progress stageFunc) {
	lockNarrated(&s.opMu, progress, "the daemon's current operation")
}

// lockClusterMutation takes one cluster's mutation lock, narrating the wait for
// the same reason lockOperation does: a create holds it across a boot wait
// measured in minutes, and the verb queued behind it must keep proving it is
// alive.
func (s *Server) lockClusterMutation(clusterName string, progress stageFunc) *sync.Mutex {
	lock := s.clusterMutationLock(clusterName)
	lockNarrated(lock, progress, fmt.Sprintf("the operation in flight on %s", clusterName))
	return lock
}

// lockNarrated takes mu, saying what it is waiting on while it is contended and
// repeating that on a ticker until the lock is held. A nil sink means nobody is
// listening, so the wait is silent; an uncontended lock narrates nothing.
func lockNarrated(mu *sync.Mutex, progress stageFunc, what string) {
	if progress == nil {
		mu.Lock()
		return
	}
	if mu.TryLock() {
		return
	}
	progress.stage("waiting for %s to finish", what)
	acquired := make(chan struct{})
	go func() {
		mu.Lock()
		close(acquired)
	}()
	ticker := time.NewTicker(opWaitNarrationInterval)
	defer ticker.Stop()
	for {
		select {
		case <-acquired:
			return
		case <-ticker.C:
			progress.stage("still waiting for %s to finish", what)
		}
	}
}
